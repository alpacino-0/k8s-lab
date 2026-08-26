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

// Package sqlite is the identity store for an installation on one node.
//
// Pure Go, for the same reason the evidence store is: the published images are
// built CGO_ENABLED=0 onto distroless/static, so a cgo driver is excluded by
// construction.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	// Registers the "sqlite" driver.
	_ "modernc.org/sqlite"

	"github.com/damgahq/damga/identity"
	"github.com/damgahq/damga/identity/internal/sqlstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

type dialect struct{}

func (dialect) Name() string           { return "identity/sqlite" }
func (dialect) Rebind(q string) string { return q }
func (dialect) Migrations() fs.FS      { return migrations }

// memoryPath is the throwaway database.
const memoryPath = ":memory:"

// Open opens or creates the database at path. Use memoryPath for a throwaway
// store.
func Open(ctx context.Context, path string) (*sqlstore.Store, error) {
	if path == "" {
		return nil, errors.New("identity/sqlite: no database path")
	}
	// foreign_keys is off by default in SQLite, which would make every
	// REFERENCES clause in the schema decorative — and this schema leans on
	// them: a membership pointing at no account is a role granted to nobody.
	pragmas := []string{
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_txlock=immediate",
	}
	// WAL does not apply to an in-memory database, and a shared cache is what
	// keeps one alive across connections rather than giving each its own empty
	// copy.
	dsn := path + "?_pragma=journal_mode(WAL)&" + strings.Join(pragmas, "&")
	if path == memoryPath {
		dsn = "file::memory:?cache=shared&" + strings.Join(pragmas, "&")
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("identity/sqlite: opening %s: %w", path, err)
	}
	// One connection: SQLite has one write lock, and a second writer turns a
	// lock that would have been waited on into an error returned to the caller.
	db.SetMaxOpenConns(1)

	store, err := sqlstore.New(ctx, db, dialect{})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return store, nil
}

var _ identity.Store = (*sqlstore.Store)(nil)
