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

// Package sqlstore is identity.Store in SQL, written once and configured per
// engine — the same arrangement the evidence store uses, and for the same
// reason: SQLite is what a one-node install runs and PostgreSQL is what the
// paid archive requires, so writing the logic twice would mean auditing every
// write path against both engines forever.
//
// The dialect surface here is smaller than the evidence store's. That one needs
// a row-locking clause because its state transitions are compare-and-set over a
// value it has just read; nothing in identity is. Uniqueness is enforced by the
// constraints, and the two operations that must be atomic — creating an account
// with its credential, and replacing a password while revoking every session —
// are transactions rather than read-modify-writes.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/damgahq/damga/identity"
	"github.com/damgahq/damga/internal/sqlmigrate"
)

// Dialect is what this package needs of an engine, which is exactly what the
// migration runner needs. Declared as an alias rather than restated so the two
// cannot drift.
type Dialect = sqlmigrate.Dialect

// timeLayout is fixed-width RFC3339 in UTC, matching the evidence schema:
// lexicographic order is chronological on both engines, with no CAST.
const timeLayout = "2006-01-02T15:04:05.000000Z"

func asText(t time.Time) string { return t.UTC().Truncate(time.Microsecond).Format(timeLayout) }

func fromText(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, s)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fold matches the email_folded column. A person who signed up as Orhan@ and
// types orhan@ has not made a mistake.
func fold(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// Store is identity.Store backed by a SQL database.
type Store struct {
	db  *sql.DB
	d   Dialect
	now func() time.Time
}

// New wraps an open pool and applies any pending migrations. The caller builds
// the pool, because the DSN and the limits are the engine-specific part.
func New(ctx context.Context, db *sql.DB, d Dialect) (*Store, error) {
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: database is not usable: %w", d.Name(), err)
	}
	// Its own table. Sharing the evidence store's would mean one version read
	// with MAX(version), and this sequence's 1 would be silently skipped.
	if err := sqlmigrate.Run(ctx, db, d, "identity_schema_migration"); err != nil {
		return nil, err
	}
	return &Store{db: db, d: d, now: time.Now}, nil
}

func (s *Store) exec(ctx context.Context, q string, args ...any) error {
	_, err := s.db.ExecContext(ctx, s.d.Rebind(q), args...)
	return err
}

func (s *Store) row(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.d.Rebind(q), args...)
}

// isUnique reports whether an error is a uniqueness violation. Matched on the
// message rather than on a code, because the two drivers spell the code
// differently and this package holds no driver-specific import — that is what
// keeps the engine choice in one file.
func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}

// ---------------------------------------------------------------- tenants

