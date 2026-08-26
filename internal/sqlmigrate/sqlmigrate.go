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

// Package sqlmigrate applies a numbered sequence of embedded .sql files, once,
// in order.
//
// It lives at the module root rather than beside the evidence store because a
// second schema needs it and could not reach it: importing
// evidence/internal/sqlstore from a sibling package is a compile error, which
// is the point of internal and not a thing to work around.
//
// The table name is a parameter, and that is the whole reason this exists as a
// separate package. A single shared schema_migration table holds one version
// read with MAX(version), so a second sequence starting at 1 against a database
// where the first has already reached 1 is SILENTLY SKIPPED — no error, the
// tables simply never exist, and the failure surfaces later as "no such table"
// in whatever order the queries happen to run. Renumbering cannot dodge it
// either: the loader below refuses a sequence that is not gapless from 1.
package sqlmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Dialect is the part of an engine this package needs. It is deliberately a
// subset of what a store's own dialect provides, so a store passes itself in
// without adapting anything.
// timeLayout matches the evidence schema: fixed-width RFC3339 in UTC, so
// lexicographic order is chronological on both engines with no CAST.
const timeLayout = "2006-01-02T15:04:05.000000Z"

type Dialect interface {
	// Name appears in error messages.
	Name() string
	// Rebind turns '?' placeholders into whatever the driver expects.
	Rebind(query string) string
	// Migrations holds the numbered .sql files in a directory called
	// "migrations".
	Migrations() fs.FS
}

// The obvious dependency here is goose, and it was measured rather than
// assumed: it pulls 82 modules against this repository's 188, because it
// carries a driver for every database it supports. What is actually needed is
// a linear sequence of embedded files applied once, in order, inside a
// transaction, recorded in a table. That is the code below.
//
// This is a deliberate line, not a general licence to hand-roll: the moment
// this needs branching versions, down-migrations or advisory locking across
// replicas, the right answer is a real tool and not more of this file.

// migration is one numbered file. The number is the ordering and the identity;
// the name is for the error message.
type migration struct {
	version int
	name    string
	body    string
}

func loadMigrations(src fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(src, "migrations")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("sqlmigrate: migration %q is not named <version>_<name>.sql", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("sqlmigrate: migration %q has a non-numeric version: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(src, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), body: string(body)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	for i := range out {
		if want := i + 1; out[i].version != want {
			return nil, fmt.Errorf(
				"sqlstore: migrations are not a gapless sequence from 1: found version %d where %d was expected (%s)",
				out[i].version, want, out[i].name)
		}
	}
	return out, nil
}

// migrate applies every migration the database has not seen, each in its own
// transaction. A partially applied migration would leave a schema no version
// number describes, which is the one state that cannot be recovered from
// automatically.
// Run applies every migration the database has not seen, each in its own
// transaction, recording what it did in the named table.
//
// table must be unique to this sequence. Two sequences sharing one table is the
// silent-skip described above, and nothing downstream can detect it.
func Run(ctx context.Context, db *sql.DB, d Dialect, table string) error {
	if !validTableName(table) {
		return fmt.Errorf("sqlmigrate: %q is not a usable table name", table)
	}
	// The table name is interpolated rather than bound, because a placeholder
	// cannot name a table on either engine. validTableName above is what makes
	// that safe, and it is why the name is checked rather than trusted.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+table+` (
		  version    INTEGER PRIMARY KEY,
		  name       TEXT NOT NULL,
		  applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("sqlmigrate: creating the migration table: %w", err)
	}

	var current int
	// COALESCE because MAX over no rows is NULL, and a fresh database is the
	// normal case rather than an error.
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM `+table).Scan(&current); err != nil {
		return fmt.Errorf("sqlmigrate: reading the schema version: %w", err)
	}

	all, err := loadMigrations(d.Migrations())
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.version <= current {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlmigrate: beginning migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, m.body); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlmigrate: applying migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			d.Rebind(`INSERT INTO `+table+` (version, name, applied_at) VALUES (?, ?, ?)`),
			m.version, m.name, time.Now().UTC().Format(timeLayout)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlmigrate: recording migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlmigrate: committing migration %s: %w", m.name, err)
		}
	}
	return nil
}

// validTableName keeps the interpolation above safe without pulling in a
// quoting library for two call sites. Letters, digits and underscore only, and
// it must start with a letter.
func validTableName(s string) bool {
	if s == "" || !isLetter(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isLetter(c) && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
