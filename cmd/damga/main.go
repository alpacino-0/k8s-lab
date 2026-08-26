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

// Command damga is the free control plane: a thin main over the server package.
//
// It is thin on purpose. Everything it does, a second repository's main also
// does — the difference is which implementations it passes — so anything that
// accumulates here is something the enterprise build would have to duplicate or
// fork to get.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/damgahq/damga/server"
)

func main() {
	var opts server.Options
	opts.Config.BindFlags(flag.CommandLine)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, opts); err != nil {
		slog.Error("damga stopped", "error", err)
		os.Exit(1)
	}
}
