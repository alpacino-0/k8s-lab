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

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/forge"
	forgemem "github.com/damgahq/damga/forge/memory"
	"github.com/damgahq/damga/identity"
	"github.com/damgahq/damga/placement"
	placementmem "github.com/damgahq/damga/placement/memory"
	"github.com/damgahq/damga/server"
)

func connectionURL(base string) string {
	return base + "/api/v1/tenants/" + testTenant + "/apps/" + testApp + "/connection"
}

const connectBody = `{"owner":"acme","repo":"api","branch":"main","imageRepository":"ghcr.io/acme/api"}`

func put(t *testing.T, url, body string, cookie *http.Cookie) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return resp.StatusCode, string(out)
}

// Connecting a repository and being handed the file that will be proposed is
// one action from the person's side. The whole design rests on them reading it
// before they merge it, so an endpoint that stores the connection and makes
// them ask again for the workflow has put a step between the two.
func TestConnectingReturnsTheWorkflowToBeMerged(t *testing.T) {
	store := forgemem.New()
	base := start(t, server.Options{
		Identity: identityWith(t, identity.RoleOwner),
		Forge:    store,
	})
	cookie := login(t, base)

	code, body := put(t, connectionURL(base), connectBody, cookie)
	if code != http.StatusCreated {
		t.Fatalf("PUT connection = %d %q, want 201", code, body)
	}

	var got struct {
		Connection struct {
			Identity    string `json:"identity"`
			Verified    bool   `json:"verified"`
			Enforcement string `json:"enforcement"`
			Branch      string `json:"branch"`
		} `json:"connection"`
		Workflow struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding: %v — body was %q", err, body)
	}

	want := "https://github.com/acme/api/" + server.DefaultWorkflowPath + "@refs/heads/main"
	if got.Connection.Identity != want {
		t.Errorf("identity = %q, want %q", got.Connection.Identity, want)
	}
	if got.Workflow.Path != server.DefaultWorkflowPath {
		t.Errorf("workflow path = %q, want the platform's own", got.Workflow.Path)
	}
	if !strings.Contains(got.Workflow.Content, "cosign sign") {
		t.Error("the returned file does not sign anything, which is the only reason " +
			"it is being proposed")
	}
	if !strings.Contains(got.Workflow.Content, "id-token: write") {
		t.Error("the workflow cannot get an OIDC token, so keyless signing would fail " +
			"at the step that matters")
	}

	// A connection nothing has verified must not describe itself as rejecting,
	// or the panel tells the tenant they are protected before they are.
	if got.Connection.Verified {
		t.Error("a connection made a moment ago reports as verified")
	}
	if got.Connection.Enforcement != "recording" {
		t.Errorf("enforcement = %q, want recording — enforcing before the workflow "+
			"has ever run refuses the tenant's next deploy", got.Connection.Enforcement)
	}

	// And it reads back.
	code, body = get(t, connectionURL(base), cookieHeader(cookie))
	if code != http.StatusOK {
		t.Fatalf("GET connection = %d %q, want 200", code, body)
	}
	if !strings.Contains(body, want) {
		t.Errorf("the identity did not survive the round trip: %s", body)
	}
}

// The privilege this endpoint hands out is not the deploy right.
//
// Deploying ships an image; connecting decides which signer is trusted for
// every image this app will ever run. A member who can do the second can
// arrange to be the one who does the first, so if the two shared an action the
// signature check would be a formality for anybody who could already deploy.
func TestOnlyAnOwnerMayChooseTheSigner(t *testing.T) {
	base := start(t, server.Options{
		Identity: identityWith(t, identity.RoleMember),
		Forge:    forgemem.New(),
	})
	cookie := login(t, base)

	code, body := put(t, connectionURL(base), connectBody, cookie)
	if code != http.StatusForbidden {
		t.Errorf("a member connecting a repository = %d %q, want 403", code, body)
	}

	// The same member may still read it, because seeing which repository an app
	// builds from is not the same power as choosing it.
	code, _ = get(t, connectionURL(base), cookieHeader(cookie))
	if code == http.StatusForbidden {
		t.Error("a member may not even read the connection, which makes the page " +
			"unusable for everyone but owners")
	}
}

// Two tenants who connect the same repository and branch render the same
// certificate subject, and a policy cannot tell which tenant's build produced a
// signature carrying it — each would accept the other's images, with both
// signatures genuine.
func TestARepositoryCannotBeConnectedTwice(t *testing.T) {
	store := forgemem.New()
	if _, err := store.Put(context.Background(), forge.Connection{
		TenantID: "someone-else", App: "their-app",
		Host: "github.com", Owner: "acme", Repo: "api", Branch: "main",
		WorkflowPath: server.DefaultWorkflowPath, ImageRepository: "ghcr.io/acme/api",
	}); err != nil {
		t.Fatalf("seeding the other tenant's connection: %v", err)
	}

	base := start(t, server.Options{
		Identity: identityWith(t, identity.RoleOwner),
		Forge:    store,
	})
	cookie := login(t, base)

	code, body := put(t, connectionURL(base), connectBody, cookie)
	if code != http.StatusConflict {
		t.Fatalf("connecting a repository another tenant holds = %d %q, want 409", code, body)
	}
	// Which tenant holds it is not the caller's to learn. A repository name is
	// enough to go looking with, and the answer to "who has this" is the sort
	// of thing an attacker assembles a customer list out of.
	if strings.Contains(body, "someone-else") {
		t.Errorf("the refusal names the other tenant: %s", body)
	}
}

