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

// Package storetest is the suite every evidence.Store has to pass.
//
// It exists because there will be more than one implementation and they have
// to be interchangeable in a stronger sense than "they compile". The free
// store, an in-process store used by tests, and whatever damga-ee substitutes
// all make the same promises to the evidence page and to an auditor; a promise
// that only some of them keep is worse than one none of them keeps, because it
// is the kind that is discovered by a customer.
//
// It is also the cheap half of the dual-engine decision. No single SQL
// statement is correct on both SQLite and PostgreSQL — BEGIN IMMEDIATE
// serialises writers on one and does nothing on the other, SELECT … FOR UPDATE
// parses on one and not the other. Running this suite against both is what
// turns that from an audit of every write path into a build failure.
//
// Each case below is a claim from the design under test, not coverage. The
// comment on each says which claim, so that a failure is read as "this promise
// is broken" rather than "a test is red".
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/damgahq/damga/evidence"
)

// Factory makes a fresh, empty store with the given retention window; zero
// means unbounded. Called once per case, because the cases assume they own the
// store and a shared one would make Seq and the hash chain depend on test
// order. The window is a parameter rather than a fixture so that the retention
// case can prove a real deletion instead of asserting over a store that never
// deletes anything.
type Factory func(t *testing.T, window time.Duration) evidence.Store

// Run executes the whole suite against one implementation.
//
//	storetest.Run(t, func(t *testing.T, w time.Duration) evidence.Store {
//		return memory.New(w)
//	})
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"AppendAssignsIdentity", testAppendAssignsIdentity},
		{"AppendRequiresIdempotencyKey", testAppendRequiresIdempotencyKey},
		{"DuplicateReturnsExistingRecord", testDuplicateReturnsExistingRecord},
		{"SeqIsPerRefAndGapless", testSeqIsPerRefAndGapless},
		{"TransitionIsCompareAndSet", testTransitionIsCompareAndSet},
		{"RacingTransitionsProduceOneWinner", testRacingTransitionsProduceOneWinner},
		{"VerifyHoldsAfterTransition", testVerifyHoldsAfterTransition},
		{"HashSurvivesARoundTrip", testHashSurvivesARoundTrip},
		{"CurrentPrefersRunning", testCurrentPrefersRunning},
		{"FindBySourceResolvesCommit", testFindBySourceResolvesCommit},
		{"HistoryPagesWithoutOffset", testHistoryPagesWithoutOffset},
		{"PruneNeverRemovesCurrent", testPruneNeverRemovesCurrent},
		{"ExportRoundTripsAndVerifies", testExportRoundTripsAndVerifies},
		{"NotFoundIsDistinguishable", testNotFoundIsDistinguishable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore) })
	}
}

// ---------------------------------------------------------------- helpers

func ref(app, env string) evidence.Ref {
	return evidence.Ref{TenantID: "tenant-a", App: app, Env: env}
}

// prod is the environment almost every case uses. Named so that the cases
// which deliberately use a second one stand out.
const (
	prod    = "prod"
	staging = "staging"
)

// rec is a complete record rather than a minimal one. A store that only keeps
// the fields the suite asserts on would pass a thinner fixture, and the field
// it silently drops would be the one an auditor asks about.
func rec(key string, r evidence.Ref, sha string) evidence.Record {
	return evidence.Record{
		IdempotencyKey: key,
		Ref:            r,
		Tier:           evidence.TierFree,
		Actor: evidence.Actor{
			ID: "u-1", Kind: "user",
			DisplayName: "Orhan Yavuz", Email: "orhan@example.test",
		},
		Source: evidence.Source{
			RepoURL: "https://github.com/damgahq/tenant-a.git", Ref: "main",
			Path: "apps/" + r.App, CommitSHA: sha,
			AuthorEmail: "orhan@example.test", CommitterEmail: "platform@damga.co",
		},
		Image: evidence.Image{RequestedRef: "ghcr.io/damgahq/damga:1.0.0"},
		Signature: evidence.SignatureVerdict{
			Verified: true,
			Issuer:   "https://token.actions.githubusercontent.com",
			Subject:  "https://github.com/damgahq/damga/.github/workflows/ci.yml@refs/heads/main",
			Digest:   "sha256:" + sha,
		},
		Policies: []evidence.PolicyResult{{
			Name: "damga-image-provenance", Source: "ValidatingAdmissionPolicy",
			Result: "pass", Severity: "high", Category: "Supply chain",
		}},
		Admission: evidence.AdmissionOutcome{Allowed: true},
		Note:      "conformance fixture",
	}
}

