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

package deploywatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/internal/deploywatch"
)

// The sweep needs no cluster and no sleeping: it is a store operation driven by
// a clock, and the clock is injected.

// The one Ref every case here uses. Named so that a case which deliberately
// uses a second one would stand out.
var testRef = evidence.Ref{TenantID: "tenant-a", App: "api", Env: "prod"}

func openRecord(t *testing.T, s evidence.Store, key string) evidence.Record {
	t.Helper()
	rec, err := s.Append(context.Background(), evidence.Record{
		IdempotencyKey: key,
		Ref:            testRef,
		Tier:           evidence.TierFree,
		Source:         evidence.Source{CommitSHA: key, RepoURL: "https://example.test/r.git"},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return rec
}

// What the sweep is for: a record nothing ever came back about is given up on,
// and the giving up is recorded as not-knowing rather than as a failure or a
// success. An audit record may be incomplete; it may not be confidently wrong.
func TestSweepGivesUpAndSaysSo(t *testing.T) {
	ctx := context.Background()
	store := memory.New(0)
	rec := openRecord(t, store, "commit:old")

	sweep := &deploywatch.Sweep{
		Evidence: store,
		After:    30 * time.Minute,
		// An hour later, without an hour passing.
		Now: func() time.Time { return time.Now().Add(time.Hour) },
	}
	if err := sweep.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != evidence.StateUnknown {
		t.Fatalf("state = %q, want %q", got.State, evidence.StateUnknown)
	}
	if len(got.Transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(got.Transitions))
	}
	ev := got.Transitions[0]
	if ev.Observation.Source != evidence.ObservedFromSweep {
		t.Errorf("observation source = %q, want %q — the page has to be able to say "+
			"nobody looked, rather than implying something did",
			ev.Observation.Source, evidence.ObservedFromSweep)
	}
	if ev.Reason == "" {
		t.Error("the record was given up on with no reason recorded")
	}
}

// The window is a floor, not a suggestion. A rollout that is still legitimately
// rolling must not be given up on — the progress deadline on every Deployment
// this platform renders is ten minutes, so a sweep that fires early would mark
// healthy deploys unknown for a living.
func TestSweepLeavesYoungRecordsAlone(t *testing.T) {
	ctx := context.Background()
	store := memory.New(0)
	rec := openRecord(t, store, "commit:new")

	sweep := &deploywatch.Sweep{
		Evidence: store,
		After:    30 * time.Minute,
		Now:      time.Now, // no time has passed
	}
	if err := sweep.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != evidence.StatePending {
		t.Errorf("state = %q, want it left at %q", got.State, evidence.StatePending)
	}
}

// A record the observer already answered is not the sweep's business, whatever
// its age. This is what stops the sweep rewriting settled history on a slow
// afternoon.
func TestSweepIgnoresObservedRecords(t *testing.T) {
	ctx := context.Background()
	store := memory.New(0)
	rec := openRecord(t, store, "commit:done")

	if _, err := store.Transition(ctx, rec.ID, evidence.Transition{
		From: []evidence.State{evidence.StatePending}, To: evidence.StateRunning,
		At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	sweep := &deploywatch.Sweep{
		Evidence: store, After: time.Minute,
		Now: func() time.Time { return time.Now().Add(24 * time.Hour) },
	}
	if err := sweep.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != evidence.StateRunning {
		t.Errorf("state = %q, want it left at %q", got.State, evidence.StateRunning)
	}
	if len(got.Transitions) != 1 {
		t.Errorf("transitions = %d, want the sweep to have added none", len(got.Transitions))
	}
}

// Running the sweep twice must not write twice. It is on a ticker, so this is
// the ordinary case rather than an edge one.
func TestSweepIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := memory.New(0)
	rec := openRecord(t, store, "commit:old")

	sweep := &deploywatch.Sweep{
		Evidence: store, After: time.Minute,
		Now: func() time.Time { return time.Now().Add(time.Hour) },
	}
	for range 3 {
		if err := sweep.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Transitions) != 1 {
		t.Errorf("transitions = %d after three passes, want 1", len(got.Transitions))
	}
}

// The sweep pages, so more records than one page must all be reached. A sweep
// that silently stops at the first hundred leaves the rest claiming pending for
// ever, which looks exactly like a healthy backlog.
func TestSweepReachesBeyondOnePage(t *testing.T) {
	ctx := context.Background()
	store := memory.New(0)

	const n = 250
	for i := range n {
		openRecord(t, store, "commit:"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}

	sweep := &deploywatch.Sweep{
		Evidence: store, After: time.Minute,
		Now: func() time.Time { return time.Now().Add(time.Hour) },
	}
	if err := sweep.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}

	page, err := store.History(ctx, evidence.Query{
		Ref:    testRef,
		States: []evidence.State{evidence.StatePending},
		Limit:  500,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(page.Records) != 0 {
		t.Errorf("%d records were left pending; the sweep stopped early", len(page.Records))
	}
}

// It runs on one replica. Several racing to give up on the same record would be
// harmless — the version fence lets one win — but it would be several stores'
// worth of work to reach one answer.
func TestSweepWantsLeaderElection(t *testing.T) {
	if !(&deploywatch.Sweep{}).NeedLeaderElection() {
		t.Error("the sweep does not declare NeedLeaderElection, so every replica would run it")
	}
}
