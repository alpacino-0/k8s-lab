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
	"encoding/hex"
	"time"

	"github.com/damgahq/damga/evidence"
)

// The API's own shape for the evidence types, and the reason it is not just
// JSON tags on evidence.Record.
//
// The hash chain is computed with json.Marshal over evidence.chainedRecord,
// which carries Ref, Actor, Source, Image, SignatureVerdict, PolicyResult and
// AdmissionOutcome by value. Those types have no tags, so the chain hashes
// their Go field names — "TenantID", not "tenantId". Adding tags to make the
// API read well would change the bytes every existing record was hashed over,
// and every install's Verify would start reporting a chain that had never been
// touched as broken. The same tags would change the JSON in the policies
// column, so old rows would read back empty.
//
// So the wire format is written out here instead. That is the right layering
// anyway: what the chain hashes is an integrity concern that must never move,
// and what an HTTP response looks like is a product surface that will. Coupling
// them means one of the two cannot change, and it would be the wrong one.
//
// Hashes are hex rather than the base64 encoding/json gives a []byte, because
// hex is what every other tool in this project prints them in — the digests in
// the policy reports, in cosign's output, and in git.

type wireRef struct {
	TenantID string `json:"tenantId"`
	App      string `json:"app"`
	Env      string `json:"env"`
}

type wireActor struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

type wireSource struct {
	RepoURL        string `json:"repoUrl"`
	Ref            string `json:"ref"`
	Path           string `json:"path"`
	CommitSHA      string `json:"commitSha"`
	AuthorEmail    string `json:"authorEmail"`
	CommitterEmail string `json:"committerEmail"`
}

type wireImage struct {
	RequestedRef   string `json:"requestedRef"`
	AdmittedDigest string `json:"admittedDigest"`
}

type wireSignature struct {
	Verified bool   `json:"verified"`
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	Digest   string `json:"digest"`
	Message  string `json:"message"`
}

