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

// Package gitwrite is the only way anything reaches a cluster.
//
// Principle 1: no component — human, panel, CI, automation — writes to the
// cluster directly. Every change is a commit, and Argo CD applies it. This
// package is that commit.
//
// It is also the only moment a deploy has a human attached to it. Argo CD
// reports initiatedBy automated on every sync it performs, so nothing observed
// from the cluster afterwards can say who asked. That is why the evidence
// record is opened here and not later, and why the commit carries the user as
// its author while the platform is only its committer — a distinction git has
// always had and almost nothing uses.
//
// go-git rather than shelling out, and that is not a preference: the published
// image is distroless/static, so there is no shell and no git binary to call.
// v5 rather than v6 because v6 is alpha, and the write path is the last place
// in this product to take a dependency that is still moving.
package gitwrite

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/damgahq/damga/evidence"
)

// Platform is the committer on every commit this package makes. The author is
// always the user; this identity is the one that says which machine turned that
// intent into a commit, which is exactly what git's two-identity model is for.
var Platform = object.Signature{Name: "Damga", Email: "platform@damga.co"}

// Target names where a tenant's state lives. It is a value the control plane
// holds as a row, never something parsed out of a path: the layout of tenant
// repositories has to be changeable without rewriting a single record.
type Target struct {
	// RepoURL is the tenant's state repository. Damga holds the only write
	// credential for it — that is what makes the API the single place a deploy
	// can be authorized, because git's own ACL is the other way in.
	RepoURL string

	// Branch is the ref Argo CD tracks. Empty means the remote's default.
	Branch string

	// Dir is the subtree this app owns inside the repository.
	Dir string

	// Auth is how Damga proves it may push. nil is valid and useful: a local
	// path or a file:// URL needs none, which is what the tests use and what an
	// in-cluster git server would use over a private network.
	Auth transport.AuthMethod
}

// Author is the human. Both fields are copied into the commit and into the
// evidence record, because a record has to stay readable after the account is
// gone — and because "who deployed this" is the question an audit opens with.
type Author struct {
	ID    string
	Name  string
	Email string
}

// Request is one deploy.
type Request struct {
	Target Target
	Author Author
	Ref    evidence.Ref
	Tier   evidence.Tier

	// Message is the commit subject.
	Message string

	// Render produces the files to write, given the id of the record this
	// deploy will be remembered as.
	//
	// A callback rather than a map, because the id has to be inside the
	// manifests: the observer finds a record by reading damga.co/rollout off
	// the live object, and it can only be there if it was known before the
	// manifests were rendered. Minting it after the commit would leave the
	// cluster carrying objects no record claims.
	//
	// Paths are relative to Target.Dir.
	Render func(rolloutID string) (map[string][]byte, error)
}

// Result is what happened.
type Result struct {
	// Record is the evidence row, in StatePending. It is pending on purpose:
	// this package knows a commit was pushed and nothing more. Whether the
	// cluster accepted it is the observer's to say.
	Record evidence.Record

	// CommitSHA is what was pushed.
	CommitSHA string
}

// Writer commits deploys and remembers them.
type Writer struct {
	Evidence evidence.Store

	// Now is injected for the tests.
	Now func() time.Time
}

