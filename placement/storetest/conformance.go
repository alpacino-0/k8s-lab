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

// Package storetest is the engine-agnostic contract for placement.Store.
//
// Written before the second implementation, which is the arrangement that has
// paid three times in this repository: the store that is written second passes
// first try, and the cases that only one store fails are the ones worth having.
package storetest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/damgahq/damga/placement"
)

// Factory makes a fresh, empty store. Called once per case: the cases assume
// they own it.
type Factory func(t *testing.T) placement.Store

const (
	tenantA = "t_alpha"
	tenantB = "t_beta"
	repoA   = "https://github.com/damgahq/tenant-alpha"
	repoB   = "https://github.com/damgahq/tenant-beta"
	prod    = "prod"
)

// Run executes the whole suite against one implementation.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"PlacementRoundTrips", testPlacementRoundTrips},
		{"PutReplacesWithoutMovingCreatedAt", testPutReplacesWithoutMovingCreatedAt},
		{"ListIsScopedToOneTenantAndOrdered", testListIsScopedToOneTenantAndOrdered},
		{"ARepositoryBelongsToOneTenant", testARepositoryBelongsToOneTenant},
		{"ConcurrentClaimsOfOneRepositoryAgree", testConcurrentClaimsOfOneRepositoryAgree},
		{"DeletingTheLastPlacementReleasesTheRepository", testDeletingTheLastPlacementReleasesTheRepository},
		{"AnUnusablePlacementIsRefused", testAnUnusablePlacementIsRefused},
		{"NotFoundIsDistinguishable", testNotFoundIsDistinguishable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore) })
	}
}

func place(tenant, app, env, repo, path string) placement.Placement {
	return placement.Placement{
		TenantID: tenant, App: app, Env: env,
		RepoURL: repo, Branch: "main", Path: path, Namespace: app + "-" + env,
	}
}

func mustPut(t *testing.T, s placement.Store, p placement.Placement) placement.Placement {
	t.Helper()
	got, err := s.Put(context.Background(), p)
	if err != nil {
		t.Fatalf("Put(%s/%s/%s): %v", p.TenantID, p.App, p.Env, err)
	}
	return got
}

func testPlacementRoundTrips(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	want := place(tenantA, "api", prod, repoA, "apps/api/prod")
	put := mustPut(t, s, want)
	if put.CreatedAt.IsZero() || put.UpdatedAt.IsZero() {
		t.Error("Put returned zero timestamps")
	}

	got, err := s.Get(ctx, tenantA, "api", prod)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	switch {
	case got.RepoURL != want.RepoURL:
		t.Errorf("RepoURL = %q, want %q", got.RepoURL, want.RepoURL)
	case got.Branch != want.Branch:
		t.Errorf("Branch = %q, want %q", got.Branch, want.Branch)
	case got.Path != want.Path:
		t.Errorf("Path = %q, want %q", got.Path, want.Path)
	case got.Namespace != want.Namespace:
		// Where the rendered manifest says it runs. A placement that loses it
		// renders a manifest with no namespace, which Argo CD applies into
		// whichever one its Application happens to name.
		t.Errorf("Namespace = %q, want %q", got.Namespace, want.Namespace)
	case !got.CreatedAt.Equal(put.CreatedAt):
		t.Errorf("CreatedAt did not survive: %s vs %s", got.CreatedAt, put.CreatedAt)
	}

	owner, err := s.RepoOwner(ctx, repoA)
	if err != nil {
		t.Fatalf("RepoOwner: %v", err)
	}
	if owner != tenantA {
		t.Errorf("RepoOwner = %q, want %q", owner, tenantA)
	}
}

// Put is create-or-replace, because "move this app to another directory" and
// "place it for the first time" are the same act from the caller's side. What
// it must not do is reset when the placement came into existence.
func testPutReplacesWithoutMovingCreatedAt(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	first := mustPut(t, s, place(tenantA, "api", prod, repoA, "apps/api/prod"))
	second := mustPut(t, s, place(tenantA, "api", prod, repoA, "environments/prod/api"))

	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt moved from %s to %s", first.CreatedAt, second.CreatedAt)
	}
	if second.UpdatedAt.Before(first.UpdatedAt) {
		t.Errorf("UpdatedAt went backwards: %s then %s", first.UpdatedAt, second.UpdatedAt)
	}
	got, err := s.Get(ctx, tenantA, "api", prod)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Path != "environments/prod/api" {
		t.Errorf("Path = %q, want the replacement", got.Path)
	}

	// One row, not two.
	all, err := s.List(ctx, tenantA)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("List returned %d placements after a replace, want 1", len(all))
	}
}

