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

// In package server rather than server_test because these drive the handlers
// the route table holds, and the table is unexported. The alternative — going
// through Run and a live listener — is what server_test.go does for the seams,
// and it cannot substitute a BuildCreator or read the placement store back.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/authz/rbac"
	"github.com/damgahq/damga/evidence"
	evidencemem "github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/identity"
	identitymem "github.com/damgahq/damga/identity/memory"
	"github.com/damgahq/damga/placement"
	placementmem "github.com/damgahq/damga/placement/memory"
)

const (
	tenantHome  = "t_home"
	tenantOther = "t_other"
	homeRepo    = "https://github.com/damgahq/tenant-home"
	otherRepo   = "https://github.com/damgahq/tenant-other"

	appAPI      = "api"
	envProd     = "prod"
	envStaging  = "staging"
	nsHomeProd  = "home-prod"
	pathAPIProd = "apps/api/prod"
	branchMain  = "main"

	accOwner  = "a_owner"
	accMember = "a_member"
	accViewer = "a_viewer"
	kindUser  = "user"
)

// harness is one install: two tenants, three accounts with the three roles in
// the first of them, and one account that belongs to both.
//
// Two tenants because the cases worth having are the cross-tenant ones, and an
// account in both because that is the shape a real refusal has to survive:
// somebody who is genuinely signed in, genuinely a member, and genuinely
// allowed to create apps — asking for one that belongs somewhere else.
type harness struct {
	t       *testing.T
	guard   guard
	stores  stores
	places  *placementmem.Store
	records evidence.Store
	cookies map[string]*http.Cookie
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	idStore := identitymem.New()

	for _, id := range []string{tenantHome, tenantOther} {
		if _, err := idStore.CreateTenant(ctx, identity.Tenant{
			ID: id, Slug: strings.TrimPrefix(id, "t_"), DisplayName: id,
		}); err != nil {
			t.Fatalf("CreateTenant %s: %v", id, err)
		}
	}

	sess := &auth.Sessions{Store: idStore, TTL: time.Hour}
	cookies := map[string]*http.Cookie{}
	for _, who := range []struct {
		id     string
		role   identity.Role
		tenant string
	}{
		{accOwner, identity.RoleOwner, tenantHome},
		{accMember, identity.RoleMember, tenantHome},
		{accViewer, identity.RoleViewer, tenantHome},
		// An owner of the other tenant, used to place something there that the
		// home tenant must then fail to take.
		{"a_other", identity.RoleOwner, tenantOther},
	} {
		if _, err := idStore.CreateAccount(ctx, identity.Account{
			ID: who.id, Kind: kindUser, Email: who.id + "@example.test",
			AuditEmail: who.id + "@users.damga.local", DisplayName: who.id,
		}, identity.Credential{Hash: "argon2id$fake"}); err != nil {
			t.Fatalf("CreateAccount %s: %v", who.id, err)
		}
		if err := idStore.AddMember(ctx, identity.Membership{
			AccountID: who.id, TenantID: who.tenant, Role: who.role,
		}); err != nil {
			t.Fatalf("AddMember %s: %v", who.id, err)
		}
		cookie, err := sess.Issue(ctx, who.id, "damga.example.test")
		if err != nil {
			t.Fatalf("Issue %s: %v", who.id, err)
		}
		cookies[who.id] = cookie
	}

	places := placementmem.New()
	t.Cleanup(func() { _ = places.Close() })
	records := evidencemem.New(0)
	t.Cleanup(func() { _ = records.Close() })

	return &harness{
		t:     t,
		guard: guard{authorizer: authz.Authorizer(rbac.New()), identity: idStore, sessions: sess},
		stores: stores{
			evidence: records, placement: places, gitAuth: noAuth{},
		},
		places:  places,
		records: records,
		cookies: cookies,
	}
}

