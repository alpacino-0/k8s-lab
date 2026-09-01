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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// env is everything a command needs that is not a flag of its own.
//
// The three streams are fields rather than os.Stdout and friends read at the
// point of use, so the whole program can be driven by a test — which is the
// only way the route table's promise is worth anything, since what it promises
// is about the requests the commands actually make.
type env struct {
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader

	server      string
	tenant      string
	sessionFile string
	jsonOut     bool
	timeout     time.Duration
}

// newRoot builds the command tree.
func newRoot(stdout, stderr io.Writer, stdin io.Reader) *cobra.Command {
	e := &env{stdout: stdout, stderr: stderr, stdin: stdin}

	root := &cobra.Command{
		Use:   "damga-cli",
		Short: "Deploy and inspect applications on a damga control plane",
		Long: `damga-cli talks to a damga control plane over the same HTTP API the
panel uses. Anything one of them can do, the other can: there is no endpoint
here that the panel cannot reach, and none the panel has that is missing here.

Start with:

  damga-cli login --server https://demo.example.com --email you@example.com

The session it leaves behind is a credential. It is written 0600 and every
later command refuses to read it if that has changed.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// A bare `damga-cli` prints help and exits 0. Without this it would
		// print help and exit 1, which makes a shell that checks the status
		// treat "the user asked what this does" as a failure.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetIn(stdin)

	f := root.PersistentFlags()
	f.StringVar(&e.server, "server", os.Getenv("DAMGA_SERVER"),
		"control plane URL; defaults to $DAMGA_SERVER, then to the one login used")
	f.StringVar(&e.tenant, "tenant", os.Getenv("DAMGA_TENANT"),
		"tenant id; defaults to $DAMGA_TENANT, then to the one login chose")
	f.StringVar(&e.sessionFile, "session-file", "",
		"where the session is kept; defaults to $DAMGA_SESSION_FILE, then to the user config directory")
	f.BoolVar(&e.jsonOut, "json", false,
		"print the server's own JSON, unaltered, instead of a summary")
	f.DurationVar(&e.timeout, "timeout", 30*time.Second,
		"how long to wait for one request")

	root.AddCommand(
		e.loginCmd(), e.logoutCmd(), e.whoamiCmd(), e.useCmd(),
		e.appsCmd(), e.buildCmd(), e.deployCmd(),
		e.statusCmd(), e.historyCmd(), e.verifyCmd(),
		e.retentionCmd(), e.backupCmd(), e.exportCmd(),
	)
	return root
}

// signedIn returns a client carrying the stored session.
//
// It refuses early in the one case that would otherwise be reported as an
// expired login: a --server whose host is not the host the session was issued
// for. The control plane binds a session to its host and answers "not signed
// in" for a mismatch — the same words it uses for an expired session and an
// unknown one, deliberately, so that nothing can be learned from the
// difference. That is right for a stranger and useless for the person who just
// typed the wrong hostname, and this is the one place that knows both hosts.
func (e *env) signedIn() (*client, session, error) {
	path, err := sessionPath(e.sessionFile)
	if err != nil {
		return nil, session{}, err
	}
	sess, err := loadSession(path)
	switch {
	case errors.Is(err, errNoSessionFile):
		return nil, session{}, errNotSignedIn
	case err != nil:
		return nil, session{}, err
	case sess.Cookie == "":
		return nil, session{}, errNotSignedIn
	}

	server := e.server
	if server == "" {
		server = sess.Server
	}
	want, err := parseServer(server)
	if err != nil {
		return nil, session{}, err
	}
	issued, err := parseServer(sess.Server)
	if err != nil {
		return nil, session{}, err
	}
	if !sameHost(want.Host, issued.Host) {
		return nil, session{}, fmt.Errorf(
			"%w: the stored session was issued by %s and a session is bound to the host that "+
				"issued it, so %s will refuse it", errNotSignedIn, issued.Host, want.Host)
	}

	c, err := newClient(server, sess, e.timeout)
	return c, sess, err
}

// tenantOf picks the tenant a command works in.
func (e *env) tenantOf(sess session) (string, error) {
	if t := strings.TrimSpace(e.tenant); t != "" {
		return t, nil
	}
	if t := strings.TrimSpace(sess.Tenant); t != "" {
		return t, nil
	}
	// Reached when the account belongs to more than one tenant, because login
	// only records a default when there is exactly one to record.
	return "", fmt.Errorf("%w: which tenant? pass --tenant, or run `damga-cli whoami` "+
		"to see the ones this account belongs to", errUsage)
}
