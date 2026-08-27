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

// Package server is the composition root, and the only package a second
// repository has to import:
//
//	import core "github.com/damgahq/damga/server"
//
//	core.Run(ctx, core.Options{
//		Authorizer: enterprise.NewAuthorizer(),
//		Evidence:   enterprise.NewArchive(),
//	})
//
// That is the whole shape of the open-core arrangement. The enterprise build is
// a thin main against this package; nothing it substitutes lives under
// internal/, and there is no plugin mechanism, no gRPC and no sidecar, because
// none is needed when the seam is a Go interface.
//
// Every field of Options may be zero. server.Run(ctx, server.Options{}) is a
// complete free installation, which is the property that keeps the free tier
// honest: the paid build replaces implementations, never fills in gaps.
package server

import (
	"flag"
	"io/fs"
	"net/http"
	"time"

	"k8s.io/client-go/rest"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/identity"
	"github.com/damgahq/damga/placement"
)

// Config is everything that arrives from flags, environment or a file. It holds
// no interfaces on purpose: it is the part a second main can bind to its own
// FlagSet without knowing what the core does with any of it.
type Config struct {
	// ListenAddr is where the panel and the API are served.
	ListenAddr string

	// EvidenceDSN is the control-plane database. A "postgres://" or "pgx://"
	// URL selects PostgreSQL; anything else is a SQLite path. Ignored when
	// Options.Evidence is supplied.
	//
	// Empty means an in-process store that is lost on restart. That is a
	// legitimate way to run a demo and a dishonest way to run anything else, so
	// Run says so in the log rather than choosing a file silently.
	EvidenceDSN string

	// RetentionWindow is how long a record that is not current is kept. Zero
	// means unbounded, which is the free tier's default — a sweep is what
	// blanks a page, and the page is the product.
	RetentionWindow time.Duration

	// ShutdownTimeout bounds the wait for in-flight requests.
	//
	// It must stay strictly below the manager's own grace period, and that is
	// measured rather than tidy: controller-runtime's Start returns nil on a
	// cancelled context and then its deferred stop procedure overwrites that
	// with "failed waiting for all runnables to end within grace period". One
	// in-flight request against a server whose own timeout is longer is enough
	// to turn every clean shutdown into an error.
	ShutdownTimeout time.Duration

	// ObserveDeploys watches the cluster and closes the records the git write
	// path opened.
	//
	// Off means the platform records commits and never learns what happened to
	// them. That is a legitimate way to run a control plane that does not live
	// in the target cluster, and an honest one: the records stay pending until
	// the sweep gives up on them, rather than claiming a success nobody saw.
	ObserveDeploys bool

	// LeaderElect runs the observer and the sweep on one replica. The panel and
	// the API answer on every replica regardless.
	//
	// Off by default, and that is not the same as "one replica is the leader":
	// with leader election off, controller-runtime starts the leader-election
	// group on *every* replica. Turning it off with several replicas running is
	// how two observers end up racing.
	LeaderElect bool

	// LeaderElectionNamespace is where the Lease lives. Empty means the
	// namespace the pod is in, which only resolves in-cluster.
	LeaderElectionNamespace string

	// SessionTTL is how long a login lasts. Absolute rather than sliding: a
	// sliding window means a stolen cookie never expires while it is being
	// used. Zero is twelve hours.
	SessionTTL time.Duration

	// GitTokenFile is the path to a file holding the token damga pushes with.
	//
	// A file and not a flag value or an environment variable. A flag is in the
	// process table and in the shell history; an environment variable is in
	// /proc/<pid>/environ, in a crash dump, and in `kubectl describe pod`. A
	// file is what a mounted Secret already is.
	GitTokenFile string

	// SecureCookies sets the session cookie's Secure attribute.
	//
	// Configuration rather than a constant because the documented first run is
	// plain http on localhost, and a Secure cookie is simply not stored there —
	// the login would fail silently, with nothing in the logs. The chart
	// already carries exactly this switch for the reference tenant.
	SecureCookies bool

	// PendingTimeout is how long a record may sit unobserved before the sweep
	// writes unknown. It must exceed the cluster's progress deadline — ten
	// minutes on every Deployment this platform renders — or a rollout is given
	// up on while it is still legitimately rolling.
	PendingTimeout time.Duration
}

