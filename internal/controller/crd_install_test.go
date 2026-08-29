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

package controller

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every generated CRD has to be listed for installation, and kustomize will not
// glob.
//
// This is the third instance of one failure in a single day: a hand-written
// list of files that a new or deleted file falls out of, where nothing local
// notices. Makefile.operator's GO_PKGS was the first — two packages shipped
// with green CI while their tests had never run. The e2e job's list of policy
// manifests was the second, and it named two files that had been deleted. This
// one named two CRDs out of three, so the operator installed without the type
// it was built to reconcile and the only symptom was "no matches for kind" in
// a cluster.
//
// Two of those three were fixed structurally: `./...` and a directory. Kustomize
// rejects globs in `resources` by design, so the list cannot be replaced — which
// leaves binding it to reality with a test. That is the weaker fix, and it is
// here rather than in CI so it also fails on the machine where the CRD is
// added.
func TestEveryGeneratedCRDIsInstalled(t *testing.T) {
	const (
		basesDir      = "../../config/crd/bases"
		kustomization = "../../config/crd/kustomization.yaml"
	)

	entries, err := os.ReadDir(basesDir)
	if err != nil {
		t.Fatalf("reading %s: %v", basesDir, err)
	}
	listed, err := os.ReadFile(kustomization)
	if err != nil {
		t.Fatalf("reading %s: %v", kustomization, err)
	}

	var generated, missing []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		generated = append(generated, e.Name())
		if !strings.Contains(string(listed), "bases/"+e.Name()) {
			missing = append(missing, e.Name())
		}
	}
	if len(generated) == 0 {
		t.Fatal("no CRDs found; run `make -f Makefile.operator manifests` first")
	}
	if len(missing) > 0 {
		t.Errorf("these CRDs are generated but never installed, so the operator "+
			"will start and reconcile nothing: %s\nadd them to %s",
			strings.Join(missing, ", "), kustomization)
	}

	// And the other direction, which fails later and more confusingly: a name
	// left behind after a type is deleted makes `kustomize build` fail outright,
	// so the operator cannot be installed at all.
	for line := range strings.SplitSeq(string(listed), "\n") {
		line = strings.TrimSpace(line)
		name, ok := strings.CutPrefix(line, "- bases/")
		if !ok {
			continue
		}
		if !slices.Contains(generated, name) {
			t.Errorf("%s lists %s, which does not exist in %s",
				kustomization, name, filepath.Clean(basesDir))
		}
	}
}