// call drives one handler through a mux, so the path values the guard reads are
// set the way the real router sets them. Calling a handler directly leaves them
// empty and every tenant check passes for the wrong reason.
func (h *harness) call(
	handler func(guard, stores) http.Handler,
	method, suffix, tenant, app, account, body string,
) (int, string) {
	h.t.Helper()
	mux := http.NewServeMux()
	mux.Handle(method+" "+tenantScope+suffix, handler(h.guard, h.stores))

	target := strings.NewReplacer("{tenant}", tenant, "{app}", app).Replace(tenantScope + suffix)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "damga.example.test"
	if account != "" {
		req.AddCookie(h.cookies[account])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (h *harness) createApp(tenant, account, body string) (int, string) {
	h.t.Helper()
	return h.call(createApp, http.MethodPost, "/apps", tenant, "", account, body)
}

// Always in the home tenant: what a delete does from outside one is covered by
// TestEveryTenantRouteIsGuarded, which walks the table this route is in.
func (h *harness) deleteApp(app, account string) (int, string) {
	h.t.Helper()
	return h.call(deleteApp, http.MethodDelete, "/apps/{app}", tenantHome, app, account, "")
}

// The endpoint exists to make the rest of the chain reachable, so this asserts
// the row the rest of the chain reads rather than the shape of the response.
//
// Before this endpoint the placement store had no writer at all: the deploy
// path answered "this app and environment have no repository configured yet"
// to every request, for ever, and the only way past it was an INSERT by hand.
func TestCreateAppWritesThePlacementTheDeployPathReads(t *testing.T) {
	h := newHarness(t)

	code, body := h.createApp(tenantHome, accMember, `{
		"app": "api", "env": "prod",
		"repoUrl": "`+homeRepo+`", "branch": "main", "path": "apps/api/prod",
		"namespace": "home-prod"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /apps = %d: %s", code, body)
	}

	got, err := h.places.Get(context.Background(), tenantHome, appAPI, envProd)
	if err != nil {
		t.Fatalf("the placement was not written: %v", err)
	}
	switch {
	case got.RepoURL != homeRepo:
		t.Errorf("RepoURL = %q", got.RepoURL)
	case got.Branch != branchMain:
		t.Errorf("Branch = %q", got.Branch)
	case got.Path != pathAPIProd:
		t.Errorf("Path = %q", got.Path)
	case got.Namespace != nsHomeProd:
		t.Errorf("Namespace = %q", got.Namespace)
	case got.TenantID != tenantHome:
		t.Errorf("TenantID = %q", got.TenantID)
	}

	// And the response describes what was written, because the caller has to be
	// able to show it without a second request.
	var wrapped struct {
		App wirePlacement `json:"app"`
	}
	if err := json.Unmarshal([]byte(body), &wrapped); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	if wrapped.App.App != appAPI || wrapped.App.Namespace != nsHomeProd {
		t.Errorf("the response does not describe the app it created: %+v", wrapped.App)
	}
	if wrapped.App.CreatedAt == "" {
		t.Error("the response carries no creation time")
	}
}

// The tenant is read from the path the guard checked and never from the body.
//
// A body field would be an authorization performed against one tenant and a row
// written for another, which is the whole failure the route table exists to
// make impossible — and it would not show up in TestEveryTenantRouteIsGuarded,
// because that walks paths.
func TestCreateAppIgnoresATenantInTheBody(t *testing.T) {
	h := newHarness(t)

	code, body := h.createApp(tenantHome, accMember, `{
		"tenantId": "`+tenantOther+`", "tenant": "`+tenantOther+`",
		"app": "api", "env": "prod",
		"repoUrl": "`+homeRepo+`", "branch": "main", "path": "apps/api/prod",
		"namespace": "home-prod"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /apps = %d: %s", code, body)
	}
	if _, err := h.places.Get(context.Background(), tenantOther, appAPI, envProd); !errors.Is(err, placement.ErrNotFound) {
		t.Fatal("a tenant named in the body was written to")
	}
	if _, err := h.places.Get(context.Background(), tenantHome, appAPI, envProd); err != nil {
		t.Fatalf("the tenant from the path was not written to: %v", err)
	}
}

// A member of one tenant must not be able to point an app at a namespace
// another tenant is already running in.
//
// This is the case the endpoint could not have shipped without. A namespace is
// what the ResourceQuota, the Pod Security Admission level and the
// NetworkPolicy are attached to, so an app placed in somebody else's namespace
// runs inside their fence and against their quota — and the request that does
// it is well formed, authenticated, and authorized for the tenant it names.
// Nothing above the store can refuse it, so the store's claim is what does.
func TestCreateAppWillNotTakeAnotherTenantsNamespace(t *testing.T) {
	h := newHarness(t)
	const contested = "shared-prod"

	code, body := h.createApp(tenantOther, "a_other", `{
		"app": "billing", "env": "prod",
		"repoUrl": "`+otherRepo+`", "branch": "main", "path": "apps/billing/prod",
		"namespace": "`+contested+`"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("the other tenant could not create its app: %d %s", code, body)
	}

	code, body = h.createApp(tenantHome, accOwner, `{
		"app": "api", "env": "prod",
		"repoUrl": "`+homeRepo+`", "branch": "main", "path": "apps/api/prod",
		"namespace": "`+contested+`"
	}`)
	if code != http.StatusConflict {
		t.Fatalf("an owner took another tenant's namespace: %d %s", code, body)
	}
	if _, err := h.places.Get(context.Background(), tenantHome, appAPI, envProd); !errors.Is(err, placement.ErrNotFound) {
		t.Error("the refused placement was written anyway")
	}
	// The refusal names the namespace, which the caller sent, and must not name
	// the tenant holding it — the same reason the guard's refusal does not
	// confirm that a tenant exists.
	if strings.Contains(body, tenantOther) {
		t.Errorf("the refusal names the other tenant: %s", body)
	}
}

// The same claim one field over: a repository another tenant already writes to.
// Refusing it here is what keeps the promise that one commit never touches two
// tenants, and it has to be refused before a worktree is opened.
func TestCreateAppWillNotTakeAnotherTenantsRepository(t *testing.T) {
	h := newHarness(t)

	if code, body := h.createApp(tenantOther, "a_other", `{
		"app": "billing", "env": "prod",
		"repoUrl": "`+otherRepo+`", "branch": "main", "path": "apps/billing/prod",
		"namespace": "other-prod"
	}`); code != http.StatusCreated {
		t.Fatalf("the other tenant could not create its app: %d %s", code, body)
	}

	code, body := h.createApp(tenantHome, accOwner, `{
		"app": "api", "env": "prod",
		"repoUrl": "`+otherRepo+`", "branch": "main", "path": "apps/api/prod",
		"namespace": "home-prod"
	}`)
	if code != http.StatusConflict {
		t.Fatalf("an owner took another tenant's repository: %d %s", code, body)
	}
	if strings.Contains(body, tenantOther) {
		t.Errorf("the refusal names the other tenant: %s", body)
	}
}

// Names that this platform will hand to the Kubernetes API server are refused
// where somebody can read the refusal.
//
// Every one of these is accepted by the placement store, committed by the git
// writer and then rejected by the API server — at which point the failure is a
// rollout that never appears, hours later, with the reason in an Argo CD
// condition rather than in front of whoever typed it.
func TestCreateAppRefusesNamesTheClusterWillNotAccept(t *testing.T) {
	h := newHarness(t)

	for _, c := range []struct{ name, body string }{
		{"an app name with a space", `{"app":"my api","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api","namespace":"home-prod"}`},
		{"an app name in capitals", `{"app":"API","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api","namespace":"home-prod"}`},
		{"an app name ending in a dash", `{"app":"api-","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api","namespace":"home-prod"}`},
		{"an environment with a slash", `{"app":"api","env":"prod/eu","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api","namespace":"home-prod"}`},
		{"a namespace with an underscore", `{"app":"api","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api","namespace":"home_prod"}`},
		{"no app", `{"env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api","namespace":"home-prod"}`},
		{"no environment", `{"app":"api","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api","namespace":"home-prod"}`},
		{"no namespace", `{"app":"api","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api"}`},
		{"no repository", `{"app":"api","env":"prod","branch":"main","path":"apps/api","namespace":"home-prod"}`},
		// These three reach the store's own Validate, which has the reasons.
		{"no branch", `{"app":"api","env":"prod","repoUrl":"` + homeRepo + `","path":"apps/api","namespace":"home-prod"}`},
		{"the repository root", `{"app":"api","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"","namespace":"home-prod"}`},
		{"a path that escapes", `{"app":"api","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/../../elsewhere","namespace":"home-prod"}`},
		{"not JSON at all", `{`},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.createApp(tenantHome, accMember, c.body)
			if code != http.StatusBadRequest {
				t.Errorf("POST /apps = %d, want 400: %s", code, body)
			}
		})
	}

	// And none of them left anything behind, including a repository claim that
	// would stop the tenant from ever using that repository.
	all, err := h.places.List(context.Background(), tenantHome)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("a refused create wrote %d placements", len(all))
	}
	if owner, err := h.places.RepoOwner(context.Background(), homeRepo); err != nil || owner != "" {
		t.Errorf("a refused create claimed the repository: %q, %v", owner, err)
	}
}

// Creating is a create. Sending the same app twice is a 409 and not a silent
// repointing of a live app at another repository — the store's Put is
// create-or-replace, so without this check the second call would move an app
// that is running without anybody being told.
func TestCreateAppRefusesOneThatAlreadyExists(t *testing.T) {
	h := newHarness(t)
	const first = `{"app":"api","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api/prod","namespace":"home-prod"}`

	if code, body := h.createApp(tenantHome, accMember, first); code != http.StatusCreated {
		t.Fatalf("the first create = %d: %s", code, body)
	}
	if code, _ := h.createApp(tenantHome, accMember, `{
		"app":"api","env":"prod","repoUrl":"`+homeRepo+`","branch":"main",
		"path":"somewhere/else","namespace":"home-prod"}`); code != http.StatusConflict {
		t.Fatalf("the second create = %d, want 409", code)
	}
	got, err := h.places.Get(context.Background(), tenantHome, appAPI, envProd)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Path != pathAPIProd {
		t.Errorf("the refused create moved the app to %q", got.Path)
	}
}

// A viewer may read the evidence page and may not bring an app into existence.
// The action is env:create, and this is what pins it: with app:view or
// evidence:view in its place the free authorizer allows a viewer through.
func TestCreateAppRefusesAViewer(t *testing.T) {
	h := newHarness(t)
	code, body := h.createApp(tenantHome, accViewer, `{
		"app":"api","env":"prod","repoUrl":"`+homeRepo+`","branch":"main",
		"path":"apps/api/prod","namespace":"home-prod"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a viewer created an app: %d %s", code, body)
	}
}

// Delete removes the record and nothing else: every environment of the app
// stops being placed, and the deploy history — which is what GET /apps is drawn
// from and what a rollback would need — is untouched.
func TestDeleteAppForgetsEveryEnvironmentAndKeepsTheHistory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, env := range []string{envProd, envStaging} {
		if code, body := h.createApp(tenantHome, accOwner, `{
			"app":"api","env":"`+env+`","repoUrl":"`+homeRepo+`","branch":"main",
			"path":"apps/api/`+env+`","namespace":"home-`+env+`"}`); code != http.StatusCreated {
			t.Fatalf("creating api/%s = %d: %s", env, code, body)
		}
	}
	// A second app that must survive the delete.
	if code, body := h.createApp(tenantHome, accOwner, `{
		"app":"web","env":"prod","repoUrl":"`+homeRepo+`","branch":"main",
		"path":"apps/web/prod","namespace":"home-prod"}`); code != http.StatusCreated {
		t.Fatalf("creating web/prod = %d: %s", code, body)
	}
	// One deploy recorded against the app being deleted, so that "the record is
	// deleted and the data is not" is a claim about something that exists.
	if _, err := h.records.Append(ctx, evidence.Record{
		IdempotencyKey: "commit:deadbeef:apps/api/prod",
		Ref:            evidence.Ref{TenantID: tenantHome, App: "api", Env: "prod"},
		Actor:          evidence.Actor{ID: accOwner, Kind: kindUser},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	code, body := h.deleteApp(appAPI, accOwner)
	if code != http.StatusOK {
		t.Fatalf("DELETE /apps/api = %d: %s", code, body)
	}
	// The response says where the manifests and the running objects still are,
	// because after this call nothing in the control plane points at them.
	for _, want := range []string{pathAPIProd, "apps/api/staging", nsHomeProd, "home-staging"} {
		if !strings.Contains(body, want) {
			t.Errorf("the response does not say what was left behind at %q: %s", want, body)
		}
	}

	for _, env := range []string{envProd, envStaging} {
		if _, err := h.places.Get(ctx, tenantHome, appAPI, env); !errors.Is(err, placement.ErrNotFound) {
			t.Errorf("api/%s is still placed: %v", env, err)
		}
	}
	if _, err := h.places.Get(ctx, tenantHome, "web", envProd); err != nil {
		t.Errorf("deleting api removed web too: %v", err)
	}

	// The data. Deleting the record must not touch the deploy log, which is
	// what the evidence page reads and what a rollback is argued from.
	refs, err := h.records.Refs(ctx, tenantHome)
	if err != nil {
		t.Fatalf("Refs: %v", err)
	}
	if len(refs) != 1 || refs[0].App != appAPI {
		t.Errorf("the deploy history did not survive the delete: %+v", refs)
	}
}

// Deleting the last placement releases the repository and the namespace, so a
// tenant that removes an app and creates it again is not blocked by its own
// abandoned claim. Without the release the only fix is editing the database.
func TestDeleteAppReleasesTheClaimsSoItCanBeCreatedAgain(t *testing.T) {
	h := newHarness(t)
	const body = `{"app":"api","env":"prod","repoUrl":"` + homeRepo + `","branch":"main","path":"apps/api/prod","namespace":"home-prod"}`

	if code, b := h.createApp(tenantHome, accOwner, body); code != http.StatusCreated {
		t.Fatalf("create = %d: %s", code, b)
	}
	if code, b := h.deleteApp(appAPI, accOwner); code != http.StatusOK {
		t.Fatalf("delete = %d: %s", code, b)
	}
	if code, b := h.createApp(tenantHome, accOwner, body); code != http.StatusCreated {
		t.Fatalf("re-create = %d: %s", code, b)
	}
}

// app:delete is a member's right and not an owner's, and a viewer has none.
//
// Both halves are load-bearing and each catches a different mistake. Borrowing
// tenant:admin — which is what this endpoint did until the action existed —
// refuses the member, and a member who may create an app and ship to it should
// not need an owner to unregister one. Borrowing app:view or evidence:view lets
// the viewer through, because the free authorizer allows a viewer to read.
func TestDeleteAppNeedsAMemberAndRefusesAViewer(t *testing.T) {
	h := newHarness(t)
	create := func(account string) {
		t.Helper()
		if code, body := h.createApp(tenantHome, accOwner, `{
			"app":"api","env":"prod","repoUrl":"`+homeRepo+`","branch":"main",
			"path":"apps/api/prod","namespace":"home-prod"}`); code != http.StatusCreated {
			t.Fatalf("create for %s = %d: %s", account, code, body)
		}
	}

	create(accViewer)
	if code, _ := h.deleteApp(appAPI, accViewer); code != http.StatusForbidden {
		t.Errorf("a viewer deleted an app: %d", code)
	}
	if _, err := h.places.Get(context.Background(), tenantHome, appAPI, envProd); err != nil {
		t.Errorf("the refused delete removed the placement anyway: %v", err)
	}

	if code, body := h.deleteApp(appAPI, accMember); code != http.StatusOK {
		t.Fatalf("a member could not delete an app: %d %s", code, body)
	}
	// And an owner still can: widening a right must not have narrowed it.
	create(accOwner)
	if code, body := h.deleteApp(appAPI, accOwner); code != http.StatusOK {
		t.Errorf("an owner could not delete: %d %s", code, body)
	}
}

// An app nobody placed is a 404 and not a 200 over an empty list. "There was
// nothing to remove" and "everything was removed" are different answers, and a
// caller retrying a failed delete has to be able to tell them apart.
func TestDeleteAppOnAnAppThatWasNeverPlaced(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.deleteApp("ghost", accOwner); code != http.StatusNotFound {
		t.Errorf("DELETE of an app that was never placed = %d, want 404", code)
	}
}

// The list answers from both stores, and says which of them knew.
//
// The case that made this necessary is the first one: an app that has been
// created and never deployed. Answering from the evidence store alone made
// POST /apps invisible — somebody creates an app, the page stays empty, and
// nothing anywhere says why. The other two are what the same merge has to say
// once it exists, and they must not be flattened into each other.
func TestAppsListsBothStoresAndSaysWhichOneKnew(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Placed, never deployed.
	if code, body := h.createApp(tenantHome, accMember, `{
		"app":"api","env":"prod","repoUrl":"`+homeRepo+`","branch":"main",
		"path":"apps/api/prod","namespace":"home-prod"}`); code != http.StatusCreated {
		t.Fatalf("creating api/prod = %d: %s", code, body)
	}
	// Placed and deployed.
	if code, body := h.createApp(tenantHome, accMember, `{
		"app":"admin","env":"prod","repoUrl":"`+homeRepo+`","branch":"main",
		"path":"apps/admin/prod","namespace":"home-prod"}`); code != http.StatusCreated {
		t.Fatalf("creating admin/prod = %d: %s", code, body)
	}
	for _, ref := range []evidence.Ref{
		{TenantID: tenantHome, App: "admin", Env: envProd},
		// Deployed and never placed: what DELETE /apps/{app} leaves behind.
		{TenantID: tenantHome, App: "web", Env: envProd},
	} {
		if _, err := h.records.Append(ctx, evidence.Record{
			IdempotencyKey: "commit:" + ref.App + ":apps/" + ref.App,
			Ref:            ref,
			Actor:          evidence.Actor{ID: accMember, Kind: kindUser},
		}); err != nil {
			t.Fatalf("Append %s: %v", ref.App, err)
		}
	}

	code, body := h.call(apps, http.MethodGet, "/apps", tenantHome, "", accViewer, "")
	if code != http.StatusOK {
		t.Fatalf("GET /apps = %d: %s", code, body)
	}
	var got struct {
		Apps []wireApp `json:"apps"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}

	// Sorted by app then env, because both sources arrive sorted and a merge
	// through a map would otherwise reshuffle the panel's list on every load.
	names := make([]string, 0, len(got.Apps))
	for _, a := range got.Apps {
		names = append(names, a.App+"/"+a.Env+"="+a.State)
	}
	want := "admin/prod=" + appDeployed + " api/prod=" + appNeverDeployed +
		" web/prod=" + appRecordRemoved
	if strings.Join(names, " ") != want {
		t.Errorf("GET /apps =\n\t%q\nwant\n\t%q", strings.Join(names, " "), want)
	}

	// The placement travels with the entries that have one, so the page can say
	// where an app would deploy to before it ever has.
	for _, a := range got.Apps {
		switch a.State {
		case appRecordRemoved:
			if a.Namespace != "" || a.RepoURL != "" {
				t.Errorf("%s/%s has no placement but reports one: %+v", a.App, a.Env, a)
			}
		default:
			if a.Namespace != nsHomeProd || a.RepoURL != homeRepo || a.Branch != branchMain {
				t.Errorf("%s/%s does not carry its placement: %+v", a.App, a.Env, a)
			}
		}
	}
}

// One tenant's list never reaches into another's, from either source. The guard
// stops a caller from asking about a tenant they do not belong to; this is the
// half below it — a member of both would otherwise be shown one list for two.
func TestAppsIsScopedToOneTenantInBothStores(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if code, body := h.createApp(tenantOther, "a_other", `{
		"app":"billing","env":"prod","repoUrl":"`+otherRepo+`","branch":"main",
		"path":"apps/billing/prod","namespace":"other-prod"}`); code != http.StatusCreated {
		t.Fatalf("creating the other tenant's app = %d: %s", code, body)
	}
	if _, err := h.records.Append(ctx, evidence.Record{
		IdempotencyKey: "commit:billing:apps/billing",
		Ref:            evidence.Ref{TenantID: tenantOther, App: "billing", Env: envProd},
		Actor:          evidence.Actor{ID: "a_other", Kind: kindUser},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	code, body := h.call(apps, http.MethodGet, "/apps", tenantHome, "", accOwner, "")
	if code != http.StatusOK {
		t.Fatalf("GET /apps = %d: %s", code, body)
	}
	if strings.Contains(body, "billing") || strings.Contains(body, otherRepo) {
		t.Errorf("another tenant's app is in the list: %s", body)
	}
}
