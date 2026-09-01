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
	"strings"
	"testing"
)

// The build cache's name is written in two places and nothing else connects
// them: this package pushes the tag, and the registry's collector recognises it
// by name to keep it out of the image count and to age it out on its own rule.
//
// Both directions of a mismatch are silent. If the collector's name is wrong,
// the cache is counted as an image — one of the ten kept slots spent on it, so
// nine builds are retained where the file says ten — and no cache is ever aged
// out. If this package's name is wrong, the collector ages out a tag nothing
// writes and the real cache accumulates untouched.
//
// The file is read rather than the value being copied into a constant, because
// a constant here would be the third place the name lives.
func TestTheCollectorAndTheBuilderAgreeOnTheCacheTagName(t *testing.T) {
	const manifest = "../../cluster/registry.yaml"

	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("the registry manifest is unreadable: %v", err)
	}
	want := "value: " + cacheTag
	if !strings.Contains(string(body), want) {
		t.Fatalf("%s does not carry %q. This package pushes the cache under %q; a collector "+
			"that recognises another name counts the cache as an image and ages out nothing",
			manifest, want, cacheTag)
	}

	// And that it is the CACHE_TAG variable carrying it, rather than the string
	// happening to appear somewhere else in the file.
	if !strings.Contains(string(body), "name: CACHE_TAG") {
		t.Fatalf("%s has no CACHE_TAG variable, so nothing there is treating %q as the cache",
			manifest, cacheTag)
	}
}
