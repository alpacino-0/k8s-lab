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
	"bytes"
	"testing"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// A deploy is "this app, a new image". It has an opinion about one object in
// the directory and none about the rest, and the rest is what a catalogue entry
// with more than one service puts there.
//
// The failure this guards against is silent and one-directional: renderDeploy
// returns the set of files the directory should hold, so a sibling it forgets
// is a sibling that either goes stale for ever or — the moment any caller sets
// gitwrite's Owns — is deleted by a request that only meant to change an image.
func TestADeployKeepsTheObjectsItIsNotDeploying(t *testing.T) {
	place := placement.Placement{App: "api", Namespace: "acme-prod"}

	sibling := []byte("apiVersion: platform.damga.co/v1alpha1\nkind: Database\n" +
		"metadata:\n  name: db\n  namespace: acme-prod\nspec:\n  engine: postgres\n")
	foreign := []byte("resources:\n  - workload.yaml\n")

	primary, err := manifest.Render(workloadFor(place, "ghcr.io/acme/api:1"), "r-0")
	if err != nil {
		t.Fatal(err)
	}
	current := map[string][]byte{
		manifest.File:                      primary,
		manifest.FileFor("Database", "db"): sibling,
		"kustomization.yaml":               foreign,
	}

	out, err := renderDeploy(place, deployRequest{Image: "ghcr.io/acme/api:2"})("r-1", current)
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}

	// The object being deployed changed.
	app, err := manifest.Parse(out[manifest.File])
	if err != nil {
		t.Fatalf("the rendered manifest cannot be read back: %v", err)
	}
	if app.Spec.Image != "ghcr.io/acme/api:2" {
		t.Fatalf("the image is %q; this is the one object the request was about", app.Spec.Image)
	}

	// The one beside it did not.
	kept, ok := out[manifest.FileFor("Database", "db")]
	if !ok {
		t.Fatal("the database's manifest is not in what the deploy returned. Omission is how " +
			"this platform asks for a file to be removed, so a deploy that forgets a sibling " +
			"is a deploy that deletes a database")
	}
	if !bytes.Equal(kept, sibling) {
		t.Fatalf("the database's manifest was rewritten:\n%s", kept)
	}

	// And the file that is not this platform's is not adopted. Returning it
	// would be claiming it, and the next render that omits it would remove it.
	if _, claimed := out["kustomization.yaml"]; claimed {
		t.Fatal("a file this platform did not write was returned as though it were ours")
	}
}

// workloadFor is the shape renderDeploy would have produced last time.
func workloadFor(place placement.Placement, image string) platformv1alpha1.Workload {
	w := platformv1alpha1.Workload{}
	w.Name, w.Namespace = place.App, place.Namespace
	w.Spec.Image = image
	return w
}
