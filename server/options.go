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

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
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
	ShutdownTimeout time.Duration
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

	// Ready is called once the listener is bound, with the address it actually
	// bound to. It exists because ListenAddr may name port 0 — which is how a
	// test gets a port without racing another one for a fixed number, and how a
	// sidecar learns where to send traffic.
	Ready func(addr string)
}
