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

// Package evidence is the record of what was deployed, by whom, and what
// happened to it. It is the only place history lives: the Argo CD Application
// status is a live view rather than a record, and it keeps ten entries.
//
// What that record is for changed on 2026-08-29. It was the foundation of a
// claim about supply-chain provenance, and the layer that produced that claim
// was removed. What remains — who, when, which commit, which image, admitted or
// refused — is a deploy history, which is what a rollback needs and what
// "who deployed this" is answered from. Smaller, and still load-bearing.
//
// This package declares types and one interface and performs no I/O, so an
// alternative store can satisfy it without linking any of the storage code.
package evidence

import (
	"context"
	"errors"
	"io"
	"time"
)

// Errors a Store returns. Callers branch on these, so they are part of the
// contract any implementation has to honour.
var (
	// ErrNotFound is returned by Current and Get when nothing matches.
	ErrNotFound = errors.New("evidence: not found")
	// ErrDuplicate is returned by Append when a record with the same
	// IdempotencyKey already exists. The existing record is returned with it,
	// which is what makes a restarted observer safe: it re-appends everything
	// it can still see and keeps only what was new.
	ErrDuplicate = errors.New("evidence: duplicate idempotency key")
	// ErrConflict is returned by Transition when the record is not in any of
	// the states the caller said it expected. Two observers racing on the same
	// record produce one winner and one ErrConflict, never two rows.
	ErrConflict = errors.New("evidence: state conflict")
	// ErrImmutable is returned when a caller tries to change a field a record
	// does not allow to change after it is written.
	ErrImmutable = errors.New("evidence: record is immutable")

	// ErrInvalid is a write the store refuses to record rather than record
	// wrongly. Separate from ErrConflict: a conflict means try again with a
	// fresh read, this means the call itself is wrong and retrying it will
	// not help.
	ErrInvalid = errors.New("evidence: invalid")
)

// ID is the store-assigned identifier. It is time-ordered (UUIDv7 in the free
// store) so that it doubles as the pagination cursor and as the ordering used
// by the hash chain.
type ID string

// Cursor is an opaque keyset position. Never an offset: an archive that is
// being appended to while being paged would skip and repeat rows.
type Cursor string

// Ref names what a record is about. TenantID is a column, never a path
// segment: the layout of the tenant repositories can change without rewriting
// any record.
type Ref struct {
	TenantID string
	App      string
	Env      string
}

// State is where a deploy is in its life. The record is appended at commit
// time — the only moment damga is present, because Argo CD does the applying —
// and transitions afterwards as the cluster is observed.
type State string

const (
	// StatePending: the commit is written and pushed. Nothing has been applied.
	StatePending State = "pending"
	// StateSyncing: Argo CD has an operation in flight at this revision.
	StateSyncing State = "syncing"
	// StateApplied: the API server accepted the objects. Admission passed.
	StateApplied State = "applied"
	// StateRunning: the rollout finished and the workload is Ready.
	StateRunning State = "running"
	// StateRejected: admission refused it. The reason is quotable to the user.
	StateRejected State = "rejected"
	// StateFailed: the sync or the rollout failed for a reason that is not
	// admission.
	StateFailed State = "failed"
	// StateSuperseded: a later record for the same Ref reached StateRunning
	// first.
	//
	// Declared and not yet written by anything. Recorded here rather than
	// removed because Current() already resolves the right record by Seq, so
	// nothing looks broken until someone queries by state — which an export or
	// an auditor will. The component that will write it is the deploy
	// observer, which does not exist yet.
	StateSuperseded State = "superseded"
	// StateUnknown: the observer never saw this commit reach the cluster and
	// the evidence for it is gone — Argo CD keeps ten history entries and
	// overwrites operationState. Guessing "applied" here is the one thing an
	// audit record may never do.
	StateUnknown State = "unknown"
)

// Actor is who caused this. ID references the control-plane user row so that
// an erasure request is one row, not a scan of the archive. DisplayName and
// Email are copied deliberately: an audit record has to stay readable after
// the account is gone, and the archive says which of the two happened.
type Actor struct {
	ID          string
	Kind        string // "user" | "automation"
	DisplayName string
	Email       string
}

