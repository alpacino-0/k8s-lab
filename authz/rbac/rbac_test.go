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

package rbac_test

import (
	"context"
	"testing"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/authz/rbac"
)

const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
	appAPI  = "api"
	envProd = "prod"
)

func subject(tenant string, groups ...string) authz.Subject {
	return authz.Subject{ID: "u-1", Tenant: tenant, Email: "u@example.test", Groups: groups}
}

// The tenant boundary is checked before the role, and it is checked at all.
// This is the one that stops a correct role in the wrong tenant from being
// enough, and it is the failure a multi-tenant install cannot survive.
func TestTenantIsolationBeatsRole(t *testing.T) {
	a := rbac.New()
	got, err := a.Authorize(context.Background(),
		subject(tenantB, "owner"),
		authz.ActionAppView,
		authz.Target{Tenant: tenantA, App: appAPI, Env: envProd})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.Allow {
		t.Error("an owner of one tenant was allowed to read another")
	}
	if got.Reason == "" {
		t.Error("a refusal with no reason cannot be shown to the user or recorded")
	}
}

// A subject with no tenant is not a subject with a wildcard. An unauthenticated
// request that reaches here must be refused rather than matched against an
// empty target.
func TestEmptySubjectTenantIsRefused(t *testing.T) {
	a := rbac.New()
	got, err := a.Authorize(context.Background(),
		authz.Subject{ID: "anon"},
		authz.ActionAppView,
		authz.Target{})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.Allow {
		t.Error("a subject with no tenant was allowed against an empty target")
	}
}

// The three free roles, spelled out per action rather than asserted in prose,
// so that adding an Action forces a line here.
func TestRoleMatrix(t *testing.T) {
	actions := []authz.Action{
		authz.ActionAppView,
		authz.ActionEvidenceView,
		authz.ActionAppDeploy,
		authz.ActionAppRollback,
		authz.ActionAppRestart,
		authz.ActionAppScale,
		authz.ActionAppDelete,
		authz.ActionEnvCreate,
		authz.ActionEvidenceExport,
		authz.ActionMemberInvite,
		authz.ActionTenantAdmin,
	}
	// want[role][action]
	want := map[string]map[authz.Action]bool{
		"viewer": {
			authz.ActionAppView: true, authz.ActionEvidenceView: true,
		},
		"member": {
			authz.ActionAppView: true, authz.ActionEvidenceView: true,
			authz.ActionAppDeploy: true, authz.ActionAppRollback: true,
			authz.ActionAppDelete:  true,
			authz.ActionAppRestart: true, authz.ActionAppScale: true,
			authz.ActionEnvCreate: true, authz.ActionEvidenceExport: true,
		},
		"owner": {
			authz.ActionAppView: true, authz.ActionEvidenceView: true,
			authz.ActionAppDeploy: true, authz.ActionAppRollback: true,
			authz.ActionAppDelete:  true,
			authz.ActionAppRestart: true, authz.ActionAppScale: true,
			authz.ActionEnvCreate: true, authz.ActionEvidenceExport: true,
			authz.ActionMemberInvite: true, authz.ActionTenantAdmin: true,
		},
	}

	a := rbac.New()
	target := authz.Target{Tenant: tenantA, App: appAPI, Env: envProd}
	for role, allowed := range want {
		for _, act := range actions {
			got, err := a.Authorize(context.Background(), subject(tenantA, role), act, target)
			if err != nil {
				t.Fatalf("%s/%s: %v", role, act, err)
			}
			if got.Allow != allowed[act] {
				t.Errorf("%s may %s = %v, want %v", role, act, got.Allow, allowed[act])
			}
		}
	}
}

// Roles are read from Groups, which is also where an identity provider's claims
// will arrive. The highest one wins, so a user in both owner and viewer is an
// owner — the opposite would let adding a group take rights away.
func TestHighestRoleWins(t *testing.T) {
	a := rbac.New()
	got, err := a.Authorize(context.Background(),
		subject(tenantA, "viewer", "owner", "member"),
		authz.ActionTenantAdmin,
		authz.Target{Tenant: tenantA})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !got.Allow {
		t.Error("owner alongside viewer was not treated as owner")
	}
}

// An unknown group is not an error and not a role. An IdP will send groups this
// authorizer has never heard of, and the free tier's answer is "viewer", not a
// failure.
func TestUnknownGroupsFallToViewer(t *testing.T) {
	a := rbac.New()
	got, err := a.Authorize(context.Background(),
		subject(tenantA, "engineering", "oncall"),
		authz.ActionAppDeploy,
		authz.Target{Tenant: tenantA, App: appAPI, Env: envProd})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.Allow {
		t.Error("unrecognised groups granted deploy rights")
	}
}

// CanDeploy is the plan's canUserDeploy(user, app, env). It is a helper over
// the one interface method rather than a second method on it, and this asserts
// the two agree — if they ever diverge, principle 6 has been broken by the
// convenience wrapper rather than by a new endpoint.
func TestCanDeployMatchesTheInterface(t *testing.T) {
	a := rbac.New()
	s := subject(tenantA, "member")
	target := authz.Target{Tenant: tenantA, App: appAPI, Env: envProd}

	viaHelper, err := authz.CanDeploy(context.Background(), a, s, target)
	if err != nil {
		t.Fatalf("CanDeploy: %v", err)
	}
	viaInterface, err := a.Authorize(context.Background(), s, authz.ActionAppDeploy, target)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if viaHelper != viaInterface {
		t.Errorf("CanDeploy returned %+v, Authorize returned %+v", viaHelper, viaInterface)
	}
}
