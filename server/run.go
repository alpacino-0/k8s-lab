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

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/authz/rbac"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/evidence/postgres"
	"github.com/damgahq/damga/evidence/sqlite"
	"github.com/damgahq/damga/identity"
	identitymem "github.com/damgahq/damga/identity/memory"
	identitypg "github.com/damgahq/damga/identity/postgres"
	identitysqlite "github.com/damgahq/damga/identity/sqlite"
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

	idStore := o.Identity
	if idStore == nil {
		var err error
		idStore, err = openIdentity(ctx, o.Config, log)
		if err != nil {
			return err
		}
		defer func() {
			if err := idStore.Close(); err != nil {
				log.Error("closing the identity store", "error", err)
			}
		}()
	}

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

	handler, err := o.handler(store, idStore)
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

// tenantScope is the prefix every endpoint that reads one tenant's data
// shares. The tenant is a path segment and never a header or a body field: it
// has to be visible to the router for the guard to read it before the handler
// runs, and it has to be the same segment the guard checks membership against.
//
// The scope stops at the tenant rather than at the environment so that
// endpoints above an app — the list of apps itself — are inside the table and
// therefore inside the test that walks it. A route registered next to the
// table instead of in it is the one this arrangement cannot protect.
const tenantScope = "/api/v1/tenants/{tenant}"

// tenantRoutes is every endpoint under tenantScope.
//
// Each handler takes the guard rather than the pieces it is built from, so
// there is no way to construct one that authorizes differently — and no way to
// construct one that does not authorize at all, because the guard is the only
// thing in scope that can read the session.
var tenantRoutes = []struct {
	suffix  string
	handler func(guard, evidence.Store) http.Handler
}{
	{"/apps", apps},
	{"/apps/{app}/envs/{env}/evidence", currentEvidence},
	{"/apps/{app}/envs/{env}/history", history},
	{"/apps/{app}/envs/{env}/verify", verify},
	{"/apps/{app}/envs/{env}/retention", retention},
	{"/apps/{app}/envs/{env}/export", export},
}

// handler builds the routes, then hands the mux to the Routes hook and wraps
// the result in Middleware.
func (o Options) handler(store evidence.Store, idStore identity.Store) (http.Handler, error) {
	sess := &auth.Sessions{
		Store:  idStore,
		TTL:    o.Config.SessionTTL,
		Secure: o.Config.SecureCookies,
	}
	// Concurrency is left to the default, which is derived from the CPUs the
	// process can see. The point of the bound is that there is one; see
	// auth.NewHasher for why the parameter alone does not make the peak
	// survivable.
	hasher := auth.NewHasher(auth.DefaultParams, 0)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("POST /api/v1/login", o.login(idStore, sess, hasher))
	mux.Handle("POST /api/v1/logout", o.logout(sess))
	mux.Handle("GET /api/v1/me", o.me(idStore, sess))

	// Registered from a table rather than one call each, so that "these are
	// the endpoints that expose one tenant's data" is a list something can
	// walk. TestEveryTenantRouteIsGuarded walks it and asserts each one
	// refuses an anonymous caller and a caller from another tenant — which
	// means the fifth endpoint is covered by being added to the table, not by
	// whoever adds it remembering to write the test.
	g := guard{authorizer: o.Authorizer, identity: idStore, sessions: sess}
	for _, rt := range tenantRoutes {
		mux.Handle("GET "+tenantScope+rt.suffix, rt.handler(g, store))
	}

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

	// The CSRF control, and it is this rather than the cookie's SameSite.
	// SameSite is site-scoped: a sibling subdomain is same-site, which is
	// ordinary for an internal platform and imposes no restriction at all.
	// CrossOriginProtection is origin-scoped — it rejects
	// Sec-Fetch-Site: same-site — and it deliberately allows requests with
	// neither Sec-Fetch-Site nor Origin, so the CLI and curl are unaffected.
	//
	// It always allows GET, so any future streaming endpoint (a log tail is a
	// GET) needs an Origin check of its own rather than inheriting this one.
	h := http.NewCrossOriginProtection().Handler(mux)
	if o.Middleware != nil {
		if h = o.Middleware(h); h == nil {
			return nil, errors.New("server: Middleware returned a nil handler")
		}
	}
	return h, nil
}

// openIdentity opens the identity store from the same DSN as the evidence one.
// One database, two migration sequences: see internal/sqlmigrate for why they
// cannot share a version table.
func openIdentity(ctx context.Context, c Config, log *slog.Logger) (identity.Store, error) {
	switch dsn := c.EvidenceDSN; {
	case dsn == "":
		log.Warn("no DSN: accounts and sessions are kept in memory and lost on restart",
			"flag", "-evidence-dsn")
		return identitymem.New(), nil
	case strings.HasPrefix(dsn, "postgres://"),
		strings.HasPrefix(dsn, "postgresql://"),
		strings.HasPrefix(dsn, "pgx://"):
		return identitypg.Open(ctx, dsn)
	default:
		return identitysqlite.Open(ctx, dsn)
	}
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
