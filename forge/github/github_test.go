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

package github_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/damgahq/damga/forge"
	"github.com/damgahq/damga/forge/github"
)

// The commit the tenant's branch is at, and the one the proposal branch has
// to be cut from — a proposal branched from anywhere else opens a pull request
// carrying changes nobody asked for.
const baseSHA = "basesha"

func conn() forge.Connection {
	return forge.Connection{
		TenantID: "t_acme", App: "shop",
		Host: "github.com", Owner: "acme", Repo: "shop", Branch: "main",
		WorkflowPath:    ".github/workflows/damga-sign.yml",
		ImageRepository: "ghcr.io/acme/shop",
	}
}

// fakeForge is enough of github.com to exercise the five calls: a repository
// with one branch, a set of files per branch, and a list of pull requests.
//
// A stub and not a mock. What is under test is the sequence and its behaviour
// on a second attempt, so the fake keeps state and answers the way GitHub does
// — 422 for a ref that exists, 404 for a file that does not — rather than
// asserting on the order calls arrive in.
type fakeForge struct {
	t *testing.T

	branches map[string]string            // branch -> sha
	files    map[string]map[string][]byte // branch -> path -> content
	pulls    []map[string]any
	nextPull int

	calls   []string
	created int // how many pull requests were opened

	// forbid answers 403 for any path containing this, to exercise refusal.
	forbid string
}

func newFake(t *testing.T) *fakeForge {
	return &fakeForge{
		t:        t,
		branches: map[string]string{"main": baseSHA},
		files:    map[string]map[string][]byte{},
		nextPull: 41,
	}
}

