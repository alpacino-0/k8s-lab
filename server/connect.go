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
	"github.com/damgahq/damga/forge"
)

// connectRequest is what the panel sends to point an app at the repository its
// own CI builds and signs from.
//
// Host and the workflow path are optional because damga picks them; the rest is
// the tenant's and is not guessed. Branch in particular is required, for the
// reason placement gives about its own: a wrong guess produces a policy pinned
// to an identity the workflow will never present, and the failure is a chain
// that silently never verifies rather than an error anybody sees.
type connectRequest struct {
	Host            string `json:"host,omitempty"`
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	Branch          string `json:"branch"`
	WorkflowPath    string `json:"workflowPath,omitempty"`
	ImageRepository string `json:"imageRepository"`
}

// DefaultWorkflowPath is where damga proposes to put the file it writes.
//
// Chosen by the platform rather than the tenant because it is half of the
// signing identity, and a path the tenant picks is a path the tenant can change
// afterwards without anything noticing. It stays a field on the connection so
// an existing file can be adopted, but nothing has to think of it to connect.
const DefaultWorkflowPath = ".github/workflows/damga-sign.yml"

// connection is what both handlers answer with.
//
// Identity and verified are computed rather than stored on the wire, because
// they are the two questions anybody looking at this page is actually asking:
// who is allowed to sign for this app, and has that ever happened.
type connectionView struct {
	Host            string `json:"host"`
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	Branch          string `json:"branch"`
	WorkflowPath    string `json:"workflowPath"`
	ImageRepository string `json:"imageRepository"`

	Identity string `json:"identity"`
	Verified bool   `json:"verified"`
	// Enforcing is what the rendered policy will do today, said in the words
	// the operator will see on the object rather than left to be inferred from
	// verified.
	Enforcement string `json:"enforcement"`
}

func viewOf(c forge.Connection) connectionView {
	enforcement := "recording"
	if c.Verified() {
		enforcement = "rejecting"
	}
	return connectionView{
		Host: c.Host, Owner: c.Owner, Repo: c.Repo, Branch: c.Branch,
		WorkflowPath: c.WorkflowPath, ImageRepository: c.ImageRepository,
		Identity: c.Identity(), Verified: c.Verified(), Enforcement: enforcement,
	}
}

// connectRoute points an app at the repository that builds and signs it.
//
// Owner-only, and deliberately not the deploy right. Deploying ships an image;
// connecting decides which signer is trusted for every image this app will ever
// run. Somebody who can do the second can arrange to be the one who does the
// first, so putting them under one action would make the signature check a
// formality for anyone who could already deploy.
func connectRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ref, ok := g.admit(w, r, authz.ActionAppConnect)
		if !ok {
			return
		}
		_ = sub

		if st.forge == nil {
			problem(w, http.StatusNotImplemented, "this installation has no forge store configured")
			return
		}

		var req connectRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "the request body is not the expected JSON")
			return
		}

		if req.Host == "" {
			req.Host = forge.SupportedHost
		}
		if req.WorkflowPath == "" {
			req.WorkflowPath = DefaultWorkflowPath
		}

		c := forge.Connection{
			TenantID: ref.TenantID, App: ref.App,
			Host: req.Host, Owner: req.Owner, Repo: req.Repo, Branch: req.Branch,
			WorkflowPath: req.WorkflowPath, ImageRepository: req.ImageRepository,
		}

		// Validated before the store is touched, so the reason comes back as
		// the sentence that says which field is wrong rather than as whatever
		// the database made of it.
		if err := c.Validate(); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, forge.ErrUnsupportedHost) {
				// Not the caller's mistake. Public Fulcio does not accept this
				// forge, so no request they could send would work, and a 400
				// invites them to keep trying.
				status = http.StatusUnprocessableEntity
			}
			problem(w, status, err.Error())
			return
		}

		saved, err := st.forge.Put(r.Context(), c)
		switch {
		case errors.Is(err, forge.ErrConflict):
			// Says what happened without saying who. That another tenant holds
			// this identity is the answer; which tenant is not the caller's to
			// learn, and a repository name is enough to go looking with.
			problem(w, http.StatusConflict,
				"this repository and branch are already connected elsewhere")
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "writing the connection failed")
			return
		}

		workflow, err := saved.Workflow()
		if err != nil {
			problem(w, http.StatusInternalServerError, "rendering the workflow failed")
			return
		}

		// The workflow comes back on the create, not just on a later read.
		// Connecting a repository and being shown the file that will be
		// proposed is one action from the person's side, and the whole design
		// rests on them reading it before they merge it.
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{
			"connection": viewOf(saved),
			"workflow": map[string]any{
				"path":    saved.WorkflowPath,
				"content": string(workflow),
			},
		})
	})
}