type wirePolicy struct {
	Name     string `json:"name"`
	Rule     string `json:"rule"`
	Source   string `json:"source"`
	Result   string `json:"result"`
	Severity string `json:"severity"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

type wireAdmission struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type wireObservation struct {
	Source         string `json:"source"`
	ApplicationUID string `json:"applicationUid"`
	HistoryID      *int64 `json:"historyId"`
	Revision       string `json:"revision"`
	OperationPhase string `json:"operationPhase"`
	At             string `json:"at"`
}

type wireEvent struct {
	From        string           `json:"from"`
	To          string           `json:"to"`
	At          string           `json:"at"`
	Reason      string           `json:"reason"`
	Observation *wireObservation `json:"observation,omitempty"`
	Hash        string           `json:"hash"`
}

type wireRecord struct {
	ID             string        `json:"id"`
	IdempotencyKey string        `json:"idempotencyKey"`
	Seq            int64         `json:"seq"`
	Ref            wireRef       `json:"ref"`
	Tier           string        `json:"tier"`
	Actor          wireActor     `json:"actor"`
	Source         wireSource    `json:"source"`
	Image          wireImage     `json:"image"`
	Signature      wireSignature `json:"signature"`
	// Never null. A client that writes `for (const p of record.policies)`
	// should not have to know that "no policies ran" and "the field is
	// missing" are spelled differently.
	Policies     []wirePolicy  `json:"policies"`
	Admission    wireAdmission `json:"admission"`
	Note         string        `json:"note"`
	State        string        `json:"state"`
	InitialState string        `json:"initialState"`
	CreatedAt    string        `json:"createdAt"`
	UpdatedAt    string        `json:"updatedAt"`
	Transitions  []wireEvent   `json:"transitions"`
	PrevHash     string        `json:"prevHash"`
	Hash         string        `json:"hash"`
}

type wireProof struct {
	Ref       wireRef      `json:"ref"`
	FromSeq   int64        `json:"fromSeq"`
	ToSeq     int64        `json:"toSeq"`
	Records   int64        `json:"records"`
	Valid     bool         `json:"valid"`
	BrokenAt  int64        `json:"brokenAt"`
	RootHash  string       `json:"rootHash"`
	CheckedAt string       `json:"checkedAt"`
	Anchors   []wireAnchor `json:"anchors"`
}

type wireAnchor struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Seq       int64  `json:"seq"`
	At        string `json:"at"`
}

// stamp renders a time the way every timestamp in this API is rendered, and
// renders the zero time as the empty string rather than as year 1 — a client
// showing "0001-01-01" has been handed a date it will try to format.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func toWireRef(r evidence.Ref) wireRef {
	return wireRef{TenantID: r.TenantID, App: r.App, Env: r.Env}
}

func toWireRecord(r evidence.Record) wireRecord {
	policies := make([]wirePolicy, 0, len(r.Policies))
	for _, p := range r.Policies {
		policies = append(policies, wirePolicy{
			Name: p.Name, Rule: p.Rule, Source: p.Source, Result: p.Result,
			Severity: p.Severity, Category: p.Category, Message: p.Message,
		})
	}
	transitions := make([]wireEvent, 0, len(r.Transitions))
	for _, e := range r.Transitions {
		ev := wireEvent{
			From: string(e.From), To: string(e.To), At: stamp(e.At),
			Reason: e.Reason, Hash: hex.EncodeToString(e.Hash),
		}
		// Omitted rather than sent empty: most transitions are damga's own,
		// and an observation block full of blanks reads as a failed lookup
		// rather than as "nothing observed this".
		if o := e.Observation; o.Source != "" || o.ApplicationUID != "" {
			ev.Observation = &wireObservation{
				Source: string(o.Source), ApplicationUID: o.ApplicationUID,
				HistoryID: o.HistoryID, Revision: o.Revision,
				OperationPhase: o.OperationPhase, At: stamp(o.At),
			}
		}
		transitions = append(transitions, ev)
	}
	return wireRecord{
		ID: string(r.ID), IdempotencyKey: r.IdempotencyKey, Seq: r.Seq,
		Ref: toWireRef(r.Ref), Tier: string(r.Tier),
		Actor: wireActor{
			ID: r.Actor.ID, Kind: r.Actor.Kind,
			DisplayName: r.Actor.DisplayName, Email: r.Actor.Email,
		},
		Source: wireSource{
			RepoURL: r.Source.RepoURL, Ref: r.Source.Ref, Path: r.Source.Path,
			CommitSHA: r.Source.CommitSHA, AuthorEmail: r.Source.AuthorEmail,
			CommitterEmail: r.Source.CommitterEmail,
		},
		Image: wireImage{
			RequestedRef: r.Image.RequestedRef, AdmittedDigest: r.Image.AdmittedDigest,
		},
		Signature: wireSignature{
			Verified: r.Signature.Verified, Issuer: r.Signature.Issuer,
			Subject: r.Signature.Subject, Digest: r.Signature.Digest,
			Message: r.Signature.Message,
		},
		Policies: policies,
		Admission: wireAdmission{
			Allowed: r.Admission.Allowed, Reason: r.Admission.Reason,
			Message: r.Admission.Message,
		},
		Note: r.Note, State: string(r.State), InitialState: string(r.InitialState),
		CreatedAt: stamp(r.CreatedAt), UpdatedAt: stamp(r.UpdatedAt),
		Transitions: transitions,
		PrevHash:    hex.EncodeToString(r.PrevHash),
		Hash:        hex.EncodeToString(r.Hash),
	}
}

func toWireProof(p evidence.Proof) wireProof {
	anchors := make([]wireAnchor, 0, len(p.Anchors))
	for _, a := range p.Anchors {
		anchors = append(anchors, wireAnchor{
			Kind: a.Kind, Reference: a.Reference, Seq: a.Seq, At: stamp(a.At),
		})
	}
	return wireProof{
		Ref: toWireRef(p.Ref), FromSeq: p.FromSeq, ToSeq: p.ToSeq,
		Records: p.Records, Valid: p.Valid, BrokenAt: p.BrokenAt,
		RootHash: hex.EncodeToString(p.RootHash), CheckedAt: stamp(p.CheckedAt),
		Anchors: anchors,
	}
}
