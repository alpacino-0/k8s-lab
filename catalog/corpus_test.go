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

// What the whole shipped catalogue has to be true of, measured against every
// entry rather than against a fixture.
//
// # Why none of these asserts a number
//
// The obvious test here is "275 entries install". It would be wrong. Upstream
// adds templates weekly, so the count moves on a refresh that changed nothing
// about this code, and the thing breaking the build would be somebody else's
// new application. A test that fails without telling you anything is what this
// repository calls decoration. A floor — "at least 250" — goes stale the other
// way: it passes at 275 and it passes at 251, and it never says which.
//
// It would also not have caught the bug these were written for. Measured, on
// this corpus, with the resolver in place: the installable count was 275 both
// before and after the fix that resolves a compose variable in an image. The
// count could not see it. The fourth case below could.
//
// So the number lives where a number belongs — in a commit message and in
// docs/DURUM.md, beside the corpus commit it was measured against — and what is
// asserted here is the set of things that must stay true whatever the number is.
package catalog_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/damgahq/damga/catalog"
)

// testNamespace is where these plans land. Every case needs one and none of
// them turns on which it is.
const testNamespace = "tenant-a"

// planImages is every image a plan would commit.
func planImages(p catalog.Plan) []string {
	out := make([]string, 0, len(p.Workloads)+len(p.Databases))
	for _, w := range p.Workloads {
		out = append(out, w.Spec.Image)
	}
	for _, d := range p.Databases {
		out = append(out, d.Spec.Image)
	}
	return out
}

// resolves is a resolver that always answers, which is the ceiling.
func resolves(image string) (string, error) {
	return image + "@sha256:" + strings.Repeat("a", 64), nil
}

// A resolver may rescue an entry and may never take one away.
//
// This is the property that lets image pinning be on by default, and it is not
// obvious: Pin is consulted at install time and a registry can be unreachable,
// rate limiting, or refusing an anonymous token. If a failure there could block
// an entry that installs with no resolver at all, then turning pinning on would
// trade 145 entries somebody gains for an unknown number somebody loses on a
// bad afternoon — and nobody could sensibly leave it on.
//
// What makes it hold is that pinImages resolves only the references the API
// would refuse. The resolver here fails every call, which is the worst a
// registry can do, and every entry that installs without one still installs.
func TestAResolverCanRescueAnEntryAndNeverTakeOneAway(t *testing.T) {
	c := corpus(t)
	unreachable := func(string) (string, error) {
		return "", errors.New("the registry is unreachable")
	}

	var lost []string
	compared := 0
	for _, e := range c.Entries() {
		without, err := c.Plan(e.Name, catalog.Options{Namespace: testNamespace})
		if err != nil || !without.Installable() {
			continue
		}
		compared++
		with, err := c.Plan(e.Name, catalog.Options{Namespace: testNamespace, Pin: unreachable})
		if err != nil {
			t.Errorf("%s planned without a resolver and failed with one: %v", e.Name, err)
			continue
		}
		if !with.Installable() {
			lost = append(lost, e.Name+" "+with.Blockers[0].String())
		}
	}

	// The baseline has to exist, or the loop above skipped every entry and
	// asserted nothing.
	//
	// Not a spare line. Reverting pinImages to resolve every image — the change
	// this case exists to forbid — also refuses every image when Pin is nil, so
	// the baseline collapsed to zero, the loop compared nothing, and the case
	// passed. It was written without this and it passed against the bug.
	if compared == 0 {
		t.Fatal("no entry installs without a resolver at all, so this case compared nothing: " +
			"the baseline collapsed and everything below it is vacuous")
	}
	if len(lost) > 0 {
		t.Errorf("%d of %d entries install with no resolver and stop installing when one cannot"+
			" answer; a resolver that fails must leave the catalogue where it found it:\n\t%s",
			len(lost), compared, strings.Join(lost[:min(len(lost), 5)], "\n\t"))
	}

	// The counts, logged and never asserted. They are what docs/DURUM.md
	// carries, and a gate that fails when upstream adds an application is a
	// gate somebody deletes rather than reads.
	answering := 0
	for _, e := range c.Entries() {
		p, err := c.Plan(e.Name, catalog.Options{Namespace: testNamespace, Pin: resolves})
		if err == nil && p.Installable() {
			answering++
		}
	}
	t.Logf("installable: %d with no resolver, %d with one that answers everything", compared, answering)
}

// Every image a resolver could not resolve is named in a blocker.
//
// The failure this forbids is the expensive one in this repository: leaving the
// tag in place and installing anyway. That succeeds, commits a moving tag, and
// the platform's claim that git says exactly what runs is false for that app
// with nothing anywhere saying so. A refusal that does not name the image is
// only slightly better — the operator is told the catalogue said no and not
// which of six services to look at.
func TestEveryImageAResolverRefusesIsNamedInABlocker(t *testing.T) {
	c := corpus(t)

	for _, e := range c.Entries() {
		var asked []string
		refusing := func(image string) (string, error) {
			asked = append(asked, image)
			return "", errors.New("no such repository")
		}
		p, err := c.Plan(e.Name, catalog.Options{Namespace: testNamespace, Pin: refusing})
		if err != nil {
			continue
		}
		blockers := make([]string, 0, len(p.Blockers))
		for _, b := range p.Blockers {
			blockers = append(blockers, b.String())
		}
		joined := strings.Join(blockers, "\n")
		for _, image := range asked {
			if !strings.Contains(joined, image) {
				t.Errorf("%s: the resolver refused %q and no blocker names it: %s",
					e.Name, image, joined)
			}
		}
	}
}

