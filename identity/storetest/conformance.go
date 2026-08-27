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

// Package storetest is the suite every identity.Store has to pass.
//
// Written before the second implementation, deliberately. The evidence store
// did it in that order and its SQLite backend passed unchanged on the first
// run; the alternative is discovering, one engine at a time, that three
// implementations disagree about what a valid account is.
//
// It also carries a lesson that cost this project a real defect: a suite whose
// fixtures are all well-formed tests three happy paths rather than their
// agreement. So the cases below spend most of their effort on what must be
// REFUSED, and several of them exist because refusing is a security property
// and not tidiness.
package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damgahq/damga/identity"
)

// Factory makes a fresh, empty store. Called once per case: the cases assume
// they own it.
type Factory func(t *testing.T) identity.Store

// Run executes the whole suite against one implementation.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"TenantRoundTrips", testTenantRoundTrips},
		{"TenantRejectsAnInvalidTier", testTenantRejectsAnInvalidTier},
		{"SlugIsUnique", testSlugIsUnique},
		{"UpdateTenantChangesOnlyTheMutableHalf", testUpdateTenantChangesOnlyTheMutableHalf},
		{"AccountRoundTrips", testAccountRoundTrips},
		{"EmailIsUniqueAndCaseInsensitive", testEmailIsUniqueAndCaseInsensitive},
		{"AccountRequiresAnAuditEmail", testAccountRequiresAnAuditEmail},
		{"AnAccountWithoutAPasswordIsNormal", testAnAccountWithoutAPasswordIsNormal},
		{"ChangingAPasswordRevokesSessions", testChangingAPasswordRevokesSessions},
		{"RehashingKeepsSessions", testRehashingKeepsSessions},
		{"MembershipIsTheOnlySourceOfARole", testMembershipIsTheOnlySourceOfARole},
		{"MembershipRejectsAnInvalidRole", testMembershipRejectsAnInvalidRole},
		{"MembershipRefusesDanglingReferences", testMembershipRefusesDanglingReferences},
		{"RemovingAMemberKeepsTheAccount", testRemovingAMemberKeepsTheAccount},
		{"SessionRoundTrips", testSessionRoundTrips},
		{"AnExpiredSessionIsNotASession", testAnExpiredSessionIsNotASession},
		{"SessionsCanBeTerminatedByAccount", testSessionsCanBeTerminatedByAccount},
		{"PruneRemovesOnlyWhatExpired", testPruneRemovesOnlyWhatExpired},
		{"NotFoundIsDistinguishable", testNotFoundIsDistinguishable},
		{"BootstrapMakesAnOwnerWhoCanSignIn", testBootstrapMakesAnOwnerWhoCanSignIn},
		{"BootstrapHappensAtMostOnce", testBootstrapHappensAtMostOnce},
		{"AFailedBootstrapLeavesNothingBehind", testAFailedBootstrapLeavesNothingBehind},
		{"BootstrapRefusesAnAccountWithNoPassword", testBootstrapRefusesAnAccountWithNoPassword},
		{"ConcurrentBootstrapsProduceOneOwner", testConcurrentBootstrapsProduceOneOwner},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore) })
	}
}

// ---------------------------------------------------------------- helpers

const (
	tenantID = "t_alpha"
	acctID   = "a_orhan"
	email    = "orhan@example.test"

	// The second tenant. Several cases need somewhere an account is not a
	// member of, which is the state a leak would show up in.
	otherTenantID = "t_beta"

	// The second tenant's slug and name, and the slug a rename moves the
	// first one to.
	betaSlug    = "beta"
	betaName    = "Beta"
	renamedSlug = "alpha-renamed"

	kindUser = "user"
	// Not a real hash. The cases here are about what the store keeps and
	// returns, not about argon2id — auth's own tests cover that, and hashing
	// for real would make this suite take minutes per engine.
	fakeHash = "argon2id$fake"
)

func tenant() identity.Tenant {
	return identity.Tenant{
		ID: tenantID, Slug: "alpha", DisplayName: "Alpha",
		Tier: identity.TierFree,
	}
}

func account() identity.Account {
	return identity.Account{
		ID: acctID, Kind: kindUser, Email: email,
		AuditEmail: "a_orhan@users.damga.local", DisplayName: "Orhan Yavuz",
	}
}

