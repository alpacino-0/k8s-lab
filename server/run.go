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
	"github.com/damgahq/damga/forge"
	forgemem "github.com/damgahq/damga/forge/memory"
	forgepg "github.com/damgahq/damga/forge/postgres"
	forgesqlite "github.com/damgahq/damga/forge/sqlite"
	"github.com/damgahq/damga/identity"
	identitymem "github.com/damgahq/damga/identity/memory"
	identitypg "github.com/damgahq/damga/identity/postgres"
	identitysqlite "github.com/damgahq/damga/identity/sqlite"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/placement"
	placementmem "github.com/damgahq/damga/placement/memory"
	placementpg "github.com/damgahq/damga/placement/postgres"
	placementsqlite "github.com/damgahq/damga/placement/sqlite"
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

	places := o.Placement
	if places == nil {
		var err error
		places, err = openPlacement(ctx, o.Config)
		if err != nil {
			return err
		}
		defer func() {
			if err := places.Close(); err != nil {
				log.Error("closing the placement store", "error", err)
			}
		}()
	}
	o.Placement = places

	conns := o.Forge
	if conns == nil {
		var err error
		conns, err = openForge(ctx, o.Config)
		if err != nil {
			return err
		}
		defer func() {
			if err := conns.Close(); err != nil {
				log.Error("closing the forge store", "error", err)
			}
		}()
	}
	o.Forge = conns

	if o.GitAuth == nil {
		gitCreds, err := readGitAuth(o.Config.GitTokenFile)
		if err != nil {
			// At startup rather than at the first deploy. A token file that
			// is missing or empty is a misconfiguration, and finding out when
			// somebody presses deploy means finding out from them.
			return err
		}
		o.GitAuth = gitCreds
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

	// Before the handler, because the handler captures it — and not from the
	// manager's client, which reads a cache that is not populated until the
	// manager starts. Using that one here would not be merely awkward; it would
	// be reading an empty cache and reporting every app as having no database.
	//
	// Uncached, so every page load is two Gets against the API server. That is
	// the right trade for a panel: a cache would need starting, watching and
	// invalidating for two objects somebody looks at by hand.
	if o.Config.ObserveDeploys && o.Backups == nil {
		reader, err := o.clusterReader()
		if err != nil {
			return err
		}
		o.Backups = NewClusterBackups(reader)
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

// stores is everything a tenant-scoped handler is allowed to reach.
//
// One struct rather than a parameter each, so the route table stays one table
// as endpoints grow. What is deliberately NOT in here is anything that could
// answer "who is asking": that comes from the guard and from nowhere else.
type stores struct {
	evidence  evidence.Store
	placement placement.Store
	forge     forge.Store
	proposer  forge.Proposer
	backups   BackupReader
	writer    *gitwrite.Writer
	gitAuth   GitAuth
}

// tenantRoutes is every endpoint under tenantScope.
//
// Each handler takes the guard rather than the pieces it is built from, so
// there is no way to construct one that authorizes differently — and no way to
// construct one that does not authorize at all, because the guard is the only
// thing in scope that can read the session.
var tenantRoutes = []struct {
	method  string
	suffix  string
	handler func(guard, stores) http.Handler
}{
	{http.MethodGet, "/apps", apps},
	// Per app and not per environment: an app has one source repository and one
	// signing identity, and deploys to several environments out of it.
	{http.MethodPut, "/apps/{app}/connection", connectRoute},
	{http.MethodGet, "/apps/{app}/connection", connectionRoute},
	// Opening the pull request is its own call and not part of connecting.
	// Storing the connection is local and must not fail because a forge is
	// down, and this one is safe to repeat: it finds the pull request a
	// previous attempt opened rather than opening a second.
	{http.MethodPost, "/apps/{app}/connection/proposal", proposeRoute},
	{http.MethodGet, "/apps/{app}/envs/{env}/evidence", currentEvidence},
	{http.MethodGet, "/apps/{app}/envs/{env}/history", history},
	{http.MethodGet, "/apps/{app}/envs/{env}/verify", verify},
	{http.MethodGet, "/apps/{app}/envs/{env}/retention", retention},
	// Its own route and not part of the record: a record is one deploy, and
	// a backup is a number that changes nightly inside an object whose
	// whole value is that it does not change.
	{http.MethodGet, "/apps/{app}/envs/{env}/backup", backupRoute},
	{http.MethodGet, "/apps/{app}/envs/{env}/export", export},
	// The only one that writes, and it writes to git and to nothing else.
	{http.MethodPost, "/apps/{app}/envs/{env}/deploys", deployRoute},
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
	st := stores{
		evidence: store, placement: o.Placement,
		forge:    o.Forge,
		proposer: o.Proposer,
		backups:  o.Backups,
		writer:   &gitwrite.Writer{Evidence: store}, gitAuth: o.GitAuth,
	}
	for _, rt := range tenantRoutes {
		mux.Handle(rt.method+" "+tenantScope+rt.suffix, rt.handler(g, st))
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

// openPlacement picks an engine from the DSN, the same way the other two do.
//
// It shares the evidence DSN because it is the same database: one install, one
// place its state lives. Splitting them would be one more thing to configure
// and one more thing to get half-migrated.
func openPlacement(ctx context.Context, c Config) (placement.Store, error) {
	switch dsn := c.EvidenceDSN; {
	case dsn == "":
		return placementmem.New(), nil
	case strings.HasPrefix(dsn, "postgres://"),
		strings.HasPrefix(dsn, "postgresql://"),
		strings.HasPrefix(dsn, "pgx://"):
		return placementpg.Open(ctx, dsn)
	default:
		return placementsqlite.Open(ctx, dsn)
	}
}

// openForge picks an engine from the DSN, the same way the other three do, and
// shares the same database for the same reason: one install, one place its
// state lives.
func openForge(ctx context.Context, c Config) (forge.Store, error) {
	switch dsn := c.EvidenceDSN; {
	case dsn == "":
		return forgemem.New(), nil
	case strings.HasPrefix(dsn, "postgres://"),
		strings.HasPrefix(dsn, "postgresql://"),
		strings.HasPrefix(dsn, "pgx://"):
		return forgepg.Open(ctx, dsn)
	default:
		return forgesqlite.Open(ctx, dsn)
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