// Source is the commit. Under principle 1 this is the whole write path, so
// every record has one — including records the observer invented because it
// saw a deploy damga did not originate.
type Source struct {
	RepoURL   string
	Ref       string // branch or tag
	Path      string
	CommitSHA string

	// AuthorEmail is the user; CommitterEmail is the platform. The split is
	// what keeps "approved by Orhan" on the evidence page instead of
	// "the platform did it".
	AuthorEmail    string
	CommitterEmail string
}

// Image is what actually ran. RequestedRef is what git said.
//
// AdmittedDigest has no writer. It held what the cluster ran after Kyverno's
// mutateDigest rewrote the reference, and that engine was removed with the rest
// of the admission layer; every record written since carries it empty, and the
// CLI's `if != ""` guard means it has quietly stopped printing.
//
// It stays anyway, and the reason is the chain rather than sentiment: Image is
// inside chainedRecord, so the field is covered by every hash already written.
// Removing it changes the canonical form and every existing record fails
// verification. A chained field can only be changed before the first record
// exists, and that moment is past. If digests are admitted again, this is where
// they go — and it will still verify.
type Image struct {
	RequestedRef   string
	AdmittedDigest string
}

// AdmissionOutcome is the API server's verdict, as quoted back to the user.
type AdmissionOutcome struct {
	Allowed bool
	Reason  string
	Message string
}

// ObservationSource says how the platform learned something, so that a record
// can never claim more certainty than its source had.
type ObservationSource string

const (
	// ObservedFromCommit: damga wrote the commit itself.
	ObservedFromCommit ObservationSource = "commit"
	// ObservedFromArgoHistory: .status.history[]. Argo CD appends there only
	// on a successful, non-dry-run, non-selective sync, so its presence is
	// proof of success and its absence proves nothing.
	ObservedFromArgoHistory ObservationSource = "argocd.history"
	// ObservedFromArgoOperation: .status.operationState. Holds failures, but
	// only until the next operation overwrites it.
	ObservedFromArgoOperation ObservationSource = "argocd.operationState"
	// ObservedFromWorkload: the Workload's own status.
	ObservedFromWorkload ObservationSource = "workload.status"
	// ObservedFromSweep: nothing was observed and a deadline passed.
	ObservedFromSweep ObservationSource = "sweep"
)

// Observation is the provenance of a transition: what was read, where, and
// when. It is descriptive: nothing in the store is keyed on it, and nothing
// enforces uniqueness over it. What stops a restarted or superseded observer
// writing a second row is Transition.ExpectEvents, below.
type Observation struct {
	Source         ObservationSource
	ApplicationUID string
	HistoryID      *int64
	Revision       string
	OperationPhase string
	At             time.Time
}

// Record is one deploy. Everything above the hash is written by callers;
// everything from Seq down is written by the store.
type Record struct {
	// IdempotencyKey makes Append safe to retry and safe to replay. The git
	// writer uses "commit:<sha>:<path>"; the observer uses
	// "argocd:<appUID>:<historyID>". Required.
	IdempotencyKey string

	Ref   Ref
	Actor Actor

	Source    Source
	Image     Image
	Admission AdmissionOutcome

	// Note is free text the panel shows. Not a place for structured data.
	Note string

	// Set by the store.
	ID  ID
	Seq int64 // per-Ref, monotonic, gapless: "the 41st deploy of api/prod"

	// State is the current state — a projection of Transitions, not a chained
	// fact. InitialState is what the record was appended in, and that one is
	// chained. Keeping both is what lets the log be append-only while the page
	// still reads one row.
	State        State
	InitialState State
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Transitions  []Event

	// PrevHash and Hash chain the records of one Ref in Seq order. Hash =
	// H(PrevHash || canonical(the immutable half of the record)). The free
	// store computes and stores both; Verify recomputes them. An archive that
	// adds WORM storage or an external anchor does not change this meaning.
	//
	// Per Ref rather than per tenant, because Verify is asked about a Ref: two
	// apps of one tenant deploying at the same time would interleave into a
	// tenant-wide chain that walking either app alone can never reproduce.
	PrevHash []byte
	Hash     []byte
}

