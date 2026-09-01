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
		{"ANamespaceBelongsToOneTenant", testANamespaceBelongsToOneTenant},
		{"ConcurrentClaimsOfOneRepositoryAgree", testConcurrentClaimsOfOneRepositoryAgree},
		{"DeletingTheLastPlacementReleasesTheRepository", testDeletingTheLastPlacementReleasesTheRepository},
		{"DeletingTheLastPlacementReleasesTheNamespace", testDeletingTheLastPlacementReleasesTheNamespace},
		{"AnUnusablePlacementIsRefused", testAnUnusablePlacementIsRefused},
		{"NotFoundIsDistinguishable", testNotFoundIsDistinguishable},
		{"ATriggerRoundTripsAndSurvivesAReplace", testATriggerRoundTripsAndSurvivesAReplace},
		{"OneRepositoryFeedsEveryEnvironmentThatAsked", testOneRepositoryFeedsEveryEnvironmentThatAsked},
		{"ATriggerNobodySetIsNeverMatched", testATriggerNobodySetIsNeverMatched},
		{"ATriggerWithoutAPlacementIsRefused", testATriggerWithoutAPlacementIsRefused},
		{"DeletingThePlacementDeletesTheTrigger", testDeletingThePlacementDeletesTheTrigger},
		{"OneRepositoryIsFoundUnderEverySpellingAForgeSends", testOneRepositoryIsFoundUnderEverySpelling},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore) })
	}
}

// sourceRepo is the repository a push arrives for, and it is deliberately not
// repoA or repoB. Those are STATE repositories — where damga commits manifests
// — and a test that used one for both would pass with the two confused.
const (
	sourceRepo = "https://github.com/acme/api"

	// The forge a trigger is registered for, and the app the trigger cases use.
	// Named so that adding a case does not add another copy of a string this
	// file already repeats.
	triggerProvider = "github"
	triggerApp      = "api"
)

func trigger(tenant, app, env, secret string) placement.Trigger {
	return placement.Trigger{
		TenantID: tenant, App: app, Env: env,
		Provider: triggerProvider, RepoURL: sourceRepo, Secret: secret,
	}
}

func place(tenant, app, env, repo, path string) placement.Placement {
	return placement.Placement{
		TenantID: tenant, App: app, Env: env,
		RepoURL: repo, Branch: "main", Path: path, Namespace: app + "-" + env,
	}
}

// sharedNamespace is the one both tenants ask for in the namespace cases below.
// It is chosen rather than derived from the app, because the whole question
// there is what happens when two tenants name the same one.
const sharedNamespace = "acme-prod"