// A forge public Fulcio does not accept cannot sign keyless at all, whatever
// this server does. Answering 400 invites the caller to keep adjusting the
// request; nothing they could send would work.
func TestAForgeThatCannotSignIsNotACallerMistake(t *testing.T) {
	base := start(t, server.Options{
		Identity: identityWith(t, identity.RoleOwner),
		Forge:    forgemem.New(),
	})
	cookie := login(t, base)

	code, body := put(t, connectionURL(base),
		`{"host":"git.internal.example","owner":"acme","repo":"api","branch":"main",`+
			`"imageRepository":"registry.internal/acme/api"}`, cookie)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("connecting a self-hosted forge = %d %q, want 422", code, body)
	}
	if !strings.Contains(body, "git.internal.example") {
		t.Errorf("the refusal does not name the host, so the operator cannot tell "+
			"which connection is on a weaker tier: %s", body)
	}
}

// An installation that configures nothing still gets a working one.
//
// This started as a test that connecting without a store answers 501, and the
// premise stopped being true the moment Run was taught to open a forge store
// the way it already opens the other three — which is the better behaviour, so
// the assertion follows it rather than the other way round. The failure it
// guards against now is the one that would actually happen: somebody adds a
// store to the Options struct, forgets to open it in Run, and every connection
// attempt in production answers 501 while every test passes because the tests
// pass a store in.
//
// The handler still refuses a nil store rather than panicking, because an
// embedder building the routes directly is not going through Run.
func TestConnectingWorksWithNothingConfigured(t *testing.T) {
	base := start(t, server.Options{Identity: identityWith(t, identity.RoleOwner)})
	cookie := login(t, base)

	code, body := put(t, connectionURL(base), connectBody, cookie)
	if code != http.StatusCreated {
		t.Errorf("PUT connection on a default install = %d %q, want 201 — an install "+
			"that configures no DSN gets an in-process store for everything else",
			code, body)
	}
}

// The policy that admits an image travels in the same commit as the image.
//
// Written by the same path, into the same directory, under the same authorship.
// The alternatives were a second reconciler applying it out of band, which
// makes damga stop being the only writer, and rendering it in the operator,
// which would put a second copy of the policy renderer somewhere it can drift
// from the workflow it pins.
func TestADeployCommitsTheSignaturePolicyBesideTheWorkload(t *testing.T) {
	repo := bareRepo(t)
	places := placementmem.New()
	if _, err := places.Put(t.Context(), placement.Placement{
		TenantID: testTenant, App: testApp, Env: testEnv,
		RepoURL: repo, Branch: testBranch, Path: testPath, Namespace: testNamespace,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	conns := forgemem.New()

	base := start(t, server.Options{
		Evidence: memory.New(0), Placement: places, Forge: conns,
		GitAuth:  localAuth{},
		Identity: identityWith(t, identity.RoleOwner),
	})
	session := login(t, base)

	if code, body := put(t, connectionURL(base), connectBody, session); code != http.StatusCreated {
		t.Fatalf("connecting = %d %q", code, body)
	}

	deployURL := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/deploys",
		base, testTenant, testApp, testEnv)
	if code, body := post(t, deployURL, `{"image":"ghcr.io/acme/api:1.0.0"}`, session); code != http.StatusAccepted {
		t.Fatalf("deploy = %d %q, want 202", code, body)
	}

	policy := committedFile(t, repo, testPath+"/"+forge.PolicyFile)
	for _, want := range []string{
		"kind: Policy",
		"namespace: " + testNamespace,
		"https://github.com/acme/api/" + server.DefaultWorkflowPath + "@refs/heads/main",
		"githubWorkflowTrigger: push",
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("the committed policy has no %q:\n%s", want, policy)
		}
	}
	// Nothing has verified this connection, so the rule records rather than
	// rejects. The other way round refuses the deploy that was just made.
	if !strings.Contains(policy, "validationFailureAction: Audit") {
		t.Errorf("the policy enforces on a connection nothing has verified:\n%s", policy)
	}
}

// An app nobody connected still deploys, and no policy appears beside it.
//
// Refusing an unconnected deploy would make connecting a prerequisite for the
// platform working at all, which is the opposite of what the free tier is for.
// Writing an empty or inert policy would be worse: a rule that admits anything
// looks from the outside exactly like a rule that is doing its job.
func TestAnUnconnectedAppDeploysWithNoPolicy(t *testing.T) {
	repo := bareRepo(t)
	places := placementmem.New()
	if _, err := places.Put(t.Context(), placement.Placement{
		TenantID: testTenant, App: testApp, Env: testEnv,
		RepoURL: repo, Branch: testBranch, Path: testPath, Namespace: testNamespace,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	base := start(t, server.Options{
		Evidence: memory.New(0), Placement: places, Forge: forgemem.New(),
		GitAuth:  localAuth{},
		Identity: identityWith(t, identity.RoleOwner),
	})
	session := login(t, base)

	deployURL := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/deploys",
		base, testTenant, testApp, testEnv)
	if code, body := post(t, deployURL, `{"image":"ghcr.io/acme/api:1.0.0"}`, session); code != http.StatusAccepted {
		t.Fatalf("deploying an unconnected app = %d %q, want 202 — connecting is not a "+
			"prerequisite for the platform working", code, body)
	}

	if _, err := commitFileOrNil(t, repo, testPath+"/"+forge.PolicyFile); err == nil {
		t.Error("a signature policy was committed for an app connected to nothing, " +
			"so admission would be checking a signer that was never chosen")
	}
}

// commitFileOrNil is committedFile without the fatal, for asserting a file is
// absent.
func commitFileOrNil(t *testing.T, repoPath, name string) (string, error) {
	t.Helper()
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	f, err := commit.File(name)
	if err != nil {
		return "", err
	}
	return f.Contents()
}