// Event is one state change, kept on the record so the page can draw the
// timeline without a second query.
type Event struct {
	From        State
	To          State
	At          time.Time
	Reason      string
	Observation Observation

	// Each event is its own link, chained off the record it belongs to and
	// then off the preceding event. A record's own hash covers only what was
	// true when it was appended; everything that happened afterwards is
	// chained here, because a hash over a row that still changes is not a
	// proof of anything. Measured: chaining the whole record and then
	// transitioning it makes Verify fail on the record it just wrote.
	//
	// Deliberately a sub-chain rather than a continuation of the Ref chain:
	// appends and transitions interleave, and a verifier walking records in
	// Seq order cannot reproduce that interleaving.
	PrevHash []byte
	Hash     []byte
}

// Transition is a compare-and-set. From lists the states the caller believes
// the record may be in; an empty From means "any", which callers should avoid
// because it is how two observers write over each other.
type Transition struct {
	From        []State
	To          State
	At          time.Time
	Reason      string
	Observation Observation

	// ExpectEvents fences the write on the version of the record the caller
	// derived its decision from: the number of transitions it saw. nil means
	// unfenced.
	//
	// From alone is not enough, and the reason is structural rather than a
	// bug. From compares a state, and a state is a value that recurs; every
	// guarantee worth having here is about a sequence. Reproduced against both
	// stores: an observer's write, computed before a leader handover, lands
	// after it — Kubernetes leader election is explicitly unfenced, and
	// controller-runtime does not drain an in-flight Reconcile when it loses
	// the lease. If the record has meanwhile moved away from and back to a
	// state in From, the compare-and-set accepts it. The record then claims a
	// state on the strength of a stale observation, the event log runs
	// backwards in time, and Verify still reports the chain valid — because
	// the chain proves the row was not edited, not that it was written in the
	// right order. An audit record that is provably intact and provably wrong
	// is worse than one that is obviously broken.
	//
	// A version does not recur. The value costs nothing to produce: it is
	// len(Record.Transitions), which the caller has already read.
	ExpectEvents *int

	// Admission and Image are merged into the record when the transition is
	// the one that learned them. Nil leaves what is there alone.
	Admission *AdmissionOutcome
	Image     *Image
}

// Order is the direction History and Export walk. Export defaults to Oldest
// because the hash chain can only be verified forwards.
type Order string

const (
	OrderNewest Order = "newest"
	OrderOldest Order = "oldest"
)

// Query selects records. An empty App or Env widens the selection rather than
// matching empty, so one query type serves the app page, the tenant feed and
// the export.
type Query struct {
	Ref    Ref
	States []State
	Since  time.Time
	Until  time.Time
	Actor  string
	Limit  int
	After  Cursor
	Order  Order
}

// Page is one page of History. Next is empty at the end.
type Page struct {
	Records []Record
	Next    Cursor
}

// RetentionPolicy is what the store promises. It is read, not set, through
// this interface: a store reports its fixed window, another reports
// whatever it was configured with, and the evidence page prints the answer
// instead of a marketing claim.
type RetentionPolicy struct {
	// Window is how long non-current records are kept. Zero means unbounded.
	Window time.Duration
	// KeepCurrent is always true for a conforming store: the record a Ref is
	// currently on is never pruned, or the live page would go blank at the
	// retention edge.
	KeepCurrent bool
	// Immutable says the backing store refuses in-place edits and deletes
	// (WORM object lock, append-only table with no UPDATE grant).
	Immutable bool
	// Anchor names where chain checkpoints are published, if anywhere.
	Anchor string
}

// PruneResult is what Prune did. Reported so the platform can show it rather
// than have rows disappear silently.
type PruneResult struct {
	Examined int64
	Deleted  int64
	Oldest   time.Time
}

// ExportFormat is the encoding Export writes.
type ExportFormat string

