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
		InitialState: r.InitialState, CreatedAt: r.CreatedAt.UTC(),
	})
}

// ChainEvent returns the link for one state change. The event's own hash
// fields are cleared first: a link may never cover itself, and forgetting that
// is how a chain verifies at write time and fails at read time.
func ChainEvent(prev []byte, recordID ID, e Event) []byte {
	e.PrevHash, e.Hash = nil, nil
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
