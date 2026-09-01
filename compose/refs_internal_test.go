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

// In package compose rather than compose_test because the invariant below has
// to ask the same question the code asks. Written against a copy of
// siblingHost it would pass while the real one had stopped matching anything —
// a test agreeing with itself.
package compose

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// corpusDirInternal is the vendored catalogue, from where the licence note sits
// beside it.
const corpusDirInternal = "../catalog/templates"

// converted is every corpus template that parses, converted.
type converted struct {
	name   string
	tpl    Template
	result Result
}

func convertCorpus(t *testing.T) []converted {
	t.Helper()
	entries, err := os.ReadDir(corpusDirInternal)
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var out []converted
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(corpusDirInternal, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".yaml"), ".yml")
		tpl, err := Parse(stem, body)
		if err != nil {
			continue
		}
		res, err := Convert(tpl, Options{Namespace: "t"})
		if err != nil {
			continue
		}
		out = append(out, converted{name: stem, tpl: tpl, result: res})
	}
	if len(out) == 0 {
		t.Fatal("nothing converted, so every case below is vacuous")
	}
	return out
}

// Nothing addresses a sibling by its compose name any more, except the one case
// that cannot be fixed here — and that one is always said out loud.
//
// This is the invariant, and it is the shape of assertion a count cannot
// replace. Measured on a real cluster on 2026-09-01: an entry whose second
// workload is addressed by its compose name installs, reports itself
// installable, and does not work — cryptgeon logs "cannot reach redis",
// grafana-with-postgresql logs "lookup postgresql ... no such host". Neither
// shows up in any total; both show up here.
//
// The exception is a reference to the template's own primary service. That
// workload is renamed again at install time, to the app name the person
// choosing it typed, and this package is not told what that will be. Leaving
// the compose name and saying so is honest; writing a name that also resolves
// to nothing would not be. So the assertion is not "no compose name survives"
// but the exact thing that is true: every one that survives is the primary, and
// every one of them is named in a note.
func TestNoServiceStillAddressesASiblingSilently(t *testing.T) {
	for _, c := range convertCorpus(t) {
		// Sorted, because primaryService falls back to sorted[0] and Convert
		// hands it a sorted list. Handing it map order instead made this case
		// report seven services as addressing a non-primary sibling when they
		// were addressing the primary — a test that computed the same thing a
		// different way and disagreed with the code about the answer.
		names := make([]string, 0, len(c.tpl.Services))
		for n := range c.tpl.Services {
			names = append(names, n)
		}
		slices.Sort(names)
		primary := primaryService(c.tpl, names)

		// Every note this template produced, so a leftover can be checked
		// against what the user is told rather than against nothing.
		var notes strings.Builder
		for _, n := range c.result.Notes {
			notes.WriteString(n.String())
			notes.WriteByte('\n')
		}

		for _, w := range c.result.Workloads {
			self := w.Annotations[ServiceAnnotation]
			for _, e := range w.Spec.Env {
				checkNoSilentSibling(t, c, self, primary, e.Name, e.Value, notes.String())
			}
		}
		for _, g := range c.result.Generated {
			checkNoSilentSibling(t, c, g.Service, primary, g.Key, g.Value, notes.String())
		}
	}
}

func checkNoSilentSibling(t *testing.T, c converted, self, primary, field, value, notes string) {
	t.Helper()
	for _, m := range siblingHost.FindAllStringSubmatch(value, -1) {
		host := m[1]
		if host == "" {
			host = m[2]
		}
		if host == "" || host == self {
			continue
		}
		if _, isSibling := c.tpl.Services[host]; !isSibling {
			continue
		}
		if host != primary {
			t.Errorf("%s: %s.%s still addresses %q, a sibling that is not the application; "+
				"it should have been repointed at %s and the value is %q",
				c.name, self, field, host, objectName(c.tpl.Name+"-"+host), value)
			continue
		}
		// The primary, which cannot be repointed here. It has to be said.
		if !strings.Contains(notes, host) {
			t.Errorf("%s: %s.%s addresses the application as %q and no note says so, "+
				"so the entry installs and does not work with nothing explaining why",
				c.name, self, field, host)
		}
	}
}

// How much of the corpus this moved, counted and not asserted.
//
// The counts are here because the change they measure is invisible in every
// other total: an entry that addresses a sibling by its compose name is
// installable before and after, and the difference is only whether it works.
func TestHowManySiblingAddressesWereRepointed(t *testing.T) {
	var (
		multi                         int
		withRefs, repointed, leftAsIs int
		refsRepointed, refsLeft       int
	)
	for _, c := range convertCorpus(t) {
		if len(c.tpl.Services) > 1 {
			multi++
		}
		fixed, left := 0, 0
		for _, n := range c.result.Notes {
			switch {
			case strings.Contains(n.Detail, "has been repointed at"):
				fixed++
			case strings.Contains(n.Detail, "left as compose wrote it"):
				left++
			}
		}
		if fixed+left > 0 {
			withRefs++
		}
		if fixed > 0 {
			repointed++
			refsRepointed += fixed
		}
		if left > 0 {
			leftAsIs++
			refsLeft += left
		}
	}
	t.Logf("of %d multi-service templates, %d address a sibling by its compose name",
		multi, withRefs)
	t.Logf("%d entries had %d references repointed; %d entries keep %d that name the "+
		"application itself, which is renamed at install time",
		repointed, refsRepointed, leftAsIs, refsLeft)
}
