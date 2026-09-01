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

// Package placement answers the one question the git write path cannot answer
// for itself: for this tenant's app in this environment, which repository,
// which branch, and which path.
//
// It is its own store rather than a column on a tenant, because the platform's
// recorded invariant is that tenant identity is NEVER parsed from a path.
// (repoURL, branch, path) is a row with a tenant column beside it; the moment
// the tenant is inferred from a directory name, a repository layout change
// becomes a security incident.
//
// The second invariant it enforces is that one commit never touches two
// tenants. That is not something the write path can check — by the time it has
// a worktree it has already decided where to write — so it is enforced here,
// as a constraint rather than as a rule everyone remembers.
//
// The third is the same shape one layer further out: one namespace never holds
// two tenants. A repository is where a tenant's desired state is written; a
// namespace is where it runs, and it is what the quota, the Pod Security level
// and the NetworkPolicy are attached to. Both claims exist because the caller
// that would breach them is an ordinary member of an ordinary tenant asking a
// perfectly well-formed question.
package placement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound is no such placement.
	ErrNotFound = errors.New("placement: not found")

	// ErrConflict is a placement that would break an invariant somebody else
	// already established — in practice, a repository another tenant owns.
	ErrConflict = errors.New("placement: conflict")

	// ErrInvalid is a placement that could never be written to.
	ErrInvalid = errors.New("placement: invalid")
)

