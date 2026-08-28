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
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/damgahq/damga/evidence"
)

// A hand-written wire type drifts the moment somebody adds a field to the
// store type and does not think about the API. The failure is silent: the
// endpoint keeps working and quietly stops reporting whatever was added, which
// for this product means a piece of evidence that was recorded and is not
// shown. This compares the field names and says which one is missing.
func TestTheWireTypesCoverTheStoreTypes(t *testing.T) {
	for _, c := range []struct{ store, wire any }{
		{evidence.Record{}, wireRecord{}},
		{evidence.Ref{}, wireRef{}},
		{evidence.Actor{}, wireActor{}},
		{evidence.Source{}, wireSource{}},
		{evidence.Image{}, wireImage{}},
		{evidence.AdmissionOutcome{}, wireAdmission{}},
		{evidence.Observation{}, wireObservation{}},
		{evidence.Proof{}, wireProof{}},
		{evidence.Anchor{}, wireAnchor{}},
	} {
		storeType := reflect.TypeOf(c.store)
		t.Run(storeType.Name(), func(t *testing.T) {
			missing := missingFields(storeType, reflect.TypeOf(c.wire))
			if len(missing) > 0 {
				t.Errorf("%s has fields the API never shows: %s",
					storeType.Name(), strings.Join(missing, ", "))
			}
		})
	}
}

// Event is deliberately not in that table: the wire form drops PrevHash,
// because a client verifying a chain fetches the records and recomputes it
// rather than trusting a link the same response handed over. Named here so
// that dropping a field stays a decision somebody wrote down.
func TestTheWireEventDropsOnlyWhatItMeansTo(t *testing.T) {
	missing := missingFields(reflect.TypeFor[evidence.Event](), reflect.TypeFor[wireEvent]())
	if want := []string{"PrevHash"}; !slices.Equal(missing, want) {
		t.Errorf("wireEvent omits %v, and the only omission it is meant to have is %v", missing, want)
	}
}

func missingFields(store, wire reflect.Type) []string {
	have := make(map[string]bool, wire.NumField())
	for f := range wire.Fields() {
		have[strings.ToLower(f.Name)] = true
	}
	var missing []string
	for f := range store.Fields() {
		if !have[strings.ToLower(f.Name)] {
			missing = append(missing, f.Name)
		}
	}
	slices.Sort(missing)
	return missing
}

// The two shapes a naive client breaks on: a list that is null, and a
// timestamp that is year one. Both are what encoding/json does by default with
// a zero value, so both are what this API would emit if nobody had looked.
func TestEmptyIsRenderedAsEmptyAndNotAsNull(t *testing.T) {
	b, err := json.Marshal(toWireRecord(evidence.Record{}))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := string(b)
	for _, want := range []string{`"transitions":[]`, `"createdAt":""`} {
		if !strings.Contains(body, want) {
			t.Errorf("an empty record does not contain %s\n%s", want, body)
		}
	}
	if strings.Contains(body, "0001-01-01") {
		t.Errorf("the zero time reached the wire as a date: %s", body)
	}

	// And a real timestamp still survives, so the check above cannot be
	// satisfied by emptying everything.
	at := time.Date(2026, 8, 27, 10, 30, 0, 0, time.UTC)
	b, err = json.Marshal(toWireRecord(evidence.Record{CreatedAt: at}))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"createdAt":"2026-08-27T10:30:00Z"`) {
		t.Errorf("a real timestamp did not survive: %s", b)
	}
}

// The commit reaches the reader where a reader looks for it.
//
// A record opened by the git write path cannot carry the SHA in Source: it has
// to exist before the commit does, and Source is the immutable half. Without
// this, the evidence page shows an empty commit for every deploy damga itself
// made — which is every deploy damga made.
func TestTheCommitIsReportedWhereverItWasRecorded(t *testing.T) {
	const pushedAs = "bbb222"

	// The ordinary case: an observed deploy that already knows its commit.
	direct := toWireRecord(evidence.Record{
		Source: evidence.Source{CommitSHA: "aaa111"},
	})
	if direct.Source.CommitSHA != "aaa111" {
		t.Errorf("Source.CommitSHA = %q, want aaa111", direct.Source.CommitSHA)
	}

	// The damga-originated case: nothing in Source, the SHA on a transition.
	opened := toWireRecord(evidence.Record{
		Transitions: []evidence.Event{
			{To: evidence.StateSyncing, Observation: evidence.Observation{Revision: pushedAs}},
		},
	})
	if opened.Source.CommitSHA != pushedAs {
		t.Errorf("a record opened before its commit reports %q, want bbb222", opened.Source.CommitSHA)
	}

	// Observed more than once: the commit it is about is the one it was
	// pushed as, which is the last revision anything reported.
	twice := toWireRecord(evidence.Record{
		Transitions: []evidence.Event{
			{Observation: evidence.Observation{Revision: pushedAs}},
			{Observation: evidence.Observation{}},
			{Observation: evidence.Observation{Revision: "ccc333"}},
		},
	})
	if twice.Source.CommitSHA != "ccc333" {
		t.Errorf("got %q, want the newest revision ccc333", twice.Source.CommitSHA)
	}

	// And nothing invented when there is nothing to report.
	if empty := toWireRecord(evidence.Record{}); empty.Source.CommitSHA != "" {
		t.Errorf("a record with no commit reports %q", empty.Source.CommitSHA)
	}
}
