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

// Which of the shipped catalogue could run on this machine's architecture, and
// which could not — asked of the registry, without touching a cluster.
//
// # Why this is separate from proving the catalogue works
//
// "It installed" and "it runs" are different claims, and the second one is
// about to be measured in a kind cluster on Apple silicon. An image published
// only for linux/amd64 cannot run there, and the pod fails in a way that reads
// exactly like an application that is broken. Counted together, "41 of 60 work"
// would be partly a statement about the catalogue and partly a statement about
// the laptop, and nothing in the number says which part is which.
//
// So this runs first and produces the denominator: the entries whose images
// this architecture can pull at all. Whatever the cluster then says is about
// the product, because the entries it could never have run were removed from
// the question before it was asked.
//
// # Why the failures are sorted before they are counted
//
// Three things arrive as "we do not have an arm64 build" and only one of them
// is one:
//
//   - the image is published without linux/arm64          — about the image
//   - the reference is not a reference                    — about the template
//   - the registry would not answer                       — about the afternoon
//
// The third moves between runs. A rate limit, a repository that needs a
// credential, a tag upstream deleted: none of them says anything about
// architecture, and a single number that absorbs them is a number that changes
// when nothing changed. registry.ErrReference and registry.ErrUnavailable exist
// so this file can tell them apart without reading messages.
//
// # Why it does not assert a count
//
// The same reason corpus_test.go does not: upstream adds templates weekly and a
// gate that fails when somebody else publishes an application is a gate that
// gets deleted. The numbers go to a commit message and to whoever is running
// the cluster pass. What is asserted here is that the measurement happened at
// all.
package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/damgahq/damga/catalog"
	"github.com/damgahq/damga/registry"
)

// archEnv turns this on. Off by default and skipped loudly, for the reason the
// PostgreSQL conformance suite is: it reaches the network, it takes minutes,
// and a laptop run of `go test ./...` must not depend on Docker Hub being in a
// good mood.
const archEnv = "DAMGA_TEST_REGISTRY_ARCH"

// archCacheEnv names a file the answers accumulate in, and it exists because
// the first run of this measurement found the thing it was built to keep
// separate: Docker Hub rate limits an anonymous address long before a corpus
// this size is finished, so a single pass reports mostly "unanswered" and says
// almost nothing about architecture.
//
// A rate limit resets with time and not with effort, so the answer is to make
// the measurement resumable rather than faster. With a cache the run asks only
// about what it does not already know, and several runs spread across the
// window add up to one complete answer. Without one every run starts over and
// spends its whole budget re-asking about the images it already knew.
//
// Only successful answers are kept. An image that was rate limited has not been
// measured, and caching that would freeze the afternoon into the result — which
// is the exact confusion the categories below exist to prevent.
const archCacheEnv = "DAMGA_TEST_REGISTRY_ARCH_CACHE"

// verdict is what was learned about one image reference.
type verdict int

const (
	runsHere   verdict = iota // published for the platform asked about
	elsewhere                 // published, and not for it
	notARef                   // never addressable: an unexpanded variable, say
	unanswered                // the registry did not say
)

func (v verdict) String() string {
	switch v {
	case runsHere:
		return "runs here"
	case elsewhere:
		return "published elsewhere only"
	case notARef:
		return "not a reference"
	default:
		return "unanswered"
	}
}

// looksExpanded reports whether a reference survived compose variable
// substitution.
//
// Checked before the network rather than after, and this is the whole reason
// the category exists. `${SERVICE_IMAGE}` parses as a Docker Hub name, the
// registry answers 404, and without this it would arrive as "the registry did
// not answer" — indistinguishable from a rate limit. A session measuring
// something else found 22 of 37 unresolvable images were exactly this.
func looksExpanded(image string) bool {
	return !strings.ContainsAny(image, "${}")
}

func TestWhichOfTheCatalogueThisArchitectureCouldRun(t *testing.T) {
	if os.Getenv(archEnv) == "" {
		t.Skipf("set %s=linux/arm64 (or another platform) to ask the registry", archEnv)
	}
	want := parsePlatform(t, os.Getenv(archEnv))
	c := corpus(t)

	// Every image every offered entry names, and which entries name it.
	//
	// Planned without a resolver, so what is collected is the reference the
	// template actually carries. Pinning would replace it with a digest, and a
	// digest answers a different question — the registry would report the
	// platforms of whatever the tag pointed at when the pin was taken.
	byImage := map[string][]string{}
	// How many objects each entry runs, because the entries the cluster pass is
	// about are the ones that run more than one. They are also the ones this
	// measurement answers least often: an entry is only fully answered when
	// every image it names was, so naming six images is six chances to be rate
	// limited.
	objects := map[string]int{}
	for _, e := range c.Entries() {
		p, err := c.Plan(e.Name, catalog.Options{Namespace: testNamespace})
		if err != nil {
			continue
		}
		objects[e.Name] = len(p.Workloads) + len(p.Databases)
		for _, image := range planImages(p) {
			byImage[image] = append(byImage[image], e.Name)
		}
	}
	// The baseline, for the reason the case above this one has one: a loop that
	// collected nothing passes every assertion under it.
	if len(byImage) == 0 {
		t.Fatal("no entry named an image, so everything below is vacuous: the catalogue " +
			"did not load or Plan refused every entry")
	}

	images := make([]string, 0, len(byImage))
	for image := range byImage {
		images = append(images, image)
	}
	slices.Sort(images)

	resolver := &registry.Resolver{}
	got := make(map[string]verdict, len(images))
	reasons := map[string]string{}
	ctx := context.Background()

	known := loadArchCache(t)
	asked, reused := 0, 0

	for _, image := range images {
		if platforms, ok := known[image]; ok {
			reused++
			if slices.ContainsFunc(platforms, func(p registry.Platform) bool { return p.Matches(want) }) {
				got[image] = runsHere
			} else {
				got[image], reasons[image] = elsewhere, platformList(platforms)
			}
			continue
		}
		if !looksExpanded(image) {
			got[image] = notARef
			reasons[image] = "the compose variable was never substituted"
			continue
		}
		// One deadline per image rather than one for the run. A registry that
		// has stopped answering must not decide how long the rest of the
		// corpus waits.
		each, cancel := context.WithTimeout(ctx, 30*time.Second)
		platforms, err := resolver.Platforms(each, image)
		cancel()
		switch {
		case errors.Is(err, registry.ErrReference):
			got[image], reasons[image] = notARef, err.Error()
		case err != nil:
			got[image], reasons[image] = unanswered, err.Error()
		case slices.ContainsFunc(platforms, func(p registry.Platform) bool { return p.Matches(want) }):
			known[image] = platforms
			got[image] = runsHere
		default:
			known[image] = platforms
			got[image], reasons[image] = elsewhere, platformList(platforms)
		}
		asked++
	}
	saveArchCache(t, known)

	t.Logf("%d references were asked about in this run and %d were already known", asked, reused)
	report(t, want, byImage, objects, images, got, reasons)
}

