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

// Package sqlstore is forge.Store in SQL, written once and configured per
// engine — the same arrangement the other three stores use.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/damgahq/damga/forge"
	"github.com/damgahq/damga/internal/sqlmigrate"
)

// Dialect is what this package needs of an engine, which is exactly what the
// migration runner needs. An alias rather than a restatement so the two cannot
// drift.
type Dialect = sqlmigrate.Dialect

// timeLayout is fixed-width RFC3339 in UTC, matching the other schemas:
// lexicographic order is chronological on both engines, with no CAST.
const timeLayout = "2006-01-02T15:04:05.000000Z"

func asText(t time.Time) string { return t.UTC().Truncate(time.Microsecond).Format(timeLayout) }

func fromText(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, s)
}

// Store is forge.Store backed by a SQL database.
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
	// Its own table, like the others. Sharing one means one version read with
	// MAX(version), and this sequence's 1 would be silently skipped.
	if err := sqlmigrate.Run(ctx, db, d, "forge_schema_migration"); err != nil {
		return nil, err
	}
	return &Store{db: db, d: d, now: time.Now}, nil
}

// Put is forge.Store.Put.
func (s *Store) Put(ctx context.Context, c forge.Connection) (forge.Connection, error) {
	if err := c.Validate(); err != nil {
		return forge.Connection{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	identity := c.Identity()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return forge.Connection{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Claim first: insert-if-absent, then read back who is actually there.
	//
	// The same two-part arrangement placement uses, for the same measured
	// reason. The PRIMARY KEY on identity is what makes it impossible for two
	// tenants to hold one certificate subject. The insert-if-absent-then-read
	// is what makes the loser find out in a way the API can act on: a SELECT
	// followed by a plain INSERT gives a losing racer a driver-level unique
	// violation with no sentinel behind it, which becomes a 500 telling an
	// operator the server is broken when what happened is that they connected
	// a repository somebody else already had.
	if _, err := tx.ExecContext(ctx, s.d.Rebind(`
		INSERT INTO forge_identity (identity, tenant_id, claimed_at) VALUES (?, ?, ?)
		ON CONFLICT (identity) DO NOTHING`),
		identity, c.TenantID, asText(now)); err != nil {
		return forge.Connection{}, err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, s.d.Rebind(
		`SELECT tenant_id FROM forge_identity WHERE identity = ?`), identity).Scan(&owner); err != nil {
		return forge.Connection{}, err
	}
	if owner != c.TenantID {
		return forge.Connection{}, fmt.Errorf(
			"%w: the identity %s is already connected by another tenant",
			forge.ErrConflict, identity)
	}

	// CreatedAt is preserved across a replace: changing the branch a repository
	// is built from is not the repository being connected for the first time.
	var created string
	err = tx.QueryRowContext(ctx, s.d.Rebind(`
		SELECT created_at FROM forge_connection WHERE tenant_id = ? AND app = ?`),
		c.TenantID, c.App).Scan(&created)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		created = asText(now)
	case err != nil:
		return forge.Connection{}, err
	}

	// identity is stored alongside the parts it is made of. Denormalised on
	// purpose: it is the key the claim table joins on, and deriving it in SQL
	// instead would put a second copy of Connection.Identity in another
	// language. See releaseUnused.
	if _, err := tx.ExecContext(ctx, s.d.Rebind(`
		INSERT INTO forge_connection
			(tenant_id, app, host, owner, repo, branch, workflow_path, image_repository,
			 identity, first_signature_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, app) DO UPDATE SET
			host = excluded.host,
			owner = excluded.owner,
			repo = excluded.repo,
			branch = excluded.branch,
			workflow_path = excluded.workflow_path,
			image_repository = excluded.image_repository,
			identity = excluded.identity,
			first_signature_at = excluded.first_signature_at,
			updated_at = excluded.updated_at`),
		c.TenantID, c.App, c.Host, c.Owner, c.Repo, c.Branch, c.WorkflowPath,
		c.ImageRepository, identity, firstSignature(c), created, asText(now)); err != nil {
		return forge.Connection{}, err
	}

	// A replace can be the last thing holding the OLD identity, which then has
	// to be released — otherwise an app that moves repositories leaves a claim
	// nobody can ever take, including the tenant who abandoned it.
	if err := releaseUnused(ctx, tx, s.d); err != nil {
		return forge.Connection{}, err
	}
	if err := tx.Commit(); err != nil {
		return forge.Connection{}, err
	}

	c.CreatedAt, err = fromText(created)
	if err != nil {
		return forge.Connection{}, err
	}
	c.UpdatedAt = now
	return c, nil
}

const columns = `tenant_id, app, host, owner, repo, branch, workflow_path, image_repository, ` +
	`first_signature_at, created_at, updated_at`

func scan(rows interface{ Scan(...any) error }) (forge.Connection, error) {
	var c forge.Connection
	var firstSig, created, updated string
	if err := rows.Scan(&c.TenantID, &c.App, &c.Host, &c.Owner, &c.Repo, &c.Branch,
		&c.WorkflowPath, &c.ImageRepository, &firstSig, &created, &updated); err != nil {
		return forge.Connection{}, err
	}
	var err error
	if c.FirstSignatureAt, err = fromText(firstSig); err != nil {
		return forge.Connection{}, err
	}
	if c.CreatedAt, err = fromText(created); err != nil {
		return forge.Connection{}, err
	}
	if c.UpdatedAt, err = fromText(updated); err != nil {
		return forge.Connection{}, err
	}
	return c, nil
}

// Get is forge.Store.Get.
func (s *Store) Get(ctx context.Context, k forge.Key) (forge.Connection, error) {
	row := s.db.QueryRowContext(ctx, s.d.Rebind(
		`SELECT `+columns+` FROM forge_connection WHERE tenant_id = ? AND app = ?`),
		k.TenantID, k.App)
	c, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return forge.Connection{}, fmt.Errorf("%w: %s/%s", forge.ErrNotFound, k.TenantID, k.App)
	}
	if err != nil {
		return forge.Connection{}, err
	}
	return c, nil
}

// List is forge.Store.List.
func (s *Store) List(ctx context.Context, tenantID string) ([]forge.Connection, error) {
	rows, err := s.db.QueryContext(ctx, s.d.Rebind(
		`SELECT `+columns+` FROM forge_connection WHERE tenant_id = ? ORDER BY app`), tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []forge.Connection{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Delete is forge.Store.Delete.
func (s *Store) Delete(ctx context.Context, k forge.Key) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.d.Rebind(
		`DELETE FROM forge_connection WHERE tenant_id = ? AND app = ?`), k.TenantID, k.App)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s/%s", forge.ErrNotFound, k.TenantID, k.App)
	}
	if err := releaseUnused(ctx, tx, s.d); err != nil {
		return err
	}
	return tx.Commit()
}

// IdentityOwner is forge.Store.IdentityOwner.
func (s *Store) IdentityOwner(ctx context.Context, identity string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, s.d.Rebind(
		`SELECT tenant_id FROM forge_identity WHERE identity = ?`), identity).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", forge.ErrNotFound, identity)
	}
	return owner, err
}

