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

// Package sqlite is placement.Store on modernc.org/sqlite: what a one-node
// install runs, with no cgo.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/damgahq/damga/placement"
	"github.com/damgahq/damga/placement/internal/sqlstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

type dialect struct{}

func (dialect) Name() string           { return "placement/sqlite" }
func (dialect) Rebind(q string) string { return q }
func (dialect) Migrations() fs.FS      { return migrations }

const memoryPath = ":memory:"

// Open opens or creates the database at path and applies any pending
// migrations.
func Open(ctx context.Context, path string) (*sqlstore.Store, error) {
	if path == "" {
		return nil, errors.New("placement/sqlite: no database path")
	}
	pragmas := []string{
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_txlock=immediate",
	}
	dsn := path + "?_pragma=journal_mode(WAL)&" + strings.Join(pragmas, "&")
	if path == memoryPath {
		dsn = "file::memory:?cache=shared&" + strings.Join(pragmas, "&")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("placement/sqlite: opening %s: %w", path, err)
	}
	// One writer. SQLite has one write lock, and letting database/sql open a
	// second writer turns a lock that would have been waited on into a
	// SQLITE_BUSY returned to the caller.
	db.SetMaxOpenConns(1)

	store, err := sqlstore.New(ctx, db, dialect{})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return store, nil
}

var _ placement.Store = (*sqlstore.Store)(nil)