// Placement is where one environment of one app lives in git.
//
// The three git fields are exactly gitwrite.Target minus its credentials.
// Credentials are deliberately absent: they are per-repository secrets with a
// rotation story of their own, and putting them in the row that the panel
// lists would mean every read of "where does this app live" is a read of a
// secret.
type Placement struct {
	// The key. App and Env are the tenant's own names for things and are not
	// interpreted here — nothing derives a path from them, because the path is
	// a field.
	TenantID string
	App      string
	Env      string

	RepoURL string
	// Branch is the plan's "ref". Named branch because that is what it is in
	// every case the write path can handle: it commits and pushes, and a tag
	// is not somewhere you can push a deploy to.
	Branch string
	// Path is the directory inside the repository, without a leading slash.
	Path string

	// Namespace is where the rendered manifest says it runs.
	//
	// A field and not a convention. Deriving it — tenant slug plus
	// environment, say — makes the namespace a parse of an identity, which is
	// the same mistake as parsing the tenant out of a path: the day a customer
	// renames a tenant or wants two environments in one namespace, a layout
	// decision has become a rewrite. It also means two installs can never
	// disagree about the rule, which sounds like a feature until somebody has
	// an existing namespace they need this to deploy into.
	Namespace string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Trigger is what a webhook needs to turn one push into one build.
//
// It is a type of its own and never a field on Placement, and the split is the
// whole reason this compiles the way it does. Placement is what the panel
// lists; a Secret on it would make every read of "where does this app live" a
// read of a secret, which is the objection this package's doc comment already
// makes about git credentials and which applies here word for word. The row is
// the same row — a trigger is deleted with the placement it belongs to, because
// nothing else could ever delete it — but Get and List do not select these
// columns and cannot return them.
//
// RepoURL is NOT Placement.RepoURL, and confusing the two is the mistake this
// comment exists to prevent. A placement's repository is the tenant's STATE
// repository, where damga commits manifests. This is the SOURCE repository a
// build clones, which nothing recorded before — server/builds.go says so in
// as many words, and made every caller carry it in the request body. A push
// arrives naming the source, so this is the field that turns a push into a
// tenant.
type Trigger struct {
	TenantID string
	App      string
	Env      string

	// Provider is the forge whose signature scheme the secret is for. Part of
	// the lookup rather than assumed, because the same repository can be
	// mirrored and two forges do not sign the same way.
	Provider string

	RepoURL string

	// Secret is what the payload is signed with. Held in plaintext because an
	// HMAC needs the key and not a hash of it — there is no version of this
	// that stores a digest and still verifies.
	Secret string
}

// CanonicalRepo is the one spelling a repository is stored and looked up under.
//
// It exists because a forge does not send one. GitHub's push payload carries
// clone_url ending in ".git" and html_url without it, both are correct names
// for the same repository, and whoever registered the webhook pasted whichever
// their browser showed them. An exact string match would work for about half
// of them, and the half that failed would look like a webhook that was never
// delivered — nothing logged, nothing built, no error anywhere.
//
// Applied by SetTrigger and TriggersFor and by nothing else. Placement.RepoURL
// is deliberately left alone: it is a claim key, two tenants are told apart by
// it, and quietly folding two spellings together there would change who owns
// what.
func CanonicalRepo(url string) string {
	u := strings.TrimSpace(url)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	// The scheme and host are case-insensitive and the path, on GitHub, is
	// too. Lowercasing the whole thing is wrong for a forge that is
	// case-sensitive in paths, which is why this is a documented choice rather
	// than an obvious one: matching too many spellings costs a build against a
	// repository somebody owns anyway, and matching too few costs a webhook
	// that silently never fires.
	return strings.ToLower(u)
}

// Validate is the check every implementation makes.
func (t Trigger) Validate() error {
	switch {
	case t.TenantID == "" || t.App == "" || t.Env == "":
		return fmt.Errorf("%w: a trigger needs a tenant, an app and an environment", ErrInvalid)
	case t.Provider == "":
		return fmt.Errorf("%w: a trigger needs a provider", ErrInvalid)
	case t.RepoURL == "":
		return fmt.Errorf("%w: a trigger needs the repository a push will name", ErrInvalid)
	case t.Secret == "":
		// Not defaulted and not optional. A trigger with an empty secret is an
		// endpoint that builds whatever anybody posts to it, and it would look
		// exactly like a working one until somebody noticed.
		return fmt.Errorf("%w: a trigger needs a secret", ErrInvalid)
	}
	return nil
}

// Collision reports what a placement being written would land on top of, in the
// words of what it costs rather than of which columns matched.
//
// Shared by every store rather than written in each, for the reason the rest of
// this package spells things once: three implementations answering the same
// question in three sentences is three sentences to keep true.
//
// Both messages name the environment that already holds it, because that is the
// one thing the person who typed this cannot see and the only thing they can
// act on. Neither names a tenant: a repository and a namespace both belong to
// one tenant already, so there is nobody else to name.
//
// have is assumed not to be want. Put is create-or-replace, and a row that
// conflicted with itself would make every update after the first one fail.
func Collision(want, have Placement) error {
	if have.RepoURL == want.RepoURL && have.Branch == want.Branch && have.Path == want.Path {
		return fmt.Errorf(
			"%w: %s/%s already writes to %q on branch %q of this repository, and two "+
				"environments writing one directory overwrite each other's manifest",
			ErrConflict, have.App, have.Env, want.Path, want.Branch)
	}
	if have.Namespace == want.Namespace && have.App == want.App {
		return fmt.Errorf(
			"%w: %s/%s is already the workload %q in namespace %q, and two environments "+
				"in one namespace are one running app",
			ErrConflict, have.App, have.Env, want.App, want.Namespace)
	}
	return nil
}

// Store is where placements live. The same shape as the other two stores in
// this repository: an interface the paid build can replace, with one SQL
// implementation configured per engine behind it.
type Store interface {
	// Put creates or replaces the placement for (TenantID, App, Env).
	//
	// It fails with ErrConflict if RepoURL is already owned by a different
	// tenant. A repository is claimed by the first tenant to place anything in
	// it and stays theirs until every placement pointing at it is deleted.
	//
	// Namespace is claimed the same way and for a sharper reason. A namespace
	// is the boundary the rendered manifest actually lands in: quota, Pod
	// Security Admission and NetworkPolicy are all attached to it, so a tenant
	// that could name a namespace another tenant is using would be deploying
	// inside somebody else's fence. Nothing above this store can refuse that —
	// by the time a manifest is rendered the namespace is already in it — so it
	// is refused here, as a constraint rather than as a rule everyone
	// remembers. One tenant may hold as many namespaces as it likes, and may
	// put as many apps in one as it likes; what it may not do is take one that
	// is in use elsewhere.
	//
	// It also fails with ErrConflict when two placements would land in the
	// same place, which is a different question from the two above: those are
	// about tenants and this one is about environments.
	//
	// Env reaches exactly one thing in this system — the key of this row. It is
	// not in the file a manifest is written to, which is RepoURL plus Branch
	// plus Path plus a constant filename, and it is not in the object that
	// manifest names, which is App plus Namespace. So two environments that
	// agree on either tuple are not two environments; they are one, with two
	// names, and the way that shows up is a deploy to one replacing the other's
	// desired state with nothing said.
	//
	// Measured before it was fixed: POST /apps accepted api/prod and api/qa
	// pointed at the same repository, branch and path, and accepted api/prod
	// and api/dev in one namespace. Both answered 201, and both pairs resolved
	// to one file and one Workload.
	//
	// The invariant the plan states — a tenant's environment is a namespace —
	// is therefore enforced from this side rather than assumed: what is refused
	// is the collision, not the layout, so a tenant is still free to put two
	// apps in one namespace or to feed several environments from one
	// repository along different paths.
	Put(ctx context.Context, p Placement) (Placement, error)

	// Get returns one placement.
	Get(ctx context.Context, tenantID, app, env string) (Placement, error)

	// List returns everything one tenant has placed, ordered by app then env
	// so a page built from it does not reshuffle between loads. Scoped to a
	// tenant and never global, for the same reason evidence.Refs is.
	List(ctx context.Context, tenantID string) ([]Placement, error)

	// Delete removes one placement. Removing the last one that points at a
	// repository — or at a namespace — releases that claim.
	Delete(ctx context.Context, tenantID, app, env string) error

	// RepoOwner reports which tenant holds a repository, if any. The write
	// path calls it before it opens a worktree, so that a misconfiguration is
	// a refusal rather than a commit into somebody else's history.
	RepoOwner(ctx context.Context, repoURL string) (string, error)

	// SetTrigger records how a push to a source repository reaches this
	// placement. It fails with ErrNotFound if there is no placement to attach
	// it to, because a trigger for an app that does not exist would accept
	// signed pushes and have nowhere to send them.
	SetTrigger(ctx context.Context, t Trigger) error

	// TriggersFor returns every trigger a push to this repository could match,
	// secrets included.
	//
	// Every one, and not the first: one repository legitimately feeds several
	// environments, and which of them a given push is for is decided by which
	// secret verifies — not here. It is also the only method that returns a
	// secret, which is what keeps the other reads free of them.
	//
	// It is NOT scoped to a tenant, and that is the one place in this store
	// where that is correct: the caller is an unauthenticated webhook that has
	// not yet proved which tenant it is. Proving it is what the signature does,
	// and nothing may be told apart by the answer to this call — see
	// server/hooks.go.
	TriggersFor(ctx context.Context, provider, repoURL string) ([]Trigger, error)

	Close() error
}

// Validate is the check every implementation makes, so three of them cannot
// drift into disagreeing about what a usable placement is.
func (p Placement) Validate() error {
	switch {
	case p.TenantID == "" || p.App == "" || p.Env == "":
		return fmt.Errorf("%w: a placement needs a tenant, an app and an environment", ErrInvalid)
	case p.RepoURL == "":
		return fmt.Errorf("%w: a placement needs a repository", ErrInvalid)
	case p.Namespace == "":
		return fmt.Errorf("%w: a placement needs a namespace", ErrInvalid)
	case p.Branch == "":
		// Not defaulted to main. A wrong guess here commits to a branch
		// nothing is watching, which looks exactly like a deploy that worked.
		return fmt.Errorf("%w: a placement needs a branch", ErrInvalid)
	case strings.HasPrefix(p.Path, "/"):
		return fmt.Errorf("%w: path %q must be relative to the repository root", ErrInvalid, p.Path)
	case p.Path == "" || p.Path == "." || strings.Contains(p.Path, ".."):
		// "" and "." are the repository root, which would put one app's
		// manifests where the tenant's own layout lives. ".." escapes it
		// entirely; go-git would resolve it against the worktree and write
		// outside the checkout.
		return fmt.Errorf("%w: path %q is not a directory inside the repository", ErrInvalid, p.Path)
	}
	return nil
}
