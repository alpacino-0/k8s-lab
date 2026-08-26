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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/authz/rbac"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/evidence/postgres"
	"github.com/damgahq/damga/evidence/sqlite"
)

// Run starts the control plane and blocks until ctx is cancelled.
//
// With Config.ObserveDeploys off it needs no cluster at all and serves the panel
// and the API from a plain HTTP server. With it on, a controller-runtime manager
// owns the process: the HTTP server is handed to it as a *manager.Server so that
// it keeps answering on every replica while the observer and the sweep run on
// the leader only.
func Run(ctx context.Context, o Options) error {
	o = o.withDefaults()
	log := slog.Default()

	store := o.Evidence
	if store == nil {
		var err error
		store, err = openStore(ctx, o.Config, log)
		if err != nil {
			return err
		}
		defer func() {
			if err := store.Close(); err != nil {
				log.Error("closing the evidence store", "error", err)
			}
		}()
	}

	handler, err := o.handler(store)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    o.Config.ListenAddr,
		Handler: handler,
		// Without this a client that opens a connection and sends nothing holds
		// a goroutine until the process dies.
		ReadHeaderTimeout: 10 * time.Second,
	}

	if o.Config.ObserveDeploys {
		return o.runWithManager(ctx, srv, store, log)
	}

	ln, err := net.Listen("tcp", o.Config.ListenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", o.Config.ListenAddr, err)
	}
	log.Info("damga is listening", "address", ln.Addr().String())
	if o.Ready != nil {
		o.Ready(ln.Addr().String())
	}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	// A context of its own, because the one that just fired is already
	// cancelled and would make Shutdown return immediately — which is a
	// hard stop wearing the word "graceful".
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.Config.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return <-serveErr
}

// handler builds the routes, then hands the mux to the Routes hook and wraps
// the result in Middleware.
func (o Options) handler(store evidence.Store) (http.Handler, error) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /api/v1/tenants/{tenant}/apps/{app}/envs/{env}/evidence",
		currentEvidence(o.Authorizer, store))

	// Mounted only when there is one. There is no built-in bundle yet, and
	// serving a 404 at "/" from an embedded empty FS would be a worse answer
	// than not claiming the route at all — an enterprise build could then not
	// take it either, because registering it twice panics.
	if o.Panel != nil {
		mux.Handle("GET /", http.FileServerFS(o.Panel))
	}

	if o.Routes != nil {
		// Handed the mux rather than a handler to wrap, so a route that
		// collides with a core one panics here instead of shadowing an
		// endpoint the CLI depends on.
		o.Routes(mux)
	}

	var h http.Handler = mux
	if o.Middleware != nil {
		if h = o.Middleware(h); h == nil {
			return nil, errors.New("server: Middleware returned a nil handler")
		}
	}
	return h, nil
}

// openStore picks an engine from the DSN. This function is the only place in
// the product that knows which engine is in use; everything else has an
// evidence.Store.
func openStore(ctx context.Context, c Config, log *slog.Logger) (evidence.Store, error) {
	switch dsn := c.EvidenceDSN; {
	case dsn == "":
		// Said out loud rather than silently choosing a file. An evidence store
		// that forgets on restart is a demo, and the difference between a demo
		// and an installation should not be discovered after the first upgrade.
		log.Warn("no evidence DSN: records are kept in memory and lost on restart",
			"flag", "-evidence-dsn")
		return memory.New(c.RetentionWindow), nil
	case strings.HasPrefix(dsn, "postgres://"),
		strings.HasPrefix(dsn, "postgresql://"),
		strings.HasPrefix(dsn, "pgx://"):
		return postgres.Open(ctx, dsn, postgres.Options{Window: c.RetentionWindow})
	default:
		return sqlite.Open(ctx, dsn, sqlite.Options{Window: c.RetentionWindow})
	}
}

func (o Options) withDefaults() Options {
	if o.Authorizer == nil {
		o.Authorizer = authz.Authorizer(rbac.New())
	}
	if o.Config.ListenAddr == "" {
		o.Config.ListenAddr = ":8080"
	}
	if o.Config.ShutdownTimeout == 0 {
		o.Config.ShutdownTimeout = 15 * time.Second
	}
	return o
}
