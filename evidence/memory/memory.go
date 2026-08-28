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

// Package memory is a complete, in-process evidence store. It exists so that
// `damga` runs with no database for a demo and so the tests do not need one —
// not as a reduced implementation: every method of evidence.Store is here.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/damgahq/damga/evidence"
)

type Store struct {
	mu    sync.Mutex
	byID  map[evidence.ID]*evidence.Record
	byKey map[string]evidence.ID
	order []evidence.ID
	seq   map[evidence.Ref]int64
	// last is the head of each Ref's record chain. Keyed by Ref and not by
	// tenant: Verify is asked about one Ref, so a tenant-wide chain over two
	// apps deploying concurrently could never be reproduced by walking either
	// one of them. Events chain off their own record, not off this.
	last   map[evidence.Ref][]byte
	window time.Duration
	now    func() time.Time
}

func New(window time.Duration) *Store {
	return &Store{
		byID: map[evidence.ID]*evidence.Record{}, byKey: map[string]evidence.ID{},
		seq: map[evidence.Ref]int64{}, last: map[evidence.Ref][]byte{},
		window: window, now: time.Now,
	}
}

func (s *Store) Append(_ context.Context, rec evidence.Record) (evidence.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.IdempotencyKey == "" {
		return evidence.Record{}, fmt.Errorf("evidence: IdempotencyKey is required")
	}
	if id, ok := s.byKey[rec.IdempotencyKey]; ok {
		return *s.byID[id], fmt.Errorf("%w: %s", evidence.ErrDuplicate, rec.IdempotencyKey)
	}
	if rec.State == "" {
		rec.State = evidence.StatePending
	}
	rec.InitialState = rec.State
	s.seq[rec.Ref]++
	rec.Seq = s.seq[rec.Ref]
	rec.ID = evidence.ID(fmt.Sprintf("%d-%s", s.now().UnixNano(), rec.IdempotencyKey))
	now := evidence.Canonical(s.now())
	rec.CreatedAt, rec.UpdatedAt = now, now
	rec.PrevHash = s.last[rec.Ref]
	rec.Hash = evidence.ChainRecord(rec.PrevHash, rec)
	s.last[rec.Ref] = rec.Hash
	s.byID[rec.ID] = &rec
	s.byKey[rec.IdempotencyKey] = rec.ID
	s.order = append(s.order, rec.ID)
	return rec, nil
}

func (s *Store) Transition(_ context.Context, id evidence.ID, t evidence.Transition) (evidence.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The store will not invent this. It stamps its own CreatedAt and
	// UpdatedAt, which are facts about the row, but At is a fact about the
	// world: when the deploy actually reached this state. Defaulting it to
	// now() would be right for a transition written the moment it happened
	// and wrong for exactly the case the version fence exists for — an
	// observation computed before a leader handover and written after it,
	// which would then be recorded minutes late and hashed that way forever.
	//
	// A zero At is chained like any other value, so this is not a cosmetic
	// gap: Verify would report the chain valid over an event that claims to
	// have happened in year one.
	if t.At.IsZero() {
		return evidence.Record{}, fmt.Errorf("%w: a transition needs the time it happened", evidence.ErrInvalid)
	}
	rec, ok := s.byID[id]
	if !ok {
		return evidence.Record{}, evidence.ErrNotFound
	}
	if len(t.From) > 0 && !slices.Contains(t.From, rec.State) {
		return *rec, fmt.Errorf("%w: record is %s, expected one of %v", evidence.ErrConflict, rec.State, t.From)
	}
	// The version fence. Checked with the state and under the same lock, so a
	// write derived from an older view of the record loses even when the state
	// it expected happens to be the state it finds.
	if t.ExpectEvents != nil && *t.ExpectEvents != len(rec.Transitions) {
		return *rec, fmt.Errorf("%w: record is at version %d, caller derived from %d",
			evidence.ErrConflict, len(rec.Transitions), *t.ExpectEvents)
	}
	ev := evidence.Event{
		From: rec.State, To: t.To, At: evidence.Canonical(t.At),
		Reason: t.Reason, Observation: t.Observation,
	}
	ev.Observation.At = evidence.Canonical(ev.Observation.At)
	// A record's events chain off the record itself, then off each other. Not
	// off the Ref head: appends and transitions interleave, and a verifier
	// walking records in Seq order cannot reproduce that interleaving. This
	// way each record carries a self-contained sub-chain.
	ev.PrevHash = rec.Hash
	if n := len(rec.Transitions); n > 0 {
		ev.PrevHash = rec.Transitions[n-1].Hash
	}
	ev.Hash = evidence.ChainEvent(ev.PrevHash, rec.ID, ev)
	rec.Transitions = append(rec.Transitions, ev)
	rec.State = t.To
	rec.UpdatedAt = evidence.Canonical(s.now())
	if t.Admission != nil {
		rec.Admission = *t.Admission
	}
	if t.Image != nil {
		rec.Image = *t.Image
	}
	return *rec, nil
}

