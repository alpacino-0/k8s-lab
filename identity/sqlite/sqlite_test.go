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

package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/damgahq/damga/identity"
	"github.com/damgahq/damga/identity/sqlite"
	"github.com/damgahq/damga/identity/storetest"
)

// The same suite the in-process store passes. A file rather than :memory:, so
// the suite exercises real locking and the foreign keys that only bite on disk.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) identity.Store {
		s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "identity.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return s
	})
}
