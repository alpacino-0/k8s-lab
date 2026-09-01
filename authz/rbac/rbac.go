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

// Package rbac is the free authorizer: owner, member, viewer, and nothing else.
package rbac

import (
	"context"

	"github.com/damgahq/damga/authz"
)

// The three roles the free tier knows. An identity provider will send groups
// this package has never heard of; the answer to those is viewer, not an error.
const (
	roleOwner  = "owner"
	roleMember = "member"
	roleViewer = "viewer"
)

type Authorizer struct{}

func New() *Authorizer { return &Authorizer{} }

func (a *Authorizer) Authorize(
	_ context.Context, s authz.Subject, act authz.Action, t authz.Target,
) (authz.Decision, error) {
	if s.Tenant == "" || s.Tenant != t.Tenant {
		return authz.Decision{Reason: "subject belongs to a different tenant"}, nil
	}
	role := roleViewer
	for _, g := range s.Groups {
		switch g {
		case roleOwner:
			role = roleOwner
		case roleMember:
			if role != roleOwner {
				role = roleMember
			}
		}
	}
	switch act {
	case authz.ActionAppView, authz.ActionEvidenceView:
		return authz.Decision{Allow: true, Reason: role + " may read"}, nil
	case authz.ActionAppDeploy, authz.ActionAppRollback, authz.ActionEnvCreate, authz.ActionEvidenceExport:
		return authz.Decision{Allow: role != roleViewer, Reason: role + " deploy right"}, nil
	case authz.ActionAppRestart, authz.ActionAppScale:
		// Weaker than deploying, and separate from it on purpose. Neither can
		// introduce code: a restart replaces pods with the image that is
		// already committed, and a scale changes how many of them there are.
		// Folding them into app:deploy would mean an install that wants
		// somebody who may keep the lights on without shipping releases has no
		// way to say so — and the action set is closed precisely so that a new
		// endpoint forces this decision instead of borrowing a neighbour's.
		return authz.Decision{Allow: role != roleViewer, Reason: role + " may change how an app runs"}, nil
	case authz.ActionAppDelete:
		// Grouped with deploying rather than with administration, and it was
		// tenant:admin for one release because this action did not exist yet.
		// What a delete removes is a row: the manifests stay committed, what is
		// running keeps running, and the deploy history stays readable. There is
		// no reason somebody who may create an app and ship to it may not also
		// unregister one. A viewer still may not.
		return authz.Decision{Allow: role != roleViewer, Reason: role + " may unregister an app"}, nil
	case authz.ActionAppConnect:
		// Owner only, and named here rather than left to the default, because
		// the reason is not obvious from the verb. Deploying ships an image;
		// connecting decides which signer is trusted for every image this app
		// will ever run. Somebody who can do the second can arrange to be the
		// one who does the first, so grouping it with the deploy right would
		// make the signature check a formality for anybody who could already
		// deploy.
		return authz.Decision{Allow: role == roleOwner, Reason: role + " may choose the signer"}, nil
	default:
		return authz.Decision{Allow: role == roleOwner, Reason: role + " administrative right"}, nil
	}
}
