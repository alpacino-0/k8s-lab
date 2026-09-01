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

// In package server rather than server_test because it walks whyRefused, which
// is the function that decides what a user is allowed to install. Measuring the
// shipped catalogue through anything else would be measuring a copy of the rule.
package server

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/damgahq/damga/catalog"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// shippedTemplates is the directory the control plane's image carries and
// -catalog-dir points at.
const shippedTemplates = "../catalog/templates"

// e2eTemplate is the entry the end-to-end job installs on a real cluster.
//
// Named here rather than only in ci.yml so that a change which stops it being
// installable fails a unit test in seconds instead of a kind cluster in
// minutes. It was chosen for what it exercises rather than for being small: one
// workload, an image pinned to a tag, a volume, a generated password the
// operator has to mint, and a compose healthcheck that is an HTTP URL — which
// is what gives the Deployment a probe path that its image actually serves.
const e2eTemplate = "gotify"

// Where the end-to-end job installs it. The namespace the other cluster steps
// use, and an app name prefixed like theirs so a failed run leaves something
// recognisable behind.
const (
	e2eNamespace = "damga"
	e2eApp       = "ci-gotify"
)

// The catalogue this repository ships has to load, and the entry the end-to-end
// job installs has to be installable.
//
// The counts are logged rather than asserted, deliberately. They move the day
// something resolves an image to a digest or the write path commits more than
// one file, and a gate that fails on an improvement is a gate people delete.
// What is asserted is the part that must not move: the directory loads, every
// offered entry plans without an error, and gotify has no refusals.
func TestTheShippedCatalogueLoadsAndCanInstallTheEndToEndEntry(t *testing.T) {
	c, err := catalog.Load(os.DirFS(shippedTemplates))
	if err != nil {
		t.Fatalf("loading %s: %v", shippedTemplates, err)
	}
	entries := c.Entries()
	if len(entries) == 0 {
		t.Fatal("the shipped catalogue is empty")
	}

	var installable []string
	byReason := map[string]int{}
	for _, e := range entries {
		plan, err := c.Plan(e.Name, catalog.Options{Namespace: "t"})
		if err != nil {
			// A template that parses and then cannot be planned at all is a
			// converter failure rather than a platform limit, and it would
			// reach a user as a 404 on an entry the list had just offered.
			t.Errorf("%s is offered and cannot be planned: %v", e.Name, err)
			continue
		}
		refusals := whyRefused(plan)
		if len(refusals) == 0 {
			installable = append(installable, e.Name)
			continue
		}
		byReason[firstReason(refusals[0])]++
	}
	sort.Strings(installable)

	t.Logf("shipped catalogue: %d offered, %d skipped, %d install as they stand",
		len(entries), len(c.Skipped), len(installable))
	for _, reason := range []string{"image", "object count", "generated values"} {
		t.Logf("  refused first by %-16s %d", reason, byReason[reason])
	}

	plan, err := c.Plan(e2eTemplate, catalog.Options{Namespace: "t"})
	if err != nil {
		t.Fatalf("%s: %v", e2eTemplate, err)
	}
	if refusals := whyRefused(plan); len(refusals) > 0 {
		// Named apart from every other entry: this is the one the end-to-end
		// job installs, so a refusal here means that job cannot pass and the
		// message has to say which template stopped being installable.
		t.Fatalf("%s is what the end-to-end job installs and it is now refused:\n  %s",
			e2eTemplate, strings.Join(refusals, "\n  "))
	}
	// What the job then asks the running application for. A probe path taken
	// from the template's own healthcheck is the difference between a pod that
	// goes Ready and one that never does, because the API's default paths are
	// this platform's convention and no third-party image serves them.
	if got := plan.Workloads[0].Spec.Health.ReadinessPath; got != "/health" {
		t.Errorf("%s readiness path = %q, want the /health its healthcheck names", e2eTemplate, got)
	}
}

