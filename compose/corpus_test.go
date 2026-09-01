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

package compose_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damgahq/damga/compose"
)

// corpusDir is the vendored catalogue, read from where the licence note sits
// beside it. testdata next door is three of the same files, kept small enough
// that a case can quote one in full; this is the whole set, which is what a
// count has to be taken over.
const corpusDir = "../catalog/templates"

// The counts the doc comments on Generated and Result quote, computed.
//
// They were written down and not computed, and that is the only thing that was
// wrong with them. "111 of the 369 templates that parse share a source between
// services" was true and is still true — but of a set this package has since
// split in two, and the comment carrying it sits on the half that is now the
// smaller one. Nothing computed either number, so nothing said so.
//
// A source shared between two services means one value minted for both. A
// source shared with a service that became a Database means no value minted at
// all: the Database publishes its own credentials, and a second value under the
// same name is a password the application holds and the server has never heard
// of. Generated.Sources is the first case, DatabaseSources the second, and 111
// is their sum.
//
// Nothing here is asserted beyond the counts existing. Upstream adds templates
// weekly and a gate that fails when somebody else publishes an application is a
// gate that gets deleted rather than read; what these numbers are for is the
// comments, and a comment is worth checking when somebody reads the log.
func TestTheCorpusCountsBehindTheDocComments(t *testing.T) {
	var (
		parsed       int
		sharedByGen  int
		sharedWithDB int
		wrapped      int
		withPostgres int
	)
	for _, name := range corpusFiles(t) {
		body, err := os.ReadFile(filepath.Join(corpusDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
		tpl, err := compose.Parse(stem, body)
		if err != nil {
			// Two files in the corpus are invalid YAML upstream — an anchor
			// that is a sequence where `<<:` wants a mapping. They are the
			// difference between 371 files and 369 that parse.
			continue
		}
		parsed++

		res, err := compose.Convert(tpl, compose.Options{Namespace: "t"})
		if err != nil {
			continue
		}
		services := map[string]map[string]bool{}
		builtAround := false
		for _, g := range res.Generated {
			for _, s := range g.Sources {
				if services[s.Name] == nil {
					services[s.Name] = map[string]bool{}
				}
				services[s.Name][g.Service] = true
			}
			// A value built around a credential rather than being one: a
			// connection string is a user and a password inside a URL.
			//
			// A flag and not a break. Breaking here left the loop before it had
			// finished filling services, and the two counts below came out 45
			// and 105 instead of 64 and 111 — a wrong number produced by the
			// code that exists to stop wrong numbers.
			if len(g.Sources) > 1 || (len(g.Sources) == 1 && g.Value != "${"+g.Sources[0].Name+"}") {
				builtAround = true
			}
		}
		if builtAround {
			wrapped++
		}

		betweenServices := false
		for _, in := range services {
			if len(in) > 1 {
				betweenServices = true
				break
			}
		}
		withDatabase := false
		for _, d := range res.DatabaseSources {
			if _, ok := services[d.Name]; ok {
				withDatabase = true
				break
			}
		}
		if betweenServices {
			sharedByGen++
		}
		if betweenServices || withDatabase {
			sharedWithDB++
		}
		if len(res.Databases) > 0 {
			withPostgres++
		}
	}

	if parsed == 0 {
		t.Fatal("no template parsed, so every count below is zero for the wrong reason")
	}
	t.Logf("of %d templates that parse: %d share a source between two services, "+
		"%d share one once a service that became a Database is counted too",
		parsed, sharedByGen, sharedWithDB)
	t.Logf("%d build a value around a credential rather than being one; "+
		"%d convert a service into a Database", wrapped, withPostgres)
}

func corpusFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml") {
			out = append(out, e.Name())
		}
	}
	return out
}