func testListIsScopedToOneTenantAndOrdered(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	// Inserted out of order, and "admin" sorts before "api", so an
	// implementation returning insertion order fails.
	mustPut(t, s, place(tenantA, "api", "staging", repoA, "apps/api/staging"))
	mustPut(t, s, place(tenantA, "api", prod, repoA, "apps/api/prod"))
	mustPut(t, s, place(tenantA, "admin", prod, repoA, "apps/admin/prod"))
	mustPut(t, s, place(tenantB, "billing", prod, repoB, "apps/billing/prod"))

	got, err := s.List(ctx, tenantA)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make([]string, 0, len(got))
	for _, p := range got {
		if p.TenantID != tenantA {
			t.Errorf("another tenant's placement is in the list: %+v", p)
		}
		names = append(names, p.App+"/"+p.Env)
	}
	if want := "admin/prod api/prod api/staging"; strings.Join(names, " ") != want {
		t.Errorf("List = %q, want %q", strings.Join(names, " "), want)
	}

	empty, err := s.List(ctx, "t_nobody")
	if err != nil || len(empty) != 0 {
		t.Errorf("List for a tenant with nothing = %v, %v", empty, err)
	}
}

// The invariant the write path cannot check for itself: one commit never
// touches two tenants.
//
// By the time gitwrite has a worktree it has already decided where to write, so
// this has to be refused before it gets there. A repository is claimed by the
// first tenant to place anything in it.
func testARepositoryBelongsToOneTenant(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	mustPut(t, s, place(tenantA, "api", prod, repoA, "apps/api/prod"))

	// A different tenant, a different app, a different path — and the same
	// repository, which is the only thing that matters.
	_, err := s.Put(ctx, place(tenantB, "billing", prod, repoA, "apps/billing/prod"))
	if !errors.Is(err, placement.ErrConflict) {
		t.Fatalf("placing another tenant's app in the same repository returned %v, want ErrConflict", err)
	}
	if _, err := s.Get(ctx, tenantB, "billing", prod); !errors.Is(err, placement.ErrNotFound) {
		t.Error("the refused placement was written anyway")
	}

	owner, err := s.RepoOwner(ctx, repoA)
	if err != nil {
		t.Fatalf("RepoOwner: %v", err)
	}
	if owner != tenantA {
		t.Errorf("RepoOwner = %q after a refused claim, want %q", owner, tenantA)
	}

	// The owner may keep placing more of its own apps there.
	mustPut(t, s, place(tenantA, "web", prod, repoA, "apps/web/prod"))
}

// Two tenants onboarding at the same moment, both pointed at the same
// repository by a copy-pasted value: exactly one may end up owning it, and the
// other has to be told so in a way the API can turn into an explanation.
//
// This case is a PROBABILISTIC detector and is documented as one. Replacing
// the insert-if-absent claim with a SELECT followed by a plain INSERT, it
// failed on one run in three — goroutines through database/sql do not reliably
// overlap the read-then-write window, so passing here is not evidence the
// claim is written correctly. The deterministic version, which holds one
// transaction open across another tenant's claim, is TestTheClaimIsAConstraint
// in placement/postgres. This one is here so every engine is covered at all,
// not because it is sufficient.
func testConcurrentClaimsOfOneRepositoryAgree(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	const racers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  []string
		errs []error
	)
	for i := range racers {
		wg.Go(func() {
			tenant := "t_" + string(rune('a'+i))
			_, err := s.Put(ctx, place(tenant, "api", prod, repoA, "apps/api/prod"))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won = append(won, tenant)
			} else {
				errs = append(errs, err)
			}
		})
	}
	wg.Wait()

	if len(won) != 1 {
		t.Fatalf("%d of %d tenants claimed the same repository, want exactly 1", len(won), racers)
	}
	for _, err := range errs {
		if !errors.Is(err, placement.ErrConflict) {
			t.Errorf("a losing racer got %v, want ErrConflict", err)
		}
	}
	owner, err := s.RepoOwner(ctx, repoA)
	if err != nil {
		t.Fatalf("RepoOwner: %v", err)
	}
	if owner != won[0] {
		t.Errorf("RepoOwner = %q but %q was told it won", owner, won[0])
	}
}