func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func seedTenantAndAccount(t *testing.T, s identity.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateTenant(ctx, tenant()); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := s.CreateAccount(ctx, account(), identity.Credential{Hash: fakeHash}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
}

func session(expires time.Time) identity.Session {
	return identity.Session{
		Digest: digest("token-one"), AccountID: acctID,
		IssuedFor: "damga.example.test",
		CreatedAt: time.Now(), ExpiresAt: expires,
	}
}

// ---------------------------------------------------------------- cases

func testTenantRoundTrips(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	got, err := s.CreateTenant(ctx, tenant())
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	byID, err := s.Tenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("Tenant: %v", err)
	}
	bySlug, err := s.TenantBySlug(ctx, "alpha")
	if err != nil {
		t.Fatalf("TenantBySlug: %v", err)
	}
	if byID.ID != bySlug.ID || byID.Tier != identity.TierFree {
		t.Errorf("the two lookups disagree: %+v vs %+v", byID, bySlug)
	}
}

// The tier is copied onto every evidence record, where it lands inside a hash
// chain and can never be corrected. A store that accepts a nonsense value there
// makes every retention claim built on it a guess.
func testTenantRejectsAnInvalidTier(t *testing.T, newStore Factory) {
	s := newStore(t)
	for _, tier := range []identity.Tier{"", "platinum"} {
		x := tenant()
		x.Tier = tier
		if _, err := s.CreateTenant(context.Background(), x); !errors.Is(err, identity.ErrInvalid) {
			t.Errorf("CreateTenant with tier %q returned %v, want ErrInvalid", tier, err)
		}
	}
}

func testSlugIsUnique(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateTenant(ctx, tenant()); err != nil {
		t.Fatalf("first: %v", err)
	}
	other := tenant()
	other.ID = otherTenantID
	if _, err := s.CreateTenant(ctx, other); !errors.Is(err, identity.ErrDuplicate) {
		t.Errorf("a second tenant took the same slug: %v", err)
	}
}

func testAccountRoundTrips(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	byID, err := s.Account(ctx, acctID)
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if byID.AuditEmail == byID.Email {
		t.Error("the audit email equals the login email; erasing one would erase the other")
	}
	byEmail, err := s.AccountByEmail(ctx, email)
	if err != nil {
		t.Fatalf("AccountByEmail: %v", err)
	}
	if byEmail.ID != acctID {
		t.Errorf("AccountByEmail returned %q", byEmail.ID)
	}
}

// A person who signed up as Orhan@ and types orhan@ has not made a mistake. A
// store that treats those as two accounts has, and the failure is a duplicate
// account rather than an error message.
func testEmailIsUniqueAndCaseInsensitive(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	other := account()
	other.ID = "a_other"
	other.Email = "ORHAN@Example.Test"
	if _, err := s.CreateAccount(ctx, other, identity.Credential{}); !errors.Is(err, identity.ErrDuplicate) {
		t.Errorf("a second account took the same address in another case: %v", err)
	}

	got, err := s.AccountByEmail(ctx, "ORHAN@Example.Test")
	if err != nil {
		t.Fatalf("AccountByEmail with different case: %v", err)
	}
	if got.ID != acctID {
		t.Errorf("case-folded lookup returned %q", got.ID)
	}
}

// The audit email is what goes into git commits and into the hash chain, where
// it can never be redacted. Defaulting it to the login address would quietly
// publish personal data that no erasure request can reach, so the store refuses
// to choose.
func testAccountRequiresAnAuditEmail(t *testing.T, newStore Factory) {
	s := newStore(t)
	a := account()
	a.AuditEmail = ""
	if _, err := s.CreateAccount(context.Background(), a, identity.Credential{}); !errors.Is(err, identity.ErrInvalid) {
		t.Errorf("an account with no audit email was accepted: %v", err)
	}
}

// A federated account has no password, and that is the honest representation of
// "this person's password lives at their identity provider" — not an error, and
// not an empty hash that something might compare against.
func testAnAccountWithoutAPasswordIsNormal(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateTenant(ctx, tenant()); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := s.CreateAccount(ctx, account(), identity.Credential{}); err != nil {
		t.Fatalf("CreateAccount with no credential: %v", err)
	}
	if _, err := s.Credential(ctx, acctID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("Credential for a federated account = %v, want ErrNotFound", err)
	}
}

