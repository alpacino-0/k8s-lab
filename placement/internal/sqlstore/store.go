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

// Package sqlstore is placement.Store in SQL, written once and configured per
// engine — the same arrangement the evidence and identity stores use.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/damgahq/damga/internal/sqlmigrate"
	"github.com/damgahq/damga/placement"
)

// Dialect is what this package needs of an engine, which is exactly what the
// migration runner needs. An alias rather than a restatement so the two cannot
// drift.
type Dialect = sqlmigrate.Dialect

// timeLayout is fixed-width RFC3339 in UTC, matching the other two schemas:
// lexicographic order is chronological on both engines, with no CAST.
const timeLayout = "2006-01-02T15:04:05.000000Z"

func asText(t time.Time) string { return t.UTC().Truncate(time.Microsecond).Format(timeLayout) }

func fromText(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, s)
}

// Store is placement.Store backed by a SQL database.
type Store struct {
	db  *sql.DB
	d   Dialect
	now func() time.Time
}

// New wraps an open pool and applies any pending migrations.
func New(ctx context.Context, db *sql.DB, d Dialect) (*Store, error) {
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: database is not usable: %w", d.Name(), err)
	}
	// Its own table, like the identity store's. Sharing one means one version
	// read with MAX(version), and this sequence's 1 would be silently skipped.
	if err := sqlmigrate.Run(ctx, db, d, "placement_schema_migration"); err != nil {
		return nil, err
	}
	return &Store{db: db, d: d, now: time.Now}, nil
}

