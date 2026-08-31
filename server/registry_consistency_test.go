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

package server

import (
	"os"
	"strings"
	"testing"
)

// The registry's address is written in four places and they have to agree.
//
// Not a hypothetical. Twice in one day this repository lost a CI round to one
// string held in several places that drifted: the rootless builder's home
// directory, spelled in a volume mount, an environment variable and a mkdir;
// and the CRD kustomization, a hand-written list that two of three generated
// files were missing from. Both were invisible locally and cost a cluster round
// each.
//
// The four are the compiled default here, the Service the registry actually
// listens on, the flag the deployed control plane is given, and the value CI
// exports. Three of them are YAML, so no compiler relates them; this test is
// what relates them.
//
// It reads the manifests as text rather than parsing them. The question is
// whether the string appears, and a parser would add a schema this test has no
// opinion about — plus the CI workflow is not a Kubernetes object at all.
func TestTheRegistryAddressAgreesEverywhere(t *testing.T) {
	for _, f := range []struct {
		path, why string
	}{
		{"../cluster/registry.yaml",
			"the Service the registry listens on"},
		{"../cluster/control-plane.yaml",
			"the -registry flag the deployed control plane is given"},
		{"../.github/workflows/ci.yml",
			"the REGISTRY_HOST CI pushes and pulls against"},
	} {
		body, err := os.ReadFile(f.path)
		if err != nil {
			t.Errorf("reading %s: %v", f.path, err)
			continue
		}
		if !strings.Contains(string(body), defaultRegistry) {
			t.Errorf("%s does not carry %q (%s).\n"+
				"The build pushes to one name and the kubelet pulls from another, "+
				"which fails at the pull with a message about the image rather than "+
				"about this disagreement.",
				f.path, defaultRegistry, f.why)
		}
	}
}