// A password change that leaves the old sessions alive is not a password
// change; it is a second password. This is the one case in the suite that is
// about an attacker rather than about correctness.
func testChangingAPasswordRevokesSessions(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	if err := s.CreateSession(ctx, session(time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.SetCredential(ctx, identity.Credential{AccountID: acctID, Hash: "argon2id$new"}); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}

	if _, err := s.Session(ctx, digest("token-one"), time.Now()); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("a session survived the password change: %v", err)
	}
	got, err := s.Credential(ctx, acctID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if got.Hash != "argon2id$new" {
		t.Errorf("hash = %q, want the new one", got.Hash)
	}
}

// The other half of the pair, and the reason they are two methods rather than a
// flag on one. A rehash is the same password stored under stronger parameters,
// and the only moment to do it is a successful login — where revoking sessions
// would log the user out of the session that login just created. Conflating
// them means either never raising the parameters or bouncing people at the door.
func testRehashingKeepsSessions(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	if err := s.CreateSession(ctx, session(time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.RehashCredential(ctx, identity.Credential{
		AccountID: acctID, Hash: "argon2id$stronger",
	}); err != nil {
		t.Fatalf("RehashCredential: %v", err)
	}

	if _, err := s.Session(ctx, digest("token-one"), time.Now()); err != nil {
		t.Errorf("the session was revoked by a rehash: %v", err)
	}
	got, err := s.Credential(ctx, acctID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if got.Hash != "argon2id$stronger" {
		t.Errorf("hash = %q, want the rehashed one", got.Hash)
	}

	// And it upgrades nothing that is not there. A federated account has no
	// credential, and inventing one would give it a password nobody set.
	other := account()
	other.ID, other.Email = "a_federated", "fed@example.test"
	if _, err := s.CreateAccount(ctx, other, identity.Credential{}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := s.RehashCredential(ctx, identity.Credential{
		AccountID: "a_federated", Hash: "argon2id$x",
	}); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("RehashCredential on an account with no password = %v, want ErrNotFound", err)
	}
}

// The security property this type exists for. A subject's tenant and role come
// from a membership row and from nowhere else — the free authorizer treats an
// unrecognised group as viewer and a viewer may read the evidence page, so a
// subject assembled from anything the caller sent would let a stranger read
// another tenant's deploy history. No membership means no subject.
func testMembershipIsTheOnlySourceOfARole(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	if _, err := s.Membership(ctx, acctID, tenantID); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("an account with no membership already has one: %v", err)
	}

	if err := s.AddMember(ctx, identity.Membership{
		AccountID: acctID, TenantID: tenantID, Role: identity.RoleMember,
	}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	got, err := s.Membership(ctx, acctID, tenantID)
	if err != nil {
		t.Fatalf("Membership: %v", err)
	}
	if got.Role != identity.RoleMember {
		t.Errorf("role = %q, want member", got.Role)
	}

	// And it is not a membership in some other tenant.
	if _, err := s.Membership(ctx, acctID, "t_nosuch"); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("a membership leaked into another tenant: %v", err)
	}

	all, err := s.Memberships(ctx, acctID)
	if err != nil {
		t.Fatalf("Memberships: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("Memberships returned %d, want 1", len(all))
	}
}

func testMembershipRejectsAnInvalidRole(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)
	for _, role := range []identity.Role{"", "admin", "superuser"} {
		err := s.AddMember(ctx, identity.Membership{
			AccountID: acctID, TenantID: tenantID, Role: role,
		})
		if !errors.Is(err, identity.ErrInvalid) {
			t.Errorf("AddMember with role %q returned %v, want ErrInvalid", role, err)
		}
	}
}

// A membership pointing at an account or a tenant that does not exist is a role
// granted to nobody, or in nowhere. Both engines will have a foreign key; the
// in-process one has to agree.
func testMembershipRefusesDanglingReferences(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	if err := s.AddMember(ctx, identity.Membership{
		AccountID: "a_ghost", TenantID: tenantID, Role: identity.RoleMember,
	}); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("a membership was granted to an account that does not exist: %v", err)
	}
	if err := s.AddMember(ctx, identity.Membership{
		AccountID: acctID, TenantID: "t_ghost", Role: identity.RoleMember,
	}); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("a membership was granted in a tenant that does not exist: %v", err)
	}
}

// Revoking a role is not deleting a person. The account survives, because
// evidence records point at its id across a boundary with no foreign key and
// reusing or removing it would silently reattribute history.
func testRemovingAMemberKeepsTheAccount(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)
	if err := s.AddMember(ctx, identity.Membership{
		AccountID: acctID, TenantID: tenantID, Role: identity.RoleOwner,
	}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if err := s.RemoveMember(ctx, acctID, tenantID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if _, err := s.Membership(ctx, acctID, tenantID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("the membership survived removal: %v", err)
	}
	if _, err := s.Account(ctx, acctID); err != nil {
		t.Errorf("the account was removed with the membership: %v", err)
	}
}

func testSessionRoundTrips(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	want := session(time.Now().Add(time.Hour))
	if err := s.CreateSession(ctx, want); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.Session(ctx, want.Digest, time.Now())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.AccountID != acctID {
		t.Errorf("account = %q", got.AccountID)
	}
	if got.IssuedFor != want.IssuedFor {
		t.Errorf("IssuedFor = %q, want %q — it is checked on every use, so it has to survive",
			got.IssuedFor, want.IssuedFor)
	}

	if err := s.DeleteSession(ctx, want.Digest); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.Session(ctx, want.Digest, time.Now()); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("the session survived logout: %v", err)
	}
}

// Refused by the store rather than left for the caller to check, because a
// caller that forgets is an authentication bypass rather than a stale read.
func testAnExpiredSessionIsNotASession(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	past := time.Now().Add(-time.Minute)
	if err := s.CreateSession(ctx, session(past)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.Session(ctx, digest("token-one"), time.Now()); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("an expired session was returned: %v", err)
	}
}

// What deprovisioning needs. The paid tier sells SCIM, and SCIM's whole promise
// is that removing somebody from the directory removes their access now —
// not when a token happens to expire.
func testSessionsCanBeTerminatedByAccount(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	for _, tok := range []string{"one", "two", "three"} {
		sess := session(time.Now().Add(time.Hour))
		sess.Digest = digest(tok)
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession(%s): %v", tok, err)
		}
	}

	n, err := s.DeleteAccountSessions(ctx, acctID)
	if err != nil {
		t.Fatalf("DeleteAccountSessions: %v", err)
	}
	if n != 3 {
		t.Errorf("terminated %d sessions, want 3", n)
	}
	for _, tok := range []string{"one", "two", "three"} {
		if _, err := s.Session(ctx, digest(tok), time.Now()); !errors.Is(err, identity.ErrNotFound) {
			t.Errorf("session %s survived: %v", tok, err)
		}
	}
}

