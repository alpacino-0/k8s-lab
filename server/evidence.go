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
	"errors"
	"net/http"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/identity"
)

// currentEvidence answers what is deployed right now for one app in one
// environment. It is one endpoint rather than a whole API because it is the one
// that exercises every seam at once: who is asking comes from a session and a
// membership row, whether they may is Options.Authorizer, and what is returned
// is Options.Evidence.
func currentEvidence(
	a authz.Authorizer, store evidence.Store, idStore identity.Store, sess *auth.Sessions,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := evidence.Ref{
			TenantID: r.PathValue("tenant"),
			App:      r.PathValue("app"),
			Env:      r.PathValue("env"),
		}

		live, err := sess.Resolve(r.Context(), r)
		if err != nil {
			problem(w, http.StatusUnauthorized, "not signed in")
			return
		}

		// The subject is built from the session and the membership row, and
		// from nothing the caller sent. A subject assembled from a request
		// parameter would be a viewer in whatever tenant it named — and a
		// viewer may read this page.
		sub, err := subjectFrom(r.Context(), idStore, live, ref.TenantID)
		if err != nil {
			// Deliberately the same answer as a refusal. "You are signed in but
			// not a member here" confirms the tenant exists, which is the one
			// thing a stranger probing tenant names wants to learn.
			problem(w, http.StatusForbidden, "no access to this tenant")
			return
		}

		decision, err := a.Authorize(r.Context(), sub, authz.ActionEvidenceView, authz.Target{
			Tenant: ref.TenantID, App: ref.App, Env: ref.Env,
		})
		if err != nil {
			problem(w, http.StatusInternalServerError, "authorization failed")
			return
		}
		if !decision.Allow {
			// The reason is returned, because a refusal nobody can explain is
			// a support ticket. It is the authorizer's own words: the free one
			// says which role was read, an enterprise one can say which policy
			// matched.
			problem(w, http.StatusForbidden, decision.Reason)
			return
		}

		rec, err := store.Current(r.Context(), ref)
		switch {
		case errors.Is(err, evidence.ErrNotFound):
			// Not an error. An app that has never been deployed is a normal
			// state and the page has to render it as one.
			problem(w, http.StatusNotFound, "nothing has been deployed here yet")
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "reading the evidence store failed")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(rec); err != nil {
			// The status is already written; there is nothing to say to the
			// client, and the connection is the only thing left to close.
			return
		}
	})
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "detail": detail})
}
