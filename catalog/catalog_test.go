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

package catalog_test

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damgahq/damga/catalog"
)

// corpus is the catalogue this repository ships, all 371 files of it, read from
// where they already are rather than copied.
//
// It read ../compose/testdata until 2026-09-01, three files kept for the
// compose package's own parser tests, and the comment here said there was one
// copy in the repository and that a second would be a thing to keep in step
// whose disagreement would be invisible. Both halves stopped being true when
// the templates were vendored: there are two copies of n8n.yaml — byte-identical
// today, which is exactly what an invisible disagreement looks like before it
// happens — and this package was reading the one without the licence note.
//
// So the catalogue reads the catalogue. compose/testdata stays where it is and
// keeps its own readers in compose/compose_test.go, which want three files it
// can quote in full rather than 371 it cannot.
func corpus(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load(os.DirFS("templates"))
	if err != nil {
		t.Fatalf("loading the shipped catalogue: %v", err)
	}
	return c
}

// Upstream withdraws a template with a header comment. Offering those is
// offering applications whose own authors took them down — and plausible, which
// is one of them, converts into an entry that cannot work.
//
// How many carry it is logged rather than written down here. It was a comment
// saying "28 of the 371 files" while this ran against three, which is the shape
// of a number that has stopped being checkable: nothing computed it, nothing
// would notice it going stale, and the only reader who could tell was the one
// who went and counted.
func TestAWithdrawnTemplateIsNotOffered(t *testing.T) {
	c := corpus(t)

	withdrawn := 0
	for _, s := range c.Skipped {
		if strings.Contains(s.Reason, "withdrawn") {
			withdrawn++
		}
	}
	if withdrawn == 0 {
		t.Error("no template was skipped as withdrawn, so the header is not being read at all")
	}
	t.Logf("%d of the %d files carry the withdrawal header",
		withdrawn, len(c.Entries())+len(c.Skipped))

	if _, ok := c.Get("plausible"); ok {
		t.Error("plausible carries `# ignore: true` and was offered anyway")
	}
	if _, ok := c.Get("n8n"); !ok {
		t.Error("n8n does not carry the header and was not offered")
	}
	var reason string
	for _, s := range c.Skipped {
		if s.Name == "plausible" {
			reason = s.Reason
		}
	}
	if !strings.Contains(reason, "withdrawn") {
		t.Errorf("plausible was dropped with reason %q; a skip nobody can see is a half-empty mount", reason)
	}
}

// Every way of pointing this at the wrong place produces the same thing: no
// files, no error, and a page with nothing on it.
func TestAnEmptyCatalogueIsAnError(t *testing.T) {
	_, err := catalog.Load(fstest.MapFS{
		"README.md":              {Data: []byte("# not a template")},
		"nested/thing.yaml":      {Data: []byte("services:\n  a:\n    image: x:1\n")},
		"withdrawn.yaml":         {Data: []byte("# ignore: true\nservices:\n  a:\n    image: x:1\n")},
		"broken.yaml":            {Data: []byte("services: [")},
		"alsonotatemplate.txt":   {Data: []byte("x")},
		"nested/deeper/one.yaml": {Data: []byte("services:\n  a:\n    image: x:1\n")},
	})
	if err == nil {
		t.Fatal("an empty catalogue loaded successfully")
	}
	if !strings.Contains(err.Error(), "no templates") {
		t.Errorf("error = %q, want it to say the catalogue is empty", err)
	}
}