// Nothing offered as installable still carries a compose variable in an image.
//
// This is not a guard against a bug that was fixed; it is what replaced a
// safety net that was removed, and the order matters. compose.Convert did not
// resolve ${VAR:-default} in the image field, so the reference reached the
// registry with the ${...} in it and was refused — 22 of the 37 images a real
// registry client could not resolve. That refusal was accidental cover: the
// entry was blocked, so nothing shipped.
//
// Then pinImages stopped resolving references the API already accepts, which is
// what makes a resolver safe to leave on — and `x:${VER:-latest}` is one of
// those. Its last path segment has a colon and it does not end in ":latest", so
// the rule takes it and the resolver never sees it. Measured on this corpus
// with the interpolation fix reverted: 16 image references in entries counted
// installable still read ${...}, and the installable count was 275 either way.
//
// The two changes shipped together so the hole never opened. In the other order
// it would have, and nothing else here would have noticed.
func TestNoInstallableEntryStillNamesAVariableInItsImage(t *testing.T) {
	c := corpus(t)

	for _, pin := range []struct {
		what string
		fn   func(string) (string, error)
	}{
		{"with no resolver", nil},
		{"with one that answers", resolves},
	} {
		t.Run(pin.what, func(t *testing.T) {
			for _, e := range c.Entries() {
				p, err := c.Plan(e.Name, catalog.Options{Namespace: testNamespace, Pin: pin.fn})
				if err != nil || !p.Installable() {
					continue
				}
				for _, image := range planImages(p) {
					if strings.Contains(image, "${") {
						t.Errorf("%s is offered as installable and would commit %q, "+
							"which no registry can resolve and no kubelet can pull",
							e.Name, image)
					}
				}
			}
		})
	}
}

// Plan.Primary is the answer the caller used to recover by probing.
//
// The endpoint that installs an entry needs to know which workload is the
// application: it takes the placement's name and the fixed filename every later
// deploy addresses, and the others get their own files. The converter has known
// that all along and did not report it, so the caller asked for a placeholder
// domain, saw which workload the converter attached it to, and cleared it off
// again — reading an answer out of a side effect, which holds only for as long
// as requesting a domain keeps meaning exactly what it means today.
//
// This is the case that says the field is a tidy-up and not a change of
// behaviour: for every multi-workload entry in the shipped catalogue, Primary
// has to name the workload the probe would have found. A difference here is
// more interesting than a match and the message prints which entries, because
// then one of the two is wrong and the corpus says which.
//
// The counts are logged and not asserted, for the reason at the top of this
// file: upstream adds templates weekly, and a gate that fails when somebody
// else ships an application is a gate that gets deleted rather than read.
func TestThePrimaryIsTheAnswerTheProbeUsedToRecover(t *testing.T) {
	c := corpus(t)

	// The placeholder the endpoint used to ask for. It has to be a name
	// nothing would route: .invalid is reserved by RFC 2606 for exactly this.
	const probe = "front-door.invalid"

	multi, notFirst, installableMulti := 0, 0, 0
	var differ []string
	for _, e := range c.Entries() {
		plan, err := c.Plan(e.Name, catalog.Options{Namespace: testNamespace})
		if err != nil {
			continue
		}

		// Every plan, not only the multi-workload ones. Primary is a raw index
		// into a public struct and the caller indexes with it, so out of range
		// is a panic in the install path rather than a wrong answer — and the
		// single-workload entries are most of the corpus and would otherwise
		// never be looked at here at all.
		if len(plan.Workloads) > 0 && (plan.Primary < 0 || plan.Primary >= len(plan.Workloads)) {
			t.Errorf("%s: Primary=%d with %d workloads; the install path indexes with this",
				e.Name, plan.Primary, len(plan.Workloads))
			// Reported and then skipped. Everything below indexes with it, so
			// carrying on would turn a named failure into a panic that stops
			// the case before it has looked at the rest of the corpus.
			continue
		}

		if len(plan.Workloads) < 2 {
			continue
		}
		multi++
		if plan.Installable() {
			installableMulti++
		}

		probed, err := c.Plan(e.Name, catalog.Options{Namespace: testNamespace, Domain: probe})
		if err != nil {
			t.Errorf("%s plans with no domain and fails with one: %v", e.Name, err)
			continue
		}
		// What the endpoint used to do, kept here because it is the only
		// surviving statement of the answer this field has to reproduce.
		was := 0
		for i := range probed.Workloads {
			if probed.Workloads[i].Spec.Domain == probe {
				was = i
				break
			}
		}

		if plan.Primary != was {
			differ = append(differ, fmt.Sprintf("%s: Primary=%d (%s), the probe found %d (%s)",
				e.Name, plan.Primary, plan.Workloads[plan.Primary].Name,
				was, probed.Workloads[was].Name))
		}
		// Not the first, so "take workload zero" would have been wrong here.
		if plan.Primary != 0 {
			notFirst++
		}
	}

	// Without this the loop can compare nothing and the case still passes,
	// which is how a baseline collapse ships. The corpus in this repository has
	// multi-workload entries; if it stops having them, that is the finding.
	if multi == 0 {
		t.Fatal("no entry in the shipped catalogue has two workloads, so this case " +
			"compared Primary against nothing")
	}
	if len(differ) > 0 {
		t.Errorf("Primary and the probe it replaces disagree about %d of %d entries; one of "+
			"them is wrong and this list is where to look:\n\t%s",
			len(differ), multi, strings.Join(differ, "\n\t"))
	}

	t.Logf("multi-workload entries: %d (%d of them installable). "+
		"The front door is not the first workload in %d of them, which is how often "+
		"guessing instead of reporting would name the wrong object.",
		multi, installableMulti, notFirst)
}
