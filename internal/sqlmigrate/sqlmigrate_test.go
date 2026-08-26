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

package sqlmigrate_test

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"

	"github.com/damgahq/damga/internal/sqlmigrate"
)

// A dialect with no engine-specific behaviour, which is all this package needs.
// initSQL is the file name every fixture below uses; the sequences differ in
// what they create, never in what the first file is called.
const (
	initSQL = "0001_init.sql"
	// The table the fixtures create. Its content does not matter; what is under
	// test is whether it exists afterwards.
	createAlpha = `CREATE TABLE alpha (id TEXT PRIMARY KEY)`
)

type dialect struct{ files fs.FS }

func (dialect) Name() string           { return "test" }
func (dialect) Rebind(q string) string { return q }
func (d dialect) Migrations() fs.FS    { return d.files }

func seq(files map[string]string) dialect {
	m := fstest.MapFS{}
	for name, body := range files {
		m[filepath.Join("migrations", name)] = &fstest.MapFile{Data: []byte(body)}
	}
	return dialect{files: m}
}

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("checking for %s: %v", name, err)
	}
	return n > 0
}

// The case this package was extracted to make possible, and the failure it was
// extracted to prevent.
//
// Two schemas in one database — evidence and identity — each with a migration
// numbered 1. Sharing one version table means the second one's 1 is not greater
// than the first one's 1, so it is skipped: no error, no log line, the tables
// simply never exist, and the failure surfaces later as a missing table in
// whatever order the queries happen to run. Renumbering cannot dodge it,
// because the loader refuses a sequence that is not gapless from 1.
func TestTwoSequencesInOneDatabaseDoNotCollide(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	first := seq(map[string]string{initSQL: createAlpha})
	second := seq(map[string]string{initSQL: `CREATE TABLE beta (id TEXT PRIMARY KEY)`})

	if err := sqlmigrate.Run(ctx, db, first, "alpha_migration"); err != nil {
		t.Fatalf("first sequence: %v", err)
	}
	if err := sqlmigrate.Run(ctx, db, second, "beta_migration"); err != nil {
		t.Fatalf("second sequence: %v", err)
	}

	for _, name := range []string{"alpha", "beta"} {
		if !tableExists(t, db, name) {
			t.Errorf("table %q was never created; its sequence was skipped silently", name)
		}
	}

	// And the demonstration: sharing the table is exactly the skip. Asserted so
	// that if the runner ever starts erroring on it instead, this case is
	// updated deliberately rather than quietly passing for a new reason.
	other := open(t)
	if err := sqlmigrate.Run(ctx, other, first, "shared"); err != nil {
		t.Fatalf("first into shared: %v", err)
	}
	if err := sqlmigrate.Run(ctx, other, second, "shared"); err != nil {
		t.Fatalf("second into shared: %v", err)
	}
	if tableExists(t, other, "beta") {
		t.Error("sharing one version table no longer skips; the hazard this package names has changed")
	}
}

// Applied once, not once per start. The server calls this on every boot.
func TestRunIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := seq(map[string]string{initSQL: createAlpha})

	for range 3 {
		if err := sqlmigrate.Run(ctx, db, d, "alpha_migration"); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM alpha_migration`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("%d migrations recorded after three runs, want 1", n)
	}
}

// A later migration is applied to a database that already has the earlier one.
func TestRunAppliesOnlyWhatIsNew(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	one := seq(map[string]string{initSQL: createAlpha})
	if err := sqlmigrate.Run(ctx, db, one, "alpha_migration"); err != nil {
		t.Fatalf("first: %v", err)
	}

	two := seq(map[string]string{
		initSQL:         `CREATE TABLE alpha (id TEXT PRIMARY KEY)`,
		"0002_more.sql": `CREATE TABLE gamma (id TEXT PRIMARY KEY)`,
	})
	if err := sqlmigrate.Run(ctx, db, two, "alpha_migration"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if !tableExists(t, db, "gamma") {
		t.Error("the new migration was not applied")
	}
}

// A gap means somebody deleted or renamed a file, and applying what is left
// would produce a schema no version number describes.
func TestAGapIsRefused(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := seq(map[string]string{
		initSQL:         `CREATE TABLE alpha (id TEXT PRIMARY KEY)`,
		"0003_late.sql": `CREATE TABLE gamma (id TEXT PRIMARY KEY)`,
	})
	err := sqlmigrate.Run(ctx, db, d, "alpha_migration")
	if err == nil {
		t.Fatal("a sequence with a gap was accepted")
	}
	if !strings.Contains(err.Error(), "gapless") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// A migration that fails leaves nothing behind. A partially applied one would
// be a schema no version number describes, and it is the only state that cannot
// be recovered from automatically.
func TestAFailedMigrationIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	d := seq(map[string]string{
		initSQL: createAlpha + `; CREATE TABLE alpha (oops TEXT)`,
	})
	if err := sqlmigrate.Run(ctx, db, d, "alpha_migration"); err == nil {
		t.Fatal("a broken migration was accepted")
	}
	if tableExists(t, db, "alpha") {
		t.Error("the failed migration left its first statement behind; it was not in a transaction")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM alpha_migration`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("%d migrations recorded after a failure, want 0", n)
	}
}

// The table name is interpolated, not bound — no engine takes a placeholder
// there. So it is checked rather than trusted.
func TestATableNameIsCheckedNotTrusted(t *testing.T) {
	ctx := context.Background()
	d := seq(map[string]string{initSQL: createAlpha})

	for _, name := range []string{
		"", "1_leading_digit", "has-dash", "has space",
		`x"; DROP TABLE alpha; --`,
	} {
		if err := sqlmigrate.Run(ctx, open(t), d, name); err == nil {
			t.Errorf("the table name %q was accepted", name)
		}
	}
}
