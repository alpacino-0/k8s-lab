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
	"time"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/sqlite"
	"github.com/damgahq/damga/evidence/storetest"
)

// The same suite the in-process store passes. That is the whole point of it
// existing: an implementation that only satisfies the compiler is not a
// substitute for one an auditor is going to read.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, window time.Duration) evidence.Store {
		// A file rather than :memory:, so the suite exercises WAL, the busy
		// timeout and real locking — the things that only misbehave on disk.
		path := filepath.Join(t.TempDir(), "evidence.db")
		s, err := sqlite.Open(context.Background(), path, sqlite.Options{Window: window})
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

// Migrations run on open and are idempotent. A second Open of the same file
// must not try to create the schema again — that is what the version table is
// for, and getting it wrong only shows up on the second start.
func TestReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.db")
	ctx := context.Background()

	first, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	rec, err := first.Append(ctx, evidence.Record{
		IdempotencyKey: "commit:aaa",
		Ref:            evidence.Ref{TenantID: "t", App: "api", Env: "prod"},
		Tier:           evidence.TierFree,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	got, err := second.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.IdempotencyKey != "commit:aaa" {
		t.Errorf("IdempotencyKey = %q after reopen, want the stored one", got.IdempotencyKey)
	}
	if string(got.Hash) != string(rec.Hash) {
		t.Error("the hash changed across a reopen; an export taken before a restart would stop verifying")
	}
}