// Deploy renders the manifests, commits them as the user, pushes, and returns
// the record the deploy will be remembered as.
//
// The order matters and is the opposite of the obvious one. The record is
// appended before the push, so a push that fails leaves a row saying the
// platform tried and does not know — which the sweep will later resolve to
// unknown. Appending after a successful push would mean a process that died in
// between produced a deploy with no record at all, and evidence that is missing
// entirely is worse than evidence that admits a gap.
func (w *Writer) Deploy(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}

	// The id is minted here, before anything is rendered, because the manifests
	// have to carry it.
	key := "deploy:" + req.Ref.TenantID + ":" + req.Ref.App + ":" + req.Ref.Env + ":" +
		now().UTC().Format(time.RFC3339Nano)

	rec, err := w.Evidence.Append(ctx, evidence.Record{
		IdempotencyKey: key,
		Ref:            req.Ref,
		Tier:           req.Tier,
		Actor: evidence.Actor{
			ID: req.Author.ID, Kind: "user",
			DisplayName: req.Author.Name, Email: req.Author.Email,
		},
		// Source.CommitSHA is deliberately empty here, and it is worth saying
		// why rather than leaving a blank field to be discovered.
		//
		// The record has to exist before the push, so that a push which fails
		// leaves a trace. But the SHA is not known until the commit is made,
		// and Source is in the half of the record the hash chain covers — so it
		// cannot be filled in afterwards without invalidating every hash from
		// that record onward.
		//
		// The SHA therefore lands on the transition below, as
		// Observation.Revision, which is chained as its own link and is where
		// the evidence page reads the running version from. The cost is that
		// Store.FindBySource cannot resolve a commit written by this path; the
		// observer does not need it, because it matches on the rollout
		// annotation, but a caller that reaches for FindBySource will find
		// nothing and should be told this is why.
		Source: evidence.Source{
			RepoURL: req.Target.RepoURL, Ref: req.Target.Branch, Path: req.Target.Dir,
			AuthorEmail: req.Author.Email, CommitterEmail: Platform.Email,
		},
		Note: req.Message,
	})
	if err != nil {
		return Result{}, fmt.Errorf("opening the evidence record: %w", err)
	}

	sha, err := w.commit(ctx, req, string(rec.ID), now())
	if err != nil {
		// The record already exists, so the failure is recorded against it
		// rather than lost. A best effort: if this transition also fails there
		// is nothing further to do, and the sweep will reach the row anyway.
		version := len(rec.Transitions)
		if _, tErr := w.Evidence.Transition(ctx, rec.ID, evidence.Transition{
			From: []evidence.State{evidence.StatePending}, To: evidence.StateFailed,
			At: now().UTC(), Reason: "the commit was never pushed: " + err.Error(),
			Observation:  evidence.Observation{Source: evidence.ObservedFromCommit, At: now().UTC()},
			ExpectEvents: &version,
		}); tErr != nil {
			err = errors.Join(err, tErr)
		}
		return Result{Record: rec}, err
	}

	// The SHA is only known after the commit, so the record learns it here. Not
	// a state change: the deploy has not moved, the platform has just finished
	// writing down what it did.
	version := len(rec.Transitions)
	updated, err := w.Evidence.Transition(ctx, rec.ID, evidence.Transition{
		From: []evidence.State{evidence.StatePending}, To: evidence.StateSyncing,
		At: now().UTC(), Reason: "pushed as " + sha,
		Observation: evidence.Observation{
			Source: evidence.ObservedFromCommit, Revision: sha, At: now().UTC(),
		},
		ExpectEvents: &version,
	})
	if err != nil {
		// The commit is pushed; the cluster will act on it whatever the store
		// says. Returning the record as it stands is more honest than pretending
		// the deploy did not happen.
		return Result{Record: rec, CommitSHA: sha}, fmt.Errorf("recording the push: %w", err)
	}
	return Result{Record: updated, CommitSHA: sha}, nil
}