// Put is placement.Store.Put.
func (s *Store) Put(ctx context.Context, p placement.Placement) (placement.Placement, error) {
	if err := p.Validate(); err != nil {
		return placement.Placement{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return placement.Placement{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Claim first: insert-if-absent, then read back who is actually there.
	//
	// Two things are doing work and they are worth separating. The PRIMARY KEY
	// on repo_url is what makes it impossible for two tenants to both own a
	// repository. The insert-if-absent-then-read-back is what makes the loser
	// find out in a way anything can act on: written as a SELECT followed by a
	// plain INSERT, a losing racer gets `duplicate key value violates unique
	// constraint "repo_owner_pkey" (SQLSTATE 23505)` — a driver error with no
	// sentinel, which the API turns into a 500 telling an operator the server
	// is broken when what actually happened is that they typed the wrong
	// repository. Measured, not assumed: see TestTheClaimIsAConstraint.
	if _, err := tx.ExecContext(ctx, s.d.Rebind(`
		INSERT INTO repo_owner (repo_url, tenant_id, claimed_at) VALUES (?, ?, ?)
		ON CONFLICT (repo_url) DO NOTHING`),
		p.RepoURL, p.TenantID, asText(now)); err != nil {
		return placement.Placement{}, err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, s.d.Rebind(
		`SELECT tenant_id FROM repo_owner WHERE repo_url = ?`), p.RepoURL).Scan(&owner); err != nil {
		return placement.Placement{}, err
	}
	if owner != p.TenantID {
		return placement.Placement{}, fmt.Errorf(
			"%w: repository %q belongs to another tenant", placement.ErrConflict, p.RepoURL)
	}

	// CreatedAt is preserved across a replace: moving an app to another
	// directory is not the app coming into existence.
	var created string
	err = tx.QueryRowContext(ctx, s.d.Rebind(`
		SELECT created_at FROM placement WHERE tenant_id = ? AND app = ? AND env = ?`),
		p.TenantID, p.App, p.Env).Scan(&created)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		created = asText(now)
	case err != nil:
		return placement.Placement{}, err
	}

	if _, err := tx.ExecContext(ctx, s.d.Rebind(`
		INSERT INTO placement (tenant_id, app, env, repo_url, branch, path, namespace, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, app, env) DO UPDATE SET
			repo_url = excluded.repo_url,
			branch = excluded.branch,
			path = excluded.path,
			namespace = excluded.namespace,
			updated_at = excluded.updated_at`),
		p.TenantID, p.App, p.Env, p.RepoURL, p.Branch, p.Path, p.Namespace,
		created, asText(now)); err != nil {
		return placement.Placement{}, err
	}

	// A replace can be the last thing pointing at the OLD repository, which
	// then has to be released — otherwise moving an app between repositories
	// leaves one nobody can ever claim again.
	if err := releaseUnused(ctx, tx, s.d); err != nil {
		return placement.Placement{}, err
	}
	if err := tx.Commit(); err != nil {
		return placement.Placement{}, err
	}

	p.CreatedAt, err = fromText(created)
	if err != nil {
		return placement.Placement{}, err
	}
	p.UpdatedAt = now
	return p, nil
}

// Get is placement.Store.Get.
func (s *Store) Get(ctx context.Context, tenantID, app, env string) (placement.Placement, error) {
	p := placement.Placement{TenantID: tenantID, App: app, Env: env}
	var created, updated string
	err := s.db.QueryRowContext(ctx, s.d.Rebind(`
		SELECT repo_url, branch, path, namespace, created_at, updated_at FROM placement
		WHERE tenant_id = ? AND app = ? AND env = ?`), tenantID, app, env).
		Scan(&p.RepoURL, &p.Branch, &p.Path, &p.Namespace, &created, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return placement.Placement{}, fmt.Errorf("%w: %s/%s/%s", placement.ErrNotFound, tenantID, app, env)
	case err != nil:
		return placement.Placement{}, err
	}
	return withTimes(p, created, updated)
}

// List is placement.Store.List.
func (s *Store) List(ctx context.Context, tenantID string) ([]placement.Placement, error) {
	if tenantID == "" {
		// Not "every tenant". An empty filter here would be the one query in
		// this store that crosses a customer boundary.
		return nil, fmt.Errorf("%w: List needs a tenant", placement.ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, s.d.Rebind(`
		SELECT app, env, repo_url, branch, path, namespace, created_at, updated_at FROM placement
		WHERE tenant_id = ? ORDER BY app, env`), tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []placement.Placement
	for rows.Next() {
		p := placement.Placement{TenantID: tenantID}
		var created, updated string
		if err := rows.Scan(&p.App, &p.Env, &p.RepoURL, &p.Branch, &p.Path, &p.Namespace,
			&created, &updated); err != nil {
			return nil, err
		}
		p, err = withTimes(p, created, updated)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete is placement.Store.Delete.
func (s *Store) Delete(ctx context.Context, tenantID, app, env string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.d.Rebind(
		`DELETE FROM placement WHERE tenant_id = ? AND app = ? AND env = ?`),
		tenantID, app, env); err != nil {
		return err
	}
	if err := releaseUnused(ctx, tx, s.d); err != nil {
		return err
	}
	return tx.Commit()
}

// RepoOwner is placement.Store.RepoOwner.
func (s *Store) RepoOwner(ctx context.Context, repoURL string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, s.d.Rebind(
		`SELECT tenant_id FROM repo_owner WHERE repo_url = ?`), repoURL).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Unclaimed is an answer, not an error.
		return "", nil
	case err != nil:
		return "", err
	}
	return owner, nil
}

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// releaseUnused drops the claim on any repository nothing points at any more.
// Called inside the same transaction as the write that may have orphaned it,
// so there is no window in which a repository is unreferenced and still owned.
func releaseUnused(ctx context.Context, tx *sql.Tx, d Dialect) error {
	_, err := tx.ExecContext(ctx, d.Rebind(`
		DELETE FROM repo_owner
		WHERE repo_url NOT IN (SELECT repo_url FROM placement)`))
	return err
}

func withTimes(p placement.Placement, created, updated string) (placement.Placement, error) {
	var err error
	if p.CreatedAt, err = fromText(created); err != nil {
		return placement.Placement{}, err
	}
	if p.UpdatedAt, err = fromText(updated); err != nil {
		return placement.Placement{}, err
	}
	return p, nil
}
