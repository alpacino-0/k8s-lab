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
// environment. It is the endpoint the live evidence page opens with.
func currentEvidence(g guard, store evidence.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref, ok := g.admit(w, r, authz.ActionEvidenceView)
		if !ok {
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
		writeJSON(w, toWireRecord(rec))
	})
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "detail": detail})
}
