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

package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/damgahq/damga/evidence"
)

// timeLayout is fixed-width RFC3339 in UTC. Fixed width matters: it makes
// lexicographic order chronological, on both engines, with no CAST.
const timeLayout = "2006-01-02T15:04:05.000000Z"

func nowText() string { return time.Now().UTC().Format(timeLayout) }

func asText(t time.Time) string { return t.UTC().Format(timeLayout) }

func fromText(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, s)
}

// Store is an evidence.Store backed by a SQL database.
type Store struct {
	// Writers and readers are separate pools. On SQLite the writer is pinned to
	// one connection because there is one write lock, and letting database/sql
	// open a second writer converts a lock that would have been waited on into
	// a SQLITE_BUSY returned to the caller. On PostgreSQL both are ordinary
	// pools; the split still buys a reader that a long write cannot starve.
	w      *sql.DB
	r      *sql.DB
	d      Dialect
	window time.Duration
	now    func() time.Time
}

// Every query in this file is written with '?' placeholders and goes through
// one of these, so that the dialect is applied in exactly one place per verb
// rather than at each of the twenty call sites.

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.r.QueryRowContext(ctx, s.d.Rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.r.QueryContext(ctx, s.d.Rebind(q), args...)
}

func (s *Store) txQueryRow(ctx context.Context, tx *sql.Tx, q string, args ...any) *sql.Row {
	return tx.QueryRowContext(ctx, s.d.Rebind(q), args...)
}

func (s *Store) txQuery(ctx context.Context, tx *sql.Tx, q string, args ...any) (*sql.Rows, error) {
	return tx.QueryContext(ctx, s.d.Rebind(q), args...)
}

func (s *Store) txExec(ctx context.Context, tx *sql.Tx, q string, args ...any) error {
	_, err := tx.ExecContext(ctx, s.d.Rebind(q), args...)
	return err
}

// New wraps already-open pools and applies any pending migrations. Callers
// build the pools, because the DSN and the pool limits are the engine-specific
// part and everything after this point is not.
func New(ctx context.Context, w, r *sql.DB, d Dialect, window time.Duration) (*Store, error) {
	if err := w.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("%s: database is not usable: %w", d.Name(), err)
	}
	if err := migrate(ctx, w, d); err != nil {
		return nil, err
	}
	return &Store{w: w, r: r, d: d, window: window, now: time.Now}, nil
}

// ---------------------------------------------------------------- writes

