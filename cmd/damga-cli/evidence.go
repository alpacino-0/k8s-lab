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
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// envRead builds one of the read-only commands under an app environment.
//
// A constructor rather than six near-identical RunE bodies. They differ in
// exactly two things — which route they call and which sentence they print —
// and writing them out six times is how the seventh gets the session handling
// subtly wrong.
func (e *env) envRead(
	use, shortHelp, longHelp string, rt route, render func(io.Writer, []byte) error,
) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: shortHelp,
		Long:  longHelp,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, tgt, err := e.envTarget(args)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := c.do(cmd.Context(), call{route: rt, target: tgt, out: &raw}); err != nil {
				return err
			}
			return e.show(raw, render)
		},
	}
}

// envTarget resolves the session, the tenant and the two positional arguments
// every environment-scoped command takes.
func (e *env) envTarget(args []string) (*client, target, error) {
	c, sess, err := e.signedIn()
	if err != nil {
		return nil, target{}, err
	}
	tenant, err := e.tenantOf(sess)
	if err != nil {
		return nil, target{}, err
	}
	return c, target{tenant: tenant, app: args[0], env: args[1]}, nil
}

// statusCmd is what is running now.
//
// An app that has never been deployed answers 404, and that is reported as the
// ordinary state it is rather than as a failure: "what is running" has no
// answer here, and exiting non-zero would make every fresh app look broken to
// whatever script asked.
func (e *env) statusCmd() *cobra.Command {
	cmd := e.envRead("status <app> <env>",
		"Show what is deployed right now",
		"status prints the current evidence record: the image, the commit, who asked for it,\n"+
			"and what admission said about it.",
		routeEvidence, renderRecord)
	inner := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := inner(cmd, args)
		var api *apiError
		if errors.As(err, &api) && api.status == 404 {
			printline(e.stdout, "Nothing has been deployed here yet.")
			return nil
		}
		return err
	}
	return cmd
}

func (e *env) verifyCmd() *cobra.Command {
	return e.envRead("verify <app> <env>",
		"Recompute the hash chain and report whether it holds",
		"verify asks the control plane to recompute the chain over this app's deploy\n"+
			"records and reports what it concluded. Nothing is recomputed here: the point of\n"+
			"the endpoint is that the page, this command and a script cannot reach different\n"+
			"conclusions about one deploy.\n\n"+
			"A chain that does not hold exits 4, so a script that asks does not have to read\n"+
			"the output to find out.",
		routeVerify, renderVerify)
}

func (e *env) retentionCmd() *cobra.Command {
	return e.envRead("retention <app> <env>",
		"Show what the evidence store promises to keep",
		"retention prints the window, whether the current record is always kept, and\n"+
			"whether the store can claim its history is unmodifiable — which this build\n"+
			"reports as false either way, because that is a deployment decision the server\n"+
			"can neither make nor observe.",
		routeRetention, renderRetention)
}

func (e *env) backupCmd() *cobra.Command {
	return e.envRead("backup <app> <env>",
		"Show the app's database backup and when it was last restored",
		"backup prints what the cluster knows about this app's database: when it was last\n"+
			"backed up, and whether that archive was restored into a scratch database and\n"+
			"counted. An install with no cluster to read answers 501 and says so.",
		routeBackup, renderBackup)
}

// historyCmd is the deploy log, newest first.
func (e *env) historyCmd() *cobra.Command {
	var (
		limit int
		after string
	)
	cmd := &cobra.Command{
		Use:   "history <app> <env>",
		Short: "List this app's deploys, newest first",
		Long: `history pages the deploy log. The page cursor the server returns is printed
when there is another page, and passed back with --after.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, tgt, err := e.envTarget(args)
			if err != nil {
				return err
			}
			q := url.Values{}
			if cmd.Flags().Changed("limit") {
				// Sent only when asked for, so the server's own default is the
				// default. A client that always sends one has pinned a number
				// the server owns.
				q.Set("limit", strconv.Itoa(limit))
			}
			if after != "" {
				q.Set("after", after)
			}
			var raw json.RawMessage
			if err := c.do(cmd.Context(), call{
				route: routeHistory, target: tgt, query: q, out: &raw,
			}); err != nil {
				return err
			}
			return e.show(raw, renderHistory)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "how many records to ask for (the server caps this)")
	cmd.Flags().StringVar(&after, "after", "", "the page cursor a previous run printed")
	return cmd
}

// exportCmd writes the whole log for one app environment.
//
// The bytes are copied through untouched. The export is the store's own
// encoding rather than the API's presentation shape, because it exists to be
// re-verified later and therefore has to carry the form the hash chain was
// computed over — so a client that decoded and re-encoded it would hand back a
// file that no longer verifies, and would do it silently.
func (e *env) exportCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "export <app> <env>",
		Short: "Write every record for one app environment, oldest first, as JSONL",
		Long: `export streams the evidence log in the store's own encoding.

The bytes are not touched on the way past. The export carries the form the hash
chain was computed over, so re-encoding it would produce a file that no longer
verifies — and a truncated download fails to verify at the point it was cut,
which is the only reason a short file is detectable at all.

jsonl is the only format. Asking for csv is refused by the server rather than
quietly answered with jsonl.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, tgt, err := e.envTarget(args)
			if err != nil {
				return err
			}
			sink := e.stdout
			if out != "" {
				// 0600: this is one tenant's whole deploy history, and the
				// default umask on a shared build host is not a decision
				// anybody made about it.
				f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
				if err != nil {
					return fmt.Errorf("opening %s: %w", out, err)
				}
				defer func() { _ = f.Close() }()
				sink = f
			}
			if err := c.do(cmd.Context(), call{
				route: routeExport, target: tgt, stream: sink,
			}); err != nil {
				return err
			}
			if out != "" {
				printf(e.stderr, "Wrote %s\n", out)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write to this file instead of stdout")
	return cmd
}