func testPruneRemovesOnlyWhatExpired(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	seedTenantAndAccount(t, s)

	live := session(time.Now().Add(time.Hour))
	live.Digest = digest("live")
	dead := session(time.Now().Add(-time.Hour))
	dead.Digest = digest("dead")
	for _, sess := range []identity.Session{live, dead} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	n, err := s.PruneExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, err := s.Session(ctx, live.Digest, time.Now()); err != nil {
		t.Errorf("the live session was pruned: %v", err)
	}
}

// Callers branch on these. A store that returns a bare error for a missing
// account turns "no such user" into a 500 on the login page — and, worse,
// makes the login handler unable to take the same path for an unknown address
// as for a wrong password.
func testNotFoundIsDistinguishable(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	checks := map[string]error{}
	_, checks["Tenant"] = s.Tenant(ctx, "nope")
	_, checks["TenantBySlug"] = s.TenantBySlug(ctx, "nope")
	_, checks["Account"] = s.Account(ctx, "nope")
	_, checks["AccountByEmail"] = s.AccountByEmail(ctx, "nope@example.test")
	_, checks["Credential"] = s.Credential(ctx, "nope")
	_, checks["Membership"] = s.Membership(ctx, "nope", "nope")
	_, checks["Session"] = s.Session(ctx, digest("nope"), time.Now())
	checks["DeleteSession"] = s.DeleteSession(ctx, digest("nope"))
	checks["RemoveMember"] = s.RemoveMember(ctx, "nope", "nope")

	for name, err := range checks {
		if !errors.Is(err, identity.ErrNotFound) {
			t.Errorf("%s on a missing row = %v, want ErrNotFound", name, err)
		}
	}
}

// ---------------------------------------------------------------- bootstrap