const (
	// ExportJSONL is one record per line, in Seq order, with the hash chain
	// intact so the file verifies on its own.
	ExportJSONL ExportFormat = "jsonl"
	// ExportCSV is declared and not implemented. Export refuses it rather
	// than falling back to JSONL, so that a caller asking for it finds out
	// here instead of from a spreadsheet that opened as one long column.
	//
	// Flattening a record is a decision, not a detail: the policies and the
	// transitions are lists, and an export that drops them cannot be
	// re-verified — which is the only reason to export at all.
	ExportCSV ExportFormat = "csv"
)

// ExportRequest is a query plus an encoding.
type ExportRequest struct {
	Query  Query
	Format ExportFormat
}

// ExportResult describes what was written, including the chain endpoints so
// the file can be checked against the store later.
type ExportResult struct {
	Records   int64
	Bytes     int64
	FirstHash []byte
	LastHash  []byte
	Written   time.Time
}

// Proof is the result of re-deriving the hash chain over a range.
type Proof struct {
	Ref       Ref
	FromSeq   int64
	ToSeq     int64
	Records   int64
	Valid     bool
	BrokenAt  int64 // first Seq whose recomputed hash disagrees; 0 when Valid
	RootHash  []byte
	CheckedAt time.Time
	// Anchors are external attestations covering this range, if the store
	// publishes any. Empty when nothing does, which is a complete answer
	// rather than a missing one: the chain is still verifiable, it is just not
	// witnessed by anyone else.
	Anchors []Anchor
}

// Anchor is a published checkpoint of the chain.
type Anchor struct {
	Kind      string // "rekor" | "s3-object-lock" | "signed-checkpoint"
	Reference string
	Seq       int64
	At        time.Time
}

// Store is the whole persistence surface of the evidence system. Three
// implementations satisfy it — in-memory, SQLite and PostgreSQL — and what
// differs between them is the policy the same methods run under (retention
// window, whether the backing store refuses in-place edits), never whether a
// method works.
type Store interface {
	// Append writes a new record in StatePending unless the caller set a
	// different State, which only the observer does when it records a deploy
	// damga did not originate. On a repeated IdempotencyKey it returns the
	// stored record together with ErrDuplicate.
	Append(ctx context.Context, rec Record) (Record, error)

	// Transition compare-and-sets the state. Returns ErrConflict if the record
	// is in none of t.From, ErrNotFound if there is no such record.
	Transition(ctx context.Context, id ID, t Transition) (Record, error)

	// Get returns one record.
	Get(ctx context.Context, id ID) (Record, error)

	// Current returns the record the Ref is on now: the newest in StateRunning,
	// or the newest in StateApplied if none is running yet. This is what the
	// live evidence page reads, and it is never pruned.
	Current(ctx context.Context, ref Ref) (Record, error)

	// FindBySource resolves a commit back to its record. The observer calls it
	// with the revision Argo CD reported, to attach an observation to the row
	// the git writer already appended.
	FindBySource(ctx context.Context, repoURL, commitSHA string) ([]Record, error)

	// History pages through records. Keyset, not offset.
	History(ctx context.Context, q Query) (Page, error)

	// Refs lists the app and environment pairs one tenant has evidence for,
	// ordered by app then env so a page built from it does not reshuffle
	// between loads.
	//
	// Scoped to a tenant and never global. A method that could list every Ref
	// in the store is a directory of every customer on the install, and the
	// only thing standing between it and a caller would be whoever remembers
	// to filter at the call site.
	//
	// A Ref that has ever been deployed to stays listed for as long as its
	// records do, and Prune never removes the current record — so this does
	// not go quiet at the retention edge while the app is still running.
	Refs(ctx context.Context, tenantID string) ([]Ref, error)

	// Export streams records in the requested encoding.
	Export(ctx context.Context, req ExportRequest, w io.Writer) (ExportResult, error)

	// Retention reports the policy in force. The page prints it.
	Retention(ctx context.Context) (RetentionPolicy, error)

	// Prune enforces Retention. It never removes a Ref's current record.
	Prune(ctx context.Context, now time.Time) (PruneResult, error)

	// Verify recomputes the hash chain over a range and reports where, if
	// anywhere, it breaks.
	Verify(ctx context.Context, ref Ref, from, to Cursor) (Proof, error)

	// Close releases the store's resources.
	Close() error
}
