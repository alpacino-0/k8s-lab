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
		if g.Kind == "password" {
			found = true
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