func (s *Store) Get(_ context.Context, id evidence.ID) (evidence.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return evidence.Record{}, evidence.ErrNotFound
	}
	return *rec, nil
}

func (s *Store) Current(_ context.Context, ref evidence.Ref) (evidence.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *evidence.Record
	for _, id := range s.order {
		r := s.byID[id]
		if r.Ref != ref {
			continue
		}
		if r.State != evidence.StateRunning && r.State != evidence.StateApplied {
			continue
		}
		if best == nil || r.Seq > best.Seq {
			best = r
		}
	}
	if best == nil {
		return evidence.Record{}, evidence.ErrNotFound
	}
	return *best, nil
}

func (s *Store) FindBySource(_ context.Context, repoURL, sha string) ([]evidence.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []evidence.Record
	for _, id := range s.order {
		if r := s.byID[id]; r.Source.RepoURL == repoURL && r.Source.CommitSHA == sha {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *Store) History(_ context.Context, q evidence.Query) (evidence.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ids := slices.Clone(s.order)
	if q.Order != evidence.OrderOldest {
		slices.Reverse(ids)
	}
	var page evidence.Page
	started := q.After == ""
	for _, id := range ids {
		if !started {
			started = evidence.Cursor(id) == q.After
			continue
		}
		r := s.byID[id]
		if !match(r, q) {
			continue
		}
		if len(page.Records) == limit {
			page.Next = evidence.Cursor(page.Records[len(page.Records)-1].ID)
			break
		}
		page.Records = append(page.Records, *r)
	}
	// A cursor that was never reached names a record this Ref does not have.
	// Returning an empty page for it would be indistinguishable from the end
	// of the log, so a client paging with a mangled cursor would stop early
	// and believe it had read everything.
	if !started {
		return evidence.Page{}, fmt.Errorf("%w: no record for cursor %q", evidence.ErrInvalid, q.After)
	}
	return page, nil
}

func match(r *evidence.Record, q evidence.Query) bool {
	if q.Ref.TenantID != "" && r.Ref.TenantID != q.Ref.TenantID {
		return false
	}
	if q.Ref.App != "" && r.Ref.App != q.Ref.App {
		return false
	}
	if q.Ref.Env != "" && r.Ref.Env != q.Ref.Env {
		return false
	}
	if len(q.States) > 0 && !slices.Contains(q.States, r.State) {
		return false
	}
	if !q.Since.IsZero() && r.CreatedAt.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && r.CreatedAt.After(q.Until) {
		return false
	}
	return true
}

func (s *Store) Export(ctx context.Context, req evidence.ExportRequest, w io.Writer) (evidence.ExportResult, error) {
	// The format is checked rather than ignored. ExportCSV is a declared
	// constant that nothing here writes, and an Export that quietly returned
	// JSONL for it would hand somebody a file whose name, content type and
	// contents disagree — and they would find out when their spreadsheet
	// opened one long column.
	//
	// CSV is not implemented rather than not wanted. A record has nested
	// policies and transitions, so flattening it is a real decision about what
	// a row is and what is lost, and an export that loses the policy results
	// cannot be re-verified. That is a format to design, not to infer here.
	switch req.Format {
	case "", evidence.ExportJSONL:
	default:
		return evidence.ExportResult{}, fmt.Errorf(
			"%w: export format %q is not implemented", evidence.ErrInvalid, req.Format)
	}
	q := req.Query
	q.Order = evidence.OrderOldest
	var res evidence.ExportResult
	counted := &countingWriter{w: w}
	enc := json.NewEncoder(counted)
	for {
		page, err := s.History(ctx, q)
		if err != nil {
			return res, err
		}
		for _, r := range page.Records {
			if res.Records == 0 {
				res.FirstHash = r.Hash
			}
			res.LastHash = r.Hash
			if err := enc.Encode(r); err != nil {
				return res, err
			}
			res.Records++
		}
		if page.Next == "" {
			break
		}
		q.After = page.Next
	}
	res.Bytes = counted.n
	res.Written = s.now()
	return res, nil
}

// countingWriter reports what was actually written. ExportResult.Bytes is what
// ties a file on disk to the export that produced it, so leaving it zero makes
// the result unfalsifiable.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (s *Store) Retention(context.Context) (evidence.RetentionPolicy, error) {
	return evidence.RetentionPolicy{
		Window: s.window, KeepCurrent: true, Immutable: false,
	}, nil
}

func (s *Store) Prune(ctx context.Context, now time.Time) (evidence.PruneResult, error) {
	if s.window == 0 {
		return evidence.PruneResult{}, nil
	}
	cutoff := now.Add(-s.window)
	current := map[evidence.ID]bool{}
	s.mu.Lock()
	refs := map[evidence.Ref]bool{}
	for _, id := range s.order {
		refs[s.byID[id].Ref] = true
	}
	s.mu.Unlock()
	for ref := range refs {
		if r, err := s.Current(ctx, ref); err == nil {
			current[r.ID] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var res evidence.PruneResult
	kept := s.order[:0]
	for _, id := range s.order {
		r := s.byID[id]
		res.Examined++
		if r.CreatedAt.Before(cutoff) && !current[id] {
			delete(s.byID, id)
			delete(s.byKey, r.IdempotencyKey)
			res.Deleted++
			continue
		}
		if res.Oldest.IsZero() || r.CreatedAt.Before(res.Oldest) {
			res.Oldest = r.CreatedAt
		}
		kept = append(kept, id)
	}
	s.order = kept
	return res, nil
}

func (s *Store) Verify(ctx context.Context, ref evidence.Ref, from, to evidence.Cursor) (evidence.Proof, error) {
	page, err := s.History(ctx, evidence.Query{Ref: ref, Order: evidence.OrderOldest, After: from, Limit: 200})
	if err != nil {
		return evidence.Proof{}, err
	}
	proof := evidence.Proof{Ref: ref, Valid: true, CheckedAt: s.now()}
	var prev []byte
	for _, r := range page.Records {
		if to != "" && evidence.Cursor(r.ID) > to {
			break
		}
		if got := evidence.ChainRecord(prev, r); !bytes.Equal(got, r.Hash) {
			proof.Valid, proof.BrokenAt = false, r.Seq
			break
		}
		// The record's own sub-chain, which starts at the record and never
		// rejoins the Ref chain. RootHash below is therefore the head of the
		// record chain, which is what an export anchors itself to.
		evPrev := r.Hash
		for _, ev := range r.Transitions {
			if got := evidence.ChainEvent(evPrev, r.ID, ev); !bytes.Equal(got, ev.Hash) {
				proof.Valid, proof.BrokenAt = false, r.Seq
				return proof, nil
			}
			evPrev = ev.Hash
		}
		prev = r.Hash
		proof.Records++
		proof.ToSeq = r.Seq
		if proof.FromSeq == 0 {
			proof.FromSeq = r.Seq
		}
	}
	proof.RootHash = prev
	return proof, nil
}

func (s *Store) Close() error { return nil }

var _ evidence.Store = (*Store)(nil)

// Refs is evidence.Store.Refs.
//
// Built from the live records rather than from s.seq, which keeps a Ref's
// counter after its records are pruned: a gapless Seq has to survive pruning,
// but a Ref whose records are all gone is not something to offer the panel as
// somewhere to look.
func (s *Store) Refs(_ context.Context, tenantID string) ([]evidence.Ref, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: Refs needs a tenant", evidence.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := map[evidence.Ref]bool{}
	var out []evidence.Ref
	for _, rec := range s.byID {
		if rec.Ref.TenantID != tenantID || seen[rec.Ref] {
			continue
		}
		seen[rec.Ref] = true
		out = append(out, rec.Ref)
	}
	// Map iteration is deliberately unordered in Go, so this has to sort or
	// the panel's list would reshuffle on every load — and the SQL stores,
	// which order in the query, would disagree with this one.
	slices.SortFunc(out, func(a, b evidence.Ref) int {
		if c := strings.Compare(a.App, b.App); c != 0 {
			return c
		}
		return strings.Compare(a.Env, b.Env)
	})
	return out, nil
}
