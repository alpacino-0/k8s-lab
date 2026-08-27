/*
Copyright 2026 Orhan Yavuz.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package gitwrite_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/internal/gitwrite"
)

// Against a real repository on disk rather than a fake. A git writer that is
// tested against a mock proves the mock's behaviour, and the parts that are
// easy to get wrong here — which identity ends up as author, whether a push
// actually moved the remote ref, what happens when nothing changed — are
// exactly the parts a mock would be written to agree with.

const (
	branch       = "main"
	authorEmail  = "orhan@example.test"
	manifestPath = "workload.yaml"
)

var testRef = evidence.Ref{TenantID: "tenant-a", App: "api", Env: "prod"}

// remote makes a bare repository with one commit on main, and returns a URL
// that needs no credentials.
func remote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "state.git")

	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init: %v", err)
	}

	// A bare repository has no worktree, so the first commit is made beside it
	// and pushed. An empty repository also cannot be cloned, so the seed is
	// initialised rather than cloned.
	work := filepath.Join(dir, "seed")
	seed, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatalf("seed init: %v", err)
	}
	if _, err := seed.CreateRemote(&config.RemoteConfig{
		Name: "origin", URLs: []string{bare},
	}); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	tree, err := seed.Worktree()
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("state\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := tree.Add("README.md"); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	who := &object.Signature{Name: "seed", Email: "seed@example.test", When: time.Now()}
	if _, err := tree.Commit("seed", &git.CommitOptions{Author: who, Committer: who}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	// PlainInit's default branch is master; the writer is pointed at main.
	headRef, err := seed.Head()
	if err != nil {
		t.Fatalf("seed head: %v", err)
	}
	if err := seed.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), headRef.Hash()),
	); err != nil {
		t.Fatalf("seed branch: %v", err)
	}
	if err := seed.Push(&git.PushOptions{RefSpecs: []config.RefSpec{
		config.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch),
	}}); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	return bare
}

func newWriter(store evidence.Store) *gitwrite.Writer {
	return &gitwrite.Writer{Evidence: store}
}

func request(target string, render func(string, map[string][]byte) (map[string][]byte, error)) gitwrite.Request {
	return gitwrite.Request{
		Target:  gitwrite.Target{RepoURL: target, Branch: branch, Dir: "apps/api"},
		Author:  gitwrite.Author{ID: "u-1", Name: "Orhan Yavuz", Email: authorEmail},
		Ref:     testRef,
		Tier:    evidence.TierFree,
		Message: "deploy api to prod",
		Render:  render,
	}
}

func manifest(rolloutID string, _ map[string][]byte) (map[string][]byte, error) {
	return map[string][]byte{
		manifestPath: []byte("metadata:\n  annotations:\n    damga.co/rollout: " + rolloutID + "\n"),
	}, nil
}

// head reads the remote's branch tip as a commit.
func head(t *testing.T, url string) *object.Commit {
	t.Helper()
	repo, err := git.PlainOpen(url)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("resolve %s: %v", branch, err)
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	return c
}

// ---------------------------------------------------------------- cases

// The whole point of the package, in one case: the commit reaches the remote,
// the record exists, and the two agree about which commit it was.
func TestDeployPushesAndOpensARecord(t *testing.T) {
	url := remote(t)
	store := memory.New(0)
	w := newWriter(store)

	res, err := w.Deploy(context.Background(), request(url, manifest))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("Deploy returned no commit")
	}

	got := head(t, url)
	if got.Hash.String() != res.CommitSHA {
		t.Errorf("the remote is at %s, Deploy reported %s", got.Hash, res.CommitSHA)
	}
	// The SHA is on the transition rather than in Source, because Source is
	// chained and the record has to exist before the commit does. Asserted so
	// that the evidence page has somewhere to read the running version from —
	// a record that cannot name its own commit answers none of the questions
	// it exists for.
	var found string
	for _, ev := range res.Record.Transitions {
		if ev.Observation.Revision != "" {
			found = ev.Observation.Revision
		}
	}
	if found != res.CommitSHA {
		t.Errorf("no transition names the commit: got %q, pushed %q", found, res.CommitSHA)
	}
	if res.Record.Source.CommitSHA != "" {
		t.Errorf("Source.CommitSHA = %q; it cannot be filled before the commit exists "+
			"and cannot be filled after, because Source is chained", res.Record.Source.CommitSHA)
	}
}

// The distinction git has always had and almost nothing uses. Argo CD reports
// initiatedBy automated on every sync, so this commit is the only place a
// deploy is attached to a person — and if the platform were the author, the
// evidence page would have nothing to say but "the platform did it".
func TestTheUserIsTheAuthorAndThePlatformIsTheCommitter(t *testing.T) {
	url := remote(t)
	w := newWriter(memory.New(0))

	if _, err := w.Deploy(context.Background(), request(url, manifest)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	c := head(t, url)
	if c.Author.Email != authorEmail {
		t.Errorf("author = %q, want the user", c.Author.Email)
	}
	if c.Committer.Email != gitwrite.Platform.Email {
		t.Errorf("committer = %q, want the platform", c.Committer.Email)
	}
	if c.Author.Email == c.Committer.Email {
		t.Error("author and committer are the same identity; the attribution is lost")
	}
}

// The id has to be inside the manifests, because the observer finds the record
// by reading it off the live object. Minting it after the commit would leave
// the cluster carrying objects no record claims.
func TestTheRolloutIDIsAvailableBeforeTheManifestsAreRendered(t *testing.T) {
	url := remote(t)
	store := memory.New(0)
	w := newWriter(store)

	var seen string
	res, err := w.Deploy(context.Background(), request(url, func(id string, _ map[string][]byte) (map[string][]byte, error) {
		seen = id
		return manifest(id, nil)
	}))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if seen == "" {
		t.Fatal("Render was called with no rollout id")
	}
	if seen != string(res.Record.ID) {
		t.Errorf("Render saw %q, the record is %q", seen, res.Record.ID)
	}

	// And it really is in the tree, not just handed to the callback.
	c := head(t, url)
	f, err := c.File("apps/api/" + manifestPath)
	if err != nil {
		t.Fatalf("the manifest is not at the expected path: %v", err)
	}
	body, err := f.Contents()
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}
	if !strings.Contains(body, seen) {
		t.Errorf("the committed manifest does not carry the rollout id:\n%s", body)
	}
}

// A push that never happened must leave a record saying so, not nothing. The
// record is opened before the push for exactly this reason: a process that died
// between a successful push and a later append would produce a deploy with no
// record at all, and evidence that is missing entirely is worse than evidence
// that admits a gap.
func TestAFailedPushIsRecordedAgainstTheRecord(t *testing.T) {
	store := memory.New(0)
	w := newWriter(store)

	req := request(filepath.Join(t.TempDir(), "does-not-exist.git"), manifest)
	res, err := w.Deploy(context.Background(), req)
	if err == nil {
		t.Fatal("Deploy succeeded against a repository that does not exist")
	}
	if res.Record.ID == "" {
		t.Fatal("no record was opened, so the attempt left no trace at all")
	}

	got, getErr := store.Get(context.Background(), res.Record.ID)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if got.State != evidence.StateFailed {
		t.Errorf("state = %q, want failed", got.State)
	}
	if got.Actor.Email != authorEmail {
		t.Errorf("the failed record lost the author: %q", got.Actor.Email)
	}
}

// Redeploying an identical manifest is a legitimate thing to ask for, and it is
// not a commit. Inventing an empty one would put a deploy in the history that
// changed nothing.
func TestAnIdenticalManifestIsNotACommit(t *testing.T) {
	url := remote(t)
	store := memory.New(0)
	w := newWriter(store)

	first, err := w.Deploy(context.Background(), request(url, func(string, map[string][]byte) (map[string][]byte, error) {
		return map[string][]byte{manifestPath: []byte("same\n")}, nil
	}))
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	_, err = w.Deploy(context.Background(), request(url, func(string, map[string][]byte) (map[string][]byte, error) {
		return map[string][]byte{manifestPath: []byte("same\n")}, nil
	}))
	if !errors.Is(err, gitwrite.ErrNoChange) {
		t.Fatalf("second Deploy error = %v, want ErrNoChange", err)
	}

	if got := head(t, url); got.Hash.String() != first.CommitSHA {
		t.Errorf("the remote moved to %s on a no-op deploy", got.Hash)
	}
}

// A deploy no person can be named for is refused rather than defaulted. Quietly
// substituting the platform is exactly how the evidence page ends up unable to
// answer the question an audit opens with.
func TestADeployWithoutAnAuthorIsRefused(t *testing.T) {
	w := newWriter(memory.New(0))
	req := request("irrelevant", manifest)
	req.Author.Email = ""

	if _, err := w.Deploy(context.Background(), req); err == nil {
		t.Fatal("a deploy with no author was accepted")
	} else if !strings.Contains(err.Error(), "attributable") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// The record is pending, and stays that way until something observes the
// cluster. This package knows a commit was pushed and nothing more; claiming
// the deploy succeeded here would be the platform marking its own homework.
func TestTheRecordDoesNotClaimSuccess(t *testing.T) {
	url := remote(t)
	store := memory.New(0)
	w := newWriter(store)

	res, err := w.Deploy(context.Background(), request(url, manifest))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	switch res.Record.State {
	case evidence.StatePending, evidence.StateSyncing:
	default:
		t.Errorf("state = %q after a push; the cluster has not been observed yet", res.Record.State)
	}
}

// Two deploys of the same app must be two records, or a redeploy would
// overwrite the evidence of the one before it.
func TestConsecutiveDeploysAreSeparateRecords(t *testing.T) {
	url := remote(t)
	store := memory.New(0)
	w := newWriter(store)

	first, err := w.Deploy(context.Background(), request(url, func(id string, _ map[string][]byte) (map[string][]byte, error) {
		return map[string][]byte{manifestPath: []byte("one " + id + "\n")}, nil
	}))
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	second, err := w.Deploy(context.Background(), request(url, func(id string, _ map[string][]byte) (map[string][]byte, error) {
		return map[string][]byte{manifestPath: []byte("two " + id + "\n")}, nil
	}))
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	if first.Record.ID == second.Record.ID {
		t.Fatal("both deploys share a record")
	}
	if second.Record.Seq <= first.Record.Seq {
		t.Errorf("seq went %d then %d; the order is lost", first.Record.Seq, second.Record.Seq)
	}
}

// Render is handed what is already committed, which is what makes a deploy
// that changes one field possible without the caller holding a copy of the
// spec. The committed manifest is the state; this is how it is read.
func TestRenderSeesWhatIsAlreadyCommitted(t *testing.T) {
	url := remote(t)
	store := memory.New(0)
	w := newWriter(store)

	// First deploy: the directory does not exist yet, which is the normal
	// case for a new app and must not be an error.
	var sawFirst map[string][]byte
	if _, err := w.Deploy(context.Background(), request(url,
		func(id string, current map[string][]byte) (map[string][]byte, error) {
			sawFirst = current
			return manifest(id, nil)
		})); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	if sawFirst == nil {
		t.Error("Render was handed a nil map for a directory that does not exist yet")
	}
	if len(sawFirst) != 0 {
		t.Errorf("Render saw %v before anything was written", sawFirst)
	}

	// Second deploy: it sees the first one's file, by name and by content.
	var sawSecond map[string][]byte
	if _, err := w.Deploy(context.Background(), request(url,
		func(id string, current map[string][]byte) (map[string][]byte, error) {
			sawSecond = current
			// Something different, or there would be no commit to make.
			return map[string][]byte{manifestPath: []byte("image: two\n")}, nil
		})); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	body, ok := sawSecond[manifestPath]
	if !ok {
		t.Fatalf("Render did not see %s; it saw %v", manifestPath, keysOf(sawSecond))
	}
	if !strings.Contains(string(body), "damga.co/rollout") {
		t.Errorf("Render saw %s but not its contents: %q", manifestPath, body)
	}

	// Keyed relative to Target.Dir, the same way the return value is —
	// otherwise a caller that passes current straight back would write the
	// files one directory deeper on every deploy.
	for name := range sawSecond {
		if strings.Contains(name, "/") {
			t.Errorf("Render saw %q, want a name relative to the directory", name)
		}
	}
	// And it is that app's directory, not the repository root: the seed's
	// README is not this app's business.
	if _, leaked := sawSecond["README.md"]; leaked {
		t.Error("Render saw the repository root rather than the app's directory")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