func inSharedNamespace(p placement.Placement) placement.Placement {
	p.Namespace = sharedNamespace
	return p
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

// The invariant one layer out from the repository claim: one namespace never
// holds two tenants.
//
// The repository claim protects what gets committed. This protects where it
// runs — the namespace is what the ResourceQuota, the Pod Security Admission
// level and the NetworkPolicy are attached to, so a tenant that could name a
// namespace another tenant is using would be deploying inside somebody else's
// fence. Nothing above this store can refuse it: by the time a manifest is
// rendered the namespace is already in it, and the request that got there was a
// well-formed one from an ordinary member of an ordinary tenant.
func testANamespaceBelongsToOneTenant(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	mustPut(t, s, inSharedNamespace(place(tenantA, "api", prod, repoA, "apps/api/prod")))

	// A different tenant, a different repository, a different app — and the
	// same namespace, which is the only thing that matters here.
	_, err := s.Put(ctx, inSharedNamespace(place(tenantB, "billing", prod, repoB, "apps/billing/prod")))
	if !errors.Is(err, placement.ErrConflict) {
		t.Fatalf("placing another tenant's app in the same namespace returned %v, want ErrConflict", err)
	}
	if _, err := s.Get(ctx, tenantB, "billing", prod); !errors.Is(err, placement.ErrNotFound) {
		t.Error("the refused placement was written anyway")
	}
	// And the refusal took the repository claim down with it. Without this the
	// loser has claimed repoB on its way to being told no, and can never place
	// anything there again.
	if owner, err := s.RepoOwner(ctx, repoB); err != nil || owner != "" {
		t.Errorf("a placement refused on its namespace claimed its repository: %q, %v", owner, err)
	}

	// The owner may keep putting its own apps in its own namespace: one
	// namespace per tenant and environment is the arrangement, not one per app.
	mustPut(t, s, inSharedNamespace(place(tenantA, "web", prod, repoA, "apps/web/prod")))
}

// A namespace stays claimed while anything points at it and is released when
// nothing does. Released independently of the repository, because an app that
// moves between namespaces keeps its repository and the reverse is also true.
func testDeletingTheLastPlacementReleasesTheNamespace(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	mustPut(t, s, inSharedNamespace(place(tenantA, "api", prod, repoA, "apps/api/prod")))
	mustPut(t, s, inSharedNamespace(place(tenantA, "web", prod, repoA, "apps/web/prod")))

	if err := s.Delete(ctx, tenantA, "api", prod); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Still claimed: web is still in it, so tenantB must still be refused.
	_, err := s.Put(ctx, inSharedNamespace(place(tenantB, "billing", prod, repoB, "apps/billing/prod")))
	if !errors.Is(err, placement.ErrConflict) {
		t.Fatalf("the namespace was released while a placement still pointed at it: %v", err)
	}

	if err := s.Delete(ctx, tenantA, "web", prod); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// And now somebody else can have it. Without the release, a tenant that is
	// removed and re-created leaves a namespace nobody can ever use again and
	// the only fix is editing the database.
	mustPut(t, s, inSharedNamespace(place(tenantB, "billing", prod, repoB, "apps/billing/prod")))
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

// A trigger is written against a placement that already exists and is found by
// the repository a push will name.
//
// The second half is the one worth having. Put is create-or-replace and is
// called by every edit of an app, so a Put that dropped the trigger would leave
// a webhook that GitHub still delivers to and that this store no longer
// recognises — a push that silently stops building, with the placement looking
// entirely correct.
func testATriggerRoundTripsAndSurvivesAReplace(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	mustPut(t, s, place(tenantA, triggerApp, prod, repoA, "apps/api/prod"))
	if err := s.SetTrigger(ctx, trigger(tenantA, triggerApp, prod, "s3cr3t")); err != nil {
		t.Fatalf("SetTrigger: %v", err)
	}

	got, err := s.TriggersFor(ctx, triggerProvider, sourceRepo)
	if err != nil {
		t.Fatalf("TriggersFor: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("TriggersFor = %d triggers, want 1", len(got))
	}
	if got[0].Secret != "s3cr3t" || got[0].App != triggerApp || got[0].Env != prod {
		t.Errorf("TriggersFor = %+v", got[0])
	}

	// The same app, moved to another directory. Everything about the placement
	// changes except the trigger.
	mustPut(t, s, place(tenantA, triggerApp, prod, repoA, "apps/api/live"))
	after, err := s.TriggersFor(ctx, triggerProvider, sourceRepo)
	if err != nil {
		t.Fatalf("TriggersFor after replace: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("the trigger did not survive Put: %d triggers", len(after))
	}
	if after[0].Secret != "s3cr3t" {
		t.Errorf("secret after replace = %q, want the one that was set", after[0].Secret)
	}
}

// One repository feeds several environments, and every one of them has to come
// back. Returning the first would build staging for ever and never production,
// with nothing anywhere saying why.
func testOneRepositoryFeedsEveryEnvironmentThatAsked(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	for _, env := range []string{"dev", prod} {
		mustPut(t, s, place(tenantA, triggerApp, env, repoA, "apps/api/"+env))
		if err := s.SetTrigger(ctx, trigger(tenantA, triggerApp, env, "shared")); err != nil {
			t.Fatalf("SetTrigger %s: %v", env, err)
		}
	}
	// Another tenant, the same source repository, a secret of their own. Two
	// tenants building one public repository is legitimate; being handed each
	// other's secret is not.
	mustPut(t, s, place(tenantB, "fork", prod, repoB, "apps/fork/prod"))
	if err := s.SetTrigger(ctx, trigger(tenantB, "fork", prod, "theirs")); err != nil {
		t.Fatalf("SetTrigger for the second tenant: %v", err)
	}

	got, err := s.TriggersFor(ctx, triggerProvider, sourceRepo)
	if err != nil {
		t.Fatalf("TriggersFor: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("TriggersFor = %d, want all three", len(got))
	}
	// Ordered, or which trigger a push matches depends on map iteration when
	// two share a secret.
	names := make([]string, 0, len(got))
	for _, tr := range got {
		names = append(names, tr.TenantID+"/"+tr.App+"/"+tr.Env)
	}
	if strings.Join(names, ",") != "t_alpha/api/dev,t_alpha/api/prod,t_beta/fork/prod" {
		t.Errorf("order = %v", names)
	}
}

// Every placement written before triggers existed carries empty columns, and
// the lookup is made by an unauthenticated caller. A query that matched them
// would hand the whole install to anyone who posted an empty body.
func testATriggerNobodySetIsNeverMatched(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	mustPut(t, s, place(tenantA, triggerApp, prod, repoA, "apps/api/prod"))
	mustPut(t, s, place(tenantB, "web", prod, repoB, "apps/web/prod"))

	for _, q := range []struct{ provider, repo string }{
		{"", ""},
		{triggerProvider, ""},
		{"", sourceRepo},
	} {
		got, err := s.TriggersFor(ctx, q.provider, q.repo)
		if err != nil {
			t.Fatalf("TriggersFor(%q, %q): %v", q.provider, q.repo, err)
		}
		if len(got) != 0 {
			t.Errorf("TriggersFor(%q, %q) = %d triggers, want none: an app that never "+
				"registered a webhook was returned to an unauthenticated caller",
				q.provider, q.repo, len(got))
		}
	}
}

// A trigger for an app that does not exist would verify signatures and have
// nowhere to send the build.
func testATriggerWithoutAPlacementIsRefused(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	err := s.SetTrigger(ctx, trigger(tenantA, "ghost", prod, "s3cr3t"))
	if !errors.Is(err, placement.ErrNotFound) {
		t.Errorf("SetTrigger for an app with no placement = %v, want ErrNotFound", err)
	}

	// And the shapes Validate refuses, because an empty secret is an endpoint
	// that builds whatever anybody posts to it.
	mustPut(t, s, place(tenantA, triggerApp, prod, repoA, "apps/api/prod"))
	for _, bad := range []struct {
		name string
		tr   placement.Trigger
	}{
		{"no secret", placement.Trigger{
			TenantID: tenantA, App: triggerApp, Env: prod, Provider: triggerProvider, RepoURL: sourceRepo}},
		{"no repository", placement.Trigger{
			TenantID: tenantA, App: triggerApp, Env: prod, Provider: triggerProvider, Secret: "x"}},
		{"no provider", placement.Trigger{
			TenantID: tenantA, App: triggerApp, Env: prod, RepoURL: sourceRepo, Secret: "x"}},
	} {
		if err := s.SetTrigger(ctx, bad.tr); !errors.Is(err, placement.ErrInvalid) {
			t.Errorf("SetTrigger with %s = %v, want ErrInvalid", bad.name, err)
		}
	}
}

// Deleting an app has to take its trigger with it. A trigger that outlived its
// placement is a webhook that verifies and then builds an app the control plane
// has forgotten.
func testDeletingThePlacementDeletesTheTrigger(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	mustPut(t, s, place(tenantA, triggerApp, prod, repoA, "apps/api/prod"))
	if err := s.SetTrigger(ctx, trigger(tenantA, triggerApp, prod, "s3cr3t")); err != nil {
		t.Fatalf("SetTrigger: %v", err)
	}
	if err := s.Delete(ctx, tenantA, triggerApp, prod); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := s.TriggersFor(ctx, triggerProvider, sourceRepo)
	if err != nil {
		t.Fatalf("TriggersFor: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("TriggersFor after Delete = %+v, want nothing", got)
	}
}

// A forge does not send one spelling. GitHub's push payload carries clone_url
// ending in ".git" and html_url without it, and whoever pasted the URL into the
// webhook form used whichever their browser showed them. An exact match works
// for about half of these, and the half that fails looks like a delivery that
// never happened.
func testOneRepositoryIsFoundUnderEverySpelling(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	mustPut(t, s, place(tenantA, triggerApp, prod, repoA, "apps/api/prod"))
	// Registered with the trailing .git, which is what a clone URL looks like.
	registered := trigger(tenantA, triggerApp, prod, "s3cr3t")
	registered.RepoURL = sourceRepo + ".git"
	if err := s.SetTrigger(ctx, registered); err != nil {
		t.Fatalf("SetTrigger: %v", err)
	}

	for _, spelling := range []string{
		sourceRepo,
		sourceRepo + ".git",
		sourceRepo + "/",
		strings.ToUpper(sourceRepo[:8]) + sourceRepo[8:],
	} {
		got, err := s.TriggersFor(ctx, triggerProvider, spelling)
		if err != nil {
			t.Fatalf("TriggersFor(%q): %v", spelling, err)
		}
		if len(got) != 1 {
			t.Errorf("TriggersFor(%q) = %d, want the one that was registered", spelling, len(got))
		}
	}
	// And still not something else.
	if got, err := s.TriggersFor(ctx, triggerProvider, "https://github.com/acme/other"); err != nil || len(got) != 0 {
		t.Errorf("TriggersFor for another repository = %d triggers, %v", len(got), err)
	}
}