func (s *Store) Append(ctx context.Context, rec evidence.Record) (evidence.Record, error) {
	if rec.IdempotencyKey == "" {
		return evidence.Record{}, errors.New("evidence: IdempotencyKey is required")
	}
	if rec.State == "" {
		rec.State = evidence.StatePending
	}
	rec.InitialState = rec.State

	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return evidence.Record{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Returning the stored record with ErrDuplicate is what makes a restarted
	// writer safe: it re-appends everything it can still see and keeps only
	// what was new, with no read-before-write race.
	var existingID string
	err = s.txQueryRow(ctx, tx,
		`SELECT id FROM record WHERE idempotency_key = ?`, rec.IdempotencyKey).Scan(&existingID)
	switch {
	case err == nil:
		if err := tx.Commit(); err != nil {
			return evidence.Record{}, err
		}
		stored, getErr := s.Get(ctx, evidence.ID(existingID))
		if getErr != nil {
			return evidence.Record{}, getErr
		}
		return stored, fmt.Errorf("%w: %s", evidence.ErrDuplicate, rec.IdempotencyKey)
	case !errors.Is(err, sql.ErrNoRows):
		return evidence.Record{}, err
	}

	var maxSeq int64
	var prevHash string
	err = s.txQueryRow(ctx, tx, `
		SELECT seq, hash FROM record
		 WHERE tenant_id = ? AND app = ? AND env = ?
		 ORDER BY seq DESC LIMIT 1`,
		rec.Ref.TenantID, rec.Ref.App, rec.Ref.Env).Scan(&maxSeq, &prevHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return evidence.Record{}, err
	}

	rec.Seq = maxSeq + 1
	// Canonical, not raw: what is hashed has to be what is stored and what
	// comes back, or the chain depends on the host clock's resolution.
	now := evidence.Canonical(s.now())
	rec.CreatedAt, rec.UpdatedAt = now, now
	rec.ID = evidence.ID(fmt.Sprintf("%s-%s-%s-%d", rec.Ref.TenantID, rec.Ref.App, rec.Ref.Env, rec.Seq))
	rec.PrevHash, err = hex.DecodeString(prevHash)
	if err != nil {
		return evidence.Record{}, fmt.Errorf("%s: stored hash is not hex: %w", s.d.Name(), err)
	}
	rec.Hash = evidence.ChainRecord(rec.PrevHash, rec)
	rec.Transitions = nil

	policies, err := json.Marshal(rec.Policies)
	if err != nil {
		return evidence.Record{}, err
	}

	if err := s.txExec(ctx, tx, `
		INSERT INTO record (
		  id, idempotency_key, tenant_id, app, env, seq, tier,
		  actor_id, actor_kind, actor_name, actor_email,
		  repo_url, git_ref, repo_path, commit_sha, author_email, committer_email,
		  image_requested, image_admitted,
		  sig_verified, sig_issuer, sig_subject, sig_digest, sig_message,
		  policies, adm_allowed, adm_reason, adm_message, note,
		  state, initial_state, created_at, updated_at, prev_hash, hash
		) VALUES (?,?,?,?,?,?,?, ?,?,?,?, ?,?,?,?,?,?, ?,?, ?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?)`,
		string(rec.ID), rec.IdempotencyKey, rec.Ref.TenantID, rec.Ref.App, rec.Ref.Env, rec.Seq, string(rec.Tier),
		rec.Actor.ID, rec.Actor.Kind, rec.Actor.DisplayName, rec.Actor.Email,
		rec.Source.RepoURL, rec.Source.Ref, rec.Source.Path, rec.Source.CommitSHA,
		rec.Source.AuthorEmail, rec.Source.CommitterEmail,
		rec.Image.RequestedRef, rec.Image.AdmittedDigest,
		boolInt(rec.Signature.Verified), rec.Signature.Issuer, rec.Signature.Subject,
		rec.Signature.Digest, rec.Signature.Message,
		string(policies), boolInt(rec.Admission.Allowed), rec.Admission.Reason, rec.Admission.Message, rec.Note,
		string(rec.State), string(rec.InitialState), asText(rec.CreatedAt), asText(rec.UpdatedAt),
		hex.EncodeToString(rec.PrevHash), hex.EncodeToString(rec.Hash),
	); err != nil {
		return evidence.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return evidence.Record{}, err
	}
	return rec, nil
}

func (s *Store) Transition(ctx context.Context, id evidence.ID, t evidence.Transition) (evidence.Record, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return evidence.Record{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Take the row lock before reading the state this transition is a
	// compare-and-set against. On SQLite the clause is empty and the write
	// lock taken at BEGIN IMMEDIATE already serialises this; on PostgreSQL,
	// READ COMMITTED would otherwise let two transactions read the same state,
	// both find it acceptable, and both write. The symptom there is a lost
	// update rather than an error, which is why it is done here rather than
	// discovered later.
	var locked string
	switch err := s.txQueryRow(ctx, tx,
		`SELECT id FROM record WHERE id = ?`+s.d.LockRow(), string(id)).Scan(&locked); {
	case errors.Is(err, sql.ErrNoRows):
		return evidence.Record{}, evidence.ErrNotFound
	case err != nil:
		return evidence.Record{}, err
	}

	rec, err := s.loadTx(ctx, tx, id)
	if err != nil {
		return evidence.Record{}, err
	}
	if len(t.From) > 0 && !slices.Contains(t.From, rec.State) {
		return rec, fmt.Errorf("%w: record is %s, expected one of %v", evidence.ErrConflict, rec.State, t.From)
	}
	// The version fence, checked inside the transaction that already holds the
	// row lock. The PRIMARY KEY (record_id, seq) on record_event would reject a
	// second writer at the same version anyway; comparing here turns that from
	// a constraint violation into the ErrConflict the caller knows how to read.
	if t.ExpectEvents != nil && *t.ExpectEvents != len(rec.Transitions) {
		return rec, fmt.Errorf("%w: record is at version %d, caller derived from %d",
			evidence.ErrConflict, len(rec.Transitions), *t.ExpectEvents)
	}

	ev := evidence.Event{
		From: rec.State, To: t.To, At: evidence.Canonical(t.At),
		Reason: t.Reason, Observation: t.Observation,
	}
	ev.Observation.At = evidence.Canonical(ev.Observation.At)
	ev.PrevHash = rec.Hash
	if n := len(rec.Transitions); n > 0 {
		ev.PrevHash = rec.Transitions[n-1].Hash
	}
	ev.Hash = evidence.ChainEvent(ev.PrevHash, rec.ID, ev)

	var histID any
	if ev.Observation.HistoryID != nil {
		histID = *ev.Observation.HistoryID
	}
	if err := s.txExec(ctx, tx, `
		INSERT INTO record_event (
		  record_id, seq, from_state, to_state, at, reason,
		  obs_source, obs_app_uid, obs_history_id, obs_revision, obs_phase, obs_at,
		  prev_hash, hash
		) VALUES (?,?,?,?,?,?, ?,?,?,?,?,?, ?,?)`,
		string(rec.ID), len(rec.Transitions)+1, string(ev.From), string(ev.To), asText(ev.At), ev.Reason,
		string(ev.Observation.Source), ev.Observation.ApplicationUID, histID,
		ev.Observation.Revision, ev.Observation.OperationPhase, asText(ev.Observation.At),
		hex.EncodeToString(ev.PrevHash), hex.EncodeToString(ev.Hash),
	); err != nil {
		return evidence.Record{}, err
	}

	rec.Transitions = append(rec.Transitions, ev)
	rec.State = t.To
	rec.UpdatedAt = evidence.Canonical(s.now())
	rec.Policies = append(rec.Policies, t.Policies...)
	if t.Admission != nil {
		rec.Admission = *t.Admission
	}
	if t.Image != nil {
		rec.Image = *t.Image
	}

	policies, err := json.Marshal(rec.Policies)
	if err != nil {
		return evidence.Record{}, err
	}
	if err := s.txExec(ctx, tx, `
		UPDATE record SET state = ?, updated_at = ?, policies = ?,
		       adm_allowed = ?, adm_reason = ?, adm_message = ?,
		       image_requested = ?, image_admitted = ?
		 WHERE id = ?`,
		string(rec.State), asText(rec.UpdatedAt), string(policies),
		boolInt(rec.Admission.Allowed), rec.Admission.Reason, rec.Admission.Message,
		rec.Image.RequestedRef, rec.Image.AdmittedDigest, string(rec.ID),
	); err != nil {
		return evidence.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return evidence.Record{}, err
	}
	return rec, nil
}

// ---------------------------------------------------------------- reads

func (s *Store) Get(ctx context.Context, id evidence.ID) (evidence.Record, error) {
	return s.load(ctx, s.r, id)
}

func (s *Store) Current(ctx context.Context, ref evidence.Ref) (evidence.Record, error) {
	// Newest running, else newest applied. What the live page shows is what is
	// actually serving, not what was asked for most recently.
	for _, state := range []evidence.State{evidence.StateRunning, evidence.StateApplied} {
		var id string
		err := s.queryRow(ctx, `
			SELECT id FROM record
			 WHERE tenant_id = ? AND app = ? AND env = ? AND state = ?
			 ORDER BY seq DESC LIMIT 1`,
			ref.TenantID, ref.App, ref.Env, string(state)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return evidence.Record{}, err
		}
		return s.Get(ctx, evidence.ID(id))
	}
	return evidence.Record{}, evidence.ErrNotFound
}

func (s *Store) FindBySource(ctx context.Context, repoURL, commitSHA string) ([]evidence.Record, error) {
	rows, err := s.query(ctx,
		`SELECT id FROM record WHERE repo_url = ? AND commit_sha = ? ORDER BY seq`, repoURL, commitSHA)
	if err != nil {
		return nil, err
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, err
	}
	out := make([]evidence.Record, 0, len(ids))
	for _, id := range ids {
		rec, err := s.Get(ctx, evidence.ID(id))
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (s *Store) History(ctx context.Context, q evidence.Query) (evidence.Page, error) {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	// Keyset on seq, never an offset: an archive being appended to while it is
	// paged would skip and repeat rows under an offset, and those are exactly
	// the rows an auditor is reading.
	where := []string{"1 = 1"}
	args := []any{}
	if q.Ref.TenantID != "" {
		where, args = append(where, "tenant_id = ?"), append(args, q.Ref.TenantID)
	}
	if q.Ref.App != "" {
		where, args = append(where, "app = ?"), append(args, q.Ref.App)
	}
	if q.Ref.Env != "" {
		where, args = append(where, "env = ?"), append(args, q.Ref.Env)
	}
	if q.Actor != "" {
		where, args = append(where, "actor_id = ?"), append(args, q.Actor)
	}
	if !q.Since.IsZero() {
		where, args = append(where, "created_at >= ?"), append(args, asText(q.Since))
	}
	if !q.Until.IsZero() {
		where, args = append(where, "created_at < ?"), append(args, asText(q.Until))
	}
	if len(q.States) > 0 {
		marks := make([]string, len(q.States))
		for i, st := range q.States {
			marks[i], args = "?", append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(marks, ",")+")")
	}

	asc := q.Order != evidence.OrderNewest
	if q.After != "" {
		cursor, err := strconv.ParseInt(string(q.After), 10, 64)
		if err != nil {
			return evidence.Page{}, fmt.Errorf("%s: malformed cursor %q: %w", s.d.Name(), q.After, err)
		}
		if asc {
			where, args = append(where, "seq > ?"), append(args, cursor)
		} else {
			where, args = append(where, "seq < ?"), append(args, cursor)
		}
	}

	order := "ASC"
	if !asc {
		order = "DESC"
	}
	// One row over the limit, so Next is only set when there is genuinely
	// another page. A cursor that leads to an empty page reads as data loss.
	rows, err := s.query(ctx,
		`SELECT id, seq FROM record WHERE `+strings.Join(where, " AND ")+
			` ORDER BY seq `+order+` LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return evidence.Page{}, err
	}
	type row struct {
		id  string
		seq int64
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.seq); err != nil {
			_ = rows.Close()
			return evidence.Page{}, err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return evidence.Page{}, err
	}
	if err := rows.Close(); err != nil {
		return evidence.Page{}, err
	}

	var page evidence.Page
	if len(found) > limit {
		page.Next = evidence.Cursor(strconv.FormatInt(found[limit-1].seq, 10))
		found = found[:limit]
	}
	for _, r := range found {
		rec, err := s.Get(ctx, evidence.ID(r.id))
		if err != nil {
			return evidence.Page{}, err
		}
		page.Records = append(page.Records, rec)
	}
	return page, nil
}

func (s *Store) Export(ctx context.Context, req evidence.ExportRequest, w io.Writer) (evidence.ExportResult, error) {
	q := req.Query
	// Oldest first, always: the chain can only be verified forwards.
	q.Order = evidence.OrderOldest
	var res evidence.ExportResult
	counted := &countingWriter{w: w}
	enc := json.NewEncoder(counted)
	for {
		page, err := s.History(ctx, q)
		if err != nil {
			return res, err
		}
		for _, rec := range page.Records {
			if res.Records == 0 {
				res.FirstHash = rec.Hash
			}
			res.LastHash = rec.Hash
			if err := enc.Encode(rec); err != nil {
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
	res.Written = s.now().UTC()
	return res, nil
}

func (s *Store) Retention(context.Context) (evidence.RetentionPolicy, error) {
	return evidence.RetentionPolicy{
		Window:      s.window,
		KeepCurrent: true,
		// Reported honestly rather than aspirationally, because the evidence
		// page prints this answer. Neither engine is WORM as configured here:
		// SQLite cannot be, having no roles and no REVOKE, and PostgreSQL only
		// becomes so once the application connects as a role without UPDATE or
		// DELETE — which is a deployment decision this package cannot observe.
		Immutable: false,
		Tier:      evidence.TierFree,
	}, nil
}

func (s *Store) Prune(ctx context.Context, now time.Time) (evidence.PruneResult, error) {
	var res evidence.PruneResult
	if s.window == 0 {
		return res, nil
	}
	cutoff := asText(now.Add(-s.window))

	refs, err := s.refs(ctx)
	if err != nil {
		return res, err
	}
	keep := map[string]bool{}
	for _, ref := range refs {
		if rec, err := s.Current(ctx, ref); err == nil {
			keep[string(rec.ID)] = true
		} else if !errors.Is(err, evidence.ErrNotFound) {
			return res, err
		}
	}

	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := s.txQuery(ctx, tx,
		`SELECT id, created_at FROM record ORDER BY created_at`)
	if err != nil {
		return res, err
	}
	type candidate struct{ id, createdAt string }
	var all []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.createdAt); err != nil {
			_ = rows.Close()
			return res, err
		}
		all = append(all, c)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if err := rows.Close(); err != nil {
		return res, err
	}

	for _, c := range all {
		res.Examined++
		// The current record is never pruned. A retention sweep that can reach
		// it is not a policy, it is an outage on a timer.
		if c.createdAt < cutoff && !keep[c.id] {
			if err := s.txExec(ctx, tx, `DELETE FROM record WHERE id = ?`, c.id); err != nil {
				return res, err
			}
			res.Deleted++
			continue
		}
		if at, err := fromText(c.createdAt); err == nil && (res.Oldest.IsZero() || at.Before(res.Oldest)) {
			res.Oldest = at
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Store) Verify(ctx context.Context, ref evidence.Ref, from, to evidence.Cursor) (evidence.Proof, error) {
	proof := evidence.Proof{Ref: ref, Valid: true, CheckedAt: s.now().UTC()}
	q := evidence.Query{Ref: ref, Order: evidence.OrderOldest, After: from, Limit: 200}
	var prev []byte
	for {
		page, err := s.History(ctx, q)
		if err != nil {
			return evidence.Proof{}, err
		}
		for _, rec := range page.Records {
			if to != "" && evidence.Cursor(strconv.FormatInt(rec.Seq, 10)) > to {
				proof.RootHash = prev
				return proof, nil
			}
			if got := evidence.ChainRecord(prev, rec); !bytes.Equal(got, rec.Hash) {
				proof.Valid, proof.BrokenAt = false, rec.Seq
				proof.RootHash = prev
				return proof, nil
			}
			// The record's own sub-chain. It starts at the record and never
			// rejoins the Ref chain, because appends and transitions interleave
			// and a walk in Seq order cannot reproduce that interleaving.
			evPrev := rec.Hash
			for _, ev := range rec.Transitions {
				if got := evidence.ChainEvent(evPrev, rec.ID, ev); !bytes.Equal(got, ev.Hash) {
					proof.Valid, proof.BrokenAt = false, rec.Seq
					proof.RootHash = prev
					return proof, nil
				}
				evPrev = ev.Hash
			}
			prev = rec.Hash
			proof.Records++
			proof.ToSeq = rec.Seq
			if proof.FromSeq == 0 {
				proof.FromSeq = rec.Seq
			}
		}
		if page.Next == "" {
			break
		}
		q.After = page.Next
	}
	proof.RootHash = prev
	return proof, nil
}

func (s *Store) Close() error {
	return errors.Join(s.w.Close(), s.r.Close())
}

var _ evidence.Store = (*Store)(nil)

// ---------------------------------------------------------------- internals

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *Store) load(ctx context.Context, q querier, id evidence.ID) (evidence.Record, error) {
	var (
		rec                        evidence.Record
		tier, state, initial       string
		createdAt, updatedAt       string
		prevHash, hash             string
		policiesJSON               string
		sigVerified, admAllowed    int
		recID                      string
		tenantID, appName, envName string
	)
	err := q.QueryRowContext(ctx, s.d.Rebind(`
		SELECT id, idempotency_key, tenant_id, app, env, seq, tier,
		       actor_id, actor_kind, actor_name, actor_email,
		       repo_url, git_ref, repo_path, commit_sha, author_email, committer_email,
		       image_requested, image_admitted,
		       sig_verified, sig_issuer, sig_subject, sig_digest, sig_message,
		       policies, adm_allowed, adm_reason, adm_message, note,
		       state, initial_state, created_at, updated_at, prev_hash, hash
		  FROM record WHERE id = ?`), string(id)).Scan(
		&recID, &rec.IdempotencyKey, &tenantID, &appName, &envName, &rec.Seq, &tier,
		&rec.Actor.ID, &rec.Actor.Kind, &rec.Actor.DisplayName, &rec.Actor.Email,
		&rec.Source.RepoURL, &rec.Source.Ref, &rec.Source.Path, &rec.Source.CommitSHA,
		&rec.Source.AuthorEmail, &rec.Source.CommitterEmail,
		&rec.Image.RequestedRef, &rec.Image.AdmittedDigest,
		&sigVerified, &rec.Signature.Issuer, &rec.Signature.Subject,
		&rec.Signature.Digest, &rec.Signature.Message,
		&policiesJSON, &admAllowed, &rec.Admission.Reason, &rec.Admission.Message, &rec.Note,
		&state, &initial, &createdAt, &updatedAt, &prevHash, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return evidence.Record{}, evidence.ErrNotFound
	}
	if err != nil {
		return evidence.Record{}, err
	}

	rec.ID = evidence.ID(recID)
	rec.Ref = evidence.Ref{TenantID: tenantID, App: appName, Env: envName}
	rec.Tier = evidence.Tier(tier)
	rec.State, rec.InitialState = evidence.State(state), evidence.State(initial)
	rec.Signature.Verified = sigVerified == 1
	rec.Admission.Allowed = admAllowed == 1
	if rec.CreatedAt, err = fromText(createdAt); err != nil {
		return evidence.Record{}, err
	}
	if rec.UpdatedAt, err = fromText(updatedAt); err != nil {
		return evidence.Record{}, err
	}
	if rec.PrevHash, err = hex.DecodeString(prevHash); err != nil {
		return evidence.Record{}, err
	}
	if rec.Hash, err = hex.DecodeString(hash); err != nil {
		return evidence.Record{}, err
	}
	if err := json.Unmarshal([]byte(policiesJSON), &rec.Policies); err != nil {
		return evidence.Record{}, fmt.Errorf("%s: stored policies are not JSON: %w", s.d.Name(), err)
	}

	rec.Transitions, err = s.loadEvents(ctx, q, rec.ID)
	if err != nil {
		return evidence.Record{}, err
	}
	return rec, nil
}

func (s *Store) loadTx(ctx context.Context, tx *sql.Tx, id evidence.ID) (evidence.Record, error) {
	return s.load(ctx, tx, id)
}

func (s *Store) loadEvents(ctx context.Context, q querier, id evidence.ID) ([]evidence.Event, error) {
	rows, err := q.QueryContext(ctx, s.d.Rebind(`
		SELECT from_state, to_state, at, reason,
		       obs_source, obs_app_uid, obs_history_id, obs_revision, obs_phase, obs_at,
		       prev_hash, hash
		  FROM record_event WHERE record_id = ? ORDER BY seq`), string(id))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []evidence.Event
	for rows.Next() {
		var (
			ev                     evidence.Event
			from, to               string
			at, obsAt              string
			source                 string
			histID                 sql.NullInt64
			prevHash, hash         string
			appUID, revision, ph   string
			parsedAt, parsedObsAt  time.Time
			errAt, errObsAt, errHx error
		)
		if err := rows.Scan(&from, &to, &at, &ev.Reason,
			&source, &appUID, &histID, &revision, &ph, &obsAt,
			&prevHash, &hash); err != nil {
			return nil, err
		}
		ev.From, ev.To = evidence.State(from), evidence.State(to)
		if parsedAt, errAt = fromText(at); errAt != nil {
			return nil, errAt
		}
		if parsedObsAt, errObsAt = fromText(obsAt); errObsAt != nil {
			return nil, errObsAt
		}
		ev.At = parsedAt
		ev.Observation = evidence.Observation{
			Source: evidence.ObservationSource(source), ApplicationUID: appUID,
			Revision: revision, OperationPhase: ph, At: parsedObsAt,
		}
		if histID.Valid {
			v := histID.Int64
			ev.Observation.HistoryID = &v
		}
		if ev.PrevHash, errHx = hex.DecodeString(prevHash); errHx != nil {
			return nil, errHx
		}
		if ev.Hash, errHx = hex.DecodeString(hash); errHx != nil {
			return nil, errHx
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) refs(ctx context.Context) ([]evidence.Ref, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT tenant_id, app, env FROM record`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []evidence.Ref
	for rows.Next() {
		var ref evidence.Ref
		if err := rows.Scan(&ref.TenantID, &ref.App, &ref.Env); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
