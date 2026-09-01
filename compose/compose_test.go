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

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/damgahq/damga/compose"
)

func load(t *testing.T, name string) compose.Template {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".yaml"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	tpl, err := compose.Parse(name, data)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return tpl
}

func convert(t *testing.T, name string) compose.Result {
	t.Helper()
	res, err := compose.Convert(load(t, name), compose.Options{
		Namespace: "tenant-a", VolumeSize: resource.MustParse("2Gi"),
	})
	if err != nil {
		t.Fatalf("Convert(%s): %v", name, err)
	}
	return res
}

// The metadata compose has nowhere to put, which is what a catalogue page is
// made of. It lives in leading comments and is therefore invisible to a YAML
// parser — the reason this package reads the file twice.
func TestTheCatalogueMetadataIsRead(t *testing.T) {
	tpl := load(t, "n8n")

	if tpl.Slogan == "" || tpl.Category == "" || tpl.Logo == "" {
		t.Errorf("metadata missing: %+v", tpl)
	}
	if tpl.Port != 5678 {
		t.Errorf("port = %d, want 5678", tpl.Port)
	}
	if len(tpl.Tags) == 0 {
		t.Error("no tags parsed")
	}
	if len(tpl.Services) != 2 {
		t.Errorf("services = %d, want 2 (n8n and its task runner)", len(tpl.Services))
	}
}

// The single-service, single-volume case, and the one the catalogue is for.
func TestN8nBecomesSomethingThatCouldRun(t *testing.T) {
	res := convert(t, "n8n")

	if len(res.Workloads) != 2 {
		t.Fatalf("workloads = %d, want 2", len(res.Workloads))
	}
	var main *struct {
		image string
		port  int32
		vols  int
	}
	for i := range res.Workloads {
		w := res.Workloads[i]
		if strings.Contains(w.Spec.Image, "n8nio/n8n:") {
			main = &struct {
				image string
				port  int32
				vols  int
			}{w.Spec.Image, w.Spec.Port, len(w.Spec.Volumes)}
		}
	}
	if main == nil {
		t.Fatal("the n8n service did not become a workload")
	}
	if main.port != 5678 {
		t.Errorf("port = %d, want the 5678 the template declares", main.port)
	}
	if main.vols != 1 {
		t.Errorf("volumes = %d, want the one n8n keeps its data in", main.vols)
	}
}

// The whole reason Result carries Notes. A converter that returns a Workload
// and says nothing is indistinguishable from one that understood the file, and
// this template contains three separate things that do not convert.
func TestWhatDoesNotConvertIsReported(t *testing.T) {
	res := convert(t, "n8n")

	if len(res.Notes) == 0 {
		t.Fatal("nothing reported; a template with a volume and depends_on always has something to say")
	}
	all := strings.Join(noteStrings(res), "\n")

	// The size is invented. Compose has no size at all — a Docker volume grows
	// into the host's disk — so this is a number this package chose, and a
	// claim cannot be shrunk afterwards.
	if !strings.Contains(all, "compose declares no size") {
		t.Errorf("the invented volume size is not reported:\n%s", all)
	}
	// depends_on has no equivalent and the consequence is a slow first start
	// rather than a failure, which is exactly the kind that gets missed.
	if !strings.Contains(all, "start order") {
		t.Errorf("depends_on is not reported:\n%s", all)
	}
}

