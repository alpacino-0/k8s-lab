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

	"github.com/damgahq/damga/placement"
	"github.com/damgahq/damga/placement/postgres"
	"github.com/damgahq/damga/placement/storetest"
)

const dsnEnv = "DAMGA_TEST_POSTGRES_DSN"

// The same suite again. Skipped without a database so a laptop run stays
// useful; CI sets the variable, which is what stops the skip becoming
// permanent.
func TestConformance(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run the PostgreSQL conformance suite", dsnEnv)
	}
	storetest.Run(t, func(t *testing.T) placement.Store {
		// A schema per case: the suite assumes it owns an empty store, and
		// dropping a schema cannot fail because somebody still holds a
		// connection to a database.
		schema := fmt.Sprintf("placement_%d", time.Now().UnixNano())
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatalf("CREATE SCHEMA: %v", err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
				t.Errorf("DROP SCHEMA: %v", err)
			}
			if err := admin.Close(); err != nil {
				t.Errorf("closing the admin connection: %v", err)
			}
		})

		scoped, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parsing the DSN: %v", err)
		}
		q := scoped.Query()
		q.Set("search_path", schema)
		scoped.RawQuery = q.Encode()

		s, err := postgres.Open(context.Background(), scoped.String())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