func (f *fakeForge) serve() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/acme/shop/git/ref/heads/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		branch := strings.TrimPrefix(r.URL.Path, "/repos/acme/shop/git/ref/heads/")
		sha, ok := f.branches[branch]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"object": map[string]any{"sha": sha}})
	})

	mux.HandleFunc("/repos/acme/shop/git/refs", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		var body struct{ Ref, SHA string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := strings.TrimPrefix(body.Ref, "refs/heads/")
		if _, exists := f.branches[name]; exists {
			// What GitHub answers for a reference that is already there.
			http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		f.branches[name] = body.SHA
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"ref": body.Ref})
	})

	mux.HandleFunc("/repos/acme/shop/contents/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/shop/contents/")
		switch r.Method {
		case http.MethodGet:
			branch := r.URL.Query().Get("ref")
			body, ok := f.files[branch][path]
			if !ok {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{
				"sha": "blobsha", "content": base64.StdEncoding.EncodeToString(body),
			})
		case http.MethodPut:
			var body struct{ Content, Branch, SHA string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, err := base64.StdEncoding.DecodeString(body.Content)
			if err != nil {
				f.t.Errorf("the file content is not base64: %v", err)
			}
			if f.files[body.Branch] == nil {
				f.files[body.Branch] = map[string][]byte{}
			}
			f.files[body.Branch][path] = raw
			writeJSON(w, map[string]any{"content": map[string]any{"path": path}})
		}
	})

	mux.HandleFunc("/repos/acme/shop/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, f.pulls)
		case http.MethodPost:
			f.created++
			f.nextPull++
			pull := map[string]any{
				"number":   f.nextPull,
				"html_url": "https://github.com/acme/shop/pull/" + strconv.Itoa(f.nextPull),
			}
			f.pulls = append(f.pulls, pull)
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, pull)
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.forbid != "" && strings.Contains(r.URL.Path, f.forbid) {
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			f.t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got == "" {
			f.t.Error("no API version header; the unversioned API can change shape under a " +
				"running control plane")
		}
		mux.ServeHTTP(w, r)
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *fakeForge) record(r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func client(t *testing.T, f *fakeForge) *github.Client {
	t.Helper()
	srv := f.serve()
	return &github.Client{Token: "test-token", API: srv.URL, HTTP: srv.Client()}
}

func TestProposePutsTheWorkflowOnABranchAndOpensAPullRequest(t *testing.T) {
	f := newFake(t)
	c := client(t, f)

	got, err := c.Propose(context.Background(), conn())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got.Number == 0 || got.URL == "" {
		t.Errorf("the pull request has no number or url: %+v", got)
	}
	if got.Existing {
		t.Error("the first proposal reported itself as one it found")
	}
	if got.Branch != forge.ProposalBranch {
		t.Errorf("branch = %q, want %q", got.Branch, forge.ProposalBranch)
	}

	// The branch was cut from the branch the identity names, and the file
	// landed on it rather than on the tenant's own branch.
	if f.branches[forge.ProposalBranch] != baseSHA {
		t.Errorf("the proposal branch is at %q, want the base", f.branches[forge.ProposalBranch])
	}
	if _, onMain := f.files["main"]; onMain {
		t.Error("the workflow was written straight to the tenant's branch; the merge is " +
			"the approval this whole design rests on and there was none")
	}

	// And what landed is the file the policy pins, not something else.
	written := f.files[forge.ProposalBranch][conn().WorkflowPath]
	want, err := conn().Workflow()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	if string(written) != string(want) {
		t.Error("the file proposed is not the file the connection renders, so the policy " +
			"would pin an identity the merged workflow does not present")
	}
}

// The part that matters in a control plane. A timeout retried, an operator
// pressing the button twice, a pod restarting mid-flight — each of those is a
// second call, and a second call must not be a second pull request in somebody
// else's repository.
func TestProposingTwiceFindsTheFirstPullRequest(t *testing.T) {
	f := newFake(t)
	c := client(t, f)
	ctx := context.Background()

	first, err := c.Propose(ctx, conn())
	if err != nil {
		t.Fatalf("first Propose: %v", err)
	}
	second, err := c.Propose(ctx, conn())
	if err != nil {
		t.Fatalf("second Propose: %v", err)
	}

	if f.created != 1 {
		t.Errorf("%d pull requests were opened; the fastest way to have an integration "+
			"turned off is for it to accumulate them in a repository it does not own",
			f.created)
	}
	if second.Number != first.Number {
		t.Errorf("second proposal = #%d, first = #%d", second.Number, first.Number)
	}
	if !second.Existing {
		t.Error("the second call claimed to have opened a pull request it found; the panel " +
			"would tell the tenant to go and review something they already have")
	}
}

// A branch left behind by an attempt that failed after creating it, with the
// pull request never opened. The next attempt has to finish the job rather than
// fall over on the branch it made itself.
func TestAnAbandonedBranchIsPickedUpRatherThanFatal(t *testing.T) {
	f := newFake(t)
	f.branches[forge.ProposalBranch] = baseSHA
	c := client(t, f)

	got, err := c.Propose(context.Background(), conn())
	if err != nil {
		t.Fatalf("Propose over an existing branch: %v", err)
	}
	if got.Number == 0 {
		t.Error("no pull request was opened")
	}
	if f.files[forge.ProposalBranch][conn().WorkflowPath] == nil {
		t.Error("the workflow was never written to the branch that was already there")
	}
}

// Rewriting the file when it is already identical would be a commit that
// changes nothing, on a branch somebody may be reading.
func TestAnIdenticalFileIsNotRewritten(t *testing.T) {
	f := newFake(t)
	body, err := conn().Workflow()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	f.branches[forge.ProposalBranch] = baseSHA
	f.files[forge.ProposalBranch] = map[string][]byte{conn().WorkflowPath: body}
	c := client(t, f)

	if _, err := c.Propose(context.Background(), conn()); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "PUT ") {
			t.Errorf("the identical file was written again (%s)", call)
		}
	}
}

// A refusal and a failure need opposite responses: one is fixed by granting
// access, the other may work on the next attempt. Retrying a refusal for ever
// is how a platform gets rate-limited for asking the same forbidden question.
func TestARefusalIsDistinguishedFromAFailure(t *testing.T) {
	f := newFake(t)
	f.forbid = "/repos/"
	c := client(t, f)

	_, err := c.Propose(context.Background(), conn())
	if !errors.Is(err, forge.ErrNotPermitted) {
		t.Fatalf("err = %v, want ErrNotPermitted", err)
	}
	// And it carries what the forge said, because "403" on its own sends an
	// operator looking in the wrong place.
	if !strings.Contains(err.Error(), "not accessible by integration") {
		t.Errorf("the error does not carry the forge's own message: %v", err)
	}
}

