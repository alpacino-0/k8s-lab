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
	now    func() time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{
		byKey:  map[string]placement.Placement{},
		owners: map[string]string{},
		now:    time.Now,
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
	s.releaseUnused()
	return nil
}

// RepoOwner is placement.Store.RepoOwner.
func (s *Store) RepoOwner(_ context.Context, repoURL string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owners[repoURL], nil
}

// Close is a no-op: there is nothing to release.
func (s *Store) Close() error { return nil }

// releaseUnused drops the claim on any repository nothing points at any more.
// Called with the mutex held, from the same critical section as the write that
// may have orphaned it.
func (s *Store) releaseUnused() {
	inUse := make(map[string]bool, len(s.byKey))
	for _, p := range s.byKey {
		inUse[p.RepoURL] = true
	}
	for repo := range s.owners {
		if !inUse[repo] {
			delete(s.owners, repo)
		}
	}
}

var _ placement.Store = (*Store)(nil)
