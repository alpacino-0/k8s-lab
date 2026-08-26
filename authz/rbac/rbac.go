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
	default:
		return authz.Decision{Allow: role == roleOwner, Reason: role + " administrative right"}, nil
	}
}
