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

	CreatedAt time.Time
	UpdatedAt time.Time
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
	Put(ctx context.Context, p Placement) (Placement, error)

	// Get returns one placement.
	Get(ctx context.Context, tenantID, app, env string) (Placement, error)

	// List returns everything one tenant has placed, ordered by app then env
	// so a page built from it does not reshuffle between loads. Scoped to a
	// tenant and never global, for the same reason evidence.Refs is.
	List(ctx context.Context, tenantID string) ([]Placement, error)

	// Delete removes one placement. Removing the last one that points at a
	// repository releases that repository's claim.
	Delete(ctx context.Context, tenantID, app, env string) error

	// RepoOwner reports which tenant holds a repository, if any. The write
	// path calls it before it opens a worktree, so that a misconfiguration is
	// a refusal rather than a commit into somebody else's history.
	RepoOwner(ctx context.Context, repoURL string) (string, error)

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
