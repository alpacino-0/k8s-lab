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
	"log/slog"
	"net/http"
	"strconv"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/identity"
)

// guard is the one place a request becomes a subject and an authorization.
//
// It exists because the alternative is every handler repeating the same four
// steps, and four copies of a security check are three chances to write it
// slightly differently. The one that would differ is whichever endpoint gets
// added in a hurry — and the free authorizer treats an unrecognised group as a
// viewer, so getting it wrong does not fail closed. It reads another tenant's
// deploy history.
type guard struct {
	authorizer authz.Authorizer
	identity   identity.Store
	sessions   *auth.Sessions
}

// admit answers whether this request may do this, to this. It writes the
// refusal itself and reports false, so a handler that forgets to stop has
// already sent the right status and cannot also send a body.
//
// It does not return the subject. Every endpoint so far is a read, and none of
// them needs to know who is reading; the first one that writes will need the
// actor for the evidence record, and can have it back then. A return value
// nobody uses is one nobody notices going wrong.
func (g guard) admit(
	w http.ResponseWriter, r *http.Request, action authz.Action,
) (evidence.Ref, bool) {
	ref := evidence.Ref{
		TenantID: r.PathValue("tenant"),
		App:      r.PathValue("app"),
		Env:      r.PathValue("env"),
	}

	live, err := g.sessions.Resolve(r.Context(), r)
	if err != nil {
		problem(w, http.StatusUnauthorized, "not signed in")
		return ref, false
	}

	sub, err := subjectFrom(r.Context(), g.identity, live, ref.TenantID)
	if err != nil {
		// Deliberately the same answer as a refusal. "You are signed in but
		// not a member here" confirms the tenant exists, which is the one
		// thing a stranger probing tenant names wants to learn.
		problem(w, http.StatusForbidden, "no access to this tenant")
		return ref, false
	}

	decision, err := g.authorizer.Authorize(r.Context(), sub, action, authz.Target{
		Tenant: ref.TenantID, App: ref.App, Env: ref.Env,
	})
	if err != nil {
		problem(w, http.StatusInternalServerError, "authorization failed")
		return ref, false
	}
	if !decision.Allow {
		// The reason is returned, because a refusal nobody can explain is a
		// support ticket. It is the authorizer's own words: the free one says
		// which role was read, an enterprise one can say which policy matched.
		problem(w, http.StatusForbidden, decision.Reason)
		return ref, false
	}
	return ref, true
}