func (s *Store) CreateTenant(ctx context.Context, t identity.Tenant) (identity.Tenant, error) {
	if t.ID == "" || t.Slug == "" {
		return identity.Tenant{}, fmt.Errorf("%w: a tenant needs an id and a slug", identity.ErrInvalid)
	}
	if !t.Tier.Valid() {
		return identity.Tenant{}, fmt.Errorf("%w: tier %q", identity.ErrInvalid, t.Tier)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = s.now()
	}
	t.CreatedAt = t.CreatedAt.UTC().Truncate(time.Microsecond)

	err := s.exec(ctx, `
		INSERT INTO tenant (id, slug, display_name, tier, suspended, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Slug, t.DisplayName, string(t.Tier), boolInt(t.Suspended), asText(t.CreatedAt))
	switch {
	case isUnique(err):
		return identity.Tenant{}, fmt.Errorf("%w: tenant %q or slug %q", identity.ErrDuplicate, t.ID, t.Slug)
	case err != nil:
		return identity.Tenant{}, err
	}
	return t, nil
}

func (s *Store) Tenant(ctx context.Context, id string) (identity.Tenant, error) {
	return s.scanTenant(s.row(ctx, tenantSelect+` WHERE id = ?`, id))
}

func (s *Store) TenantBySlug(ctx context.Context, slug string) (identity.Tenant, error) {
	return s.scanTenant(s.row(ctx, tenantSelect+` WHERE slug = ?`, slug))
}

const tenantSelect = `SELECT id, slug, display_name, tier, suspended, created_at FROM tenant`

func (s *Store) scanTenant(r *sql.Row) (identity.Tenant, error) {
	var (
		t         identity.Tenant
		tier      string
		suspended int
		created   string
	)
	err := r.Scan(&t.ID, &t.Slug, &t.DisplayName, &tier, &suspended, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Tenant{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.Tenant{}, err
	}
	t.Tier, t.Suspended = identity.Tier(tier), suspended == 1
	if t.CreatedAt, err = fromText(created); err != nil {
		return identity.Tenant{}, err
	}
	return t, nil
}

// ---------------------------------------------------------------- accounts

func (s *Store) CreateAccount(
	ctx context.Context, a identity.Account, cred identity.Credential,
) (identity.Account, error) {
	switch {
	case a.ID == "" || a.Email == "":
		return identity.Account{}, fmt.Errorf("%w: an account needs an id and an email", identity.ErrInvalid)
	case a.Kind != "user" && a.Kind != "automation":
		return identity.Account{}, fmt.Errorf("%w: kind %q", identity.ErrInvalid, a.Kind)
	case a.AuditEmail == "":
		// Never defaulted. What goes into a commit and into the hash chain can
		// never be redacted, so choosing it is the caller's.
		return identity.Account{}, fmt.Errorf("%w: an account needs an audit email", identity.ErrInvalid)
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	a.CreatedAt = a.CreatedAt.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.Account{}, err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, s.d.Rebind(`
		INSERT INTO account (id, kind, email, email_folded, audit_email, display_name, disabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		a.ID, a.Kind, a.Email, fold(a.Email), a.AuditEmail, a.DisplayName,
		boolInt(a.Disabled), asText(a.CreatedAt))
	switch {
	case isUnique(err):
		return identity.Account{}, fmt.Errorf("%w: account %q or email %q", identity.ErrDuplicate, a.ID, a.Email)
	case err != nil:
		return identity.Account{}, err
	}

	// The credential goes in the same transaction, so an account can never
	// exist with a password that failed to write — which would be an account
	// nobody can log into and nothing reports.
	if cred.Hash != "" {
		if cred.UpdatedAt.IsZero() {
			cred.UpdatedAt = a.CreatedAt
		}
		if _, err := tx.ExecContext(ctx, s.d.Rebind(
			`INSERT INTO credential (account_id, hash, updated_at) VALUES (?, ?, ?)`),
			a.ID, cred.Hash, asText(cred.UpdatedAt)); err != nil {
			return identity.Account{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return identity.Account{}, err
	}
	return a, nil
}

const accountSelect = `SELECT id, kind, email, audit_email, display_name, disabled, created_at FROM account`

func (s *Store) Account(ctx context.Context, id string) (identity.Account, error) {
	return s.scanAccount(s.row(ctx, accountSelect+` WHERE id = ?`, id))
}

func (s *Store) AccountByEmail(ctx context.Context, email string) (identity.Account, error) {
	return s.scanAccount(s.row(ctx, accountSelect+` WHERE email_folded = ?`, fold(email)))
}

func (s *Store) scanAccount(r *sql.Row) (identity.Account, error) {
	var (
		a        identity.Account
		disabled int
		created  string
	)
	err := r.Scan(&a.ID, &a.Kind, &a.Email, &a.AuditEmail, &a.DisplayName, &disabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Account{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.Account{}, err
	}
	a.Disabled = disabled == 1
	if a.CreatedAt, err = fromText(created); err != nil {
		return identity.Account{}, err
	}
	return a, nil
}

func (s *Store) Credential(ctx context.Context, accountID string) (identity.Credential, error) {
	var (
		c       identity.Credential
		updated string
	)
	err := s.row(ctx,
		`SELECT account_id, hash, updated_at FROM credential WHERE account_id = ?`, accountID).
		Scan(&c.AccountID, &c.Hash, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Credential{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.Credential{}, err
	}
	if c.UpdatedAt, err = fromText(updated); err != nil {
		return identity.Credential{}, err
	}
	return c, nil
}

func (s *Store) SetCredential(ctx context.Context, cred identity.Credential) error {
	if cred.AccountID == "" || cred.Hash == "" {
		return fmt.Errorf("%w: a credential needs an account and a hash", identity.ErrInvalid)
	}
	if cred.UpdatedAt.IsZero() {
		cred.UpdatedAt = s.now()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var exists string
	err = tx.QueryRowContext(ctx, s.d.Rebind(`SELECT id FROM account WHERE id = ?`), cred.AccountID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrNotFound
	}
	if err != nil {
		return err
	}

	// An upsert spelled as delete-then-insert, because the two engines write
	// ON CONFLICT differently enough that it would be the fourth entry in a
	// dialect list this package is trying to keep at three.
	if _, err := tx.ExecContext(ctx, s.d.Rebind(
		`DELETE FROM credential WHERE account_id = ?`), cred.AccountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.d.Rebind(
		`INSERT INTO credential (account_id, hash, updated_at) VALUES (?, ?, ?)`),
		cred.AccountID, cred.Hash, asText(cred.UpdatedAt)); err != nil {
		return err
	}
	// In the same transaction. A password change that leaves the old sessions
	// alive is not a password change; it is a second password.
	if _, err := tx.ExecContext(ctx, s.d.Rebind(
		`DELETE FROM session WHERE account_id = ?`), cred.AccountID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RehashCredential(ctx context.Context, cred identity.Credential) error {
	if cred.AccountID == "" || cred.Hash == "" {
		return fmt.Errorf("%w: a credential needs an account and a hash", identity.ErrInvalid)
	}
	if cred.UpdatedAt.IsZero() {
		cred.UpdatedAt = s.now()
	}
	// One UPDATE, and no session touched. See identity.Store for why this is a
	// different operation from SetCredential rather than a flag on it.
	res, err := s.db.ExecContext(ctx, s.d.Rebind(
		`UPDATE credential SET hash = ?, updated_at = ? WHERE account_id = ?`),
		cred.Hash, asText(cred.UpdatedAt), cred.AccountID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return identity.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- membership

func (s *Store) AddMember(ctx context.Context, m identity.Membership) error {
	if !m.Role.Valid() {
		return fmt.Errorf("%w: role %q", identity.ErrInvalid, m.Role)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = s.now()
	}
	err := s.exec(ctx,
		`INSERT INTO membership (account_id, tenant_id, role, created_at) VALUES (?, ?, ?, ?)`,
		m.AccountID, m.TenantID, string(m.Role), asText(m.CreatedAt))
	switch {
	case isUnique(err):
		return identity.ErrDuplicate
	case err != nil && isForeignKey(err):
		// A role granted to nobody, or in nowhere. The constraint catches it;
		// this turns the driver's wording into the error the caller branches on.
		return fmt.Errorf("%w: no such account or tenant", identity.ErrNotFound)
	case err != nil:
		return err
	}
	return nil
}

func isForeignKey(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "foreign key")
}

func (s *Store) Membership(ctx context.Context, accountID, tenantID string) (identity.Membership, error) {
	var (
		m       identity.Membership
		role    string
		created string
	)
	err := s.row(ctx, `
		SELECT account_id, tenant_id, role, created_at FROM membership
		 WHERE account_id = ? AND tenant_id = ?`, accountID, tenantID).
		Scan(&m.AccountID, &m.TenantID, &role, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Membership{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.Membership{}, err
	}
	m.Role = identity.Role(role)
	if m.CreatedAt, err = fromText(created); err != nil {
		return identity.Membership{}, err
	}
	return m, nil
}

func (s *Store) Memberships(ctx context.Context, accountID string) ([]identity.Membership, error) {
	rows, err := s.db.QueryContext(ctx, s.d.Rebind(`
		SELECT account_id, tenant_id, role, created_at FROM membership
		 WHERE account_id = ? ORDER BY tenant_id`), accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []identity.Membership
	for rows.Next() {
		var (
			m       identity.Membership
			role    string
			created string
		)
		if err := rows.Scan(&m.AccountID, &m.TenantID, &role, &created); err != nil {
			return nil, err
		}
		m.Role = identity.Role(role)
		if m.CreatedAt, err = fromText(created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) RemoveMember(ctx context.Context, accountID, tenantID string) error {
	res, err := s.db.ExecContext(ctx, s.d.Rebind(
		`DELETE FROM membership WHERE account_id = ? AND tenant_id = ?`), accountID, tenantID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return identity.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- sessions

func (s *Store) CreateSession(ctx context.Context, sess identity.Session) error {
	switch {
	case len(sess.Digest) == 0 || sess.AccountID == "":
		return fmt.Errorf("%w: a session needs a digest and an account", identity.ErrInvalid)
	case sess.ExpiresAt.IsZero():
		return fmt.Errorf("%w: a session needs an expiry", identity.ErrInvalid)
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = s.now()
	}
	err := s.exec(ctx, `
		INSERT INTO session (digest, account_id, issued_for, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		hex.EncodeToString(sess.Digest), sess.AccountID, sess.IssuedFor,
		asText(sess.CreatedAt), asText(sess.ExpiresAt))
	switch {
	case isUnique(err):
		return identity.ErrDuplicate
	case err != nil && isForeignKey(err):
		return fmt.Errorf("%w: no such account", identity.ErrNotFound)
	case err != nil:
		return err
	}
	return nil
}

func (s *Store) Session(ctx context.Context, dg []byte, now time.Time) (identity.Session, error) {
	var (
		sess    identity.Session
		hexDg   string
		created string
		expires string
	)
	// Expiry is in the WHERE clause rather than checked afterwards, so a caller
	// cannot forget — and forgetting here is an authentication bypass, not a
	// stale read. Fixed-width RFC3339 makes the string comparison a
	// chronological one on both engines.
	err := s.row(ctx, `
		SELECT digest, account_id, issued_for, created_at, expires_at FROM session
		 WHERE digest = ? AND expires_at > ?`,
		hex.EncodeToString(dg), asText(now)).
		Scan(&hexDg, &sess.AccountID, &sess.IssuedFor, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Session{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.Session{}, err
	}
	if sess.Digest, err = hex.DecodeString(hexDg); err != nil {
		return identity.Session{}, err
	}
	if sess.CreatedAt, err = fromText(created); err != nil {
		return identity.Session{}, err
	}
	if sess.ExpiresAt, err = fromText(expires); err != nil {
		return identity.Session{}, err
	}
	return sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, dg []byte) error {
	res, err := s.db.ExecContext(ctx, s.d.Rebind(
		`DELETE FROM session WHERE digest = ?`), hex.EncodeToString(dg))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return identity.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAccountSessions(ctx context.Context, accountID string) (int, error) {
	res, err := s.db.ExecContext(ctx, s.d.Rebind(
		`DELETE FROM session WHERE account_id = ?`), accountID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) PruneExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, s.d.Rebind(
		`DELETE FROM session WHERE expires_at <= ?`), asText(now))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) Close() error { return s.db.Close() }

var _ identity.Store = (*Store)(nil)