func TestNoTokenIsARefusalAndNotACrash(t *testing.T) {
	c := &github.Client{API: "http://127.0.0.1:1"}
	_, err := c.Propose(context.Background(), conn())
	if !errors.Is(err, forge.ErrNotPermitted) {
		t.Errorf("err = %v, want ErrNotPermitted before any request is made", err)
	}
}

// The body is the only thing arguing for a merge, in a repository whose owner
// may not recognise the account it came from.
func TestTheBodyExplainsWhatMergingDoes(t *testing.T) {
	// Whitespace-normalised before matching. The body is prose and wraps, so a
	// phrase that happens to straddle a line break is still the phrase — the
	// assertion is about what the reader is told, not about where the text
	// happened to fold.
	body := strings.Join(strings.Fields(forge.ProposalBody(conn())), " ")
	for _, want := range []string{
		conn().WorkflowPath,
		conn().Identity(),
		"holds no signing key",
		"records what it would have rejected",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the pull request body does not mention %q", want)
		}
	}
}

// The check the identity cannot make for itself.
//
// A policy pins a workflow file at a ref, so an edited file at the same path on
// the same branch presents exactly the accepted identity. The signature stays
// genuine; what it vouches for does not. Nothing in Sigstore, Kyverno or git
// notices, which is why something has to read the file back.
func TestMergeStatusTellsMergedFromEditedFromAbsent(t *testing.T) {
	written, err := conn().Workflow()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}

	for name, tc := range map[string]struct {
		onBranch []byte
		want     forge.MergeState
	}{
		"never merged":      {onBranch: nil, want: forge.MergeAbsent},
		"merged as written": {onBranch: written, want: forge.MergeMatches},
		"edited since": {
			onBranch: append(append([]byte{}, written...), []byte("\n      - run: curl evil\n")...),
			want:     forge.MergeDrifted,
		},
		// A file replaced entirely rather than appended to. Same identity, and
		// nothing about it is the workflow damga proposed.
		"replaced": {onBranch: []byte("name: mine\non:\n  push:\n"), want: forge.MergeDrifted},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			if tc.onBranch != nil {
				f.files["main"] = map[string][]byte{conn().WorkflowPath: tc.onBranch}
			}
			c := client(t, f)

			got, err := c.MergeStatus(context.Background(), conn())
			if err != nil {
				t.Fatalf("MergeStatus: %v", err)
			}
			if got.State != tc.want {
				t.Errorf("state = %q, want %q", got.State, tc.want)
			}
			if got.Detail == "" {
				t.Error("no detail; the page has nothing to say beyond a word")
			}
		})
	}
}

// It reads the branch the identity names, not the branch damga pushed to. A
// file sitting on an unmerged proposal branch signs nothing, and reporting it
// as present would say the chain is live when it is one click short.
func TestMergeStatusIgnoresTheProposalBranch(t *testing.T) {
	written, err := conn().Workflow()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	f := newFake(t)
	f.branches[forge.ProposalBranch] = baseSHA
	f.files[forge.ProposalBranch] = map[string][]byte{conn().WorkflowPath: written}
	c := client(t, f)

	got, err := c.MergeStatus(context.Background(), conn())
	if err != nil {
		t.Fatalf("MergeStatus: %v", err)
	}
	if got.State != forge.MergeAbsent {
		t.Errorf("state = %q; an unmerged proposal was reported as merged, so the page "+
			"would say the chain is live while the pull request is still open", got.State)
	}
}

// Drift is not a refusal and a refusal is not drift. A forge that will not let
// us read is something the tenant fixes by granting access; saying "edited"
// about a file nobody could open would send them to change something that is
// not wrong.
func TestAForgeThatRefusesTheReadIsNotDrift(t *testing.T) {
	f := newFake(t)
	f.forbid = "/contents/"
	c := client(t, f)

	_, err := c.MergeStatus(context.Background(), conn())
	if !errors.Is(err, forge.ErrNotPermitted) {
		t.Errorf("err = %v, want ErrNotPermitted", err)
	}
}