// A repository stays claimed while anything points at it and is released when
// nothing does. Without the release, a tenant that is removed and re-created —
// or one that moves between repositories — leaves a repository nobody can ever
// use again, and the only fix is editing the database.
func testDeletingTheLastPlacementReleasesTheRepository(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	mustPut(t, s, place(tenantA, "api", prod, repoA, "apps/api/prod"))
	mustPut(t, s, place(tenantA, "api", "staging", repoA, "apps/api/staging"))

	if err := s.Delete(ctx, tenantA, "api", prod); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Still claimed: staging is still there.
	if owner, err := s.RepoOwner(ctx, repoA); err != nil || owner != tenantA {
		t.Fatalf("RepoOwner after one delete = %q, %v", owner, err)
	}
	// And the other environment is untouched.
	if _, err := s.Get(ctx, tenantA, "api", "staging"); err != nil {
		t.Errorf("deleting prod removed staging too: %v", err)
	}

	if err := s.Delete(ctx, tenantA, "api", "staging"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	owner, err := s.RepoOwner(ctx, repoA)
	if err != nil {
		t.Fatalf("RepoOwner: %v", err)
	}
	if owner != "" {
		t.Errorf("RepoOwner = %q after the last placement was deleted, want it released", owner)
	}
	// And somebody else can now have it.
	mustPut(t, s, place(tenantB, "billing", prod, repoA, "apps/billing/prod"))

	// Deleting what is not there is not an error: the caller wanted it gone
	// and it is gone.
	if err := s.Delete(ctx, tenantA, "api", prod); err != nil {
		t.Errorf("Delete of an absent placement: %v", err)
	}
}

func testAnUnusablePlacementIsRefused(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	for _, c := range []struct {
		name string
		p    placement.Placement
	}{
		{"no tenant", place("", "api", prod, repoA, "apps/api")},
		{"no app", place(tenantA, "", prod, repoA, "apps/api")},
		{"no environment", place(tenantA, "api", "", repoA, "apps/api")},
		{"no repository", place(tenantA, "api", prod, "", "apps/api")},
		{"no namespace", placement.Placement{
			TenantID: tenantA, App: "api", Env: prod, RepoURL: repoA,
			Branch: "main", Path: "apps/api",
		}},
		// Not defaulted to main: a wrong guess commits to a branch nothing is
		// watching, which looks exactly like a deploy that worked.
		{"no branch", placement.Placement{
			TenantID: tenantA, App: "api", Env: prod, RepoURL: repoA,
			Path: "apps/api", Namespace: "api-prod",
		}},
		{"the repository root", place(tenantA, "api", prod, repoA, "")},
		{"the repository root, spelled .", place(tenantA, "api", prod, repoA, ".")},
		{"an absolute path", place(tenantA, "api", prod, repoA, "/etc/apps/api")},
		// go-git resolves this against the worktree and writes outside the
		// checkout.
		{"a path that escapes", place(tenantA, "api", prod, repoA, "apps/../../elsewhere")},
	} {
		if _, err := s.Put(ctx, c.p); !errors.Is(err, placement.ErrInvalid) {
			t.Errorf("%s: Put returned %v, want ErrInvalid", c.name, err)
		}
	}

	// Nothing was written, and the repository was not claimed by any of them.
	if owner, err := s.RepoOwner(ctx, repoA); err != nil || owner != "" {
		t.Errorf("a refused placement claimed the repository: %q, %v", owner, err)
	}
}

func testNotFoundIsDistinguishable(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.Get(ctx, tenantA, "api", prod); !errors.Is(err, placement.ErrNotFound) {
		t.Errorf("Get of nothing returned %v, want ErrNotFound", err)
	}
	// An unclaimed repository is the empty string and not an error: "nobody
	// owns this" is an answer.
	owner, err := s.RepoOwner(ctx, repoA)
	if err != nil {
		t.Errorf("RepoOwner of an unclaimed repository: %v", err)
	}
	if owner != "" {
		t.Errorf("RepoOwner of an unclaimed repository = %q", owner)
	}
}
