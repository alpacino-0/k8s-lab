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

// Command damga-cli is the terminal client for a damga control plane.
//
// It calls the same HTTP API the panel calls and it cannot call anything else.
// Every request it can make is a row in the table in client.go, and do()
// refuses a route that is not in that table — which makes "the panel is a view;
// there is no capability in one client that is missing from the API" a property
// of the code rather than something a reviewer has to remember. A CLI-only
// endpoint is how the two clients start answering the same question
// differently, and the answer this product sells is the one about what a deploy
// actually did.
//
// It holds no logic of its own about what a deploy means. verify reports what
// /verify said; status reports what /evidence said; --json hands back the
// server's bytes untouched. The panel is written to the same rule and for the
// same reason: two clients that each compute a verdict are two verdicts.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

// Exit codes. Two of them are their own number so that a script does not have
// to parse a message that belongs to the server and may be reworded.
//
// cmd/damga already does this for "already bootstrapped", and this is the same
// decision in a different binary: the two share no codes because they share no
// caller.
const (
	// exitFailed is any other failure: a refusal, a store that is down, a
	// request that did not arrive.
	exitFailed = 1
	// exitUsage is a command line this program could not make sense of.
	exitUsage = 2
	// exitNotSignedIn is a session that is missing, expired, or issued for a
	// different host. All three look the same from here on purpose — the server
	// deliberately refuses to say which, so that a stranger cannot use the
	// difference to learn anything.
	exitNotSignedIn = 3
	// exitChainBroken is `verify` reporting a chain that does not hold. Its own
	// code because this is the one answer the product exists to give: a
	// verification that fails must fail the script that asked for it, not print
	// a line and exit 0 next to every command that succeeded.
	exitChainBroken = 4
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRoot(os.Stdout, os.Stderr, os.Stdin).ExecuteContext(ctx); err != nil {
		printline(os.Stderr, "damga:", err)
		os.Exit(exitFor(err))
	}
}

// exitFor maps a failure onto a code.
//
// The mapping is here and not at each call site, because a status code that
// means "sign in again" is a fact about the API and not about which command was
// running when it arrived.
func exitFor(err error) int {
	var api *apiError
	switch {
	case errors.As(err, &api) && api.status == 401:
		return exitNotSignedIn
	case errors.Is(err, errNotSignedIn):
		return exitNotSignedIn
	case errors.Is(err, errChainBroken):
		return exitChainBroken
	case errors.Is(err, errUsage):
		return exitUsage
	}
	return exitFailed
}
