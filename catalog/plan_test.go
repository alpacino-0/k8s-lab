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
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damgahq/damga/catalog"
	"github.com/damgahq/damga/compose"
)

func planOf(t *testing.T, c *catalog.Catalog, name string, o catalog.Options) catalog.Plan {
	t.Helper()
	if o.Namespace == "" {
		o.Namespace = testNamespace
	}
	p, err := c.Plan(name, o)
	if err != nil {
		t.Fatalf("Plan(%s): %v", name, err)
	}
	return p
}

func loadOne(t *testing.T, name, body string) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load(fstest.MapFS{name + ".yaml": {Data: []byte(body)}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// The identity of a value is its source, not the key it lands under, and not
// the service that asks. n8n's two containers agree on a shared token: mint one
// per key and the task runner authenticates against the broker with a secret
// the broker has never seen.
//
// 111 of the 369 templates that parse share a source between two services.
func TestOneValueIsMintedPerSourceNotPerKey(t *testing.T) {
	p := planOf(t, corpus(t), "n8n", catalog.Options{})

	if len(p.Secrets) != 2 {
		t.Fatalf("secrets = %d, want one per service that asks for something", len(p.Secrets))
	}
	var templates []string
	for _, s := range p.Secrets {
		for _, k := range s.Keys {
			if k.Name == "N8N_RUNNERS_AUTH_TOKEN" {
				templates = append(templates, s.Name+":"+k.Template)
			}
		}
	}
	if len(templates) != 2 {
		t.Fatalf("the shared token appears %d times, want once in each service's secret: %v",
			len(templates), templates)
	}
	_, first, _ := strings.Cut(templates[0], ":")
	_, second, _ := strings.Cut(templates[1], ":")
	if first != second {
		t.Errorf("%q and %q; the broker and its runner would be given different tokens",
			templates[0], templates[1])
	}

	minted := 0
	for _, s := range p.Mint {
		if s.Name == "SERVICE_PASSWORD_N8N" {
			minted++
		}
	}
	if minted != 1 {
		t.Errorf("SERVICE_PASSWORD_N8N is listed %d times in Mint, want once", minted)
	}
}

var placeholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// The plan is only complete if everything left to substitute is something the
// plan also says to produce. A placeholder with no source behind it reaches the
// container as the literal text ${SERVICE_PASSWORD_X}, which is a deployment
// that starts and whose password is printable.
func TestEveryPlaceholderNamesAValueThatWillBeMinted(t *testing.T) {
	p := planOf(t, corpus(t), "n8n", catalog.Options{})

	if len(p.Secrets) == 0 {
		t.Fatal("nothing to check; n8n asks for a token")
	}
	for _, s := range p.Secrets {
		for _, k := range s.Keys {
			for _, m := range placeholder.FindAllStringSubmatch(k.Template, -1) {
				if !slices.ContainsFunc(k.Sources, func(src compose.Source) bool { return src.Name == m[1] }) {
					t.Errorf("%s/%s: %q leaves %s behind and does not name it as a source",
						s.Name, k.Name, k.Template, m[1])
				}
				if !slices.ContainsFunc(p.Mint, func(src compose.Source) bool { return src.Name == m[1] }) {
					t.Errorf("%s/%s: %q leaves %s behind and nothing will mint it",
						s.Name, k.Name, k.Template, m[1])
				}
			}
		}
	}
}

// One Secret per workload, not one per install. Two services in one template
// can want the same variable name with different values, and a single Secret
// has one key of that name — the install succeeds and one of the two silently
// receives the other's value.
func TestAWorkloadReadsOnlyItsOwnSecret(t *testing.T) {
	p := planOf(t, corpus(t), "n8n", catalog.Options{})

	seen := map[string]bool{}
	for _, w := range p.Workloads {
		if len(w.Spec.EnvFrom) != 1 {
			t.Fatalf("%s reads %v, want exactly its own secret", w.Name, w.Spec.EnvFrom)
		}
		name := w.Spec.EnvFrom[0]
		if name != w.Name+catalog.SecretSuffix {
			t.Errorf("%s reads %q, want %q", w.Name, name, w.Name+catalog.SecretSuffix)
		}
		if seen[name] {
			t.Errorf("%s is shared between workloads", name)
		}
		seen[name] = true
	}
	for _, s := range p.Secrets {
		if s.Namespace != p.Namespace {
			t.Errorf("%s is in %q, want %q", s.Name, s.Namespace, p.Namespace)
		}
	}
}

// This package does not mint, and the one place a value could sneak in is a
// default written next to the request. `${SERVICE_PASSWORD_X:-hunter2}` is a
// credential published in a public repository; using it produces an install
// that starts, serves, and is open.
func TestNoValueIsInvented(t *testing.T) {
	c := loadOne(t, "defaulted", `
services:
  app:
    image: example/app:1.0
    environment:
      - TOKEN=${SERVICE_PASSWORD_APP:-hunter2}
`)
	p := planOf(t, c, "defaulted", catalog.Options{})

	if len(p.Mint) != 1 || p.Mint[0].Name != "SERVICE_PASSWORD_APP" {
		t.Fatalf("mint = %+v, want the one value to produce", p.Mint)
	}
	for _, s := range p.Secrets {
		for _, k := range s.Keys {
			if strings.Contains(k.Template, "hunter2") {
				t.Errorf("%s/%s = %q carries the published default", s.Name, k.Name, k.Template)
			}
			if k.Template != "${SERVICE_PASSWORD_APP}" {
				t.Errorf("%s/%s = %q, want the request and nothing else", s.Name, k.Name, k.Template)
			}
		}
	}
	for _, w := range p.Workloads {
		for _, e := range w.Spec.Env {
			if e.Name == "TOKEN" {
				t.Errorf("TOKEN reached the workload as %q, where anyone who can read it can read it", e.Value)
			}
		}
	}
}

// The credentials a Database publishes are the Database's. Minting a second
// value under the same name hands the application a password the server does
// not have, and the failure looks like the application is misconfigured.
func TestCredentialsTheDatabaseOwnsAreNotMinted(t *testing.T) {
	c := loadOne(t, "withdb", `
# port: 8000
services:
  app:
    image: example/app:1.0
    environment:
      - DATABASE_URL=postgres://${SERVICE_USER_PG}:${SERVICE_PASSWORD_PG}@db:5432/app
  db:
    image: postgres:16-alpine
    environment:
      - POSTGRES_USER=${SERVICE_USER_PG}
      - POSTGRES_PASSWORD=${SERVICE_PASSWORD_PG}
`)
	p := planOf(t, c, "withdb", catalog.Options{})

	for _, s := range p.Mint {
		if s.Name == "SERVICE_PASSWORD_PG" || s.Name == "SERVICE_USER_PG" {
			t.Errorf("%s would be minted a second time; the database already published it", s.Name)
		}
	}
	if p.Installable() {
		t.Fatal("the entry is offered as installable and the application cannot connect")
	}
	var said string
	for _, b := range p.Blockers {
		if b.Field == "DATABASE_URL" {
			said = b.String()
		}
	}
	if !strings.Contains(said, "SERVICE_USER_PG") || !strings.Contains(said, "SERVICE_PASSWORD_PG") {
		t.Errorf("blocker = %q, want it to name both values it cannot supply", said)
	}
	// The database itself still converts, or the note would be advice about
	// nothing.
	if len(p.Databases) != 1 {
		t.Errorf("databases = %d, want the one postgres service", len(p.Databases))
	}
}

// The API refuses an image with no tag and refuses :latest, because a rollback
// to a tag that moved restores something other than what was rolled back from.
// An entry that is offered, clicked and then rejected at apply time fails where
// nobody is looking — and the catalogue knew before the click.
//
// 193 of the 341 offered upstream templates name at least one such image.
func TestAnImageTheAPIRefusesBlocksTheEntry(t *testing.T) {
	for _, tc := range []struct{ name, image, want string }{
		{"no tag", "example/app", "carries no tag or digest"},
		{"latest", "example/app:latest", "uses the :latest tag"},
		{"a registry port is not a tag", "registry.local:5000/team/app", "carries no tag or digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := loadOne(t, "e", "services:\n  app:\n    image: "+tc.image+"\n")
			p := planOf(t, c, "e", catalog.Options{})

			if p.Installable() {
				t.Fatalf("%s was offered as installable", tc.image)
			}
			if !strings.Contains(p.Blockers[0].String(), tc.want) {
				t.Errorf("blocker = %q, want it to say %q", p.Blockers[0], tc.want)
			}
		})
	}

	t.Run("a tag is enough", func(t *testing.T) {
		c := loadOne(t, "e", "services:\n  app:\n    image: example/app:1.2.3\n")
		if p := planOf(t, c, "e", catalog.Options{}); !p.Installable() {
			t.Errorf("blockers = %v", p.Blockers)
		}
	})
}

// The seam that takes the offered entries that install from 119 to 280.
func TestPinTurnsARefusedImageIntoAnInstallableOne(t *testing.T) {
	c := loadOne(t, "e", "services:\n  app:\n    image: example/app:latest\n")

	asked := 0
	p := planOf(t, c, "e", catalog.Options{Pin: func(image string) (string, error) {
		asked++
		return image + "@sha256:" + strings.Repeat("a", 64), nil
	}})
	if asked != 1 {
		t.Errorf("Pin called %d times, want once per image", asked)
	}
	if !p.Installable() {
		t.Fatalf("blockers = %v", p.Blockers)
	}
	if !strings.Contains(p.Workloads[0].Spec.Image, "@sha256:") {
		t.Errorf("image = %q, want what Pin returned", p.Workloads[0].Spec.Image)
	}
}

// A registry that cannot answer blocks the one image it could not resolve and
// names it, rather than failing the whole entry with nothing to act on.
//
// Both images are ones the API refuses, because only those are resolved at all.
// They were explicit tags when this case was written, and it kept passing after
// the rule changed for a reason that had stopped being true: the resolver was
// never consulted and the entry was installable, so a blocker count of one
// became a blocker count of zero.
func TestAnImageThatCannotBeResolvedNamesItself(t *testing.T) {
	c := loadOne(t, "e", `
services:
  app:
    image: example/app:latest
  side:
    image: example/side:latest
`)
	p := planOf(t, c, "e", catalog.Options{Pin: func(image string) (string, error) {
		if strings.Contains(image, "side") {
			return "", fmt.Errorf("no such repository")
		}
		return image + "@sha256:" + strings.Repeat("b", 64), nil
	}})

	if len(p.Blockers) != 1 {
		t.Fatalf("blockers = %v, want the one image that failed", p.Blockers)
	}
	if !strings.Contains(p.Blockers[0].String(), "example/side:latest") {
		t.Errorf("blocker = %q, want it to name the image", p.Blockers[0])
	}
}

// An image the API already takes is never sent to the resolver.
//
// This is what lets pinning be on by default. Resolving everything made a
// registry that is unreachable, or rate limiting, into a reason that an entry
// naming an explicit tag — one of the 119 that install with no resolver at all
// — stopped installing. A resolver may rescue an entry and it may not take one
// away.
func TestAnAcceptableImageIsNeverSentToTheResolver(t *testing.T) {
	c := loadOne(t, "e", `
services:
  app:
    image: example/app:1.2.3
  side:
    image: example/side@sha256:`+strings.Repeat("c", 64)+`
`)
	var asked []string
	p := planOf(t, c, "e", catalog.Options{Pin: func(image string) (string, error) {
		asked = append(asked, image)
		return "", fmt.Errorf("the registry is unreachable")
	}})

	if len(asked) != 0 {
		t.Errorf("the resolver was asked about %v, which the API already accepts", asked)
	}
	if !p.Installable() {
		t.Fatalf("an entry that installs without a resolver stopped installing with one: %v",
			p.Blockers)
	}
}

// A resolver that answers without fixing anything is not believed. The check is
// on the value that would be committed, not on the fact that a call returned.
func TestAResolverThatChangesNothingStillBlocks(t *testing.T) {
	c := loadOne(t, "e", "services:\n  app:\n    image: example/app:latest\n")
	p := planOf(t, c, "e", catalog.Options{Pin: func(image string) (string, error) {
		return image, nil
	}})
	if p.Installable() {
		t.Fatal("a resolver that handed the tag back left the entry installable")
	}
	if !strings.Contains(p.Blockers[0].String(), ":latest") {
		t.Errorf("blocker = %q, want it to name the tag", p.Blockers[0])
	}
}

// The same entry has to plan to the same bytes twice, or an unchanged
// catalogue produces a new commit on every reconcile and every diff is noise.
func TestPlanningIsDeterministic(t *testing.T) {
	c := corpus(t)
	first := planOf(t, c, "n8n", catalog.Options{})
	second := planOf(t, c, "n8n", catalog.Options{})

	if render(first) != render(second) {
		t.Errorf("two plans of one entry differ:\n%s\n---\n%s", render(first), render(second))
	}
}

func render(p catalog.Plan) string {
	var b strings.Builder
	for _, w := range p.Workloads {
		fmt.Fprintf(&b, "workload %s %s %v\n", w.Name, w.Spec.Image, w.Spec.EnvFrom)
	}
	for _, d := range p.Databases {
		fmt.Fprintf(&b, "database %s %s\n", d.Name, d.Spec.Image)
	}
	for _, s := range p.Secrets {
		for _, k := range s.Keys {
			fmt.Fprintf(&b, "secret %s %s=%s\n", s.Name, k.Name, k.Template)
		}
	}
	for _, s := range p.Mint {
		fmt.Fprintf(&b, "mint %s %s\n", s.Name, s.Kind)
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "note %s\n", n)
	}
	for _, x := range p.Blockers {
		fmt.Fprintf(&b, "blocker %s\n", x)
	}
	return b.String()
}
