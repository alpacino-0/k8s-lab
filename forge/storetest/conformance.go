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

// Package storetest is the engine-agnostic contract for forge.Store.
//
// Written before the second implementation, which is the arrangement that has
// paid four times in this repository now: the store written second passes first
// try, and the cases only one store fails are the ones worth having.
package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/damgahq/damga/forge"
)

// Factory makes a fresh, empty store. Called once per case: the cases assume
// they own it.
type Factory func(t *testing.T) forge.Store

const (
	tenantA = "t_alpha"
	tenantB = "t_beta"
	appA    = "shop"
	appB    = "billing"
)

// Run executes the whole suite against one implementation.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"ConnectionRoundTrips", testConnectionRoundTrips},
		{"PutReplacesWithoutMovingCreatedAt", testPutReplacesWithoutMovingCreatedAt},
		{"ListIsScopedToOneTenantAndOrdered", testListIsScopedToOneTenantAndOrdered},
		{"AnIdentityBelongsToOneTenant", testAnIdentityBelongsToOneTenant},
		{"OneTenantMayConnectOneRepoTwice", testOneTenantMayConnectOneRepoTwice},
		{"ConcurrentClaimsOfOneIdentityAgree", testConcurrentClaimsOfOneIdentityAgree},
		{"MovingRepositoryReleasesTheOldIdentity", testMovingRepositoryReleasesTheOldIdentity},
		{"DeletingReleasesTheIdentity", testDeletingReleasesTheIdentity},
		{"AnUnusableConnectionIsRefused", testAnUnusableConnectionIsRefused},
		{"MissingThingsAreNotFound", testMissingThingsAreNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore) })
	}
}

func conn(tenant, app string, mutate ...func(*forge.Connection)) forge.Connection {
	c := forge.Connection{
		TenantID:        tenant,
		App:             app,
		Host:            "github.com",
		Owner:           "acme",
		Repo:            app,
		Branch:          "main",
		WorkflowPath:    ".github/workflows/damga-sign.yml",
		ImageRepository: "ghcr.io/acme/" + app,
	}
	for _, m := range mutate {
		m(&c)
	}
	return c
}

func testConnectionRoundTrips(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	in := conn(tenantA, appA)
	out, err := s.Put(ctx, in)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if out.CreatedAt.IsZero() || out.UpdatedAt.IsZero() {
		t.Error("the store did not stamp the connection, so nothing can tell when a " +
			"repository was connected")
	}

	got, err := s.Get(ctx, in.Key())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The identity is the whole point of storing this, so it is what is
	// compared: a store that round-trips every field but one is a store that
	// renders a policy for the wrong workflow.
	if got.Identity() != in.Identity() {
		t.Errorf("identity = %q, want %q", got.Identity(), in.Identity())
	}
	if got.ImageRepository != in.ImageRepository {
		t.Errorf("image repository = %q, want %q", got.ImageRepository, in.ImageRepository)
	}
}

func testPutReplacesWithoutMovingCreatedAt(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	first, err := s.Put(ctx, conn(tenantA, appA))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	second, err := s.Put(ctx, conn(tenantA, appA, func(c *forge.Connection) {
		c.Branch = "release"
	}))
	if err != nil {
		t.Fatalf("Put again: %v", err)
	}

	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("createdAt moved on update: %s then %s — the answer to \"since when has "+
			"this been connected\" is not supposed to change when the branch does",
			first.CreatedAt, second.CreatedAt)
	}
	if second.UpdatedAt.Before(first.UpdatedAt) {
		t.Error("updatedAt went backwards")
	}
}

func testListIsScopedToOneTenantAndOrdered(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	// Inserted out of order on purpose.
	for _, c := range []forge.Connection{
		conn(tenantA, appA),
		conn(tenantA, appB),
		conn(tenantB, "other"),
	} {
		if _, err := s.Put(ctx, c); err != nil {
			t.Fatalf("Put %s/%s: %v", c.TenantID, c.App, err)
		}
	}

	got, err := s.List(ctx, tenantA)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d connections, want 2 — a list that crosses tenants is a list "+
			"that shows one customer another's repositories", len(got))
	}
	if got[0].App != appB || got[1].App != appA {
		t.Errorf("order = %s, %s; want %s, %s — a page that reshuffles between loads "+
			"reads as data changing", got[0].App, got[1].App, appB, appA)
	}
}

// The invariant this store exists to hold.
//
// Two tenants connecting the same repository and branch would render the same
// certificate subject, and a policy cannot tell which tenant's build produced a
// signature carrying it. Each would accept the other's images.
func testAnIdentityBelongsToOneTenant(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.Put(ctx, conn(tenantA, appA)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A different tenant, a different app name, the same source repository.
	_, err := s.Put(ctx, conn(tenantB, "clone", func(c *forge.Connection) {
		c.Repo = appA
		c.ImageRepository = "ghcr.io/acme/" + appA
	}))
	if !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("second tenant claiming the same identity: err = %v, want ErrConflict — "+
			"both policies would then accept both tenants' signatures", err)
	}

	owner, err := s.IdentityOwner(ctx, conn(tenantA, appA).Identity())
	if err != nil {
		t.Fatalf("IdentityOwner: %v", err)
	}
	if owner != tenantA {
		t.Errorf("owner = %q, want %q", owner, tenantA)
	}
}

