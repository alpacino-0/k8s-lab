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
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/identity"
)

// ErrAlreadyBootstrapped is returned when this install already has an owner.
// Named rather than described, because the caller's exit code depends on it:
// running bootstrap twice is a mistake, not a failure, and a deployment script
// that reruns it should be able to tell those apart.
var ErrAlreadyBootstrapped = errors.New("this install already has an owner")

// Bootstrap creates the first tenant and the first owner.
//
// It is a subcommand and not an HTTP endpoint, and that is the whole design.
// The alternatives were considered and each gives something away:
//
//   - A first-run window where whoever arrives first becomes owner is a race
//     against the internet. Between `kubectl apply` and the operator opening a
//     browser there is a period, however short, in which the platform belongs
//     to whoever finds it.
//   - A one-time token printed by the server at startup lands in the pod's
//     stdout — which, in this repository, Alloy ships to Loki unfiltered. The
//     token would be readable by everyone with log access, which is a wider
//     set than everyone with install authority, and it would stay readable for
//     the retention period.
//   - A token written to a Secret needs the control plane to hold create
//     permission on Secrets in its own namespace, forever, to use it once.
//
// A subcommand needs none of that, because it asks for an authority the
// operator already had to have: reaching the database. `kubectl exec` streams
// through the CRI exec channel and is never appended to the container log the
// collector tails, so the generated password is shown to the person who ran
// the command and to nobody else. Outside Kubernetes it is a terminal.
//
// The password is returned rather than printed, so the caller owns where it
// goes and this package stays testable without capturing os.Stdout.
func Bootstrap(ctx context.Context, c Config, req BootstrapRequest) (BootstrapResult, error) {
	email := strings.TrimSpace(req.Email)
	slug := strings.TrimSpace(req.TenantSlug)
	if email == "" || slug == "" {
		return BootstrapResult{}, fmt.Errorf("bootstrap needs an email and a tenant")
	}
	if !strings.Contains(email, "@") {
		return BootstrapResult{}, fmt.Errorf("%q is not an email address", email)
	}

	if strings.TrimSpace(c.EvidenceDSN) == "" {
		// openIdentity would hand back an in-memory store, and this command
		// would report an owner into a database that vanishes when it exits.
		return BootstrapResult{}, fmt.Errorf("bootstrap needs a database: pass -evidence-dsn")
	}

	// A discarding logger: openIdentity only logs the missing-DSN warning,
	// which cannot happen here, and this command's output is a credential the
	// operator is reading — nothing else belongs in it.
	idStore, err := openIdentity(ctx, c, slog.New(slog.DiscardHandler))
	if err != nil {
		return BootstrapResult{}, err
	}
	defer func() { _ = idStore.Close() }()

	// Asked before doing any work, so the common mistake — running this twice —
	// gets an answer that says what happened rather than a duplicate-key error
	// the operator has to interpret.
	if done, err := idStore.Bootstrapped(ctx); err != nil {
		return BootstrapResult{}, fmt.Errorf("reading the identity store: %w", err)
	} else if done {
		return BootstrapResult{}, ErrAlreadyBootstrapped
	}

	password := strings.TrimSpace(req.Password)
	generated := password == ""
	if generated {
		// 26 base32 characters, ~130 bits. Long enough that its strength does
		// not depend on the owner changing it, because they will not.
		password = rand.Text()
	}
	if len([]rune(password)) < 12 {
		return BootstrapResult{}, fmt.Errorf("the password is too short: %d characters, minimum 12", len([]rune(password)))
	}

	hash, err := auth.NewHasher(auth.DefaultParams, 1).Hash(password)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("hashing the password: %w", err)
	}

	accountID := "a_" + newID()
	account := identity.Account{
		ID: accountID, Kind: "user", Email: email,
		// An instance-local alias, never the login address. This is what gets
		// copied into git commits and into the evidence chain, where it can
		// never be redacted — so what is published was never personal data.
		AuditEmail:  accountID + "@users.damga.local",
		DisplayName: cmpOr(strings.TrimSpace(req.DisplayName), email),
	}
	tenant := identity.Tenant{
		ID: "t_" + newID(), Slug: slug,
		DisplayName: cmpOr(strings.TrimSpace(req.TenantName), slug),
		// Always free. The tier is provenance copied onto evidence records,
		// and a bootstrap that could write 'enterprise' would be a licence
		// check with a command-line flag for a bypass.
		Tier: identity.TierFree,
	}

	if err := idStore.Bootstrap(ctx, tenant, account, identity.Credential{Hash: hash}); err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{
		AccountID: account.ID, Email: account.Email,
		TenantID: tenant.ID, TenantSlug: tenant.Slug,
		Password: password, Generated: generated,
	}, nil
}

// BootstrapRequest is what the operator supplies. Everything except the email
// and the tenant slug has a defensible default.
type BootstrapRequest struct {
	Email       string
	DisplayName string
	TenantSlug  string
	TenantName  string
	// Password is optional. Empty means generate one, which is the path that
	// does not put a password in the operator's shell history.
	Password string
}

// BootstrapResult carries the created identifiers and, once, the password.
type BootstrapResult struct {
	AccountID  string
	Email      string
	TenantID   string
	TenantSlug string
	// Password is the plaintext, returned exactly once and stored nowhere.
	Password  string
	Generated bool
}

func newID() string { return strings.ToLower(rand.Text()[:12]) }

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
