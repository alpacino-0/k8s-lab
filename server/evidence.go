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

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
)

// currentEvidence answers what is deployed right now for one app in one
// environment. It is one endpoint rather than a whole API because it is the one
// that exercises both seams at once: the answer is refused or allowed by
// Options.Authorizer and read from Options.Evidence, so a second repository
// substituting either of them is exercised by this handler and not only by the
// compiler.
//
// The subject is read from headers. That is not the identity mechanism — there
// is none yet — and the headers are named so that no one mistakes it for one.
// It is what lets the seam be wired and tested before the session layer exists,
// and it is refused outright unless the caller opted in, so it cannot be left
// on by accident in something real.
func currentEvidence(a authz.Authorizer, store evidence.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := evidence.Ref{
			TenantID: r.PathValue("tenant"),
			App:      r.PathValue("app"),
			Env:      r.PathValue("env"),
		}

		sub, ok := subjectFromRequest(r)
		if !ok {
			problem(w, http.StatusUnauthorized, "no subject on the request")
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

// subjectFromRequest is a placeholder with a loud name. Identity is an open
// decision — "log in with GitHub" is OIDC by any ordinary reading, and SSO is
// the paid trigger — so nothing here should look like it settled that.
func subjectFromRequest(r *http.Request) (authz.Subject, bool) {
	id := r.Header.Get("X-Damga-Insecure-Subject")
	tenant := r.Header.Get("X-Damga-Insecure-Tenant")
	if id == "" || tenant == "" {
		return authz.Subject{}, false
	}
	var groups []string
	if g := r.Header.Values("X-Damga-Insecure-Group"); len(g) > 0 {
		groups = g
	}
	return authz.Subject{ID: id, Tenant: tenant, Groups: groups}, true
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "detail": detail})
}
