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

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/damgahq/damga/placement"
	"github.com/damgahq/damga/placement/postgres"
)

// The deterministic version of ConcurrentClaimsOfOneRepositoryAgree.
//
// That one spawns goroutines and hopes they overlap, and measurement says they
// mostly do not: with the primary-key claim replaced by a SELECT followed by an
// INSERT, it failed on one run in three. A control that a test only sometimes
// notices the absence of is a control nobody can be said to have tested.
//
// So this forces the interleaving instead. One transaction claims the
// repository for tenant B and is held open; a second tenant's Put then runs
// against it. There is no timing to get lucky with: the second claim cannot
// have seen the first, because the first has not committed.
//
// PostgreSQL and not SQLite, because SQLite serialises every writer behind one
// lock and would give the right answer even if the claim were a read followed
// by a write — which is exactly the false confidence this exists to avoid.
func TestTheClaimIsAConstraint(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run this", dsnEnv)
	}
	ctx := context.Background()
	store, scopedDSN := scopedStore(t, dsn, "claim")

	const repo = "https://github.com/damgahq/contested"

	// A second pool, so the held transaction cannot be the same connection the
	// store is about to use.
	other, err := sql.Open("pgx", scopedDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = other.Close() }()

	tx, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	// Rolled back on every path out of here, including a failing assertion.
	// Without this, a Fatalf below leaves the transaction holding locks on the
	// schema, and the cleanup's DROP SCHEMA CASCADE waits for them — the test
	// stops reporting a failure and starts hanging until the whole package
	// times out. Measured: ten minutes instead of one line.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO repo_owner (repo_url, tenant_id, claimed_at) VALUES ($1, $2, $3)`,
		repo, "t_beta", "2026-08-27T00:00:00.000000Z"); err != nil {
		t.Fatalf("the held claim: %v", err)
	}
	// Not committed. Tenant alpha's Put now runs while tenant beta's claim
	// exists and is invisible to any read.

	done := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, placement.Placement{
			TenantID: "t_alpha", App: "api", Env: "prod",
			RepoURL: repo, Branch: "main", Path: "apps/api/prod",
			Namespace: "alpha-prod",
		})
		done <- err
	}()

	// Give the Put long enough to reach the claim and block on it. If it has
	// already returned by now, it did not block — which is the failure this
	// test exists to catch, and it is reported below rather than here.
	select {
	case err := <-done:
		t.Fatalf("Put returned %v before the conflicting claim was committed: it never blocked", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("committing the held claim: %v", err)
	}
	committed = true

	select {
	case err := <-done:
		if !errors.Is(err, placement.ErrConflict) {
			t.Fatalf("Put returned %v, want ErrConflict", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Put never returned after the conflicting claim was committed")
	}

	// And the repository is beta's, with nothing of alpha's written.
	owner, err := store.RepoOwner(ctx, repo)
	if err != nil {
		t.Fatalf("RepoOwner: %v", err)
	}
	if owner != "t_beta" {
		t.Errorf("RepoOwner = %q, want t_beta", owner)
	}
	if _, err := store.Get(ctx, "t_alpha", "api", "prod"); !errors.Is(err, placement.ErrNotFound) {
		t.Errorf("the refused placement was written anyway: %v", err)
	}
}

// scopedStore opens a placement store inside a schema of its own, so two cases
// against one database cannot see each other's rows or each other's claims.
//
// Extracted when the second case arrived rather than copied: the cleanup here
// has a failure mode worth having in one place — a DROP SCHEMA CASCADE waits on
// any transaction still holding a lock, so a case that leaves one open stops
// reporting a failure and starts hanging until the package times out.
func scopedStore(t *testing.T, dsn, what string) (placement.Store, string) {
	t.Helper()
	ctx := context.Background()

	schema := fmt.Sprintf("placement_%s_%d", what, time.Now().UnixNano())
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Errorf("DROP SCHEMA: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("closing the admin connection: %v", err)
		}
	})

	scoped, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	q := scoped.Query()
	q.Set("search_path", schema)
	scoped.RawQuery = q.Encode()

	store, err := postgres.Open(ctx, scoped.String())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, scoped.String()
}

// The unique indexes in 0005, against the thing the SELECT in Put cannot do.
//
// Put asks "does anything already land here" and then writes. Under READ
// COMMITTED that question is answered from a snapshot: a row another
// transaction has inserted and not committed is invisible, so two environments
// landing in one directory both see nothing and both proceed. The index is what
// makes the second one fail, and without it the SELECT is a check that passes
// at exactly the moment it is needed.
//
// The same forced interleaving as TestTheClaimIsAConstraint, and PostgreSQL for
// the same reason: SQLite serialises writers behind one lock and would answer
// correctly even with no index at all.
//
// What this asserts is narrower than the conformance suite's version, and
// deliberately so. There the refusal is ErrConflict with a sentence, because
// the SELECT saw the row. Here the SELECT could not, so what is left is the
// constraint — and a driver error is the right outcome. The claim being tested
// is that the write does not succeed.
func TestTwoEnvironmentsCannotRaceIntoOneDirectory(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run this", dsnEnv)
	}
	ctx := context.Background()
	store, scopedDSN := scopedStore(t, dsn, "landing")

	const (
		repo   = "https://github.com/damgahq/two-envs"
		tenant = "t_alpha"
		dir    = "apps/api"
		app    = "api"
		branch = "main"
	)

	// prod exists and is committed, so the repository and namespace claims are
	// already alpha's and neither of those is what answers below.
	if _, err := store.Put(ctx, placement.Placement{
		TenantID: tenant, App: app, Env: "prod",
		RepoURL: repo, Branch: branch, Path: "other/api", Namespace: "alpha-prod",
	}); err != nil {
		t.Fatalf("the committed placement: %v", err)
	}

	other, err := sql.Open("pgx", scopedDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = other.Close() }()

	tx, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	// staging takes the directory, and does not commit. Nothing can read this
	// row, which is the whole point.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO placement (tenant_id, app, env, repo_url, branch, path, namespace, created_at, updated_at)
		VALUES ($1, $5, 'staging', $2, $6, $3, 'alpha-staging', $4, $4)`,
		tenant, repo, dir, "2026-09-01T00:00:00.000000Z", app, branch); err != nil {
		t.Fatalf("the held placement: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, placement.Placement{
			TenantID: tenant, App: app, Env: "qa",
			RepoURL: repo, Branch: branch, Path: dir, Namespace: "alpha-qa",
		})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Put returned %v while an uncommitted row held %q: it never blocked, so "+
			"nothing but timing was keeping two environments out of one directory", err, dir)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("committing the held placement: %v", err)
	}
	committed = true

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("both environments were written into one directory: each deploy now " +
				"overwrites the other's workload.yaml, and neither says so")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Put never returned after the held row was committed")
	}

	if _, err := store.Get(ctx, tenant, app, "qa"); !errors.Is(err, placement.ErrNotFound) {
		t.Errorf("the refused placement was written anyway: %v", err)
	}
}