// The same tenant is not somebody else. Two of a tenant's own apps building
// from one monorepo on different branches are two identities; on the same
// branch they are one, and it is theirs either way.
func testOneTenantMayConnectOneRepoTwice(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.Put(ctx, conn(tenantA, appA)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Put(ctx, conn(tenantA, appB, func(c *forge.Connection) {
		c.Repo = appA
		c.Branch = "release"
	})); err != nil {
		t.Fatalf("the same tenant was refused its own repository on another branch: %v", err)
	}
}

// Two onboardings at the same moment, both pointed at one repository by a
// copy-pasted value. Whatever happens, exactly one wins and both agree who.
func testConcurrentClaimsOfOneIdentityAgree(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Go(func() {
			tenant := tenantA
			if i%2 == 1 {
				tenant = tenantB
			}
			_, errs[i] = s.Put(ctx, conn(tenant, appA))
		})
	}
	wg.Wait()

	owner, err := s.IdentityOwner(ctx, conn(tenantA, appA).Identity())
	if err != nil {
		t.Fatalf("IdentityOwner after the race: %v", err)
	}

	// Everything that failed must have failed as a conflict, and everything
	// that succeeded must belong to the tenant that holds the claim.
	for i, err := range errs {
		tenant := tenantA
		if i%2 == 1 {
			tenant = tenantB
		}
		switch {
		case err == nil && tenant != owner:
			t.Errorf("racer %d (%s) succeeded but %s holds the identity", i, tenant, owner)
		case err != nil && !errors.Is(err, forge.ErrConflict):
			t.Errorf("racer %d failed with %v, want ErrConflict", i, err)
		}
	}
}

func testMovingRepositoryReleasesTheOldIdentity(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	original := conn(tenantA, appA)
	if _, err := s.Put(ctx, original); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Put(ctx, conn(tenantA, appA, func(c *forge.Connection) {
		c.Repo = "storefront"
		c.ImageRepository = "ghcr.io/acme/storefront"
	})); err != nil {
		t.Fatalf("moving the connection: %v", err)
	}

	// Somebody else may now take what was released.
	if _, err := s.Put(ctx, conn(tenantB, "inherited", func(c *forge.Connection) {
		c.Repo = appA
		c.ImageRepository = "ghcr.io/acme/" + appA
	})); err != nil {
		t.Errorf("the abandoned identity stayed claimed: %v — an app that moves "+
			"repositories would leave one nobody can ever connect again", err)
	}
	_ = original
}

func testDeletingReleasesTheIdentity(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	c := conn(tenantA, appA)
	if _, err := s.Put(ctx, c); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, c.Key()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, c.Key()); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("Get after Delete: %v, want ErrNotFound", err)
	}
	if _, err := s.IdentityOwner(ctx, c.Identity()); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("IdentityOwner after Delete: %v, want ErrNotFound", err)
	}
	if _, err := s.Put(ctx, conn(tenantB, appA)); err != nil {
		t.Errorf("the identity stayed claimed after the connection was deleted: %v", err)
	}
}

// Validation is in one place so that three implementations cannot drift into
// disagreeing about what a connectable repository is.
func testAnUnusableConnectionIsRefused(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	for name, mutate := range map[string]func(*forge.Connection){
		"no tenant":        func(c *forge.Connection) { c.TenantID = "" },
		"no app":           func(c *forge.Connection) { c.App = "" },
		"no branch":        func(c *forge.Connection) { c.Branch = "" },
		"a wildcard repo":  func(c *forge.Connection) { c.Repo = "*" },
		"an unknown forge": func(c *forge.Connection) { c.Host = "git.internal.example" },
		"a workflow outside actions": func(c *forge.Connection) {
			c.WorkflowPath = "sign.yml"
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Put(ctx, conn(tenantA, appA, mutate)); err == nil {
				t.Error("stored, and would render a policy nobody can satisfy or " +
					"one that accepts more than the workflow the tenant approved")
			}
		})
	}
}

func testMissingThingsAreNotFound(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.Get(ctx, forge.Key{TenantID: tenantA, App: "absent"}); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("Get: %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, forge.Key{TenantID: tenantA, App: "absent"}); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("Delete: %v, want ErrNotFound", err)
	}
	absent := "https://github.com/nobody/nothing/.github/workflows/x.yml@refs/heads/main"
	if _, err := s.IdentityOwner(ctx, absent); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("IdentityOwner: %v, want ErrNotFound", err)
	}
	got, err := s.List(ctx, "t_nobody")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List for an unknown tenant returned %d rows", len(got))
	}
}