// The attribution Apache-2.0 asks for, beside the files it covers.
//
// A test because the licence obligation is not satisfied by having done it once:
// a refresh that copies the templates and forgets these two files leaves the
// repository redistributing Apache-2.0 work with no notice and no licence text.
func TestTheVendoredTemplatesCarryTheirAttribution(t *testing.T) {
	for _, name := range []string{"README.md", "LICENSE.apache-2.0"} {
		body, err := os.ReadFile(filepath.Join(shippedTemplates, name))
		if err != nil {
			t.Errorf("%s is missing from the vendored templates: %v", name, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	readme, err := os.ReadFile(filepath.Join(shippedTemplates, "README.md"))
	if err != nil {
		return
	}
	for _, want := range []string{"coollabsio/coolify", "Apache-2.0", "2018e7f329"} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("the attribution does not name %q, so the copy is not traceable", want)
		}
	}
}

// firstReason buckets a refusal by which of the three limits produced it, so the
// log above says where the catalogue is actually stopped rather than how many
// sentences it printed.
func firstReason(refusal string) string {
	switch {
	case strings.Contains(refusal, "image"):
		return "image"
	case strings.Contains(refusal, "objects"):
		return "object count"
	default:
		return "generated values"
	}
}

// installGolden is the manifest the end-to-end job applies to a real cluster.
const installGolden = "testdata/catalog-gotify.yaml"

// The manifest CI installs has to be the one the catalogue produces, byte for
// byte.
//
// It is committed rather than generated in the job, and the seam is worth
// naming rather than hiding: the real install path writes this file through a
// git commit that Argo CD applies, and the end-to-end job has neither a git
// remote nor Argo CD. So the job applies the file directly, and this test is
// what keeps that from becoming a hand-written fixture that proves nothing —
// it renders the manifest through renderInstall, the same function the endpoint
// calls, and fails if a byte differs.
//
// What the job therefore proves is everything from the vendored templates to a
// running application, minus the commit. What it does not prove is the commit,
// and that is said here so nobody reads a green job as covering it.
func TestTheEndToEndManifestIsWhatTheCatalogueProduces(t *testing.T) {
	c, err := catalog.Load(os.DirFS(shippedTemplates))
	if err != nil {
		t.Fatalf("loading %s: %v", shippedTemplates, err)
	}
	plan, err := c.Plan(e2eTemplate, catalog.Options{Namespace: e2eNamespace})
	if err != nil {
		t.Fatalf("%s: %v", e2eTemplate, err)
	}
	secrets, unmintable := plannedSecrets(plan)
	if len(unmintable) > 0 {
		t.Fatalf("%s asks for values this platform cannot mint: %v", e2eTemplate, unmintable)
	}

	place := placement.Placement{
		TenantID: "t_ci", App: e2eApp, Env: "prod",
		RepoURL: "https://example.test/ci", Branch: "main", Path: "apps/" + e2eApp + "/prod",
		Namespace: e2eNamespace,
	}
	// No rollout id: it is a per-deploy annotation, and a golden file that
	// carried one would differ on every run for a reason that is not a change.
	files, err := renderInstall(place, plan, secrets)("", nil)
	if err != nil {
		t.Fatalf("rendering the install: %v", err)
	}
	got := files[manifest.File]

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(installGolden, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", installGolden, err)
		}
		t.Logf("wrote %s (%d bytes)", installGolden, len(got))
		return
	}

	want, err := os.ReadFile(installGolden)
	if err != nil {
		t.Fatalf("reading %s: %v (regenerate with UPDATE_GOLDEN=1)", installGolden, err)
	}
	if string(got) != string(want) {
		// Named apart from a plain diff: this file is applied by the
		// end-to-end job, so a mismatch means CI is installing something the
		// catalogue no longer produces.
		t.Errorf("%s is not what the catalogue produces any more, so the end-to-end job "+
			"installs a manifest this platform would not write.\nregenerate with: "+
			"UPDATE_GOLDEN=1 go test ./server/ -run TestTheEndToEndManifestIsWhatTheCatalogueProduces"+
			"\n--- committed ---\n%s\n--- produced ---\n%s", installGolden, want, got)
	}
}
