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

package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/postgres"
	"github.com/damgahq/damga/evidence/storetest"
)

// dsnEnv names the database to run against. Skipping without it keeps
// `go test ./...` useful on a laptop; CI setting it is what stops the skip
// from becoming permanent.
const dsnEnv = "DAMGA_TEST_POSTGRES_DSN"

// The same suite the in-process and SQLite stores pass, which is the whole
// reason it exists: no single statement is correct on both engines —
// BEGIN IMMEDIATE serialises writers on one and does nothing on the other,
// FOR UPDATE parses on one and not the other. The alternative to this job is
// auditing every write path by eye, forever.
func TestConformance(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run the PostgreSQL conformance suite", dsnEnv)
	}

	storetest.Run(t, func(t *testing.T, window time.Duration) evidence.Store {
		// A schema per case rather than a database per case. The suite assumes
		// it owns an empty store; creating a schema is cheap and dropping one
		// cannot fail because somebody else still holds a connection.
		schema := fmt.Sprintf("conformance_%d", time.Now().UnixNano())
		admin := mustOpen(t, dsn)
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatalf("creating schema: %v", err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
				t.Errorf("dropping schema: %v", err)
			}
			if err := admin.Close(); err != nil {
				t.Errorf("closing admin connection: %v", err)
			}
		})

		s, err := postgres.Open(context.Background(), withSearchPath(t, dsn, schema),
			postgres.Options{Window: window})
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

func mustOpen(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

// withSearchPath points a copy of the DSN at one schema. Done through
// net/url rather than string concatenation because the DSN may already carry
// query parameters, and getting that wrong silently connects every case to the
// same schema — which would look like the suite passing.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing %s: %v", dsnEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// Why LockRow returns " FOR UPDATE", demonstrated rather than asserted.
//
// The conformance suite does not prove this. Its racing-transitions case was
// measured against this engine with the clause removed and passed sixty runs
// out of sixty: eight goroutines contending through database/sql do not
// reliably overlap the window between the read and the write, so the suite
// establishes that the store is correct and says nothing about why. Leaving it
// there would mean the one line that separates a compare-and-set from a lost
// update is held in place by a comment.
//
// So the mechanism is exercised directly, at the level it lives at. Two
// transactions, the interleaving forced rather than hoped for.
func TestForUpdateIsLoadBearing(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run the PostgreSQL locking check", dsnEnv)
	}
	ctx := context.Background()
	db := mustOpen(t, dsn)
	t.Cleanup(func() { _ = db.Close() })

	table := fmt.Sprintf("lock_probe_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE "+table+" (id TEXT PRIMARY KEY, state TEXT NOT NULL)"); err != nil {
		t.Fatalf("creating probe table: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, "DROP TABLE "+table); err != nil {
			t.Errorf("dropping probe table: %v", err)
		}
	})

	// readThenWrite is the shape of every state transition in the store: read
	// the current state, decide it is acceptable, write the new one.
	readThenWrite := func(lock, to string) (seen string, err error) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		defer func() { _ = tx.Rollback() }()
		if err := tx.QueryRowContext(ctx,
			"SELECT state FROM "+table+" WHERE id = 'x'"+lock).Scan(&seen); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE "+table+" SET state = $1 WHERE id = 'x'", to); err != nil {
			return seen, err
		}
		return seen, tx.Commit()
	}

	for _, tc := range []struct {
		name     string
		lock     string
		wantSeen string
	}{
		// Without the clause both transactions read the same state under READ
		// COMMITTED, both find it acceptable, and both write. Nobody is told.
		{"without FOR UPDATE", "", "pending"},
		// With it the second SELECT blocks until the first commits and then
		// reads what the first wrote, which is what makes the compare fail.
		{"with FOR UPDATE", " FOR UPDATE", "applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx,
				"INSERT INTO "+table+" (id, state) VALUES ('x', 'pending') "+
					"ON CONFLICT (id) DO UPDATE SET state = 'pending'"); err != nil {
				t.Fatalf("resetting the probe row: %v", err)
			}

			// The first transaction is held open across the second one's read,
			// which is the interleaving the goroutine version never reached.
			first, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			var firstSaw string
			if err := first.QueryRowContext(ctx,
				"SELECT state FROM "+table+" WHERE id = 'x'"+tc.lock).Scan(&firstSaw); err != nil {
				t.Fatalf("first SELECT: %v", err)
			}
			if _, err := first.ExecContext(ctx,
				"UPDATE "+table+" SET state = 'applied' WHERE id = 'x'"); err != nil {
				t.Fatalf("first UPDATE: %v", err)
			}

			type result struct {
				seen string
				err  error
			}
			done := make(chan result, 1)
			go func() {
				seen, err := readThenWrite(tc.lock, "running")
				done <- result{seen, err}
			}()

			// Long enough that the second transaction has certainly reached its
			// SELECT: without the lock it has already read and moved on, with
			// it is it blocked.
			time.Sleep(150 * time.Millisecond)
			if err := first.Commit(); err != nil {
				t.Fatalf("first Commit: %v", err)
			}

			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("second transaction: %v", got.err)
				}
				if got.seen != tc.wantSeen {
					t.Errorf("the second transaction read %q, want %q — %s",
						got.seen, tc.wantSeen,
						"this is the difference between a compare-and-set and a lost update")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the second transaction never finished")
			}
		})
	}
}
