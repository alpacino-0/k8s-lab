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

// Package postgres is the evidence store for an installation that has more
// than one node, and the only one the paid archive can be built on.
//
// The reason is not scale. SQLite has no roles and no REVOKE, so the process
// that writes evidence can always rewrite it: that engine can carry "we do not
// modify evidence" and never "we cannot". PostgreSQL can be handed a role with
// INSERT and SELECT and no UPDATE or DELETE, enforced in a different process
// from the one making the claim. That is the difference between a promise and
// a property, and it is what an auditor is buying.
//
// Nothing here enables that on its own — the grants are a deployment decision,
// and Retention still reports Immutable: false until something can observe
// them. What this package does is make them possible.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	// Registers the "pgx" driver for database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/internal/sqlstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

type dialect struct{}

func (dialect) Name() string { return "postgres" }

// Rebind numbers the '?' placeholders the shared queries are written with.
//
// It is a scan rather than a Replace because a '?' inside a quoted string is
// not a placeholder, and the store writes JSON into TEXT columns.
func (dialect) Rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n, inQuote := 0, false
	for i := 0; i < len(q); i++ {
		switch c := q[i]; {
		case c == '\'':
			inQuote = !inQuote
			b.WriteByte(c)
		case c == '?' && !inQuote:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// LockRow is what SQLite does not need and PostgreSQL cannot do without.
//
// READ COMMITTED lets two transactions read the same row, both decide its
// state permits their transition, and both write. The loser is not told: the
// symptom is a lost update, not an error, and the record ends up in whichever
// state was written second. SQLite has no equivalent because its single write
// lock is taken when the transaction begins.
func (dialect) LockRow() string { return " FOR UPDATE" }

func (dialect) Migrations() fs.FS { return migrations }

// Options configures Open. The zero value is usable.
type Options struct {
	// Window is how long a record that is not current is kept. Zero means
	// unbounded.
	Window time.Duration

	// MaxConns bounds each of the two pools. Zero leaves database/sql's
	// default.
	MaxConns int
}

// Open connects to the database named by dsn and applies any pending
// migrations. dsn is anything pgx accepts, including a postgres:// URL.
func Open(ctx context.Context, dsn string, opts Options) (*sqlstore.Store, error) {
	if dsn == "" {
		return nil, errors.New("postgres: no DSN")
	}

	w, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: opening: %w", err)
	}
	r, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("postgres: opening for reading: %w", err), w.Close())
	}
	if opts.MaxConns > 0 {
		w.SetMaxOpenConns(opts.MaxConns)
		r.SetMaxOpenConns(opts.MaxConns)
	}

	store, err := sqlstore.New(ctx, w, r, dialect{}, opts.Window)
	if err != nil {
		return nil, errors.Join(err, w.Close(), r.Close())
	}
	return store, nil
}

var _ evidence.Store = (*sqlstore.Store)(nil)
