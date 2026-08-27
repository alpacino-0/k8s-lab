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

// Package postgres is forge.Store on pgx: what the paid archive requires
// and what a second node makes necessary.
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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/damgahq/damga/forge"
	"github.com/damgahq/damga/forge/internal/sqlstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

type dialect struct{}

func (dialect) Name() string      { return "forge/postgres" }
func (dialect) Migrations() fs.FS { return migrations }

// Rebind turns the ? the queries are written with into $1, $2, …
func (dialect) Rebind(q string) string {
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Open connects and applies any pending migrations.
func Open(ctx context.Context, dsn string) (*sqlstore.Store, error) {
	if dsn == "" {
		return nil, errors.New("forge/postgres: no DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("forge/postgres: opening: %w", err)
	}
	store, err := sqlstore.New(ctx, db, dialect{})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return store, nil
}

var _ forge.Store = (*sqlstore.Store)(nil)
