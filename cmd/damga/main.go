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

// Command damga is the control plane: a thin main over the server package.
//
// It is thin on purpose. Wiring belongs in server.Options, where it can be
// tested and substituted; logic that accumulates in a main is logic no test
// reaches.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/damgahq/damga/panel"
	"github.com/damgahq/damga/server"
)

// exitAlreadyBootstrapped is its own code so a deployment script can rerun this
// command safely. "Already done" is not a failure, and a script that cannot
// distinguish it from one has to either parse the message or never rerun.
const exitAlreadyBootstrapped = 3

func main() {
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		os.Exit(bootstrap(os.Args[2:]))
	}

	var opts server.Options
	// The one implementation this main supplies rather than defaults.
	// Everything else it passes is a default the server picks for itself.
	opts.Panel = panel.FS()
	opts.Config.BindFlags(flag.CommandLine)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, opts); err != nil {
		slog.Error("damga stopped", "error", err)
		os.Exit(1)
	}
}

// bootstrap creates the first tenant and the first owner, then prints the
// password once.
//
// It writes to this process's stdout, which is safe precisely because this
// process is not the server: an operator runs it through `kubectl exec` or a
// terminal, and neither is the container log stream that the cluster's log
// collector tails. Printing the same string from the running server would put
// it in Loki for the retention period.
func bootstrap(argv []string) int {
	fs := flag.NewFlagSet("damga bootstrap", flag.ExitOnError)
	var (
		cfg           server.Config
		req           server.BootstrapRequest
		passwordStdin = fs.Bool("password-stdin", false,
			"read the password from stdin instead of generating one")
	)
	cfg.BindFlags(fs)
	fs.StringVar(&req.Email, "email", "", "the owner's email address (required)")
	fs.StringVar(&req.DisplayName, "name", "", "the owner's display name (default: the email)")
	fs.StringVar(&req.TenantSlug, "tenant", "", "the first tenant's slug (required)")
	fs.StringVar(&req.TenantName, "tenant-name", "", "the first tenant's display name (default: the slug)")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `damga bootstrap creates the first tenant and its owner.

It runs once per install and refuses to run again. There is no HTTP equivalent:
the authority it needs is reaching the database, which is the same authority
installing the platform needed, and no window exists in which a stranger can
claim an unowned install.

  damga bootstrap -evidence-dsn /var/lib/damga/damga.db \
      -email you@example.com -tenant acme

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}

	if *passwordStdin {
		// No -password flag. A password on the command line is in the shell
		// history, in the process table while it runs, and in the audit log of
		// anything that records exec calls -- including kubectl.
		pw, err := readPassword(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "reading the password from stdin:", err)
			return 1
		}
		req.Password = pw
	}

	res, err := server.Bootstrap(context.Background(), cfg, req)
	switch {
	case errors.Is(err, server.ErrAlreadyBootstrapped):
		fmt.Fprintln(os.Stderr, "damga: this install already has an owner; nothing to do")
		return exitAlreadyBootstrapped
	case err != nil:
		fmt.Fprintln(os.Stderr, "damga: bootstrap failed:", err)
		return 1
	}

	fmt.Printf(`
Damga is ready.

  Tenant   %s  (%s)
  Owner    %s  (%s)
`, res.TenantSlug, res.TenantID, res.Email, res.AccountID)
	if res.Generated {
		fmt.Printf(`  Password %s

That password is shown here once and is stored only as a hash. Copy it now.
`, res.Password)
	}
	fmt.Println()
	return 0
}

// readPassword takes everything on stdin, so a password containing spaces
// survives, and strips one trailing newline, so `echo` and a heredoc both work.
func readPassword(r io.Reader) (string, error) {
	// Bounded: stdin here is a person or a Secret, and an unbounded read from
	// a pipe nobody closes is a command that hangs with no output.
	b, err := io.ReadAll(io.LimitReader(r, 4<<10))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