func mustAppend(t *testing.T, s evidence.Store, r evidence.Record) evidence.Record {
	t.Helper()
	got, err := s.Append(context.Background(), r)
	if err != nil {
		t.Fatalf("Append(%s): %v", r.IdempotencyKey, err)
	}
	return got
}

func mustTransition(
	t *testing.T, s evidence.Store, id evidence.ID, from []evidence.State, to evidence.State,
) evidence.Record {
	t.Helper()
	got, err := s.Transition(context.Background(), id, evidence.Transition{
		From: from, To: to, At: time.Now().UTC(),
		Observation: evidence.Observation{Source: evidence.ObservedFromWorkload, At: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("Transition(%s -> %s): %v", id, to, err)
	}
	return got
}

// ---------------------------------------------------------------- cases

// The store assigns identity; the caller never does. If a caller could choose
// the ID, two writers would collide on it and the chain would fork.
func testAppendAssignsIdentity(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	got := mustAppend(t, s, rec("commit:aaa:apps/api", ref("api", prod), "aaa"))

	if got.ID == "" {
		t.Error("Append returned an empty ID")
	}
	if got.Seq == 0 {
		t.Error("Append returned Seq 0; Seq is 1-based and gapless per Ref")
	}
	if got.State != evidence.StatePending {
		t.Errorf("State = %q, want %q: a record with no state named is not yet applied", got.State, evidence.StatePending)
	}
	if got.InitialState != got.State {
		t.Errorf("InitialState = %q, State = %q: the chained half must record what it was appended as",
			got.InitialState, got.State)
	}
	if len(got.Hash) == 0 {
		t.Error("Append returned no Hash; an unchained record cannot be part of an immutability claim")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// Without a key, Append is neither retry-safe nor replay-safe, and a restarted
// writer doubles every row on the evidence page.
func testAppendRequiresIdempotencyKey(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	r := rec("", ref("api", prod), "aaa")
	if _, err := s.Append(context.Background(), r); err == nil {
		t.Fatal("Append accepted an empty IdempotencyKey")
	}
}

// The duplicate must come back with the record that is already stored, not
// just an error. That is what lets a restarted observer re-append everything
// it can still see and keep only what was new, without a read-before-write
// race.
func testDuplicateReturnsExistingRecord(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	first := mustAppend(t, s, rec("commit:aaa:apps/api", ref("api", prod), "aaa"))

	again, err := s.Append(context.Background(), rec("commit:aaa:apps/api", ref("api", prod), "aaa"))
	if !errors.Is(err, evidence.ErrDuplicate) {
		t.Fatalf("second Append error = %v, want ErrDuplicate", err)
	}
	if again.ID != first.ID {
		t.Errorf("duplicate returned ID %q, want the stored %q", again.ID, first.ID)
	}
	if again.Seq != first.Seq {
		t.Errorf("duplicate returned Seq %d, want %d: a rejected append must not consume a sequence number",
			again.Seq, first.Seq)
	}
}

// "the 41st deploy of api/prod" has to mean that. Per-Ref, so a busy app does
// not renumber a quiet one.
func testSeqIsPerRefAndGapless(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	api, web, apiStaging := ref("api", prod), ref("web", prod), ref("api", staging)

	for i := 1; i <= 3; i++ {
		got := mustAppend(t, s, rec(fmt.Sprintf("commit:api%d", i), api, fmt.Sprintf("a%02d", i)))
		if got.Seq != int64(i) {
			t.Fatalf("api append %d got Seq %d, want %d", i, got.Seq, i)
		}
	}
	got := mustAppend(t, s, rec("commit:web1", web, "w01"))
	if got.Seq != 1 {
		t.Errorf("first web append got Seq %d, want 1: Seq is per Ref, not global", got.Seq)
	}

	// Same app, different environment. The environment is part of the Ref, so
	// deploying api to staging must not renumber api in production — "the 41st
	// deploy of api/prod" has to survive a busy staging.
	got = mustAppend(t, s, rec("commit:apistg1", apiStaging, "s01"))
	if got.Seq != 1 {
		t.Errorf("first api/staging append got Seq %d, want 1: Env is part of the Ref", got.Seq)
	}
}

// A transition names the states it believes the record may be in. Without
// that, a slow observer overwrites a newer verdict with an older one.
func testTransitionIsCompareAndSet(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	r := mustAppend(t, s, rec("commit:aaa", ref("api", prod), "aaa"))

	got := mustTransition(t, s, r.ID, []evidence.State{evidence.StatePending}, evidence.StateApplied)
	if got.State != evidence.StateApplied {
		t.Fatalf("State = %q, want applied", got.State)
	}
	if len(got.Transitions) != 1 {
		t.Fatalf("Transitions = %d, want 1: each change is its own link in the chain", len(got.Transitions))
	}
	if got.Transitions[0].From != evidence.StatePending {
		t.Errorf("event From = %q, want pending", got.Transitions[0].From)
	}
	if len(got.Transitions[0].Hash) == 0 {
		t.Error("the transition carries no Hash; a state change outside the chain is unaccounted history")
	}

	_, err := s.Transition(context.Background(), r.ID, evidence.Transition{
		From: []evidence.State{evidence.StatePending}, To: evidence.StateRunning, At: time.Now().UTC(),
	})
	if !errors.Is(err, evidence.ErrConflict) {
		t.Fatalf("stale transition error = %v, want ErrConflict", err)
	}

	if _, err := s.Transition(context.Background(), evidence.ID("no-such-id"), evidence.Transition{
		To: evidence.StateRunning, At: time.Now().UTC(),
	}); !errors.Is(err, evidence.ErrNotFound) {
		t.Errorf("transition on a missing record = %v, want ErrNotFound", err)
	}
}

// Two observers is the normal case, not the exotic one: a lease expiry hands
// the same work to a new leader while the old one is still finishing. Exactly
// one must win.
func testRacingTransitionsProduceOneWinner(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	r := mustAppend(t, s, rec("commit:aaa", ref("api", prod), "aaa"))

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = s.Transition(context.Background(), r.ID, evidence.Transition{
				From: []evidence.State{evidence.StatePending},
				To:   evidence.StateApplied, At: time.Now().UTC(),
				Observation: evidence.Observation{Source: evidence.ObservedFromWorkload},
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var won, conflicted int
	for i, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, evidence.ErrConflict):
			conflicted++
		default:
			t.Errorf("racer %d: unexpected error %v", i, err)
		}
	}
	if won != 1 {
		t.Errorf("%d racers succeeded, want exactly 1", won)
	}
	if conflicted != racers-1 {
		t.Errorf("%d racers got ErrConflict, want %d", conflicted, racers-1)
	}

	final, err := s.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(final.Transitions) != 1 {
		t.Errorf("record has %d transitions after the race, want 1", len(final.Transitions))
	}
}

// The defect this suite exists to keep fixed. Hashing the record as it stands
// breaks the moment it transitions, because State, UpdatedAt and Transitions
// all change — and a store that cannot verify a record it has just written
// cannot carry an immutability claim at any price.
func testVerifyHoldsAfterTransition(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	ctx := context.Background()
	r := ref("api", prod)

	ids := make([]evidence.ID, 0, 3)
	for i := 1; i <= 3; i++ {
		ids = append(ids, mustAppend(t, s, rec(fmt.Sprintf("commit:%02d", i), r, fmt.Sprintf("c%02d", i))).ID)
	}

	proof, err := s.Verify(ctx, r, "", "")
	if err != nil {
		t.Fatalf("Verify after appends: %v", err)
	}
	if !proof.Valid {
		t.Fatalf("chain invalid straight after append, first break at Seq %d", proof.BrokenAt)
	}

	for _, id := range ids {
		mustTransition(t, s, id, []evidence.State{evidence.StatePending}, evidence.StateApplied)
	}
	mustTransition(t, s, ids[2], []evidence.State{evidence.StateApplied}, evidence.StateRunning)

	proof, err = s.Verify(ctx, r, "", "")
	if err != nil {
		t.Fatalf("Verify after transitions: %v", err)
	}
	if !proof.Valid {
		t.Fatalf("chain invalid after transitions, first break at Seq %d — "+
			"the chain is being computed over mutable fields", proof.BrokenAt)
	}
	if proof.Records != 3 {
		t.Errorf("Proof.Records = %d, want 3", proof.Records)
	}
	if len(proof.RootHash) == 0 {
		t.Error("Proof has no RootHash")
	}
}

// The bug this case exists for was found by CI and hidden by a laptop. The
// chain hashed whatever time.Now() returned while the store persisted
// microseconds — so on Darwin, whose clock is already microseconds, the
// truncation was a no-op and every test passed; on Linux the nanoseconds were
// dropped on the way to disk and the record read back with a different hash
// than the one written. An archive written by one node and verified on another
// is the entire promise, so a hash that depends on the host clock is worthless.
//
// The assertion that catches it everywhere is not "the chain verifies" — that
// is what passed on the laptop. It is that a store never hands back a
// timestamp finer than the one it can store.
func testHashSurvivesARoundTrip(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	ctx := context.Background()
	r := ref("api", prod)

	appended := mustAppend(t, s, rec("commit:aaa", r, "aaa"))
	if got := appended.CreatedAt; !got.Equal(evidence.Canonical(got)) {
		t.Errorf("Append returned CreatedAt %s, finer than evidence.Precision (%s); "+
			"it cannot be persisted and read back unchanged",
			got.Format(time.RFC3339Nano), evidence.Precision)
	}

	// A deliberately sub-precision instant, which is what a real caller hands
	// in: time.Now() on Linux has nanoseconds.
	odd := time.Now().UTC().Truncate(time.Second).Add(1234567 * time.Nanosecond)
	moved, err := s.Transition(ctx, appended.ID, evidence.Transition{
		From: []evidence.State{evidence.StatePending}, To: evidence.StateApplied,
		At:          odd,
		Observation: evidence.Observation{Source: evidence.ObservedFromWorkload, At: odd},
	})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if len(moved.Transitions) != 1 {
		t.Fatalf("Transitions = %d, want 1", len(moved.Transitions))
	}
	if got := moved.Transitions[0].At; !got.Equal(evidence.Canonical(got)) {
		t.Errorf("Transition kept At at %s, finer than evidence.Precision", got.Format(time.RFC3339Nano))
	}

	// The round trip itself: what comes back out has to hash to what went in.
	stored, err := s.Get(ctx, appended.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(stored.Hash, appended.Hash) {
		t.Error("stored Hash differs from the one Append returned; " +
			"the record does not survive its own store")
	}
	if !stored.CreatedAt.Equal(appended.CreatedAt) {
		t.Errorf("CreatedAt = %s after a round trip, was %s",
			stored.CreatedAt.Format(time.RFC3339Nano), appended.CreatedAt.Format(time.RFC3339Nano))
	}
	if got := evidence.ChainRecord(stored.PrevHash, stored); !bytes.Equal(got, stored.Hash) {
		t.Error("recomputing the chain over the record as it was read back does not reproduce its hash")
	}
	if ev := stored.Transitions[0]; !bytes.Equal(evidence.ChainEvent(stored.Hash, stored.ID, ev), ev.Hash) {
		t.Error("recomputing the chain over the event as it was read back does not reproduce its hash")
	}
}

// The live evidence page reads exactly this. Newest running wins; if nothing
// is running yet, the newest applied — so the page shows what is actually
// serving traffic rather than what was asked for most recently.
func testCurrentPrefersRunning(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	ctx := context.Background()
	r := ref("api", prod)

	if _, err := s.Current(ctx, r); !errors.Is(err, evidence.ErrNotFound) {
		t.Errorf("Current on an empty store = %v, want ErrNotFound", err)
	}

	first := mustAppend(t, s, rec("commit:01", r, "c01"))
	mustTransition(t, s, first.ID, []evidence.State{evidence.StatePending}, evidence.StateApplied)
	mustTransition(t, s, first.ID, []evidence.State{evidence.StateApplied}, evidence.StateRunning)

	second := mustAppend(t, s, rec("commit:02", r, "c02"))

	got, err := s.Current(ctx, r)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("Current = %q, want the running record %q: a pending deploy is not what is serving", got.ID, first.ID)
	}

	mustTransition(t, s, second.ID, []evidence.State{evidence.StatePending}, evidence.StateApplied)
	mustTransition(t, s, second.ID, []evidence.State{evidence.StateApplied}, evidence.StateRunning)

	got, err = s.Current(ctx, r)
	if err != nil {
		t.Fatalf("Current after second rollout: %v", err)
	}
	if got.ID != second.ID {
		t.Errorf("Current = %q, want the newer running record %q", got.ID, second.ID)
	}
}

// The observer knows a revision and nothing else. Without this it cannot
// attach what it saw to the row the git writer already opened, and every
// observation becomes a second row for the same deploy.
func testFindBySourceResolvesCommit(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	ctx := context.Background()
	repo := "https://github.com/damgahq/tenant-a.git"

	want := mustAppend(t, s, rec("commit:abc", ref("api", prod), "abc"))
	mustAppend(t, s, rec("commit:def", ref("api", prod), "def"))

	got, err := s.FindBySource(ctx, repo, "abc")
	if err != nil {
		t.Fatalf("FindBySource: %v", err)
	}
	if len(got) != 1 || got[0].ID != want.ID {
		t.Fatalf("FindBySource returned %d records, want exactly the one for abc", len(got))
	}

	none, err := s.FindBySource(ctx, repo, "nosuchsha")
	if err != nil {
		t.Fatalf("FindBySource for an unknown sha: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("FindBySource for an unknown sha returned %d records, want 0", len(none))
	}
}

// Keyset, never offset. An archive being appended to while it is paged would
// skip and repeat rows under an offset, and those are exactly the rows an
// auditor is reading.
func testHistoryPagesWithoutOffset(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	ctx := context.Background()
	r := ref("api", prod)

	const total = 7
	for i := 1; i <= total; i++ {
		mustAppend(t, s, rec(fmt.Sprintf("commit:%02d", i), r, fmt.Sprintf("c%02d", i)))
	}

	seen := map[evidence.ID]bool{}
	var cursor evidence.Cursor
	for pages := 0; ; pages++ {
		if pages > total+2 {
			t.Fatal("History did not terminate; Next is not advancing")
		}
		page, err := s.History(ctx, evidence.Query{Ref: r, Limit: 3, After: cursor, Order: evidence.OrderOldest})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		for _, got := range page.Records {
			if seen[got.ID] {
				t.Errorf("record %q returned twice across pages", got.ID)
			}
			seen[got.ID] = true
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(seen) != total {
		t.Errorf("paged over %d records, want %d", len(seen), total)
	}
}

// The flagship free page must not be able to go blank. A retention sweep that
// can reach the record a Ref is currently on is not a retention policy, it is
// an outage on a timer.
func testPruneNeverRemovesCurrent(t *testing.T, newStore Factory) {
	// A window short enough that everything written here is already outside it
	// by the time Prune runs, so the sweep has to actively refuse the current
	// record rather than simply find nothing to do.
	s := newStore(t, time.Nanosecond)
	ctx := context.Background()
	r := ref("api", prod)

	old := mustAppend(t, s, rec("commit:old", r, "old"))
	mustTransition(t, s, old.ID, []evidence.State{evidence.StatePending}, evidence.StateApplied)
	mustTransition(t, s, old.ID, []evidence.State{evidence.StateApplied}, evidence.StateRunning)

	live := mustAppend(t, s, rec("commit:live", r, "liv"))
	mustTransition(t, s, live.ID, []evidence.State{evidence.StatePending}, evidence.StateApplied)
	mustTransition(t, s, live.ID, []evidence.State{evidence.StateApplied}, evidence.StateRunning)

	// Far enough in the future that every record is outside any finite window.
	res, err := s.Prune(ctx, time.Now().Add(100*365*24*time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Deleted == 0 {
		t.Errorf("Prune deleted nothing with a 1ns window over %d records; "+
			"the case cannot prove the current record was spared", res.Examined)
	}

	got, err := s.Current(ctx, r)
	if err != nil {
		t.Fatalf("Current after Prune deleted %d records: %v — the live page is blank", res.Deleted, err)
	}
	if got.ID != live.ID {
		t.Errorf("Current after Prune = %q, want %q", got.ID, live.ID)
	}

	pol, err := s.Retention(ctx)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if !pol.KeepCurrent {
		t.Error("Retention reports KeepCurrent=false; a conforming store always keeps the current record")
	}
}

// An export handed to an auditor has to verify on its own, and two stores
// given the same records have to produce the same bytes — otherwise the file
// only proves something about which implementation wrote it.
func testExportRoundTripsAndVerifies(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	ctx := context.Background()
	r := ref("api", prod)

	for i := 1; i <= 3; i++ {
		got := mustAppend(t, s, rec(fmt.Sprintf("commit:%02d", i), r, fmt.Sprintf("c%02d", i)))
		mustTransition(t, s, got.ID, []evidence.State{evidence.StatePending}, evidence.StateApplied)
	}

	var buf bytes.Buffer
	res, err := s.Export(ctx, evidence.ExportRequest{
		Query:  evidence.Query{Ref: r, Order: evidence.OrderOldest},
		Format: evidence.ExportJSONL,
	}, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Records != 3 {
		t.Errorf("Export wrote %d records, want 3", res.Records)
	}
	if buf.Len() == 0 {
		t.Fatal("Export wrote no bytes")
	}
	if int64(buf.Len()) != res.Bytes {
		t.Errorf("Export reported %d bytes, wrote %d", res.Bytes, buf.Len())
	}
	if len(res.LastHash) == 0 {
		t.Error("Export reported no LastHash; the file cannot be tied back to the store")
	}

	proof, err := s.Verify(ctx, r, "", "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !proof.Valid {
		t.Errorf("chain invalid at Seq %d after export", proof.BrokenAt)
	}
	if !bytes.Equal(proof.RootHash, res.LastHash) {
		t.Error("Export.LastHash and Proof.RootHash disagree; the export is not anchored to the chain it came from")
	}
}

// Callers branch on these. A store that returns a bare error for a missing
// record turns "no deploys yet" into a 500 on the evidence page.
func testNotFoundIsDistinguishable(t *testing.T, newStore Factory) {
	s := newStore(t, 0)
	ctx := context.Background()

	if _, err := s.Get(ctx, evidence.ID("nope")); !errors.Is(err, evidence.ErrNotFound) {
		t.Errorf("Get on a missing id = %v, want ErrNotFound", err)
	}
	if _, err := s.Current(ctx, ref("nosuch", prod)); !errors.Is(err, evidence.ErrNotFound) {
		t.Errorf("Current on an unknown Ref = %v, want ErrNotFound", err)
	}
}
