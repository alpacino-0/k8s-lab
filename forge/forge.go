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

// Package forge connects a tenant's own source repository, and renders the two
// halves of the supply chain that have to agree about who is allowed to sign.
//
// # Two repositories, and they are not the same one
//
// A tenant has two. The one placement knows about is the *state* repository:
// damga owns it, damga is the only writer, and the tenant has no push identity
// for it — that is what makes the API the only real writer. This package is
// about the other one, the tenant's *source* repository, which damga does not
// own, cannot write to, and reaches only by opening a pull request a human
// merges.
//
// Conflating them would be the expensive mistake. A source repository the
// tenant can push to is by definition not a place a deploy can be authorised
// from, and a state repository the tenant cannot push to is not somewhere their
// CI can build.
//
// # The platform never signs
//
// The obvious design is for the platform to build the image and sign it. It was
// rejected, and not on effort: an image the platform signs and the platform
// verifies reduces the claim to "I trust what I produced", which is the sentence
// that has to survive being read out to an auditor.
//
// So the user's own forge builds and signs, with the user's own OIDC identity,
// and damga only verifies. The platform is not a signing party anywhere in the
// chain — and a party that never signs cannot forge a signature, so declining
// the job is the stronger position rather than the weaker one. What the platform
// contributes is the workflow file, delivered as a one-file pull request. The
// merge click is not friction to be removed; it is the approval that makes the
// signature mean anything.
//
// # Why this package renders both halves
//
// The workflow decides what identity Fulcio will put in the certificate. The
// admission policy decides what identity is accepted. Nothing in Kubernetes,
// Sigstore or git checks that those two agree — a mismatch fails closed and
// looks like a broken deploy, and the looser kind of mismatch does not fail at
// all, which is worse.
//
// Both are therefore rendered here from one Connection, and TestIdentityIsOne
// takes the rendered workflow apart and rebuilds the identity from what the
// file actually says, then compares it to what the policy actually pins. Drift
// between the two is a test failure rather than a support ticket.
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Connection is a tenant's source repository, and everything about it that
// determines the signing identity.
//
// Every field is here because it appears in the certificate subject or in what
// the policy matches. Nothing is derived, for the reason placement gives about
// namespaces: a value derived from another value is a layout decision that has
// become a rewrite the first time somebody needs it to be different.
type Connection struct {
	// Whose it is. TenantID scopes the policy that gets rendered; App is the
	// tenant's own name and is not interpreted.
	TenantID string
	App      string

	// Where the source lives. Host is separate from Owner so that the day a
	// second forge is supported, the thing that changes is a value rather than
	// a parse of a URL.
	Host  string
	Owner string
	Repo  string

	// Branch is the ref the signing workflow runs on, and half of the identity.
	// A signature made on any other branch is not this identity.
	Branch string

	// WorkflowPath is where the file this package renders will live in the
	// tenant's repository, relative to its root. It is the other half of the
	// identity: pinning the repository alone would accept any workflow any
	// contributor could add on any branch.
	WorkflowPath string

	// ImageRepository is where the built image is pushed, and what the policy
	// matches before it checks a signature at all. Without it the policy would
	// have to apply to every image in the namespace, including ones this tenant
	// never built.
	//
	// Per app and not per environment, like everything else here. Environments
	// deploy different digests out of one repository; they do not each get
	// their own.
	ImageRepository string

	// FirstSignatureAt is when a signature bearing this identity was first
	// seen, and it is what decides whether the policy rejects or merely
	// records.
	//
	// The ordering here only works in one direction, and getting it backwards
	// breaks the tenant rather than protecting them. Enforce the policy before
	// the workflow has ever produced a signed image and the next deploy is
	// refused — the tenant connected a repository and their deploys stopped.
	// Merge the workflow before anything verifies and images are signed while
	// nothing checks them, which delivers nothing but breaks nothing. So the
	// rule is: record until there is proof the chain works, reject afterwards.
	//
	// Zero until something observes that signature. Nothing does yet; the
	// observation belongs with the evidence the deploy path already writes,
	// and until it exists every connection renders an auditing policy, which
	// is the safe end of the wrong answer.
	FirstSignatureAt time.Time

	// Set by the store, ignored on the way in. UpdatedAt is what the drift
	// check will compare against: a connection edited after the pull request
	// was merged is the open question phase 2 still owes an answer to.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Verified reports whether the chain has been proven end to end at least once.
func (c Connection) Verified() bool { return !c.FirstSignatureAt.IsZero() }

// Key is what identifies a connection. One source repository per app, and the
// environments are not part of it.
//
// This was nearly (tenant, app, env), and it is worth writing down why that is
// wrong: an app has one source repository and one signing identity, and it
// deploys to several environments out of it. Keying by environment would mean
// three rows differing only in namespace, each rendering the identical identity
// — and then "which repository is this app connected to" has no single answer,
// which is the question the pull request and the drift check both have to ask.
type Key struct {
	TenantID string
	App      string
}

// Key returns this connection's key.
func (c Connection) Key() Key { return Key{TenantID: c.TenantID, App: c.App} }

var (
	// ErrNotFound is no such connection.
	ErrNotFound = errors.New("forge: not found")

	// ErrConflict is a connection that would break an invariant somebody else
	// already relies on.
	ErrConflict = errors.New("forge: conflict")

	// ErrInvalid is a connection that could never produce a signing identity.
	ErrInvalid = errors.New("forge: invalid")
)

// Store is where connections live. The same shape as the other stores here: an
// interface the paid build can replace, with one SQL implementation per engine
// behind it.
type Store interface {
	// Put creates or replaces the connection for (TenantID, App).
	//
	// It fails with ErrConflict when the identity this connection would render
	// already belongs to a different tenant. Two tenants pointing at one source
	// repository and branch would be two tenants whose images are accepted by
	// each other's policy — the subject is the same string, and a policy cannot
	// tell which tenant's build produced a signature bearing it.
	Put(ctx context.Context, c Connection) (Connection, error)

	// Get returns one connection.
	Get(ctx context.Context, k Key) (Connection, error)

	// List returns everything one tenant has connected, ordered by app so a
	// page built from it does not reshuffle between loads. Scoped to a tenant
	// and never global, for the same reason placement.List is.
	List(ctx context.Context, tenantID string) ([]Connection, error)

	// Delete removes one connection, and releases the identity it held.
	Delete(ctx context.Context, k Key) error

	// IdentityOwner reports which tenant holds an identity, if any.
	//
	// Read before the pull request is opened, so that proposing a workflow into
	// a repository whose identity is already spoken for is a refusal rather
	// than a second policy accepting the first tenant's signatures.
	IdentityOwner(ctx context.Context, identity string) (string, error)

	Close() error
}

// SupportedHost is the only forge this build can express a keyless identity
// for.
//
// Not a limitation of this code. Public Fulcio accepts GitHub Actions,
// gitlab.com, CircleCI, Buildkite, Buddy and Codefresh, and does not accept
// self-hosted GitLab, Gitea, Forgejo or Bitbucket — a user on one of those
// cannot sign keyless at all, whatever this package does. The others are
// absent because each has its own certificate subject shape, and a shape
// guessed rather than measured against a real signature is a policy that
// silently accepts nothing.
const SupportedHost = "github.com"

// OIDCIssuer is the issuer in the certificate GitHub Actions gets from Fulcio.
//
// A public issuer that already exists, which is the whole reason this design is
// nearly free: no private Fulcio, no CA key, no TUF root, no rotation ceremony.
// Moving the build into the tenant's repository changes the identity string and
// nothing else about the mechanism.
const OIDCIssuer = "https://token.actions.githubusercontent.com"

// RekorURL is the transparency log the signature is anchored in.
const RekorURL = "https://rekor.sigstore.dev"

// WorkflowTrigger is the event the signing workflow runs on, and it is part of
// the identity: GitHub puts it in the certificate as an extension, and the
// policy checks it.
//
// Pinned to push because that is the only trigger whose ref is the branch the
// identity names. A workflow_dispatch or a pull_request run on the same file
// presents a different ref, so accepting more than one trigger would mean
// accepting an identity the branch pin no longer constrains.
const WorkflowTrigger = "push"

// ErrUnsupportedHost is returned for a forge whose keyless identity this build
// cannot express. It is deliberately not a fallback to something weaker: a
// tenant who cannot sign should be told so and shown the tier they are on, not
// quietly verified against a rule that accepts anything.
var ErrUnsupportedHost = errors.New("forge: keyless signing is not available for this host")

// Validate reports why this connection cannot produce a signing identity.
//
// Every rule here exists to stop an identity that is broader than it looks. A
// glob in an owner or a repository name, an empty branch, a workflow path that
// is not under .github/workflows — each renders a subject that either matches
// nothing, or matches more than the one workflow this tenant approved.
func (c Connection) Validate() error {
	if c.Host != SupportedHost {
		return fmt.Errorf("%w: %q (only %q can sign keyless here)",
			ErrUnsupportedHost, c.Host, SupportedHost)
	}
	for _, f := range []struct {
		name, value string
	}{
		{"tenant", c.TenantID}, {"app", c.App},
		{"owner", c.Owner}, {"repo", c.Repo}, {"branch", c.Branch},
		{"image repository", c.ImageRepository},
		{"workflow path", c.WorkflowPath},
	} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("forge: %s is required", f.name)
		}
		// A wildcard anywhere in the identity is the failure this whole design
		// is trying to avoid. `.../repo/*` was the subject once and it accepted
		// any workflow on any branch, which survives only while the repository
		// is one you control — and in phase 2 it is the customer's.
		if strings.ContainsAny(f.value, "*?") {
			return fmt.Errorf("forge: %s may not contain a wildcard, got %q", f.name, f.value)
		}
	}
	if strings.ContainsAny(c.Owner+c.Repo, "/") {
		return fmt.Errorf("forge: owner and repo are separate fields, got %q/%q", c.Owner, c.Repo)
	}
	if !strings.HasPrefix(c.WorkflowPath, workflowDir) {
		return fmt.Errorf("forge: workflow path must be under %s, got %q", workflowDir, c.WorkflowPath)
	}
	if strings.HasPrefix(c.Branch, "refs/") {
		// Branch is a branch, and Ref() is what turns it into a ref. Taking
		// "refs/heads/main" here would render "refs/heads/refs/heads/main",
		// which matches nothing and reads as a policy that is simply never
		// satisfied.
		return fmt.Errorf("forge: branch is a branch name, not a ref, got %q", c.Branch)
	}
	return nil
}

const workflowDir = ".github/workflows/"

// Ref is the git ref the identity names.
func (c Connection) Ref() string { return "refs/heads/" + c.Branch }

// Identity is the certificate subject the tenant's workflow will present, and
// the exact string the policy has to accept.
//
// It is one function because it is one fact. Rendering the subject in the
// policy template and the workflow's own location independently is how the two
// drift, and a drift here is either every deploy rejected or, in the other
// direction, an identity nobody meant to accept.
func (c Connection) Identity() string {
	return fmt.Sprintf("https://%s/%s/%s/%s@%s", c.Host, c.Owner, c.Repo, c.WorkflowPath, c.Ref())
}
