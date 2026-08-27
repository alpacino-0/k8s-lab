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

// Package identity is who may act, in which tenant, and how they proved it.
//
// It declares types and one interface and performs no I/O, the same shape as
// package evidence — but it is deliberately NOT an Options seam, and the
// difference is the point. The evidence store is replaced wholesale by a paid
// build, because an audit archive is a different product. Identity is not:
// single sign-on changes how an account row is *created*, never where accounts
// live, so making this a seam would force the enterprise build to reimplement
// teams and memberships to gain nothing.
//
// # The line this package draws
//
// Authenticating against an identity provider is free — a password, GitHub,
// or the customer's own Keycloak or Okta over OIDC. What is paid is making
// that provider the record of authority: SAML, SCIM, standing group-to-team
// synchronisation, and enforced single sign-on as a policy a local config edit
// cannot bypass. Everything in this package is the free half.
//
// # Its relationship to the evidence store
//
// There is no foreign key into the evidence schema and there must never be a
// join across the two. That is what the copied name and email on an evidence
// record buy: the archive is a store a paid build replaces entirely, possibly
// against another database, and it can only be replaced without carrying the
// account tables if nothing ever joined to them. The first query that resolves
// a fresher display name for an old deploy converts a seam into a schema
// contract, and nothing fails until the enterprise archive ships.
package identity

import (
	"context"
	"errors"
	"time"
)

// Errors callers branch on. Part of the contract every store honours.
var (
	// ErrNotFound is returned when nothing matches.
	ErrNotFound = errors.New("identity: not found")
	// ErrDuplicate is returned when a unique value is already taken — an email,
	// a tenant slug, a session token.
	ErrDuplicate = errors.New("identity: already exists")
	// ErrInvalid is returned for a value the store will not write.
	ErrInvalid = errors.New("identity: invalid")
)

// Tier is what a tenant is paying for. It mirrors evidence.Tier and must keep
// mirroring it: the value is copied onto every evidence record so that a
// retention claim made two years later is checkable against what was true then.
//
// Nothing in the free build branches on this, which is what keeps it out of the
// crippleware reading — writing "enterprise" here on a free binary buys the
// writer nothing at all.
type Tier string

const (
	TierFree       Tier = "free"
	TierEnterprise Tier = "enterprise"
)

func (t Tier) Valid() bool { return t == TierFree || t == TierEnterprise }

// Role is the free tier's whole permission model: three values, no custom
// roles. Fine-grained roles are the paid tier, and they arrive by replacing the
// Authorizer rather than by adding rows here.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

func (r Role) Valid() bool {
	return r == RoleOwner || r == RoleMember || r == RoleViewer
}

// Tenant is one customer. Its ID is opaque and permanent because evidence
// records hold it and are never rewritten; the slug is the part that changes.
type Tenant struct {
	ID          string
	Slug        string
	DisplayName string
	Tier        Tier
	Suspended   bool
	CreatedAt   time.Time
}

// Account is a person or an automation. Never deleted and never reused:
// evidence records point at this ID across a boundary that deliberately has no
// foreign key, so recycling one silently reattributes somebody else's history.
type Account struct {
	ID   string
	Kind string // "user" | "automation"

	// Email is the login and the contact address. Erasable.
	Email string

	// AuditEmail is what gets written into git commits and copied into evidence
	// records. Separate from Email on purpose: those copies are inside a hash
	// chain and inside history this platform does not own, so they can never be
	// redacted. Defaulting it to an instance-local alias is what makes an
	// erasure request answerable — what was published was never personal data.
	AuditEmail string

	DisplayName string
	Disabled    bool
	CreatedAt   time.Time
}

// Credential is how an account proves it is that account. One per account, and
// only for local passwords: a federated account has none, which is the honest
// representation of "this person's password lives at their identity provider".
type Credential struct {
	AccountID string

	// Hash is the encoded password hash, algorithm and parameters included, so
	// the parameters can be raised later without a flag day.
	Hash string

	UpdatedAt time.Time
}

// Membership is what an account may do inside one tenant. It is the ONLY source
// of a subject's tenant and role.
//
// That is a security property rather than a design preference. The free
// authorizer treats an unrecognised group as viewer, and a viewer may read the
// evidence page — so a subject assembled from anything the caller sent would
// let an unauthenticated request read another tenant's deploy history. No
// membership means no subject, never a viewer.
type Membership struct {
	AccountID string
	TenantID  string
	Role      Role
	CreatedAt time.Time
}

// Session is a live login.
//
// The store never sees the token itself. It holds the token's SHA-256 digest,
// which is what makes a leaked database dump useless for impersonation — and
// which is why lookup is by digest rather than by a secret compared in Go.
type Session struct {
	// Digest is sha256(token). The unique key.
	Digest []byte

	AccountID string

	// IssuedFor is the Host the session was created for, and it is checked on
	// every use. This is the anti-injection property the __Host- cookie prefix
	// would have given, obtained a way that still works on http://localhost —
	// where Chrome and Safari reject that prefix outright, and where this
	// project's own documented first run lives.
	IssuedFor string

	CreatedAt time.Time
	ExpiresAt time.Time
}

