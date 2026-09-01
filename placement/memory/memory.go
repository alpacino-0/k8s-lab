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

// Package memory is placement.Store in process: what the tests use and what an
// installation with no DSN gets, which is a demo rather than an installation.
package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/damgahq/damga/placement"
)

// Store is placement.Store held in maps.
type Store struct {
	mu sync.Mutex

	byKey map[string]placement.Placement // tenant \x00 app \x00 env
	// owners is the same singleton claim the SQL schema keeps in a table:
	// repository URL to the tenant that holds it. Under one mutex here, which
	// gives the atomicity the primary key gives there.
	owners map[string]string
	// nsOwners is the second claim, keyed by namespace. A separate map rather
	// than a wider value on the first: the two are released independently,
	// because an app that moves to another repository keeps its namespace and
	// an app that moves to another namespace keeps its repository.
	nsOwners map[string]string
	// triggers is keyed the same way byKey is, because a trigger belongs to
	// exactly one placement and is deleted with it. Held apart from the
	// placement value rather than inside it so that no read which returns a
	// Placement can return a secret — the same split placement.Trigger
	// describes, enforced here by there being nowhere to put one.
	triggers map[string]placement.Trigger
	now      func() time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{
		byKey:    map[string]placement.Placement{},
		owners:   map[string]string{},
		nsOwners: map[string]string{},
		triggers: map[string]placement.Trigger{},
		now:      time.Now,
	}
}

func key(tenantID, app, env string) string {
	return tenantID + "\x00" + app + "\x00" + env
}

// Put is placement.Store.Put.
func (s *Store) Put(_ context.Context, p placement.Placement) (placement.Placement, error) {
	if err := p.Validate(); err != nil {
		return placement.Placement{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if owner, claimed := s.owners[p.RepoURL]; claimed && owner != p.TenantID {
		return placement.Placement{}, fmt.Errorf(
			"%w: repository %q belongs to another tenant", placement.ErrConflict, p.RepoURL)
	}
	// Both claims are checked before either is written, so a placement refused
	// on its namespace does not leave the repository claimed by a tenant whose
	// row was never stored.
	if owner, claimed := s.nsOwners[p.Namespace]; claimed && owner != p.TenantID {
		return placement.Placement{}, fmt.Errorf(
			"%w: namespace %q belongs to another tenant", placement.ErrConflict, p.Namespace)
	}

	now := s.now().UTC().Truncate(time.Microsecond)
	p.UpdatedAt = now
	// Preserved across a replace: moving an app to another directory is not
	// the app coming into existence.
	if old, ok := s.byKey[key(p.TenantID, p.App, p.Env)]; ok {
		p.CreatedAt = old.CreatedAt
	} else {
		p.CreatedAt = now
	}

	s.owners[p.RepoURL] = p.TenantID
	s.nsOwners[p.Namespace] = p.TenantID
	s.byKey[key(p.TenantID, p.App, p.Env)] = p
	s.releaseUnused()
	return p, nil
}

// Get is placement.Store.Get.
func (s *Store) Get(_ context.Context, tenantID, app, env string) (placement.Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byKey[key(tenantID, app, env)]
	if !ok {
		return placement.Placement{}, fmt.Errorf("%w: %s/%s/%s", placement.ErrNotFound, tenantID, app, env)
	}
	return p, nil
}

// List is placement.Store.List.
func (s *Store) List(_ context.Context, tenantID string) ([]placement.Placement, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: List needs a tenant", placement.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []placement.Placement
	for _, p := range s.byKey {
		if p.TenantID == tenantID {
			out = append(out, p)
		}
	}
	// Map iteration is deliberately unordered in Go, so this has to sort or
	// the panel's list would reshuffle on every load — and the SQL stores,
	// which order in the query, would disagree with this one.
	slices.SortFunc(out, func(a, b placement.Placement) int {
		if c := strings.Compare(a.App, b.App); c != 0 {
			return c
		}
		return strings.Compare(a.Env, b.Env)
	})
	return out, nil
}

// Delete is placement.Store.Delete.
func (s *Store) Delete(_ context.Context, tenantID, app, env string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byKey, key(tenantID, app, env))
	// With the placement, never after it. A trigger outliving its placement is
	// an endpoint that verifies a signature and then has no app to build.
	delete(s.triggers, key(tenantID, app, env))
	s.releaseUnused()
	return nil
}

// SetTrigger is placement.Store.SetTrigger.
func (s *Store) SetTrigger(_ context.Context, t placement.Trigger) error {
	if err := t.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byKey[key(t.TenantID, t.App, t.Env)]; !ok {
		return fmt.Errorf("%w: %s/%s/%s", placement.ErrNotFound, t.TenantID, t.App, t.Env)
	}
	t.RepoURL = placement.CanonicalRepo(t.RepoURL)
	s.triggers[key(t.TenantID, t.App, t.Env)] = t
	return nil
}

// TriggersFor is placement.Store.TriggersFor.
func (s *Store) TriggersFor(_ context.Context, provider, repoURL string) ([]placement.Trigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := placement.CanonicalRepo(repoURL)
	var out []placement.Trigger
	for _, t := range s.triggers {
		// The empty secret is excluded here as it is in the SQL query: a
		// placement written before triggers existed carries empty columns, and
		// this lookup is made by an unauthenticated caller.
		if t.Provider == provider && t.RepoURL == want && t.Secret != "" {
			out = append(out, t)
		}
	}
	// Ordered for the same reason List is, and for one more: the caller tries
	// each secret in turn, so an unordered answer makes which trigger a push
	// matched depend on map iteration when two share a secret.
	slices.SortFunc(out, func(a, b placement.Trigger) int {
		if c := strings.Compare(a.TenantID, b.TenantID); c != 0 {
			return c
		}
		if c := strings.Compare(a.App, b.App); c != 0 {
			return c
		}
		return strings.Compare(a.Env, b.Env)
	})
	return out, nil
}

// RepoOwner is placement.Store.RepoOwner.
func (s *Store) RepoOwner(_ context.Context, repoURL string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owners[repoURL], nil
}

// Close is a no-op: there is nothing to release.
func (s *Store) Close() error { return nil }

// releaseUnused drops the claim on any repository or namespace nothing points
// at any more. Called with the mutex held, from the same critical section as
// the write that may have orphaned it.
func (s *Store) releaseUnused() {
	repos := make(map[string]bool, len(s.byKey))
	namespaces := make(map[string]bool, len(s.byKey))
	for _, p := range s.byKey {
		repos[p.RepoURL] = true
		namespaces[p.Namespace] = true
	}
	for repo := range s.owners {
		if !repos[repo] {
			delete(s.owners, repo)
		}
	}
	for ns := range s.nsOwners {
		if !namespaces[ns] {
			delete(s.nsOwners, ns)
		}
	}
}

var _ placement.Store = (*Store)(nil)
