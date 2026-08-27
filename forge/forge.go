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
	"errors"
	"fmt"
	"strings"
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
	Namespace       string
	ImageRepository string
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
		{"namespace", c.Namespace}, {"image repository", c.ImageRepository},
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
