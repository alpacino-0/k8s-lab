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

// Package postgres is the identity store for an installation with more than one
// node, and the one the paid tier requires — because it is the only engine that
// can be handed a role with INSERT and SELECT and neither UPDATE nor DELETE,
// enforced in a different process from the one making the claim.
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

	// Registers the "pgx" driver for database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/damgahq/damga/identity"
	"github.com/damgahq/damga/identity/internal/sqlstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

type dialect struct{}

func (dialect) Name() string      { return "identity/postgres" }
func (dialect) Migrations() fs.FS { return migrations }

// Rebind numbers the '?' placeholders the shared queries are written with. A
// scan rather than a Replace, because a '?' inside a quoted string is not a
// placeholder.
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

// Open connects to the database named by dsn. dsn is anything pgx accepts.
func Open(ctx context.Context, dsn string) (*sqlstore.Store, error) {
	if dsn == "" {
		return nil, errors.New("identity/postgres: no DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("identity/postgres: opening: %w", err)
	}
	store, err := sqlstore.New(ctx, db, dialect{})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return store, nil
}

var _ identity.Store = (*sqlstore.Store)(nil)