// BindFlags registers Config on a FlagSet, so the free main and an enterprise
// one parse the same names.
func (c *Config) BindFlags(f *flag.FlagSet) {
	f.StringVar(&c.ListenAddr, "listen-address", ":8080",
		"address the panel and the API listen on")
	f.StringVar(&c.EvidenceDSN, "evidence-dsn", "",
		"evidence database: a postgres:// URL, a SQLite path, or empty for in-memory")
	f.DurationVar(&c.RetentionWindow, "retention-window", 0,
		"how long non-current evidence records are kept; 0 keeps them for ever")
	f.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 15*time.Second,
		"how long to wait for in-flight requests on shutdown")
	f.DurationVar(&c.SessionTTL, "session-ttl", 12*time.Hour,
		"how long a login lasts")
	f.StringVar(&c.GitTokenFile, "git-token-file", "",
		"file holding the token damga pushes tenant repositories with")
	f.BoolVar(&c.SecureCookies, "secure-cookies", false,
		"set Secure on the session cookie; required behind TLS, breaks plain http")
	f.BoolVar(&c.ObserveDeploys, "observe-deploys", false,
		"watch the cluster and close the evidence records the git write path opened")
	f.BoolVar(&c.LeaderElect, "leader-elect", false,
		"run the observer and the sweep on one replica only")
	f.StringVar(&c.LeaderElectionNamespace, "leader-election-namespace", "",
		"namespace holding the leader-election Lease; required out of cluster")
	f.DurationVar(&c.PendingTimeout, "pending-timeout", 30*time.Minute,
		"how long an unobserved record may stay pending before it is marked unknown")
}

// Options is the substitution surface. Two of its fields are seams and the rest
// are hooks; all of them have working defaults.
type Options struct {
	Config Config

	// Authorizer decides every permission in the product. nil installs the free
	// owner/member/viewer implementation.
	//
	// One field, because there is one interface with one method. Principle 6 is
	// enforced by that shape rather than by review: a second way to ask would
	// have to appear here first.
	Authorizer authz.Authorizer

	// Evidence stores the deploy records. nil opens the free store described by
	// Config.EvidenceDSN.
	//
	// Named Evidence rather than the plan's AuditStore because the record, the
	// page and the decision all say evidence. It is the same slot.
	Evidence evidence.Store

	// Identity holds accounts, memberships and sessions. nil opens the free
	// store against Config.EvidenceDSN — the same database, a separate
	// migration sequence.
	//
	// Not a seam, and the asymmetry with Evidence is deliberate: an enterprise
	// build replaces the audit archive because that is a different product,
	// but single sign-on changes how an account row is created and never where
	// accounts live. Making this replaceable would oblige damga-ee to
	// reimplement teams and memberships for nothing.
	Identity identity.Store

	// Placement is where each app environment is written in git. Replaceable
	// for the same reason Evidence is: a paid build may hold this somewhere
	// else, and nothing above it needs to know.
	Placement placement.Store

	// GitAuth answers how to authenticate to a repository. nil means the
	// free build's answer, built from Config.GitTokenFile — and with no token
	// configured, a deploy is refused with a message that says which flag is
	// missing rather than whatever the forge says about anonymous writes.
	GitAuth GitAuth

	// Panel is the front-end bundle, served at "/". nil mounts nothing, which
	// is what the free build does today because there is no bundle yet.
	//
	// An fs.FS rather than a path, so that an enterprise build can embed its
	// own bundle rather than ship a directory beside the binary. Without this
	// field the paid panel would have to fork this package to be served at all,
	// and the fork would silently stop tracking core routes — which is risk 4
	// in the plan, arriving through the front end instead of the API.
	Panel fs.FS

	// Routes mounts additional endpoints: an SSO callback, a compliance report.
	// Called after the core's own patterns are registered, so a collision
	// panics at startup rather than silently shadowing an endpoint the CLI
	// depends on. That is the correct outcome, and it is why the hook receives
	// the mux instead of a handler to wrap.
	Routes func(mux *http.ServeMux)

	// Middleware wraps the whole handler. This is where a session filter or an
	// SSO redirect goes.
	Middleware func(http.Handler) http.Handler

	// RestConfig is the cluster the observer watches. nil falls back to the
	// ambient kubeconfig, and Run only looks for one when ObserveDeploys is on
	// — a control plane that is not observing must start with no cluster at
	// all, which is also what makes it testable.
	RestConfig *rest.Config

	// Ready is called once the listener is bound, with the address it actually
	// bound to. It exists because ListenAddr may name port 0 — which is how a
	// test gets a port without racing another one for a fixed number, and how a
	// sidecar learns where to send traffic.
	Ready func(addr string)
}
