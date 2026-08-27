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

// Package memory is an in-process identity.Store.
//
// It is what tests elsewhere in the tree run against, so it is held to the same
// conformance suite as the persistent ones — a promise it quietly failed to
// keep would be assumed by every test that used it.
package memory

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/damgahq/damga/identity"
)

type Store struct {
	mu sync.Mutex

	tenants  map[string]identity.Tenant
	slugs    map[string]string // slug -> tenant id
	accounts map[string]identity.Account
	emails   map[string]string // lowercased email -> account id
	creds    map[string]identity.Credential
	members  map[string]identity.Membership // accountID + "\x00" + tenantID
	sessions map[string]identity.Session    // hex digest

	// The one-time claim. A bool rather than a derived "does any owner
	// membership exist", for the same reason the SQL stores keep a table:
	// the derived form is a read followed by a write, and two of those
	// interleave into two owners that each believe they are the first.
	bootstrapped bool
}

func New() *Store {
	return &Store{
		tenants: map[string]identity.Tenant{}, slugs: map[string]string{},
		accounts: map[string]identity.Account{}, emails: map[string]string{},
		creds: map[string]identity.Credential{}, members: map[string]identity.Membership{},
		sessions: map[string]identity.Session{},
	}
}

// Email is matched case-insensitively, because a person who signed up as
// Orhan@example.test and types orhan@example.test has not made a mistake — and
// a store that treats those as two accounts has.
func fold(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func memberKey(accountID, tenantID string) string { return accountID + "\x00" + tenantID }

// ---------------------------------------------------------------- tenants

func (s *Store) CreateTenant(_ context.Context, t identity.Tenant) (identity.Tenant, error) {
	if t.ID == "" || t.Slug == "" {
		return identity.Tenant{}, fmt.Errorf("%w: a tenant needs an id and a slug", identity.ErrInvalid)
	}
	if !t.Tier.Valid() {
		return identity.Tenant{}, fmt.Errorf("%w: tier %q", identity.ErrInvalid, t.Tier)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.slugs[t.Slug]; taken {
		return identity.Tenant{}, fmt.Errorf("%w: slug %q", identity.ErrDuplicate, t.Slug)
	}
	if _, taken := s.tenants[t.ID]; taken {
		return identity.Tenant{}, fmt.Errorf("%w: tenant %q", identity.ErrDuplicate, t.ID)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	t.CreatedAt = t.CreatedAt.UTC().Truncate(time.Microsecond)
	s.tenants[t.ID] = t
	s.slugs[t.Slug] = t.ID
	return t, nil
}

func (s *Store) Tenant(_ context.Context, id string) (identity.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tenants[id]
	if !ok {
		return identity.Tenant{}, identity.ErrNotFound
	}
	return t, nil
}

func (s *Store) TenantBySlug(ctx context.Context, slug string) (identity.Tenant, error) {
	s.mu.Lock()
	id, ok := s.slugs[slug]
	s.mu.Unlock()
	if !ok {
		return identity.Tenant{}, identity.ErrNotFound
	}
	return s.Tenant(ctx, id)
}

// ---------------------------------------------------------------- accounts

func (s *Store) CreateAccount(
	_ context.Context, a identity.Account, cred identity.Credential,
) (identity.Account, error) {
	if a.ID == "" || a.Email == "" {
		return identity.Account{}, fmt.Errorf("%w: an account needs an id and an email", identity.ErrInvalid)
	}
	if a.Kind != "user" && a.Kind != "automation" {
		return identity.Account{}, fmt.Errorf("%w: kind %q", identity.ErrInvalid, a.Kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.emails[fold(a.Email)]; taken {
		return identity.Account{}, fmt.Errorf("%w: email %q", identity.ErrDuplicate, a.Email)
	}
	if _, taken := s.accounts[a.ID]; taken {
		return identity.Account{}, fmt.Errorf("%w: account %q", identity.ErrDuplicate, a.ID)
	}
	if a.AuditEmail == "" {
		// Never left empty. What goes into a commit and into the hash chain has
		// to be something, and defaulting it to the login address is the one
		// choice that cannot later be redacted — so it is the caller's to make,
		// and the store refuses to make it silently.
		return identity.Account{}, fmt.Errorf("%w: an account needs an audit email", identity.ErrInvalid)
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	a.CreatedAt = a.CreatedAt.UTC().Truncate(time.Microsecond)
	s.accounts[a.ID] = a
	s.emails[fold(a.Email)] = a.ID
	if cred.Hash != "" {
		cred.AccountID = a.ID
		if cred.UpdatedAt.IsZero() {
			cred.UpdatedAt = a.CreatedAt
		}
		s.creds[a.ID] = cred
	}
	return a, nil
}

func (s *Store) Account(_ context.Context, id string) (identity.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return identity.Account{}, identity.ErrNotFound
	}
	return a, nil
}

func (s *Store) AccountByEmail(ctx context.Context, email string) (identity.Account, error) {
	s.mu.Lock()
	id, ok := s.emails[fold(email)]
	s.mu.Unlock()
	if !ok {
		return identity.Account{}, identity.ErrNotFound
	}
	return s.Account(ctx, id)
}

func (s *Store) Credential(_ context.Context, accountID string) (identity.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[accountID]
	if !ok {
		return identity.Credential{}, identity.ErrNotFound
	}
	return c, nil
}

func (s *Store) SetCredential(_ context.Context, cred identity.Credential) error {
	if cred.AccountID == "" || cred.Hash == "" {
		return fmt.Errorf("%w: a credential needs an account and a hash", identity.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[cred.AccountID]; !ok {
		return identity.ErrNotFound
	}
	if cred.UpdatedAt.IsZero() {
		cred.UpdatedAt = time.Now()
	}
	cred.UpdatedAt = cred.UpdatedAt.UTC().Truncate(time.Microsecond)
	s.creds[cred.AccountID] = cred
	// Every session that account holds, in the same operation. A password
	// change that leaves the old sessions alive is not a password change — it
	// is a second password.
	for k, sess := range s.sessions {
		if sess.AccountID == cred.AccountID {
			delete(s.sessions, k)
		}
	}
	return nil
}

func (s *Store) RehashCredential(_ context.Context, cred identity.Credential) error {
	if cred.AccountID == "" || cred.Hash == "" {
		return fmt.Errorf("%w: a credential needs an account and a hash", identity.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.creds[cred.AccountID]; !ok {
		return identity.ErrNotFound
	}
	if cred.UpdatedAt.IsZero() {
		cred.UpdatedAt = time.Now()
	}
	cred.UpdatedAt = cred.UpdatedAt.UTC().Truncate(time.Microsecond)
	// Sessions are deliberately untouched. See identity.Store.
	s.creds[cred.AccountID] = cred
	return nil
}

// ---------------------------------------------------------------- membership

func (s *Store) AddMember(_ context.Context, m identity.Membership) error {
	if !m.Role.Valid() {
		return fmt.Errorf("%w: role %q", identity.ErrInvalid, m.Role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[m.AccountID]; !ok {
		return fmt.Errorf("%w: account %q", identity.ErrNotFound, m.AccountID)
	}
	if _, ok := s.tenants[m.TenantID]; !ok {
		return fmt.Errorf("%w: tenant %q", identity.ErrNotFound, m.TenantID)
	}
	k := memberKey(m.AccountID, m.TenantID)
	if _, taken := s.members[k]; taken {
		return identity.ErrDuplicate
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	m.CreatedAt = m.CreatedAt.UTC().Truncate(time.Microsecond)
	s.members[k] = m
	return nil
}

func (s *Store) Membership(_ context.Context, accountID, tenantID string) (identity.Membership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.members[memberKey(accountID, tenantID)]
	if !ok {
		return identity.Membership{}, identity.ErrNotFound
	}
	return m, nil
}

func (s *Store) Memberships(_ context.Context, accountID string) ([]identity.Membership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []identity.Membership
	for _, m := range s.members {
		if m.AccountID == accountID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) RemoveMember(_ context.Context, accountID, tenantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memberKey(accountID, tenantID)
	if _, ok := s.members[k]; !ok {
		return identity.ErrNotFound
	}
	delete(s.members, k)
	return nil
}

// ---------------------------------------------------------------- sessions

func (s *Store) CreateSession(_ context.Context, sess identity.Session) error {
	if len(sess.Digest) == 0 || sess.AccountID == "" {
		return fmt.Errorf("%w: a session needs a digest and an account", identity.ErrInvalid)
	}
	if sess.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: a session needs an expiry", identity.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[sess.AccountID]; !ok {
		return fmt.Errorf("%w: account %q", identity.ErrNotFound, sess.AccountID)
	}
	k := hex.EncodeToString(sess.Digest)
	if _, taken := s.sessions[k]; taken {
		return identity.ErrDuplicate
	}
	sess.CreatedAt = sess.CreatedAt.UTC().Truncate(time.Microsecond)
	sess.ExpiresAt = sess.ExpiresAt.UTC().Truncate(time.Microsecond)
	s.sessions[k] = sess
	return nil
}

func (s *Store) Session(_ context.Context, digest []byte, now time.Time) (identity.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[hex.EncodeToString(digest)]
	if !ok {
		return identity.Session{}, identity.ErrNotFound
	}
	// An expired session is not a session. Refused here rather than left for
	// the caller, because a caller that forgets is an authentication bypass.
	if !now.Before(sess.ExpiresAt) {
		return identity.Session{}, identity.ErrNotFound
	}
	return sess, nil
}

func (s *Store) DeleteSession(_ context.Context, digest []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := hex.EncodeToString(digest)
	if _, ok := s.sessions[k]; !ok {
		return identity.ErrNotFound
	}
	delete(s.sessions, k)
	return nil
}

func (s *Store) DeleteAccountSessions(_ context.Context, accountID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, sess := range s.sessions {
		if sess.AccountID == accountID {
			delete(s.sessions, k)
			n++
		}
	}
	return n, nil
}

func (s *Store) PruneExpired(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, sess := range s.sessions {
		if !now.Before(sess.ExpiresAt) {
			delete(s.sessions, k)
			n++
		}
	}
	return n, nil
}

func (s *Store) Close() error { return nil }

var _ identity.Store = (*Store)(nil)

// Bootstrap is identity.Store.Bootstrap: the first tenant, the first account
// and the owner membership between them, all under one hold of the mutex.
//
// The SQL stores get all-or-nothing from a transaction. Here it comes from
// validating everything before writing anything, which is the same guarantee
// as long as nothing between the first write and the last can fail — so
// nothing between them is allowed to.
func (s *Store) Bootstrap(
	_ context.Context, t identity.Tenant, a identity.Account, cred identity.Credential,
) error {
	switch {
	case t.ID == "" || t.Slug == "":
		return fmt.Errorf("%w: a tenant needs an id and a slug", identity.ErrInvalid)
	case !t.Tier.Valid():
		return fmt.Errorf("%w: tier %q", identity.ErrInvalid, t.Tier)
	case a.ID == "" || a.Email == "":
		return fmt.Errorf("%w: an account needs an id and an email", identity.ErrInvalid)
	case a.Kind != "user" && a.Kind != "automation":
		return fmt.Errorf("%w: kind %q", identity.ErrInvalid, a.Kind)
	case a.AuditEmail == "":
		return fmt.Errorf("%w: an account needs an audit email", identity.ErrInvalid)
	case cred.Hash == "":
		// The one account that may not be passwordless. It is created before
		// any identity provider exists, so a federated first account is an
		// install nobody can sign in to.
		return fmt.Errorf("%w: the first account needs a password", identity.ErrInvalid)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bootstrapped {
		return fmt.Errorf("%w: this install already has an owner", identity.ErrDuplicate)
	}
	if _, taken := s.slugs[t.Slug]; taken {
		return fmt.Errorf("%w: that tenant already exists", identity.ErrDuplicate)
	}
	if _, taken := s.tenants[t.ID]; taken {
		return fmt.Errorf("%w: that tenant already exists", identity.ErrDuplicate)
	}
	if _, taken := s.emails[fold(a.Email)]; taken {
		return fmt.Errorf("%w: that account already exists", identity.ErrDuplicate)
	}
	if _, taken := s.accounts[a.ID]; taken {
		return fmt.Errorf("%w: that account already exists", identity.ErrDuplicate)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.CreatedAt = t.CreatedAt.UTC().Truncate(time.Microsecond)
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.CreatedAt = a.CreatedAt.UTC().Truncate(time.Microsecond)
	cred.AccountID, cred.UpdatedAt = a.ID, a.CreatedAt

	s.tenants[t.ID], s.slugs[t.Slug] = t, t.ID
	s.accounts[a.ID], s.emails[fold(a.Email)] = a, a.ID
	s.creds[a.ID] = cred
	s.members[memberKey(a.ID, t.ID)] = identity.Membership{
		AccountID: a.ID, TenantID: t.ID, Role: identity.RoleOwner, CreatedAt: a.CreatedAt,
	}
	s.bootstrapped = true
	return nil
}

// Bootstrapped is identity.Store.Bootstrapped.
func (s *Store) Bootstrapped(_ context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bootstrapped, nil
}