// Store is the whole persistence surface of identity.
//
// Deliberately narrow. Everything a paid build adds — SAML, SCIM, directory
// sync — creates and updates the same rows through the same methods; none of it
// needs a different store, which is why this is not an Options seam.
type Store interface {
	// CreateTenant writes a tenant. ErrDuplicate if the slug is taken.
	CreateTenant(ctx context.Context, t Tenant) (Tenant, error)
	// Tenant returns one by ID.
	Tenant(ctx context.Context, id string) (Tenant, error)
	// TenantBySlug returns one by its human-facing name.
	TenantBySlug(ctx context.Context, slug string) (Tenant, error)

	// CreateAccount writes an account and, if cred.Hash is non-empty, its
	// credential, in one transaction. ErrDuplicate if the email is taken.
	CreateAccount(ctx context.Context, a Account, cred Credential) (Account, error)
	// Account returns one by ID.
	Account(ctx context.Context, id string) (Account, error)
	// AccountByEmail is the login lookup. It returns ErrNotFound for an unknown
	// address, and callers must take the same time and say the same thing for
	// that as for a wrong password.
	AccountByEmail(ctx context.Context, email string) (Account, error)
	// Credential returns the stored hash. ErrNotFound when the account has no
	// password, which is a federated account rather than an error.
	Credential(ctx context.Context, accountID string) (Credential, error)
	// SetCredential replaces the password hash and revokes every session that
	// account holds, in one transaction. A password change that leaves the old
	// sessions alive is not a password change.
	SetCredential(ctx context.Context, cred Credential) error

	// RehashCredential replaces the hash and leaves sessions alone.
	//
	// Separate from SetCredential because they are different events wearing the
	// same shape. A password change invalidates what everyone knew; a rehash is
	// the same password stored under stronger parameters, and the moment to do
	// it is a successful login — where revoking sessions would log the user out
	// of the session that login just created. Conflating them means either
	// never raising the parameters or bouncing people at the door.
	//
	// ErrNotFound when the account has no credential: there is nothing to
	// upgrade, and creating one here would give a federated account a password
	// nobody set.
	RehashCredential(ctx context.Context, cred Credential) error

	// AddMember grants a role in a tenant. ErrDuplicate if one already exists.
	AddMember(ctx context.Context, m Membership) error
	// Membership is the authorization lookup: what this account may do here.
	Membership(ctx context.Context, accountID, tenantID string) (Membership, error)
	// Memberships lists every tenant an account belongs to, so a panel can
	// offer a choice rather than guessing.
	Memberships(ctx context.Context, accountID string) ([]Membership, error)
	// RemoveMember revokes a role. It does not delete the account, and it does
	// not touch the evidence of what that account did.
	RemoveMember(ctx context.Context, accountID, tenantID string) error

	// CreateSession stores a session by its digest.
	CreateSession(ctx context.Context, s Session) error
	// Session looks one up. It returns ErrNotFound for an expired session as
	// well as a missing one — an expired session is not a session, and making
	// the caller check would eventually mean a caller that forgets.
	Session(ctx context.Context, digest []byte, now time.Time) (Session, error)
	// DeleteSession is logout.
	DeleteSession(ctx context.Context, digest []byte) error
	// DeleteAccountSessions is what deprovisioning needs: terminate every
	// session an account holds, now, without waiting for one to expire.
	DeleteAccountSessions(ctx context.Context, accountID string) (int, error)
	// PruneExpired removes what has run out. Expired sessions are already
	// refused by Session; this is housekeeping, not a security control.
	PruneExpired(ctx context.Context, now time.Time) (int, error)

	// Bootstrap creates the first tenant, the first account, and the owner
	// membership that joins them — in one transaction, and at most once for
	// the lifetime of this store. A second call returns ErrDuplicate no
	// matter what it is given.
	//
	// It is one method rather than a claim followed by the three writes,
	// because a claim that commits separately can be followed by a crash,
	// and the install is then permanently marked as owned while having no
	// owner: nobody can log in and the only remedy is editing the database.
	// All-or-nothing is the only version of this that is safe to run once.
	//
	// The membership role is always owner. There is no argument for it
	// because there is no useful answer other than owner — an install whose
	// first account cannot grant anyone else access is an install with
	// nobody who can finish setting it up.
	Bootstrap(ctx context.Context, t Tenant, a Account, cred Credential) error

	// Bootstrapped reports whether Bootstrap has already succeeded, so a
	// caller can say so plainly instead of translating a duplicate-key
	// error into advice.
	Bootstrapped(ctx context.Context) (bool, error)

	Close() error
}
