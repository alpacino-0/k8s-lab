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

// Package catalog is the list of applications that install with one click, and
// the plan that installing one produces.
//
// The compose package turns one file into objects. This package answers the
// three questions that stand between that and a catalogue: where the files come
// from, which of them a user is offered, and where the values the template asks
// the platform to invent are supposed to end up.
//
// # Where the files come from
//
// An fs.FS, and nothing narrower. The templates are not in this repository and
// the decision about where they should live — built into the binary, mounted
// from a ConfigMap, pulled as an OCI artifact — changes how they are updated
// and how large the image is, but it does not change any code here. Load takes
// whichever filesystem that decision produces.
//
// # What it will not do
//
// It does not mint credentials. compose.Result.Generated is a request for a
// value and this package passes the request on with the one thing a minter
// needs and cannot recover on its own: which requests are the same value. It
// never chooses one. A catalogue that invented a password would be choosing
// where that password lives, and a value produced here would already have been
// through this process's memory, its logs and whatever committed the plan.
//
// # What it says no to
//
// A plan carries Blockers as well as Notes, and the difference is deliberate. A
// Note is something a person should read. A Blocker is a fact about the objects
// — an image the API refuses, a value nothing can supply — that means installing
// this entry produces something that does not run. Offering an entry that
// cannot be installed is the failure this type exists to prevent, so Blockers
// are computed rather than left for the caller to notice.
package catalog

import (
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/damgahq/damga/compose"
)

// Entry is one application on the catalogue page.
type Entry struct {
	// Name is the identifier and the filename without its extension.
	Name string

	Slogan        string
	Documentation string
	Category      string
	Tags          []string
	Logo          string

	// Services is how many containers the entry runs. On the page because
	// "this is one container" and "this is six containers and a database" are
	// different decisions and the user is making one of them.
	Services int

	template compose.Template
}

// Skip is one file that was read and not offered.
//
// Kept rather than discarded. A catalogue that quietly drops what it could not
// read looks identical to one whose source is half empty, and the difference is
// the whole question of whether the mount is right.
type Skip struct {
	Name   string
	Reason string
}

// Catalog is a loaded set of entries.
type Catalog struct {
	entries []Entry
	byName  map[string]int

	// Skipped is every file that was read and not offered, in name order.
	Skipped []Skip
}

// Load reads every compose file at the root of fsys.
//
// Both extensions, because the upstream corpus uses both: 368 .yaml and 3 .yml.
// Only the root, because a catalogue is a flat list and a nested layout would be
// a second convention nothing has asked for yet.
//
// An empty result is an error rather than an empty catalogue. Every way of
// getting the files here wrong — the wrong subdirectory in a ConfigMap, a
// volume that did not mount, an embed pattern that matched nothing — produces
// exactly this, and it produces it silently: the panel shows no applications
// and nothing in the logs says why.
func Load(fsys fs.FS) (*Catalog, error) {
	names, err := fs.Glob(fsys, "*.y*ml")
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the template directory: %w", err)
	}
	slices.Sort(names)

	c := &Catalog{byName: map[string]int{}}
	for _, file := range names {
		ext := path.Ext(file)
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		name := strings.TrimSuffix(file, ext)

		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			c.Skipped = append(c.Skipped, Skip{name, err.Error()})
			continue
		}
		tpl, err := compose.Parse(name, data)
		if err != nil {
			c.Skipped = append(c.Skipped, Skip{name, err.Error()})
			continue
		}
		if tpl.Ignore {
			c.Skipped = append(c.Skipped, Skip{name, "withdrawn by the template's own header"})
			continue
		}
		if _, taken := c.byName[name]; taken {
			// foo.yaml and foo.yml in one directory. Whichever sorted first
			// wins, and the other is reported rather than shadowing it.
			c.Skipped = append(c.Skipped, Skip{name, "a template of this name was already loaded"})
			continue
		}
		c.byName[name] = len(c.entries)
		c.entries = append(c.entries, Entry{
			Name:          name,
			Slogan:        tpl.Slogan,
			Documentation: tpl.Documentation,
			Category:      tpl.Category,
			Tags:          tpl.Tags,
			Logo:          tpl.Logo,
			Services:      len(tpl.Services),
			template:      tpl,
		})
	}
	if len(c.entries) == 0 {
		return nil, fmt.Errorf("catalog: no templates found; %d files were read and none could be offered", len(c.Skipped))
	}
	return c, nil
}

// Entries is every offered entry, in name order.
func (c *Catalog) Entries() []Entry {
	return slices.Clone(c.entries)
}

// Get returns one entry by name.
func (c *Catalog) Get(name string) (Entry, bool) {
	i, ok := c.byName[name]
	if !ok {
		return Entry{}, false
	}
	return c.entries[i], true
}

// Categories is every category that has at least one entry, in name order.
//
// Derived rather than declared. A fixed list drifts from the corpus the moment
// upstream adds a category, and the drift shows up as entries that exist and
// cannot be reached from any filter.
func (c *Catalog) Categories() []string {
	return c.distinct(func(e Entry) []string {
		if e.Category == "" {
			return nil
		}
		return []string{e.Category}
	})
}

// Tags is every tag in use, in name order.
func (c *Catalog) Tags() []string {
	return c.distinct(func(e Entry) []string { return e.Tags })
}

func (c *Catalog) distinct(of func(Entry) []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range c.entries {
		for _, v := range of(e) {
			if v = strings.TrimSpace(v); v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Query is how a user narrows the list.
//
// Every field that is set has to match. An empty Query matches everything,
// which is what the page shows before anybody types.
type Query struct {
	// Text is matched case-insensitively against the name, the slogan and the
	// tags. Against the slogan because that is where the words a user actually
	// searches for live: nothing in "plausible" says analytics.
	Text string

	Category string

	// Tags must all be present. Two tags narrow rather than widen — the
	// opposite convention exists and produces a filter that returns more
	// results the more of it you use.
	Tags []string
}

// Find returns the entries matching q, in name order.
func (c *Catalog) Find(q Query) []Entry {
	text := strings.ToLower(strings.TrimSpace(q.Text))
	var out []Entry
	for _, e := range c.entries {
		if q.Category != "" && !strings.EqualFold(e.Category, q.Category) {
			continue
		}
		if !hasAll(e.Tags, q.Tags) {
			continue
		}
		if text != "" && !matches(e, text) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func matches(e Entry, text string) bool {
	if strings.Contains(strings.ToLower(e.Name), text) ||
		strings.Contains(strings.ToLower(e.Slogan), text) {
		return true
	}
	for _, tag := range e.Tags {
		if strings.Contains(strings.ToLower(tag), text) {
			return true
		}
	}
	return false
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, w) }) {
			return false
		}
	}
	return true
}
