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

// Package sqlstore is the evidence store's SQL implementation, written once and
// configured per engine.
//
// There are two engines and there has to be: SQLite is what a one-node install
// runs, and PostgreSQL is what a larger one wants, because SQLite has no
// roles and no REVOKE and therefore cannot enforce "we cannot modify evidence"
// — only promise it. Writing the logic twice would have meant auditing every
// write path against both engines forever, so the logic is written once and the
// places where the engines genuinely disagree are named in Dialect below.
//
// The list is short on purpose. If it grows past locking and placeholders,
// something is being done in SQL that belongs in Go.
package sqlstore

import (
	"io/fs"

	"github.com/damgahq/damga/internal/sqlmigrate"
)

// Dialect is everything the two engines do not agree about.
type Dialect interface {
	// Name appears in error messages.
	Name() string

	// Rebind turns the '?' placeholders every query in this package is written
	// with into whatever the driver expects. SQLite takes them as they are;
	// pgx wants $1, $2 and counts from one.
	Rebind(query string) string

	// LockRow is appended to the SELECT that a read-modify-write depends on.
	//
	// This is the difference that matters most and the one that would have been
	// found last. SQLite has a single write lock and the connection takes it
	// when the transaction begins (_txlock=immediate), so the read is already
	// serialised against every other writer and the clause is empty. PostgreSQL
	// runs READ COMMITTED, where two transactions read the same row, both
	// decide the state permits their transition, and both write — so it needs
	// FOR UPDATE, which SQLite cannot parse. Identical SQL, opposite outcomes,
	// and the symptom is a lost update rather than an error.
	LockRow() string

	// Migrations holds the numbered .sql files for this engine, in a directory
	// called "migrations". The schemas are the same shape; they are kept apart
	// because the engines have already diverged once (SQLite's STRICT) and
	// pretending otherwise would mean a template.
	Migrations() fs.FS
}

// The three methods above a migration runner needs are exactly
// internal/sqlmigrate.Dialect, so a Dialect satisfies it without adapting
// anything. Asserted here rather than left to the call site, because the
// runner is what a second schema will reuse.
var _ sqlmigrate.Dialect = Dialect(nil)