// SERVICE_PASSWORD_N8N is not a value. Passing it through produces a working
// deployment whose password is the literal string "SERVICE_PASSWORD_N8N",
// which is the worst outcome available: it starts, it serves, and it is open.
func TestGeneratedCredentialsAreNotPassedThroughAsLiterals(t *testing.T) {
	res := convert(t, "n8n")

	found := false
	for _, g := range res.Generated {
		for _, src := range g.Sources {
			if src.Kind == "password" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no password was requested; Generated = %+v", res.Generated)
	}
	for _, w := range res.Workloads {
		for _, e := range w.Spec.Env {
			if strings.Contains(e.Value, "SERVICE_PASSWORD") {
				t.Errorf("%s=%q reached the workload as a literal", e.Name, e.Value)
			}
		}
	}
}

// The identity of a generated value is its source name, not the key it lands
// under. n8n asks for N8N_RUNNERS_AUTH_TOKEN in both of its services and both
// name ${SERVICE_PASSWORD_N8N}: mint per key and the task runner presents a
// token the broker rejects, which looks like a runner bug and is not.
//
// 111 of the 369 templates that parse share a source between services.
func TestOneSourceServesEveryKeyThatNamesIt(t *testing.T) {
	res := convert(t, "n8n")

	var seen []string
	for _, g := range res.Generated {
		if g.Key != "N8N_RUNNERS_AUTH_TOKEN" {
			continue
		}
		if len(g.Sources) != 1 {
			t.Fatalf("%s in %s names %d sources, want 1", g.Key, g.Service, len(g.Sources))
		}
		seen = append(seen, g.Service+"->"+g.Sources[0].Name)
	}
	if len(seen) != 2 {
		t.Fatalf("N8N_RUNNERS_AUTH_TOKEN requested %d times, want once per service: %v", len(seen), seen)
	}
	first, second, _ := strings.Cut(seen[0], "->")
	other, otherSrc, _ := strings.Cut(seen[1], "->")
	if second != otherSrc {
		t.Errorf("%s asks for %q and %s asks for %q; the broker and its runner would not agree",
			first, second, other, otherSrc)
	}
}

// A credential inside a larger string. Resolving the interpolation to its
// default writes the empty string where the password goes, and the result is a
// URL that parses, connects, and is refused — with no note saying why.
//
// 51 of the 369 templates that parse do this.
func TestACredentialInsideAConnectionStringIsNotBlanked(t *testing.T) {
	res := convert(t, "plausible")

	for _, w := range res.Workloads {
		for _, e := range w.Spec.Env {
			if e.Name == "DATABASE_URL" {
				t.Fatalf("DATABASE_URL reached the workload as %q", e.Value)
			}
		}
	}
	var url *compose.Generated
	for i := range res.Generated {
		if res.Generated[i].Key == "DATABASE_URL" {
			url = &res.Generated[i]
		}
	}
	if url == nil {
		t.Fatal("DATABASE_URL was dropped instead of being asked for")
	}
	if len(url.Sources) != 2 {
		t.Errorf("sources = %+v, want the user and the password", url.Sources)
	}
	for _, src := range url.Sources {
		if !strings.Contains(url.Value, "${"+src.Name+"}") {
			t.Errorf("%s is named as a source but does not appear in %q", src.Name, url.Value)
		}
	}
	// The rest of the string has to survive, or substituting into it produces
	// a URL with no host.
	if !strings.Contains(url.Value, "plausible-db:5432") {
		t.Errorf("the value lost everything around the credential: %q", url.Value)
	}
}

// A default on a generated variable is a published credential. Honouring it is
// the one outcome worse than an empty password, because it starts and is open.
func TestADefaultOnAGeneratedVariableIsNotUsed(t *testing.T) {
	tpl, err := compose.Parse("defaulted", []byte(`
services:
  app:
    image: example/app:1.0
    environment:
      - TOKEN=${SERVICE_PASSWORD_APP:-hunter2}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := compose.Convert(tpl, compose.Options{Namespace: "t"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	for _, g := range res.Generated {
		if strings.Contains(g.Value, "hunter2") {
			t.Errorf("the published default survived into %q", g.Value)
		}
	}
	for _, w := range res.Workloads {
		for _, e := range w.Spec.Env {
			if strings.Contains(e.Value, "hunter2") {
				t.Errorf("%s=%q carries the published default", e.Name, e.Value)
			}
		}
	}
}

// Upstream withdraws a template with a header comment, and 28 of the 371 files
// carry it. A catalogue that cannot see it offers 28 entries whose own authors
// took them down.
func TestAWithdrawnTemplateSaysSo(t *testing.T) {
	if !load(t, "plausible").Ignore {
		t.Error("plausible carries `# ignore: true` and parsed as offered")
	}
	if load(t, "n8n").Ignore {
		t.Error("n8n does not carry the header and parsed as withdrawn")
	}
}

// Compose puts every service on one network under its own name. Here each one
// becomes an object under a derived name, so a value that says `plausible-db`
// addresses nothing — the application starts, retries, and reports that its
// database is down, which sends the reader looking at the database.
//
// 112 of the 199 multi-service templates address a sibling this way.
func TestAddressingASiblingByItsComposeNameIsReported(t *testing.T) {
	res := convert(t, "plausible")

	all := strings.Join(noteStrings(res), "\n")
	if !strings.Contains(all, "plausible-events-db") || !strings.Contains(all, "not what it is called here") {
		t.Errorf("the clickhouse address was not reported:\n%s", all)
	}
	// The note has to say what the name became, or it is a problem with no
	// action attached.
	if !strings.Contains(all, "plausible-plausible-events-db") {
		t.Errorf("the note does not say what the name became:\n%s", all)
	}
}

// The same detection must not fire on values that merely contain a service
// name. A note that cries wolf is a note nobody finishes reading.
func TestAValueThatOnlyContainsASiblingNameIsNotReported(t *testing.T) {
	tpl, err := compose.Parse("quiet", []byte(`
services:
  app:
    image: example/app:1.0
    environment:
      - DB_NAME=db
      - NOTE=the db is fine
      - REAL=postgres://user@db:5432/x
  db:
    image: example/db:1.0
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := compose.Convert(tpl, compose.Options{Namespace: "t"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	fired := 0
	for _, n := range res.Notes {
		if strings.Contains(n.Detail, "not what it is called here") {
			fired++
			if n.Field != "REAL" {
				t.Errorf("fired on %s, which is not an address", n.Field)
			}
		}
	}
	if fired != 1 {
		t.Errorf("the sibling note fired %d times, want once for the one address", fired)
	}
}

// A Database and a Workload that do not know about each other leave the app
// with no credentials at all. All 109 templates with a postgres service
// converted that way.
func TestTheDatabaseIsAttachedToAWorkload(t *testing.T) {
	res := convert(t, "plausible")

	if len(res.Databases) != 1 {
		t.Fatalf("databases = %d, want 1", len(res.Databases))
	}
	attached := 0
	for _, w := range res.Workloads {
		if w.Spec.Database == res.Databases[0].Name {
			attached++
		}
	}
	if attached != 1 {
		t.Errorf("%d workloads name %s, want the one that fronts the template",
			attached, res.Databases[0].Name)
	}
}

// The two fields are PostgreSQL identifiers and the API refuses anything else.
// A hyphen is legal in compose and not in an identifier, so `plausible-db` is
// an object that converts, commits, and is rejected on apply — 61 of the 369
// templates that parse name one.
func TestADatabaseNameIsAPostgresIdentifier(t *testing.T) {
	res := convert(t, "plausible")

	db := res.Databases[0]
	if db.Spec.Database != "plausible_db" {
		t.Errorf("database = %q, want the hyphen replaced", db.Spec.Database)
	}
	// The user is ${SERVICE_USER_POSTGRES}, which used to resolve to "".
	if db.Spec.Username != "" {
		t.Errorf("username = %q, want it left to the API's default", db.Spec.Username)
	}
	if !strings.Contains(strings.Join(noteStrings(res), "\n"), "mints this database") {
		t.Error("the generated username was dropped without saying so")
	}
}

// ${VAR:-default} is the only value available without an environment to read,
// and dropping it would leave the variable empty rather than defaulted.
func TestDefaultsInsideInterpolationsSurvive(t *testing.T) {
	res := convert(t, "n8n")

	for _, w := range res.Workloads {
		for _, e := range w.Spec.Env {
			if strings.Contains(e.Value, "${") {
				t.Errorf("%s=%q still carries an unresolved interpolation", e.Name, e.Value)
			}
		}
	}
}

// A postgres service becomes a Database rather than a Workload, which is what
// brings it backups and the restore rehearsal.
func TestAPostgresServiceBecomesADatabase(t *testing.T) {
	res := convert(t, "plausible")

	if len(res.Databases) == 0 {
		t.Fatalf("no database recognised; workloads = %d", len(res.Workloads))
	}
	if len(res.Workloads) == 0 {
		t.Fatal("everything became a database; nothing would run")
	}
	for _, db := range res.Databases {
		if db.Spec.Storage.IsZero() {
			t.Errorf("%s has no storage", db.Name)
		}
	}
}

// A host bind mount has no host to bind to. Reported rather than dropped: a
// template that mounts ./config and silently loses it starts and misbehaves.
func TestBindMountsAreRefusedOutLoud(t *testing.T) {
	tpl, err := compose.Parse("bind", []byte(`
services:
  app:
    image: example/app:1.0
    volumes:
      - ./config:/etc/app
      - data:/var/lib/app
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := compose.Convert(tpl, compose.Options{Namespace: "t"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(res.Workloads[0].Spec.Volumes) != 1 {
		t.Errorf("volumes = %d, want only the named one", len(res.Workloads[0].Spec.Volumes))
	}
	if !strings.Contains(strings.Join(noteStrings(res), "\n"), "bind mount") {
		t.Error("the bind mount was dropped without saying so")
	}
}

// Both spellings are correct compose and the corpus uses both.
func TestEnvironmentParsesAsAListAndAsAMap(t *testing.T) {
	const listForm = `
services:
  app:
    image: example/app:1.0
    environment:
      - A=1
      - B=2
`
	const mapForm = `
services:
  app:
    image: example/app:1.0
    environment:
      A: "1"
      B: "2"
`
	for _, form := range []struct{ name, body string }{{"list", listForm}, {"map", mapForm}} {
		t.Run(form.name, func(t *testing.T) {
			tpl, err := compose.Parse("x", []byte(form.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := tpl.Services["app"].Environment["A"]; got != "1" {
				t.Errorf("A = %q, want 1", got)
			}
		})
	}
}

// The same input has to produce the same bytes, or an unchanged template
// produces a new git commit on every reconcile and every diff becomes noise.
func TestConversionIsDeterministic(t *testing.T) {
	first, second := convert(t, "plausible"), convert(t, "plausible")

	if len(first.Workloads) != len(second.Workloads) {
		t.Fatal("workload count differs between runs")
	}
	for i := range first.Workloads {
		if first.Workloads[i].Name != second.Workloads[i].Name {
			t.Errorf("workload %d: %q then %q", i, first.Workloads[i].Name, second.Workloads[i].Name)
		}
	}
	if strings.Join(noteStrings(first), "|") != strings.Join(noteStrings(second), "|") {
		t.Error("the notes came out in a different order")
	}
}

// A YAML merge key sharing one environment block between services. Common in
// hand-written compose files, and it needs a decoder that resolves `<<` — the
// YAML-to-JSON route most Go code takes does not, and fails outright.
func TestMergeKeysResolve(t *testing.T) {
	tpl, err := compose.Parse("merge", []byte(`
x-shared: &shared
  LOG_LEVEL: info
  REGION: eu

services:
  api:
    image: example/api:1.0
    environment:
      <<: *shared
      ROLE: api
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	env := tpl.Services["api"].Environment
	for k, want := range map[string]string{"LOG_LEVEL": "info", "REGION": "eu", "ROLE": "api"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
}

// YAML that is not YAML. Two of the 368 templates measured anchor a *sequence*
// and then merge it into a mapping, which no conforming parser accepts — Python
// and Go both refuse it. Recorded as a test so that a future parser change that
// starts "handling" it is recognised as accepting broken input rather than as
// an improvement.
func TestAnInvalidMergeIsRefusedRatherThanGuessed(t *testing.T) {
	_, err := compose.Parse("bad", []byte(`
x-env: &env
  - A=1

services:
  api:
    image: example/api:1.0
    environment:
      <<: *env
`))
	if err == nil {
		t.Fatal("a sequence merged into a mapping was accepted")
	}
}

func noteStrings(r compose.Result) []string {
	out := make([]string, 0, len(r.Notes))
	for _, n := range r.Notes {
		out = append(out, n.String())
	}
	return out
}

// An image naming a compose variable is resolved to its default, and one with
// no default says so.
//
// This is not cosmetic and the corpus measured both halves of it. Left
// unresolved, `ghcr.io/x/y:${VER:-latest}` reaches a registry with the ${...}
// still in it, which answers "does not exist" — true of a repository nobody
// published and indistinguishable from an image upstream withdrew. 22 of the 37
// images the registry client could not resolve across the upstream corpus were
// this.
//
// And the reference was passing the API's own check while it did. The last path
// segment contains a colon and the string does not end in ":latest", so
// `x:${VER:-latest}` was accepted as a pinned image: 4 entries counted as
// installable would have committed a manifest naming an image no kubelet can
// pull. Resolving it first is what turns those into the refusal they always
// were.
func TestAVariableInTheImageIsResolvedToItsDefault(t *testing.T) {
	tpl, err := compose.Parse("vars", []byte(`
services:
  app:
    image: ghcr.io/example/app:${APP_VERSION:-1.4.2}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := compose.Convert(tpl, compose.Options{Namespace: "t"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got := res.Workloads[0].Spec.Image; got != "ghcr.io/example/app:1.4.2" {
		t.Errorf("image = %q, want the default substituted", got)
	}
}

// A variable with no default resolves to nothing, which is what compose does
// too, and the note names the variable.
//
// What is left is refused downstream by the rule that wants a tag, and that is
// the right end for it: substituting "latest" here would invent the moving tag
// the platform exists to refuse.
func TestAVariableWithNoDefaultIsNamedRatherThanGuessed(t *testing.T) {
	tpl, err := compose.Parse("vars", []byte(`
services:
  app:
    image: ghcr.io/example/app:${APP_VERSION}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := compose.Convert(tpl, compose.Options{Namespace: "t"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got := res.Workloads[0].Spec.Image; got != "ghcr.io/example/app:" {
		t.Errorf("image = %q, want the variable resolved to nothing", got)
	}
	notes := strings.Join(noteStrings(res), "\n")
	if !strings.Contains(notes, "APP_VERSION") {
		t.Errorf("the note does not name the variable compose had no value for: %s", notes)
	}
}