// me is who the session belongs to and where they are a member.
//
// The panel needs it to render anything at all, and it is the one endpoint
// that takes no tenant: the membership list IS the answer, so there is nothing
// to authorize against. What it must not do is accept a tenant and describe
// it, which would make it a directory of every tenant on the install.
func (o Options) me(idStore identity.Store, sess *auth.Sessions) http.Handler {
	type membership struct {
		TenantID   string `json:"tenantId"`
		TenantSlug string `json:"tenantSlug"`
		TenantName string `json:"tenantName"`
		Role       string `json:"role"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		live, err := sess.Resolve(r.Context(), r)
		if err != nil {
			problem(w, http.StatusUnauthorized, "not signed in")
			return
		}
		account, err := idStore.Account(r.Context(), live.AccountID)
		if err != nil || account.Disabled {
			// A session whose account was deleted or disabled since it was
			// issued. Not signed in, as far as anything downstream is
			// concerned.
			problem(w, http.StatusUnauthorized, "not signed in")
			return
		}
		rows, err := idStore.Memberships(r.Context(), account.ID)
		if err != nil {
			problem(w, http.StatusInternalServerError, "the identity store is unavailable")
			return
		}

		out := make([]membership, 0, len(rows))
		for _, m := range rows {
			t, err := idStore.Tenant(r.Context(), m.TenantID)
			if err != nil {
				// A membership pointing at a tenant that is gone. Skipped
				// rather than failed: one broken row must not make the panel
				// unusable for every other tenant this person belongs to.
				continue
			}
			out = append(out, membership{
				TenantID: t.ID, TenantSlug: t.Slug, TenantName: t.DisplayName,
				Role: string(m.Role),
			})
		}
		writeJSON(w, map[string]any{
			"account": map[string]string{
				"id": account.ID, "email": account.Email, "displayName": account.DisplayName,
			},
			"memberships": out,
		})
	})
}

// maxHistoryPage bounds one response. Without a cap, `?limit=1000000` is a way
// to ask for the whole table in one query, which is a slow read holding a
// connection rather than an error anybody notices.
const (
	maxHistoryPage     = 200
	defaultHistoryPage = 50
)

// history is the deploy log for one app in one environment, newest first.
func history(g guard, store evidence.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref, ok := g.admit(w, r, authz.ActionEvidenceView)
		if !ok {
			return
		}

		limit := defaultHistoryPage
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				problem(w, http.StatusBadRequest, "limit must be a positive whole number")
				return
			}
			limit = min(n, maxHistoryPage)
		}

		page, err := store.History(r.Context(), evidence.Query{
			Ref: ref, Limit: limit,
			After: evidence.Cursor(r.URL.Query().Get("after")),
			Order: evidence.OrderNewest,
		})
		if err != nil {
			problem(w, http.StatusInternalServerError, "reading the evidence store failed")
			return
		}
		// An app with no deploys is an empty page, not a 404. Current answers
		// 404 because "what is running" has no answer; "what has happened" has
		// one, and it is nothing.
		records := make([]wireRecord, 0, len(page.Records))
		for _, rec := range page.Records {
			records = append(records, toWireRecord(rec))
		}
		writeJSON(w, map[string]any{"records": records, "next": string(page.Next)})
	})
}

// verify recomputes the hash chain and reports whether it holds.
//
// The whole point of the evidence store is that this can be asked, so it is an
// endpoint rather than a CLI-only operation: a claim nobody can check from the
// page they are reading is a claim they have to take on trust.
func verify(g guard, store evidence.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref, ok := g.admit(w, r, authz.ActionEvidenceView)
		if !ok {
			return
		}
		q := r.URL.Query()
		proof, err := store.Verify(r.Context(), ref,
			evidence.Cursor(q.Get("from")), evidence.Cursor(q.Get("to")))
		if err != nil {
			problem(w, http.StatusInternalServerError, "verifying the chain failed")
			return
		}
		// A broken chain is a 200 carrying valid=false, not a 5xx. It is an
		// answer the caller asked for and has to be able to render; a status
		// code that reads as "the server is broken" would send them to the
		// wrong place entirely.
		writeJSON(w, toWireProof(proof))
	})
}

// retention is what the store promises to keep, which the evidence page prints
// next to the history so that "nothing older than this" is visibly a policy
// rather than an absence of data.
func retention(g guard, store evidence.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := g.admit(w, r, authz.ActionEvidenceView); !ok {
			return
		}
		policy, err := store.Retention(r.Context())
		if err != nil {
			problem(w, http.StatusInternalServerError, "reading the retention policy failed")
			return
		}
		writeJSON(w, map[string]any{
			"windowSeconds": int64(policy.Window.Seconds()),
			"keepCurrent":   policy.KeepCurrent,
			"immutable":     policy.Immutable,
			"anchor":        policy.Anchor,
			"tier":          string(policy.Tier),
		})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already written; there is nothing left to say to the
		// client and nothing to do but let the connection close.
		return
	}
}

// export streams the evidence log for one app and environment.
//
// It writes the store's own encoding and not the wire types above, and that is
// deliberate. An export exists to be checked later — re-read, re-hashed, shown
// to somebody who was not there — so it has to carry the form the chain was
// computed over. The wire types are the opposite: a shape chosen to be
// convenient for a panel, which is free to change the day the panel does.
//
// The action is evidence:export rather than evidence:view. Reading one page in
// a browser and taking the whole log away are the same information at very
// different scales, and an install that wants the second restricted should not
// have to give up the first.
func export(g guard, store evidence.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref, ok := g.admit(w, r, authz.ActionEvidenceExport)
		if !ok {
			return
		}

		// Only what the store actually writes. csv is a declared format that
		// no store implements, and offering it here produced exactly the
		// failure the constant now warns about: a response headed text/csv,
		// named .csv, carrying JSONL.
		const format = evidence.ExportJSONL
		if got := r.URL.Query().Get("format"); got != "" && got != string(format) {
			problem(w, http.StatusBadRequest, "the only export format is jsonl")
			return
		}

		filename := "damga-" + ref.TenantID + "-" + ref.App + "-" + ref.Env + "." + string(format)
		w.Header().Set("Content-Type", "application/jsonl")
		// Quoted, because a tenant id or an app name with a space in it would
		// otherwise truncate the filename at the space.
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

		// Once Export writes its first byte the status is 200 and the headers
		// are gone. There is no way to turn a failure here into an error the
		// caller sees as an error, so it is logged and the response is left
		// truncated — which is at least detectable, unlike a 200 carrying a
		// short file that claims to be complete.
		//
		// Detectable because the last line of a JSONL export is a record whose
		// hash chains to the one before it: a truncated export fails to verify
		// at the point it was cut.
		if _, err := store.Export(r.Context(), evidence.ExportRequest{
			Query:  evidence.Query{Ref: ref, Order: evidence.OrderOldest},
			Format: format,
		}, w); err != nil {
			slog.Default().Error("the evidence export was cut short",
				"tenant", ref.TenantID, "app", ref.App, "env", ref.Env, "error", err)
		}
	})
}
