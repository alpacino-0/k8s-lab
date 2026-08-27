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

// Package memory is forge.Store in process: what the tests use and what an
// installation with no DSN gets, which is a demo rather than an installation.
package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/damgahq/damga/forge"
)

// Store is forge.Store held in maps.
type Store struct {
	mu sync.Mutex

	byKey map[forge.Key]forge.Connection
	// owners is the same singleton claim the SQL schema keeps in a table:
	// rendered identity to the tenant that holds it. Under one mutex here,
	// which gives the atomicity the primary key gives there.
	owners map[string]string
	now    func() time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{
		byKey:  map[forge.Key]forge.Connection{},
		owners: map[string]string{},
		now:    time.Now,
	}
}

// Put is forge.Store.Put.
func (s *Store) Put(_ context.Context, c forge.Connection) (forge.Connection, error) {
	if err := c.Validate(); err != nil {
		return forge.Connection{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	identity := c.Identity()
	if owner, held := s.owners[identity]; held && owner != c.TenantID {
		return forge.Connection{}, fmt.Errorf(
			"%w: the identity %s is already connected by another tenant",
			forge.ErrConflict, identity)
	}

	now := s.now().UTC()
	key := c.Key()
	if existing, ok := s.byKey[key]; ok {
		c.CreatedAt = existing.CreatedAt
		// The identity this connection used to hold is released, or an app that
		// moves to a different repository leaves a claim nobody can take.
		if old := existing.Identity(); old != identity {
			delete(s.owners, old)
		}
	} else {
		c.CreatedAt = now
	}
	c.UpdatedAt = now

	s.byKey[key] = c
	s.owners[identity] = c.TenantID
	return c, nil
}

// Get is forge.Store.Get.
func (s *Store) Get(_ context.Context, k forge.Key) (forge.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.byKey[k]
	if !ok {
		return forge.Connection{}, fmt.Errorf("%w: %s/%s", forge.ErrNotFound, k.TenantID, k.App)
	}
	return c, nil
}

// List is forge.Store.List.
func (s *Store) List(_ context.Context, tenantID string) ([]forge.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := []forge.Connection{}
	for k, c := range s.byKey {
		if k.TenantID == tenantID {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b forge.Connection) int {
		return strings.Compare(a.App, b.App)
	})
	return out, nil
}

// Delete is forge.Store.Delete.
func (s *Store) Delete(_ context.Context, k forge.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.byKey[k]
	if !ok {
		return fmt.Errorf("%w: %s/%s", forge.ErrNotFound, k.TenantID, k.App)
	}
	delete(s.byKey, k)
	delete(s.owners, c.Identity())
	return nil
}

// IdentityOwner is forge.Store.IdentityOwner.
func (s *Store) IdentityOwner(_ context.Context, identity string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	owner, ok := s.owners[identity]
	if !ok {
		return "", fmt.Errorf("%w: %s", forge.ErrNotFound, identity)
	}
	return owner, nil
}

// Close is forge.Store.Close. Nothing to release.
func (s *Store) Close() error { return nil }
