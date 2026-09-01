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
	"testing"

	"github.com/damgahq/damga/catalog"
)

// What the catalogue is made of, by shape, for whoever has to choose what to
// prove next.
//
// "Installable" is the planner's answer and not evidence that anything runs.
// One entry has been installed on a real cluster and asked for its own health —
// gotify, in the job above — and a sample of the rest was installed by hand on
// 2026-09-01. What that found is why this census exists: an entry with a second
// workload cannot work today, for two independent reasons, and the census says
// how many entries are in that shape.
//
// The reasons, both measured rather than read:
//
//   - The second workload's name changes. compose calls it `redis`, the
//     platform names the object `<entry>-redis`, and the environment the first
//     workload was given still says `redis` — which resolves to nothing.
//     Measured: cryptgeon's own log is "cannot reach redis", and
//     grafana-with-postgresql dies on "dial tcp: lookup postgresql ... no such
//     host" while its Service is grafana-with-postgresql-postgresql.
//   - Its NetworkPolicy admits the ingress-nginx namespace and nothing else, so
//     even under the right name a sibling cannot connect. Measured with a probe
//     carrying the workload's own labels: an external HTTPS call answered 200
//     and the sibling answered 000.
//
// And a third that only bites a service compose declared no ports for: the CRD
// defaults Port to 8080 and the operator probes http://:8080/healthz, so a
// redis on 6379 is never Ready. The converter already says so in a note — "no
// port declared anywhere; the API's default of 8080 will be used and is
// probably wrong" — and the entry is offered as installable anyway.
//
// Counted and never asserted, for the reason every count in this repository is:
// upstream adds templates weekly.
func TestTheShapesOfWhatTheCatalogueOffers(t *testing.T) {
	c, err := catalog.Load(os.DirFS(shippedTemplates))
	if err != nil {
		t.Fatalf("loading %s: %v", shippedTemplates, err)
	}

	var offered, installable, multi, withDatabase, withVolume, withMint, single int
	for _, e := range c.Entries() {
		offered++
		// fakeDigest rather than a registry client: what is being counted is a
		// property of the corpus, not of the network.
		plan, primary, err := planEntry(c, e.Name, catalog.Options{Namespace: "damga", Pin: fakeDigest})
		if err != nil {
			continue
		}
		_ = primary
		if _, unmintable := plannedSecrets(plan); len(plan.Blockers) > 0 || len(unmintable) > 0 {
			continue
		}
		installable++

		volumes := 0
		for _, w := range plan.Workloads {
			volumes += len(w.Spec.Volumes)
		}
		switch {
		case len(plan.Workloads)+len(plan.Databases) > 1:
			multi++
		default:
			single++
		}
		if len(plan.Databases) > 0 {
			withDatabase++
		}
		if volumes > 0 {
			withVolume++
		}
		if len(plan.Mint) > 0 {
			withMint++
		}
	}

	if installable == 0 {
		t.Fatal("nothing is installable, so every count below is zero for the wrong reason")
	}
	t.Logf("of %d entries offered, %d are installable: %d are one object and %d are more than one",
		offered, installable, single, multi)
	t.Logf("%d bring a Database, %d claim a volume, %d need a value minted",
		withDatabase, withVolume, withMint)
	t.Logf("the %d multi-object entries are the ones nothing has ever run: see this test's comment",
		multi)
}