// commit does the git half: clone, write, commit as the user, push.
func (w *Writer) commit(ctx context.Context, req Request, rolloutID string, at time.Time) (string, error) {
	files, err := req.Render(rolloutID)
	if err != nil {
		return "", fmt.Errorf("rendering the manifests: %w", err)
	}
	if len(files) == 0 {
		return "", errors.New("gitwrite: nothing to write")
	}

	// In memory, and shallow. A tenant's state repository holds manifests, so
	// it is small by construction; a worktree on disk would be one more thing
	// to clean up after a crash, and the workloads this platform renders run
	// with a read-only root filesystem anyway.
	cloneOpts := &git.CloneOptions{
		URL: req.Target.RepoURL, Auth: req.Target.Auth,
		Depth: 1, SingleBranch: true,
	}
	if req.Target.Branch != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(req.Target.Branch)
	}
	repo, err := git.CloneContext(ctx, memory.NewStorage(), memfs.New(), cloneOpts)
	if err != nil {
		return "", fmt.Errorf("cloning %s: %w", req.Target.RepoURL, err)
	}

	tree, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	for name, body := range files {
		full := path.Join(req.Target.Dir, name)
		if err := writeFile(tree, full, body); err != nil {
			return "", fmt.Errorf("writing %s: %w", full, err)
		}
		if _, err := tree.Add(full); err != nil {
			return "", fmt.Errorf("staging %s: %w", full, err)
		}
	}

	status, err := tree.Status()
	if err != nil {
		return "", err
	}
	if status.IsClean() {
		// Nothing changed. Not an error — a redeploy of an identical manifest
		// is a legitimate thing to ask for — but there is no commit to make,
		// and inventing an empty one would put a deploy in the history that
		// changed nothing.
		return "", ErrNoChange
	}

	sig := &object.Signature{Name: req.Author.Name, Email: req.Author.Email, When: at}
	committer := Platform
	committer.When = at
	hash, err := tree.Commit(req.Message, &git.CommitOptions{
		Author:    sig,
		Committer: &committer,
	})
	if err != nil {
		return "", fmt.Errorf("committing: %w", err)
	}

	// The refspec names the branch explicitly rather than pushing HEAD. When
	// no branch was requested, it is resolved from the clone's own HEAD — the
	// obvious shorthand, HEAD:refs/heads/HEAD, creates a remote branch
	// literally called HEAD, which is the kind of mistake a tenant's repository
	// keeps for ever.
	target := req.Target.Branch
	if target == "" {
		h, err := repo.Head()
		if err != nil {
			return "", fmt.Errorf("resolving the default branch: %w", err)
		}
		if !h.Name().IsBranch() {
			return "", fmt.Errorf("the remote HEAD is not a branch: %s", h.Name())
		}
		target = h.Name().Short()
	}
	ref := "refs/heads/" + target
	if err := repo.PushContext(ctx, &git.PushOptions{
		Auth:     req.Target.Auth,
		RefSpecs: []config.RefSpec{config.RefSpec(ref + ":" + ref)},
	}); err != nil {
		return "", fmt.Errorf("pushing: %w", err)
	}
	return hash.String(), nil
}

// ErrNoChange means the rendered manifests match what is already committed.
var ErrNoChange = errors.New("gitwrite: the manifests are already what is committed")

func writeFile(tree *git.Worktree, name string, body []byte) error {
	f, err := tree.Filesystem.Create(name)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (r Request) validate() error {
	switch {
	case r.Target.RepoURL == "":
		return errors.New("gitwrite: no repository")
	case r.Author.Email == "":
		// Refused rather than defaulted. A commit whose author is the platform
		// is a commit no audit can attribute, and quietly substituting one is
		// how the evidence page ends up saying "the platform did it".
		return errors.New("gitwrite: no author; a deploy has to be attributable to a person")
	case r.Ref.TenantID == "" || r.Ref.App == "" || r.Ref.Env == "":
		return errors.New("gitwrite: the record needs a tenant, an app and an environment")
	case !r.Tier.Valid():
		// The same refusal as a missing author, for the same reason: the tier
		// is copied into the record and into the hash chain, so a default here
		// would be an unverifiable claim about what the customer was paying for
		// at the moment of the deploy.
		return fmt.Errorf("gitwrite: invalid tier %q", r.Tier)
	case r.Render == nil:
		return errors.New("gitwrite: nothing to render")
	case strings.HasPrefix(r.Target.Dir, "/"):
		return errors.New("gitwrite: the directory must be relative to the repository root")
	}
	return nil
}
