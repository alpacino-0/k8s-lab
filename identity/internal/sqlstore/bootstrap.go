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

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/damgahq/damga/identity"
)

// The four statements Bootstrap shares with CreateTenant, CreateAccount and
// AddMember. Constants rather than four more string literals, because two
// copies of an INSERT are two places a column has to be added and one place it
// will be forgotten — and the one that would be forgotten is this one, which
// runs once per install and never again in any test that is not about it.
const (
	insertTenant = `
		INSERT INTO tenant (id, slug, display_name, tier, suspended, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`
	insertAccount = `
		INSERT INTO account (id, kind, email, email_folded, audit_email, display_name, disabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	insertCredential = `INSERT INTO credential (account_id, hash, updated_at) VALUES (?, ?, ?)`
	insertMembership = `INSERT INTO membership (account_id, tenant_id, role, created_at) VALUES (?, ?, ?, ?)`
)

// Bootstrap is identity.Store.Bootstrap: the first tenant, the first account
// and the owner membership, in one transaction, at most once.
func (s *Store) Bootstrap(
	ctx context.Context, t identity.Tenant, a identity.Account, cred identity.Credential,
) error {
	if err := validTenant(&t, s.now); err != nil {
		return err
	}
	if err := validAccount(&a, s.now); err != nil {
		return err
	}
	if cred.Hash == "" {
		// Every other account may exist without a password — that is what a
		// federated account is. The first one may not: it is created before
		// any identity provider has been configured, so an account with no
		// way to sign in is the one outcome that cannot be recovered from
		// through the product.
		return fmt.Errorf("%w: the first account needs a password", identity.ErrInvalid)
	}
	cred.UpdatedAt = a.CreatedAt

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The claim goes first. It is the row that can collide, so taking it up
	// front means a second bootstrap is refused before it has written
	// anything — and the refusal names the real reason rather than surfacing
	// as whichever of the tenant slug or the email happened to clash.
	_, err = tx.ExecContext(ctx, s.d.Rebind(
		`INSERT INTO bootstrap (id, claimed_at) VALUES ('singleton', ?)`), asText(s.now()))
	switch {
	case isUnique(err):
		return fmt.Errorf("%w: this install already has an owner", identity.ErrDuplicate)
	case err != nil:
		return err
	}

	if _, err := tx.ExecContext(ctx, s.d.Rebind(insertTenant),
		t.ID, t.Slug, t.DisplayName, string(t.Tier), boolInt(t.Suspended),
		asText(t.CreatedAt)); err != nil {
		return bootstrapErr(err, "tenant")
	}
	if _, err := tx.ExecContext(ctx, s.d.Rebind(insertAccount),
		a.ID, a.Kind, a.Email, fold(a.Email), a.AuditEmail, a.DisplayName,
		boolInt(a.Disabled), asText(a.CreatedAt)); err != nil {
		return bootstrapErr(err, "account")
	}
	if _, err := tx.ExecContext(ctx, s.d.Rebind(insertCredential),
		a.ID, cred.Hash, asText(cred.UpdatedAt)); err != nil {
		return bootstrapErr(err, "credential")
	}
	if _, err := tx.ExecContext(ctx, s.d.Rebind(insertMembership),
		a.ID, t.ID, string(identity.RoleOwner), asText(a.CreatedAt)); err != nil {
		return bootstrapErr(err, "membership")
	}
	return tx.Commit()
}

// Bootstrapped is identity.Store.Bootstrapped.
func (s *Store) Bootstrapped(ctx context.Context) (bool, error) {
	var claimed string
	err := s.db.QueryRowContext(ctx, s.d.Rebind(
		`SELECT id FROM bootstrap WHERE id = 'singleton'`)).Scan(&claimed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

func bootstrapErr(err error, what string) error {
	if isUnique(err) {
		// Reachable without a prior bootstrap: a tenant or an account can be
		// created by other means first, and then this install is not empty
		// even though it is unclaimed.
		return fmt.Errorf("%w: that %s already exists", identity.ErrDuplicate, what)
	}
	return fmt.Errorf("bootstrap: writing the %s: %w", what, err)
}

// validTenant and validAccount are the checks CreateTenant and CreateAccount
// make, extracted so Bootstrap makes exactly the same ones rather than an
// approximation of them.
func validTenant(t *identity.Tenant, now func() time.Time) error {
	if t.ID == "" || t.Slug == "" {
		return fmt.Errorf("%w: a tenant needs an id and a slug", identity.ErrInvalid)
	}
	if !t.Tier.Valid() {
		return fmt.Errorf("%w: tier %q", identity.ErrInvalid, t.Tier)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now()
	}
	t.CreatedAt = t.CreatedAt.UTC().Truncate(time.Microsecond)
	return nil
}

func validAccount(a *identity.Account, now func() time.Time) error {
	switch {
	case a.ID == "" || a.Email == "":
		return fmt.Errorf("%w: an account needs an id and an email", identity.ErrInvalid)
	case a.Kind != "user" && a.Kind != "automation":
		return fmt.Errorf("%w: kind %q", identity.ErrInvalid, a.Kind)
	case a.AuditEmail == "":
		return fmt.Errorf("%w: an account needs an audit email", identity.ErrInvalid)
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now()
	}
	a.CreatedAt = a.CreatedAt.UTC().Truncate(time.Microsecond)
	return nil
}
