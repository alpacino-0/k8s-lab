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

// Package sqlite is the evidence store for an installation that runs on one
// node, which is where every installation starts.
//
// Pure Go (modernc.org/sqlite): the images this project publishes are built
// CGO_ENABLED=0 onto distroless/static, so a cgo driver is excluded by
// construction rather than by preference.
//
// It is not the whole answer, and the limit is worth stating where someone
// choosing it will read it: SQLite has no roles and no REVOKE, so it can carry
// "we do not modify evidence" and never "we cannot". An install that needs
// the second sentence needs PostgreSQL and a role without UPDATE.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	// Registers the "sqlite" driver.
	_ "modernc.org/sqlite"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/internal/sqlstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

// memoryPath is the throwaway database. Shared-cache rather than private,
// because the reader and the writer are separate pools and a private in-memory
// database would give each of them its own empty one.
const memoryPath = ":memory:"

type dialect struct{}

func (dialect) Name() string { return "sqlite" }

// Rebind is the identity: SQLite takes '?' as written.
func (dialect) Rebind(q string) string { return q }

// LockRow is empty. The connection took the single write lock when the
// transaction began (_txlock=immediate), so a read inside it is already
// serialised against every other writer — and SQLite cannot parse FOR UPDATE.
func (dialect) LockRow() string { return "" }

func (dialect) Migrations() fs.FS { return migrations }

// Options configures Open. The zero value is usable.
type Options struct {
	// Window is how long a record that is not current is kept. Zero means
	// unbounded, which is the default: a sweep is what blanks a
	// page, and the page is the product.
	Window time.Duration
}

// Open opens or creates the database at path and applies any pending
// migrations. Use ":memory:" for a throwaway store.
func Open(ctx context.Context, path string, opts Options) (*sqlstore.Store, error) {
	if path == "" {
		return nil, errors.New("sqlite: no database path")
	}
	if path != memoryPath {
		if err := refuseNetworkFilesystem(path); err != nil {
			return nil, err
		}
	}

	// _txlock=immediate takes the write lock when the transaction begins rather
	// than at its first write. Without it a read-then-write returns
	// SQLITE_BUSY_SNAPSHOT immediately and ignores busy_timeout entirely, and
	// every state transition in the store is exactly that shape.
	//
	// busy_timeout makes a contended writer wait instead of failing.
	// foreign_keys is off by default, which would make every ON DELETE clause
	// in the schema decorative.
	// temp_store=MEMORY because the workloads this project renders run with a
	// read-only root filesystem, so there is nowhere to put a temp file.
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=temp_store(MEMORY)",
		"_txlock=immediate",
	}
	dsn := path + "?" + strings.Join(pragmas, "&")
	inMemory := path == memoryPath
	if inMemory {
		// A shared cache keeps the two pools looking at the same database
		// rather than at two empty ones. WAL does not apply to memory.
		dsn = "file::memory:?cache=shared&" + strings.Join(pragmas[1:], "&")
	}

	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening %s: %w", path, err)
	}
	// One writer, because SQLite has one write lock. A second would turn a
	// lock that would have been waited on into an error returned to the caller.
	w.SetMaxOpenConns(1)

	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sqlite: opening %s for reading: %w", path, err), w.Close())
	}
	if inMemory {
		// A shared-cache in-memory database lives only as long as a connection
		// to it does, so the reader must not be allowed to drop to zero.
		r.SetMaxOpenConns(1)
		r.SetMaxIdleConns(1)
		r.SetConnMaxLifetime(0)
	}

	store, err := sqlstore.New(ctx, w, r, dialect{}, opts.Window)
	if err != nil {
		return nil, errors.Join(err, w.Close(), r.Close())
	}
	return store, nil
}

var _ evidence.Store = (*sqlstore.Store)(nil)