// report prints the two numbers that were asked for and the two that keep them
// honest, at both levels: the image is what was measured and the entry is what
// somebody installs.
func report(
	t *testing.T, want registry.Platform,
	byImage map[string][]string, objects map[string]int, images []string,
	got map[string]verdict, reasons map[string]string,
) {
	t.Helper()

	counts := map[verdict]int{}
	for _, image := range images {
		counts[got[image]]++
	}

	// An entry is only as portable as its least portable image: a plan with six
	// workloads installs six, and one that cannot start is an application that
	// does not work.
	entryVerdict := map[string]verdict{}
	for image, entries := range byImage {
		for _, name := range entries {
			if v, seen := entryVerdict[name]; !seen || got[image] > v {
				entryVerdict[name] = got[image]
			}
		}
	}
	entries := map[verdict]int{}
	for _, v := range entryVerdict {
		entries[v]++
	}

	t.Logf("asked about %s, against %d entries and %d distinct image references",
		want, len(entryVerdict), len(images))
	t.Log("")
	t.Logf("  %-26s %6s %8s", "", "images", "entries")
	for _, v := range []verdict{runsHere, elsewhere, notARef, unanswered} {
		t.Logf("  %-26s %6d %8d", v, counts[v], entries[v])
	}
	t.Log("")
	t.Logf("  the denominator for a cluster pass is %d entries: every image they name is "+
		"published for %s", entries[runsHere], want)
	t.Logf("  %d entries could never run here whatever the catalogue does; %d were not asked "+
		"about and %d were not answered, and none of those three is a statement about %s",
		entries[elsewhere], entries[notARef], entries[unanswered], want)

	// And the same split by how much an entry runs. One workload and six are
	// different installations, and the six-workload ones are both what the
	// cluster pass is for and what this answers least often.
	var multi, multiRuns, single, singleRuns int
	for name, v := range entryVerdict {
		if objects[name] > 1 {
			multi++
			if v == runsHere {
				multiRuns++
			}
			continue
		}
		single++
		if v == runsHere {
			singleRuns++
		}
	}
	t.Logf("  of %d entries running more than one object, %d are known to run here; "+
		"of %d running one, %d are", multi, multiRuns, single, singleRuns)

	// Named rather than only counted, because the next run has to be able to
	// tell a rate limit that has passed from an image that is still amd64-only.
	for _, v := range []verdict{elsewhere, notARef, unanswered} {
		var named []string
		for _, image := range images {
			if got[image] == v {
				named = append(named, image+" — "+reasons[image])
			}
		}
		if len(named) == 0 {
			continue
		}
		t.Logf("\n%s (%d):\n\t%s", v, len(named), strings.Join(named, "\n\t"))
	}
}

// loadArchCache reads the platforms already known, or an empty map.
//
// A missing file is not an error: the first run has nothing to read, and so
// does a run that was pointed at a fresh path on purpose.
func loadArchCache(t *testing.T) map[string][]registry.Platform {
	t.Helper()
	out := map[string][]registry.Platform{}
	path := os.Getenv(archCacheEnv)
	if path == "" {
		return out
	}
	body, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return out
	case err != nil:
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return out
}

func saveArchCache(t *testing.T, known map[string][]registry.Platform) {
	t.Helper()
	path := os.Getenv(archCacheEnv)
	if path == "" {
		return
	}
	body, err := json.MarshalIndent(known, "", "  ")
	if err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// parsePlatform reads os/arch or os/arch/variant.
func parsePlatform(t *testing.T, s string) registry.Platform {
	t.Helper()
	parts := strings.Split(s, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("%s=%q: want something like linux/arm64", archEnv, s)
	}
	p := registry.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) > 2 {
		p.Variant = parts[2]
	}
	return p
}

func platformList(ps []registry.Platform) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return strings.Join(out, " ")
}
