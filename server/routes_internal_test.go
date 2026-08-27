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

// This file is in package server rather than server_test because it walks
// tenantRoutes, and the point of walking it is that a route added tomorrow is
// covered without anybody adding a case for it. Exporting the table to reach
// it from outside would make the arrangement part of the API.
package server

import (
	"context"
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
)

// Every endpoint that serves one tenant's data must refuse an anonymous caller
// and a caller who belongs somewhere else.
//
// The second half is the one worth having. Refusing anonymously is what any
// missing session check does anyway; the failure this catches is a handler
// that resolves the session, is satisfied that somebody is signed in, and then
// reads the tenant out of the path — which serves another customer's deploy
// history to anyone with an account.
func TestEveryTenantRouteIsGuarded(t *testing.T) {
	const (
		home    = "t_home"
		foreign = "t_foreign"
	)
	ctx := context.Background()
	idStore := identitymem.New()

	for _, id := range []string{home, foreign} {
		if _, err := idStore.CreateTenant(ctx, identity.Tenant{
			ID: id, Slug: strings.TrimPrefix(id, "t_"), DisplayName: id,
			Tier: identity.TierFree,
		}); err != nil {
			t.Fatalf("CreateTenant %s: %v", id, err)
		}
	}
	if _, err := idStore.CreateAccount(ctx, identity.Account{
		ID: "a_member", Kind: "user", Email: "member@example.test",
		AuditEmail: "a_member@users.damga.local", DisplayName: "Member",
	}, identity.Credential{Hash: "argon2id$fake"}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	// A member of home, and of nowhere else. Owner rather than viewer, so a
	// refusal below cannot be explained away as the role being too weak.
	if err := idStore.AddMember(ctx, identity.Membership{
		AccountID: "a_member", TenantID: home, Role: identity.RoleOwner,
	}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	sess := &auth.Sessions{Store: idStore, TTL: time.Hour}
	cookie, err := sess.Issue(ctx, "a_member", "damga.example.test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	g := guard{authorizer: authz.Authorizer(rbac.New()), identity: idStore, sessions: sess}
	store := evidencemem.New(0)
	t.Cleanup(func() { _ = store.Close() })

	if len(tenantRoutes) == 0 {
		t.Fatal("tenantRoutes is empty: this test is asserting nothing")
	}
	for _, rt := range tenantRoutes {
		t.Run(rt.suffix, func(t *testing.T) {
			h := rt.handler(g, store)

			t.Run("anonymous", func(t *testing.T) {
				code, _ := callRoute(t, h, rt.suffix, home, nil)
				if code != http.StatusUnauthorized {
					t.Errorf("%s without a cookie = %d, want 401", rt.suffix, code)
				}
			})
			t.Run("another tenant", func(t *testing.T) {
				code, body := callRoute(t, h, rt.suffix, foreign, cookie)
				if code != http.StatusForbidden {
					t.Errorf("%s in a tenant they do not belong to = %d, want 403", rt.suffix, code)
				}
				// The refusal must not confirm the tenant exists. It does
				// exist here, and a message that says so differently from one
				// for a name nobody has taken is a way to enumerate customers.
				if strings.Contains(body, foreign) {
					t.Errorf("the refusal names the tenant: %s", body)
				}
			})
			t.Run("their own tenant", func(t *testing.T) {
				// Not asserting a specific success code — /evidence answers
				// 404 with nothing deployed and the others answer 200. What
				// matters is that the guard is not refusing a member.
				code, _ := callRoute(t, h, rt.suffix, home, cookie)
				if code == http.StatusUnauthorized || code == http.StatusForbidden {
					t.Errorf("%s refused an owner of the tenant: %d", rt.suffix, code)
				}
			})
		})
	}
}

// callRoute drives one handler through a mux, so the {tenant} path value the
// guard reads is set the way the real router sets it. Calling the handler
// directly would leave it empty and every case would pass.
func callRoute(t *testing.T, h http.Handler, pattern, tenant string, cookie *http.Cookie) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET "+tenantScope+pattern, h)

	// Built by substituting the pattern rather than written out, so a route
	// whose shape differs from the others is still driven through the router
	// that sets its path values.
	target := strings.NewReplacer(
		"{tenant}", tenant, "{app}", "api", "{env}", "prod",
	).Replace(tenantScope + pattern)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "damga.example.test"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// A compile-time reminder that the table's handlers all have one shape. If a
// future endpoint needs something the guard cannot give it, that is a design
// question, not a signature to widen.
var _ = []func(guard, evidence.Store) http.Handler{
	apps, currentEvidence, history, verify, retention, export,
}