// firstSignature is the zero time as the empty string, so an unverified
// connection stores what the schema defaults to rather than a year-1 timestamp
// that sorts before everything and reads as a real observation.
func firstSignature(c forge.Connection) string {
	if c.FirstSignatureAt.IsZero() {
		return ""
	}
	return asText(c.FirstSignatureAt)
}

// Close is forge.Store.Close.
func (s *Store) Close() error { return s.db.Close() }

// releaseUnused drops the claim on any identity no connection holds any more.
//
// It joins on a stored column rather than rebuilding the subject in SQL, and
// that is the whole reason forge_connection carries one. Concatenating the five
// parts here would be a second implementation of Connection.Identity living in
// a different language, and the failure it produces is silent: change the
// format in Go and this DELETE stops matching anything, so every claim ever
// made leaks and the second tenant to connect a repository is refused for a
// connection that no longer exists.
//
// The stored column can only go stale in one direction, and that direction is
// loud — the conformance suite looks the claim up by the Go-computed identity,
// so a row written with a different string is a failing test rather than a
// missing row nobody queries.
func releaseUnused(ctx context.Context, tx *sql.Tx, d Dialect) error {
	_, err := tx.ExecContext(ctx, d.Rebind(`
		DELETE FROM forge_identity
		WHERE identity NOT IN (SELECT identity FROM forge_connection)`))
	return err
}
