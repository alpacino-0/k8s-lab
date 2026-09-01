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

package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The reference cluster/control-plane.yaml carries, split where this test needs
// the halves apart.
const (
	controlPlaneRepo = "ghcr.io/damgahq/damga-control-plane"
	// The sentence upgrade.sh prints when the apply changed nothing, and the
	// string CI greps for. Named once here because two files agree on it.
	nothingMoved = "nothing moved"
)

// upgradeSource reads the script, and the read is load-bearing rather than
// defensive: `go test` invalidates a cached result when a file the test process
// opened changes, and a script handed to a child bash was never opened. Without
// this, editing upgrade.sh and rerunning the gate replays the previous verdict.
// Measured in this package on link-docs.sh — a guard was deleted and the suite
// answered ok from cache.
func upgradeSource(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("upgrade.sh")
	if err != nil {
		t.Fatalf("reading upgrade.sh: %v", err)
	}
	return string(body)
}

// upgradePlan runs the dry run and returns its lines.
func upgradePlan(t *testing.T, args ...string) []string {
	t.Helper()
	_ = upgradeSource(t)

	cmd := exec.Command("bash", append([]string{"./upgrade.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "DRY_RUN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("upgrade.sh dry run failed: %v\n%s", err, out)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// TestTheTypesGoInBeforeTheBinaryThatWritesThem.
//
// The same ordering install.sh carries, bought for a second reason here. A
// release that adds a field to a type must have the type before the control
// plane that writes it: the API server prunes an unknown field out of every
// object it is handed, without an error, so a control plane upgraded first
// writes objects that come back missing exactly the new thing.
func TestTheTypesGoInBeforeTheBinaryThatWritesThem(t *testing.T) {
	lines := upgradePlan(t)

	crds := firstLineWith(lines, "config/crd")
	plane := firstLineWith(lines, "cluster/control-plane.yaml")
	switch {
	case crds < 0:
		t.Fatal("the plan never applies the CRDs")
	case plane < 0:
		t.Fatal("the plan never applies cluster/control-plane.yaml")
	case crds > plane:
		t.Errorf("the plan applies the control plane (line %d) before the types it writes "+
			"(line %d); the API server prunes a field it has no schema for and reports nothing",
			plane+1, crds+1)
	}
}

// TestTheImageOverrideTravelsInTheApplyRatherThanAfterIt.
//
// An install that builds its own control plane names it with --image. Applying
// the manifest first and correcting the image afterwards looks equivalent and
// is not: the manifest's reference is one that install has no way to pull, and
// with strategy Recreate the old pod is gone before the replacement fails. The
// correction then lands after an outage this script caused rather than one the
// upgrade required.
func TestTheImageOverrideTravelsInTheApplyRatherThanAfterIt(t *testing.T) {
	const override = "local.test/damga-control-plane:probe"
	lines := upgradePlan(t, "--image", override)

	applied := firstLineWith(lines, "cluster/control-plane.yaml")
	if applied < 0 {
		t.Fatal("the plan never applies cluster/control-plane.yaml")
	}
	if !strings.Contains(lines[applied], override) {
		t.Errorf("the apply does not carry the override:\n%s", lines[applied])
	}
	for i, line := range lines {
		if strings.Contains(line, "set image") {
			t.Errorf("line %d patches the image after the apply: %s", i+1, line)
		}
	}
}

// fakeTree builds the smallest checkout upgrade.sh will run in, so a manifest
// can be broken on purpose.
//
// A copy rather than an environment variable that redirects the real script.
// A hook that exists only so a test can reach it is a second code path, and the
// path under test would be the one nobody runs.
func fakeTree(t *testing.T, manifest string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"scripts", "cluster", filepath.Join("config", "crd", "bases")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "cluster", "control-plane.yaml"), manifest)

	body, err := os.ReadFile("upgrade.sh")
	if err != nil {
		t.Fatalf("reading upgrade.sh: %v", err)
	}
	script := filepath.Join(root, "scripts", "upgrade.sh")
	if err := os.WriteFile(script, body, 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// runInFake runs the copied script and returns its combined output and whether
// it refused.
func runInFake(t *testing.T, script string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "DRY_RUN=1")
	out, err := cmd.CombinedOutput()
	return string(out), err != nil
}

// A manifest with the two names upgrade.sh addresses and one image line.
const goodManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: damga
  namespace: damga-system
spec:
  template:
    spec:
      containers:
        - name: damga
          image: ` + controlPlaneRepo + `:1.0.0
`

// TestARenamedControlPlaneIsRefusedByName.
//
// upgrade.sh addresses damga-system/damga from constants rather than parsing
// them out of the manifest. A rename in cluster/control-plane.yaml that this
// script does not follow would otherwise apply the new namespace and then wait
// for a rollout in the old one, and kubectl's answer for that is
// `deployments.apps "damga" not found` — which names neither the rename nor
// this script, and reads as "you have not installed this".
func TestARenamedControlPlaneIsRefusedByName(t *testing.T) {
	renamed := strings.Replace(goodManifest, "name: damga\n", "name: damga-control\n", 1)
	out, refused := runInFake(t, fakeTree(t, renamed))
	if !refused {
		t.Fatalf("a manifest that no longer carries the deployment name was accepted:\n%s", out)
	}
	if !strings.Contains(out, "would upgrade nothing") {
		t.Errorf("the refusal does not say what it would have done:\n%s", out)
	}

	// And the unmodified shape is accepted, so the case above failed for the
	// reason it claims rather than because the fixture is unusable.
	if out, refused := runInFake(t, fakeTree(t, goodManifest)); refused {
		t.Errorf("the intact manifest was refused too:\n%s", out)
	}
}

// TestAManifestWithATaggedSecondImageIsRefused.
//
// The image is substituted with sed, which cannot know which of two matching
// lines the caller meant. Rewriting the first and leaving the second is the
// failure that would not surface here at all: the apply succeeds, one container
// runs the override and the other runs whatever git said.
func TestAManifestWithATaggedSecondImageIsRefused(t *testing.T) {
	doubled := goodManifest + `        - name: sidecar
          image: ` + controlPlaneRepo + `:1.0.0
`
	out, refused := runInFake(t, fakeTree(t, doubled), "--image", "local.test/cp:probe")
	if !refused {
		t.Fatalf("a manifest with two control-plane image lines was rewritten anyway:\n%s", out)
	}
	if !strings.Contains(out, "found 2") {
		t.Errorf("the refusal does not say how many it found:\n%s", out)
	}
}

// TestTheControlPlaneRunsAnImageThisPipelinePublishes.
//
// The two halves of the same gap, asserted against each other. The manifest
// named an image nothing built for as long as it existed, and the reason it
// survived is that the e2e job builds the control plane into kind and overrides
// this field — so no CI step ever pulled the reference. Deleting the publish
// step would restore that exactly, and this is what notices.
func TestTheControlPlaneRunsAnImageThisPipelinePublishes(t *testing.T) {
	manifest, err := os.ReadFile("../cluster/control-plane.yaml")
	if err != nil {
		t.Fatalf("reading cluster/control-plane.yaml: %v", err)
	}
	found := regexp.MustCompile(`(?m)^\s*image:\s*(\S+)\s*$`).FindSubmatch(manifest)
	if found == nil {
		t.Fatal("cluster/control-plane.yaml names no image")
	}
	ref := string(found[1])
	if !strings.HasPrefix(ref, controlPlaneRepo+":") {
		t.Fatalf("the control plane runs %q, which is not %s", ref, controlPlaneRepo)
	}
	if strings.HasSuffix(ref, ":unpublished") {
		t.Fatalf("the control plane still names %q", ref)
	}

	workflow, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("reading the workflow: %v", err)
	}
	for _, want := range []string{
		// It has to be built, assembled for both architectures, and signed.
		// Any one of the three missing publishes something the cluster cannot
		// pull or cannot verify.
		"name=${{ env.IMAGE_NAME }}-control-plane",
		`"control_plane:${IMAGE_NAME}-control-plane"`,
		`"${IMAGE_NAME}-control-plane@${{ steps.assemble.outputs.control_plane_digest }}"`,
	} {
		if !strings.Contains(string(workflow), want) {
			t.Errorf("cluster/control-plane.yaml runs %s and the workflow no longer carries %q",
				ref, want)
		}
	}
}

// TestCIWatchesForTheSentenceThisScriptPrints.
//
// The e2e step asserts the upgrade actually moved by grepping upgrade.sh's own
// output. Reword the script and that grep stops matching — which does not fail,
// it silently stops proving anything, and the step goes on asserting the
// absence of an event.
func TestCIWatchesForTheSentenceThisScriptPrints(t *testing.T) {
	if !strings.Contains(upgradeSource(t), nothingMoved) {
		t.Errorf("upgrade.sh no longer prints %q", nothingMoved)
	}
	workflow, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("reading the workflow: %v", err)
	}
	if !strings.Contains(string(workflow), "grep -q '"+nothingMoved+"'") {
		t.Errorf("the e2e step no longer greps for %q, so it cannot tell an upgrade "+
			"that rolled from one that changed nothing", nothingMoved)
	}
}

// TestUpgradeRefusesACommandLineItDoesNotUnderstand, exit 2, so a wrapper can
// tell a typo from a failed upgrade.
func TestUpgradeRefusesACommandLineItDoesNotUnderstand(t *testing.T) {
	cmd := exec.Command("bash", "./upgrade.sh", "--force")
	cmd.Env = append(os.Environ(), "DRY_RUN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("upgrade.sh accepted an option it does not have:\n%s", out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Errorf("exit %v, want 2:\n%s", err, out)
	}
}
