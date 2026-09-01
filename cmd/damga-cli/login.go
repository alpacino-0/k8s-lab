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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// meResponse is what GET /me answers.
type meResponse struct {
	Account struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
	} `json:"account"`
	Memberships []struct {
		TenantID   string `json:"tenantId"`
		TenantSlug string `json:"tenantSlug"`
		TenantName string `json:"tenantName"`
		Role       string `json:"role"`
	} `json:"memberships"`
}

// loginCmd exchanges an email and a password for a session.
//
// There is no --password flag, and that is the same decision `damga bootstrap`
// records rather than a different one: a password in argv is in the shell
// history, in the process table for as long as the command runs, and in the
// audit log of anything that records process execution. A terminal prompt is
// read with the echo off; anything that is not a terminal is read from stdin,
// so `... | damga-cli login` works in a pipeline without a flag that tempts
// somebody to type the password on the line instead.
func (e *env) loginCmd() *cobra.Command {
	var email string
	var passwordStdin bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to a control plane and remember the session",
		Long: `login exchanges an email and a password for a session cookie and writes it
to the session file, 0600.

The password is never a flag. It is read from the terminal with the echo off,
or from stdin when this is not a terminal:

  damga-cli login --server https://demo.example.com --email you@example.com
  printf '%s' "$PASSWORD" | damga-cli login --server ... --email ... --password-stdin

A session is bound by the control plane to the host that issued it, so the
--server used here is the one every later command talks to.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return e.login(cmd, email, passwordStdin)
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "the account's email address (required)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false,
		"read the password from stdin even when this is a terminal")
	return cmd
}

func (e *env) login(cmd *cobra.Command, email string, passwordStdin bool) error {
	if strings.TrimSpace(email) == "" {
		return fmt.Errorf("%w: login needs --email", errUsage)
	}
	if strings.TrimSpace(e.server) == "" {
		return fmt.Errorf("%w: login needs --server (or $DAMGA_SERVER): "+
			"there is no stored session to take one from", errUsage)
	}
	path, err := sessionPath(e.sessionFile)
	if err != nil {
		return err
	}

	password, err := e.readPassword(passwordStdin)
	if err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("%w: no password was given", errUsage)
	}

	c, err := newClient(e.server, session{}, e.timeout)
	if err != nil {
		return err
	}

	var set []*http.Cookie
	err = c.do(cmd.Context(), call{
		route:   routeLogin,
		body:    map[string]string{"email": email, "password": password},
		cookies: &set,
	})
	if err != nil {
		return err
	}

	sess := session{Server: c.base.String(), Email: email}
	for _, got := range set {
		// The first cookie with a value. There is one, and taking it by
		// position rather than by name is what keeps the name out of this
		// binary — the server owns that string and this client should not hold
		// a second copy of it to disagree with.
		if got.Value != "" {
			sess.CookieName, sess.Cookie = got.Name, got.Value
			break
		}
	}
	if sess.Cookie == "" {
		// A 200 with no cookie. Something between here and the control plane
		// stripped it — an ingress, a proxy — and every later command would
		// report "not signed in" from a login that looked like it worked.
		return fmt.Errorf("%s accepted the password but set no session cookie", c.base.Host)
	}
	c.cookie = &http.Cookie{Name: sess.CookieName, Value: sess.Cookie}

	var me meResponse
	if err := c.do(cmd.Context(), call{route: routeMe, out: &me}); err != nil {
		return err
	}
	sess.Tenant, err = defaultTenant(me, e.tenant)
	if err != nil {
		return err
	}
	if err := saveSession(path, sess); err != nil {
		return err
	}

	printf(e.stdout, "Signed in to %s as %s.\n", c.base.Host, displayName(me))
	if sess.Tenant == "" {
		printf(e.stdout,
			"This account belongs to %d tenants, so no default was chosen. "+
				"Pick one with `damga-cli use <tenant>`.\n", len(me.Memberships))
		return nil
	}
	printf(e.stdout, "Tenant %s.\n", sess.Tenant)
	return nil
}

// defaultTenant decides which tenant later commands work in.
//
// One membership is the ordinary install and needs no decision. More than one
// and nothing is chosen, deliberately: guessing the first would make `deploy`
// ship to whichever tenant sorted earliest, and the person who typed it would
// have no reason to look.
func defaultTenant(me meResponse, asked string) (string, error) {
	asked = strings.TrimSpace(asked)
	if asked != "" {
		for _, m := range me.Memberships {
			// Either spelling, because the id is what the API path takes and
			// the slug is what a person knows. Resolving it here is why the
			// stored value is always the id.
			if asked == m.TenantID || asked == m.TenantSlug {
				return m.TenantID, nil
			}
		}
		return "", fmt.Errorf("%w: this account is not a member of a tenant called %q", errUsage, asked)
	}
	if len(me.Memberships) == 1 {
		return me.Memberships[0].TenantID, nil
	}
	return "", nil
}

func displayName(me meResponse) string {
	if me.Account.DisplayName != "" {
		return me.Account.DisplayName
	}
	return me.Account.Email
}

// readPassword takes the password from wherever it is.
func (e *env) readPassword(forceStdin bool) (string, error) {
	if f, ok := e.stdin.(*os.File); ok && !forceStdin && term.IsTerminal(int(f.Fd())) {
		printf(e.stderr, "Password: ")
		raw, err := term.ReadPassword(int(f.Fd()))
		printline(e.stderr)
		if err != nil {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		return string(raw), nil
	}
	// Bounded, and one trailing newline stripped so that `echo` and a heredoc
	// both work — the same handling cmd/damga's bootstrap gives stdin, because
	// a password that silently gains a newline fails to log in with a message
	// that says the password is wrong.
	raw, err := io.ReadAll(io.LimitReader(e.stdin, 4<<10))
	if err != nil {
		return "", fmt.Errorf("reading the password from stdin: %w", err)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// logoutCmd revokes the session and forgets it.
func (e *env) logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the session and remove the session file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := sessionPath(e.sessionFile)
			if err != nil {
				return err
			}
			// The file goes whatever the server says, which is what the panel
			// does too: the person asked to be logged out, and leaving a
			// credential on disk because the network was down is the one
			// outcome that is wrong in both directions.
			c, _, err := e.signedIn()
			if err == nil {
				if err := c.do(cmd.Context(), call{route: routeLogout}); err != nil {
					printf(e.stderr,
						"damga: the server was not reached, so the session may still be live there: %v\n", err)
				}
			} else if !errors.Is(err, errNotSignedIn) {
				printf(e.stderr, "damga: %v\n", err)
			}
			if err := clearSession(path); err != nil {
				return err
			}
			printline(e.stdout, "Signed out.")
			return nil
		},
	}
}

// whoamiCmd prints who the session belongs to and where it may work.
func (e *env) whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the account this session belongs to and its tenants",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, sess, err := e.signedIn()
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := c.do(cmd.Context(), call{route: routeMe, out: &raw}); err != nil {
				return err
			}
			return e.show(raw, func(w io.Writer, body []byte) error {
				return renderMe(w, body, sess.Tenant)
			})
		},
	}
}

// useCmd changes which tenant later commands work in.
//
// Its own command rather than a flag people repeat, and it lives here because
// the value it writes is the one login chose: an account in one tenant never
// needs it, and an account in several needs it once.
func (e *env) useCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <tenant>",
		Short: "Set the tenant later commands work in",
		Long: `use records a default tenant in the session file.

The name may be the tenant's id or its slug; it is resolved against the
memberships GET /api/v1/me returns, and the id is what gets stored, because a
slug can be renamed and every API path takes the id.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, sess, err := e.signedIn()
			if err != nil {
				return err
			}
			var me meResponse
			if err := c.do(cmd.Context(), call{route: routeMe, out: &me}); err != nil {
				return err
			}
			id, err := defaultTenant(me, args[0])
			if err != nil {
				return err
			}
			sess.Tenant = id
			path, err := sessionPath(e.sessionFile)
			if err != nil {
				return err
			}
			if err := saveSession(path, sess); err != nil {
				return err
			}
			printf(e.stdout, "Tenant %s.\n", id)
			return nil
		},
	}
}