// The whole point of the call: an install that had nobody now has somebody who
// can sign in and grant access to everyone else. Checked through the same reads
// login uses, not through Bootstrapped — the claim being set proves the row was
// written, not that the owner exists.
func testBootstrapMakesAnOwnerWhoCanSignIn(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	if was, err := s.Bootstrapped(ctx); err != nil || was {
		t.Fatalf("a fresh store reports Bootstrapped=%v, err=%v", was, err)
	}
	if err := s.Bootstrap(ctx, tenant(), account(), identity.Credential{Hash: fakeHash}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	acct, err := s.AccountByEmail(ctx, email)
	if err != nil {
		t.Fatalf("the owner cannot be found by the address they would type: %v", err)
	}
	if _, err := s.Credential(ctx, acct.ID); err != nil {
		t.Fatalf("the owner has no password: %v", err)
	}
	m, err := s.Membership(ctx, acct.ID, tenantID)
	if err != nil {
		t.Fatalf("Membership: %v", err)
	}
	if m.Role != identity.RoleOwner {
		t.Errorf("the first account is %q, want owner — nobody can finish the install", m.Role)
	}
	if _, err := s.Tenant(ctx, tenantID); err != nil {
		t.Fatalf("Tenant: %v", err)
	}
	if was, err := s.Bootstrapped(ctx); err != nil || !was {
		t.Errorf("after Bootstrap the claim reads %v, err=%v", was, err)
	}
}

// A second bootstrap is refused on the claim, not on a collision. Everything
// about the second call is different — other tenant, other address, other id —
// so anything that lets it through is a stranger with an owner account on
// somebody else's install.
func testBootstrapHappensAtMostOnce(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	cred := identity.Credential{Hash: fakeHash}

	if err := s.Bootstrap(ctx, tenant(), account(), cred); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	other := identity.Tenant{ID: otherTenantID, Slug: betaSlug, DisplayName: betaName, Tier: identity.TierFree}
	stranger := identity.Account{
		ID: "a_stranger", Kind: kindUser, Email: "stranger@example.test",
		AuditEmail: "a_stranger@users.damga.local", DisplayName: "Stranger",
	}
	err := s.Bootstrap(ctx, other, stranger, cred)
	if !errors.Is(err, identity.ErrDuplicate) {
		t.Fatalf("a second bootstrap returned %v, want ErrDuplicate", err)
	}
	if _, err := s.AccountByEmail(ctx, stranger.Email); !errors.Is(err, identity.ErrNotFound) {
		t.Error("the refused bootstrap created its account anyway")
	}
	if _, err := s.Tenant(ctx, other.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Error("the refused bootstrap created its tenant anyway")
	}
}

// Bootstrap writes four rows. A failure on any of them has to leave zero — and
// crucially has to leave the claim unspent, because a claim without an owner is
// an install that can never be signed into and can never be bootstrapped again.
//
// The failure is provoked with a tenant slug that is already taken, which is
// reachable in practice: this store is also what a migration or a restore
// writes into, so "empty" and "unclaimed" are not the same state.
func testAFailedBootstrapLeavesNothingBehind(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.CreateTenant(ctx, tenant()); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	err := s.Bootstrap(ctx, tenant(), account(), identity.Credential{Hash: fakeHash})
	if !errors.Is(err, identity.ErrDuplicate) {
		t.Fatalf("Bootstrap over a taken slug returned %v, want ErrDuplicate", err)
	}
	if _, err := s.AccountByEmail(ctx, email); !errors.Is(err, identity.ErrNotFound) {
		t.Error("the account survived a bootstrap that failed after it")
	}
	if was, err := s.Bootstrapped(ctx); err != nil || was {
		t.Fatal("the claim was spent by a bootstrap that failed: nobody can ever own this install")
	}

	// And the proof that the state is still usable: a bootstrap that does not
	// collide still works.
	free := identity.Tenant{ID: otherTenantID, Slug: betaSlug, DisplayName: betaName, Tier: identity.TierFree}
	if err := s.Bootstrap(ctx, free, account(), identity.Credential{Hash: fakeHash}); err != nil {
		t.Fatalf("the store is unusable after a failed bootstrap: %v", err)
	}
}

// Every other account may exist without a password; that is what a federated
// account is. This one may not — it is created before any identity provider is
// configured, so it would be an owner nobody can sign in as.
func testBootstrapRefusesAnAccountWithNoPassword(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.Bootstrap(ctx, tenant(), account(), identity.Credential{}); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("Bootstrap without a password returned %v, want ErrInvalid", err)
	}
	if was, err := s.Bootstrapped(ctx); err != nil || was {
		t.Error("a refused bootstrap spent the claim")
	}
}