// 368 of the 371 upstream files are .yaml and 3 are .yml. Reading one extension
// loses three applications and says nothing.
func TestBothExtensionsAreRead(t *testing.T) {
	c, err := catalog.Load(fstest.MapFS{
		"long.yaml":  {Data: []byte("# category: tools\nservices:\n  a:\n    image: x:1\n")},
		"short.yml":  {Data: []byte("# category: tools\nservices:\n  a:\n    image: x:1\n")},
		"README.md":  {Data: []byte("#")},
		"logo.svg":   {Data: []byte("<svg/>")},
		"a.yamlish":  {Data: []byte("services:\n  a:\n    image: x:1\n")},
		"b.yml.bak":  {Data: []byte("services:\n  a:\n    image: x:1\n")},
		"c.yamlfile": {Data: []byte("services:\n  a:\n    image: x:1\n")},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(c.Entries()); got != 2 {
		names := make([]string, 0, len(c.Entries()))
		for _, e := range c.Entries() {
			names = append(names, e.Name)
		}
		t.Errorf("entries = %d %v, want the .yaml and the .yml and nothing else", got, names)
	}
}

// A file that does not parse is skipped and reported. Two of the 371 upstream
// templates anchor a sequence and merge it into a mapping, which no conforming
// YAML parser accepts.
func TestAFileThatDoesNotParseIsReportedRatherThanDropped(t *testing.T) {
	c, err := catalog.Load(fstest.MapFS{
		"good.yaml": {Data: []byte("services:\n  a:\n    image: x:1\n")},
		"bad.yaml": {Data: []byte("x-env: &env\n  - A=1\nservices:\n  a:\n    image: x:1\n" +
			"    environment:\n      <<: *env\n")},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Entries()) != 1 {
		t.Fatalf("entries = %d, want the one that parses", len(c.Entries()))
	}
	if len(c.Skipped) != 1 || c.Skipped[0].Name != "bad" {
		t.Errorf("skipped = %+v, want the file that did not parse", c.Skipped)
	}
}

// The three fields a catalogue page is built from, and the reason the parser
// reads each file twice: none of them is anywhere a YAML parser looks.
func TestTheEntryCarriesWhatThePageShows(t *testing.T) {
	c := corpus(t)

	e, ok := c.Get("n8n")
	if !ok {
		t.Fatal("n8n is missing")
	}
	if e.Slogan == "" || e.Category == "" || e.Logo == "" {
		t.Errorf("entry = %+v, want the header metadata", e)
	}
	if e.Services != 2 {
		t.Errorf("services = %d, want the two n8n runs", e.Services)
	}
}

// Named rather than repeated so that the table below reads as three
// applications and not as three strings.
const (
	automate    = "automate"
	measure     = "measure"
	orchestrate = "orchestrate"
)

func TestSelectionNarrows(t *testing.T) {
	c, err := catalog.Load(fstest.MapFS{
		automate + ".yaml": {Data: []byte("# slogan: an extendable workflow automation tool\n" +
			"# category: automation\n# tags: workflow, low-code\nservices:\n  a:\n    image: x:1\n")},
		measure + ".yaml": {Data: []byte("# slogan: a privacy friendly analytics package\n" +
			"# category: analytics\n# tags: privacy\nservices:\n  a:\n    image: x:1\n")},
		orchestrate + ".yaml": {Data: []byte("# slogan: another workflow runner\n" +
			"# category: automation\n# tags: workflow\nservices:\n  a:\n    image: x:1\n")},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, tc := range []struct {
		name  string
		query catalog.Query
		want  []string
	}{
		{"everything", catalog.Query{}, []string{automate, measure, orchestrate}},
		{"by category", catalog.Query{Category: "automation"}, []string{automate, orchestrate}},
		{"category is case-insensitive", catalog.Query{Category: "Automation"}, []string{automate, orchestrate}},
		{"by name", catalog.Query{Text: "orch"}, []string{orchestrate}},
		// The word a user types is rarely in the name. Nothing in "measure"
		// says analytics; its slogan does.
		{"by slogan", catalog.Query{Text: "analytics"}, []string{measure}},
		{"by tag", catalog.Query{Tags: []string{"workflow"}}, []string{automate, orchestrate}},
		// Two tags narrow. The other convention returns more results the more
		// of the filter you use, which reads as the filter being broken.
		{"two tags narrow", catalog.Query{Tags: []string{"workflow", "low-code"}}, []string{automate}},
		{"fields combine", catalog.Query{Category: "automation", Text: "another"}, []string{orchestrate}},
		{"no match", catalog.Query{Text: "postgres"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make([]string, 0, len(c.Entries()))
			for _, e := range c.Find(tc.query) {
				got = append(got, e.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Find(%+v) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}

	if got := strings.Join(c.Categories(), ","); got != "analytics,automation" {
		t.Errorf("categories = %q", got)
	}
	if got := strings.Join(c.Tags(), ","); got != "low-code,privacy,workflow" {
		t.Errorf("tags = %q", got)
	}
}
