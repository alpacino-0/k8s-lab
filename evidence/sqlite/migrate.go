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

package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrations embed.FS

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

func load() ([]migration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
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
			return nil, fmt.Errorf("sqlite: migration %q is not named <version>_<name>.sql", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("sqlite: migration %q has a non-numeric version: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(migrations, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), body: string(body)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	for i := range out {
		if want := i + 1; out[i].version != want {
			return nil, fmt.Errorf(
				"sqlite: migrations are not a gapless sequence from 1: found version %d where %d was expected (%s)",
				out[i].version, want, out[i].name)
		}
	}
	return out, nil
}

// migrate applies every migration the database has not seen, each in its own
// transaction. A partially applied migration would leave a schema no version
// number describes, which is the one state that cannot be recovered from
// automatically.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migration (
		  version    INTEGER PRIMARY KEY,
		  name       TEXT NOT NULL,
		  applied_at TEXT NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("sqlite: creating the migration table: %w", err)
	}

	var current int
	// COALESCE because MAX over no rows is NULL, and a fresh database is the
	// normal case rather than an error.
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migration`).Scan(&current); err != nil {
		return fmt.Errorf("sqlite: reading the schema version: %w", err)
	}

	all, err := load()
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.version <= current {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite: beginning migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, m.body); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: applying migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migration (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, nowText()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: recording migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: committing migration %s: %w", m.name, err)
		}
	}
	return nil
}