// The reason the claim is a row rather than "does any owner membership exist".
// The derived form is a read followed by a write, and two of those interleave
// into two owners that were each told they were the first.
func testConcurrentBootstrapsProduceOneOwner(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	const racers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := range racers {
		wg.Go(func() {
			// Each racer wants a different tenant and a different account, so
			// no two of them can collide on anything except the claim itself.
			tn := identity.Tenant{
				ID: "t_" + string(rune('a'+i)), Slug: string(rune('a' + i)),
				DisplayName: "T", Tier: identity.TierFree,
			}
			ac := identity.Account{
				ID: "a_" + string(rune('a'+i)), Kind: kindUser,
				Email:      string(rune('a'+i)) + "@example.test",
				AuditEmail: "a@users.damga.local", DisplayName: "A",
			}
			err := s.Bootstrap(ctx, tn, ac, identity.Credential{Hash: fakeHash})
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		})
	}
	wg.Wait()

	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, identity.ErrDuplicate):
		default:
			t.Errorf("a losing racer got %v, want ErrDuplicate", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent bootstraps succeeded, want exactly 1", won, racers)
	}
}

// The administrative change: suspend a tenant, move it to another plan.
//
// Two things must not move. The id, because evidence records carry a copy of
// it across a boundary with no foreign key — changing it orphans every record
// ever written about the tenant. And CreatedAt, because a caller that passes a
// zero value should not silently reset when the tenant came into existence.
func testUpdateTenantChangesOnlyTheMutableHalf(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.CreateTenant(ctx, tenant())
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	// Deliberately passing a zero CreatedAt, which is what any caller that
	// builds the row from a form will do.
	updated, err := s.UpdateTenant(ctx, identity.Tenant{
		ID: tenantID, Slug: renamedSlug, DisplayName: "Alpha Renamed",
		Tier: identity.TierEnterprise, Suspended: true,
	})
	if err != nil {
		t.Fatalf("UpdateTenant: %v", err)
	}
	switch {
	case updated.Tier != identity.TierEnterprise:
		t.Errorf("Tier = %q, want enterprise", updated.Tier)
	case !updated.Suspended:
		t.Error("Suspended did not take")
	case updated.Slug != renamedSlug:
		t.Errorf("Slug = %q", updated.Slug)
	case !updated.CreatedAt.Equal(created.CreatedAt):
		t.Errorf("CreatedAt moved from %s to %s", created.CreatedAt, updated.CreatedAt)
	}

	// It is the same row, and reachable under the new slug and not the old.
	if _, err := s.TenantBySlug(ctx, renamedSlug); err != nil {
		t.Errorf("TenantBySlug after rename: %v", err)
	}
	if _, err := s.TenantBySlug(ctx, "alpha"); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("the old slug still resolves: %v", err)
	}
	stored, err := s.Tenant(ctx, tenantID)
	if err != nil || !stored.Suspended || stored.Tier != identity.TierEnterprise {
		t.Errorf("the change did not survive a read: %+v err=%v", stored, err)
	}

	// An update to a tenant that does not exist is not a silent success. Both
	// engines treat an UPDATE matching no rows as fine, so this is the case
	// that has to be checked rather than assumed.
	if _, err := s.UpdateTenant(ctx, identity.Tenant{
		ID: "t_nobody", Slug: "nobody", Tier: identity.TierFree,
	}); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("updating a tenant that does not exist returned %v, want ErrNotFound", err)
	}

	// And a slug another tenant already holds is refused rather than taken.
	if _, err := s.CreateTenant(ctx, identity.Tenant{
		ID: otherTenantID, Slug: betaSlug, DisplayName: betaName, Tier: identity.TierFree,
	}); err != nil {
		t.Fatalf("CreateTenant beta: %v", err)
	}
	if _, err := s.UpdateTenant(ctx, identity.Tenant{
		ID: otherTenantID, Slug: renamedSlug, Tier: identity.TierFree,
	}); !errors.Is(err, identity.ErrDuplicate) {
		t.Errorf("taking another tenant's slug returned %v, want ErrDuplicate", err)
	}
}
