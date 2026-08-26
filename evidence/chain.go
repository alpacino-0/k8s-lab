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

package evidence

import (
	"crypto/sha256"
	"encoding/json"
	"time"
)

// The chain lives in this package, not in a store, for a reason found by
// testing: hashing the record as it currently stands breaks the moment the
// record transitions, because State, UpdatedAt and Transitions all change. So
// what is chained is append-time facts and each state change as its own link.
// The store is an append-only log and the record's State is a projection of it.
//
// It is exported because the free store and a paid archive have to produce
// identical hashes, or an export taken before an upgrade stops verifying after
// one — which would make the retention promise unfalsifiable exactly when it
// starts being paid for.

// Precision is the resolution every timestamp is reduced to before it is
// hashed, and therefore the resolution a store has to persist at.
//
// It exists because the first version did not have it, and the result was a
// hash that depended on the host clock. time.Now() returns microseconds on
// Darwin and nanoseconds on Linux; a store that wrote microseconds and hashed
// whatever it was handed produced a chain that verified on a laptop and was
// invalid on the first record on CI. Not a portability nicety: an archive
// written by one node and verified on another is the whole promise.
//
// Microseconds because that is what a fixed-width RFC3339 column holds and
// what PostgreSQL's timestamp type stores. Changing this value invalidates
// every hash ever written, so it does not change.
const Precision = time.Microsecond

// Canonical reduces a timestamp to the resolution the chain hashes at. Stores
// call it on everything they persist, so that what is read back hashes to what
// was written.
func Canonical(t time.Time) time.Time { return t.UTC().Truncate(Precision) }

// chainedRecord is the immutable half of a Record. Anything not named here may
// change after the record is written and therefore cannot be chained.
type chainedRecord struct {
	IdempotencyKey string           `json:"idempotencyKey"`
	ID             ID               `json:"id"`
	Seq            int64            `json:"seq"`
	Ref            Ref              `json:"ref"`
	Tier           Tier             `json:"tier"`
	Actor          Actor            `json:"actor"`
	Source         Source           `json:"source"`
	Image          Image            `json:"image"`
	Signature      SignatureVerdict `json:"signature"`
	Policies       []PolicyResult   `json:"policies"`
	Admission      AdmissionOutcome `json:"admission"`
	Note           string           `json:"note"`
	InitialState   State            `json:"initialState"`
	CreatedAt      time.Time        `json:"createdAt"`
}

// ChainRecord returns the link for a newly appended record.
func ChainRecord(prev []byte, r Record) []byte {
	return link(prev, chainedRecord{
		IdempotencyKey: r.IdempotencyKey, ID: r.ID, Seq: r.Seq, Ref: r.Ref, Tier: r.Tier,
		Actor: r.Actor, Source: r.Source, Image: r.Image, Signature: r.Signature,
		Policies: r.Policies, Admission: r.Admission, Note: r.Note,
		InitialState: r.InitialState, CreatedAt: Canonical(r.CreatedAt),
	})
}

// ChainEvent returns the link for one state change. The event's own hash
// fields are cleared first: a link may never cover itself, and forgetting that
// is how a chain verifies at write time and fails at read time.
func ChainEvent(prev []byte, recordID ID, e Event) []byte {
	e.PrevHash, e.Hash = nil, nil
	// Same reduction as ChainRecord, for the same reason: these timestamps are
	// persisted at Precision and have to hash to what comes back.
	e.At = Canonical(e.At)
	e.Observation.At = Canonical(e.Observation.At)
	return link(prev, struct {
		RecordID ID    `json:"recordId"`
		Event    Event `json:"event"`
	}{recordID, e})
}

func link(prev []byte, payload any) []byte {
	// json.Marshal orders struct fields by declaration and map keys by sort
	// order, which is the canonical form this depends on. A store that stores
	// a Record as JSONB and re-marshals it from the database must round-trip
	// through these structs, not through the database's own JSON.
	b, err := json.Marshal(payload)
	if err != nil {
		panic("evidence: unmarshalable record: " + err.Error())
	}
	h := sha256.New()
	h.Write(prev)
	h.Write(b)
	return h.Sum(nil)
}