// connectionRoute reads back what an app is connected to.
func connectionRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionAppView)
		if !ok {
			return
		}
		if st.forge == nil {
			problem(w, http.StatusNotImplemented, "this installation has no forge store configured")
			return
		}

		c, err := st.forge.Get(r.Context(), forge.Key{TenantID: ref.TenantID, App: ref.App})
		switch {
		case errors.Is(err, forge.ErrNotFound):
			problem(w, http.StatusNotFound, "this app is not connected to a repository")
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "reading the connection failed")
			return
		}

		workflow, err := c.Workflow()
		if err != nil {
			problem(w, http.StatusInternalServerError, "rendering the workflow failed")
			return
		}
		writeJSON(w, map[string]any{
			"connection": viewOf(c),
			"workflow": map[string]any{
				"path":    c.WorkflowPath,
				"content": string(workflow),
			},
		})
	})
}

// proposeRoute opens the pull request that carries the signing workflow.
//
// Its own call rather than part of connecting, because the two fail
// differently: storing a connection is local, and this is a request into a
// forge somebody else runs. Connecting would otherwise fail because GitHub was
// having an afternoon, and the tenant would have nothing — not even the file to
// add by hand.
//
// Safe to repeat, which is the property that makes a button honest. The
// implementation finds the pull request a previous attempt opened instead of
// opening a second one in a repository damga does not own.
func proposeRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The same right as connecting, because it is the same decision
		// reaching further: this writes a branch into the tenant's own
		// repository, which is not something the deploy right should carry.
		_, ref, ok := g.admit(w, r, authz.ActionAppConnect)
		if !ok {
			return
		}
		if st.forge == nil {
			problem(w, http.StatusNotImplemented, "this installation has no forge store configured")
			return
		}
		if st.proposer == nil {
			// Not an error in the install. The workflow is on screen and can be
			// added by hand, which is the documented fallback for every forge
			// this build cannot reach — so the message says what to do rather
			// than only what is missing.
			problem(w, http.StatusNotImplemented,
				"this installation cannot open pull requests; add the workflow shown here by hand")
			return
		}

		c, err := st.forge.Get(r.Context(), forge.Key{TenantID: ref.TenantID, App: ref.App})
		switch {
		case errors.Is(err, forge.ErrNotFound):
			problem(w, http.StatusConflict, "this app is not connected to a repository yet")
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "reading the connection failed")
			return
		}

		proposed, err := st.proposer.Propose(r.Context(), c)
		switch {
		case errors.Is(err, forge.ErrNotPermitted):
			// The tenant fixes this by granting access, so it is theirs to see
			// and not a 500 that sends them to the platform's operator.
			problem(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			problem(w, http.StatusBadGateway, "the forge did not accept the pull request: "+err.Error())
			return
		}

		status := http.StatusCreated
		if proposed.Existing {
			// Nothing was created, so it does not say 201. The panel says "your
			// pull request is still open" off this rather than claiming to have
			// made one every time somebody presses the button.
			status = http.StatusOK
		}
		w.WriteHeader(status)
		writeJSON(w, map[string]any{
			"pullRequest": map[string]any{
				"url": proposed.URL, "number": proposed.Number,
				"branch": proposed.Branch, "existing": proposed.Existing,
			},
		})
	})
}
