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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The scopes, spelled the way server/run.go spells them.
//
// Written out here rather than imported, because importing them would not make
// the two agree — the server's are unexported, and exporting them to satisfy a
// client would turn a routing detail into API surface. What keeps these honest
// is not the constant, it is TestEveryRouteTheCLICallsExistsOnTheServer, which
// starts the real control plane and asks it every path in the table below. A
// path this file gets wrong is a 404 in that test, with the path in the message.
const (
	apiRoot     = "/api/v1"
	tenantScope = apiRoot + "/tenants/{tenant}"
	envScope    = tenantScope + "/apps/{app}/envs/{env}"
)

// route is one endpoint, as a method and a pattern with path variables.
type route struct {
	method  string
	pattern string
}

func (r route) String() string { return r.method + " " + r.pattern }

// Every endpoint this CLI is allowed to call. One var each rather than string
// literals at the call sites, so that the table below is provably the whole set
// — a command cannot reach a path that is not one of these without adding one
// here, and do() refuses a route the table does not carry.
var (
	routeLogin  = route{http.MethodPost, apiRoot + "/login"}
	routeLogout = route{http.MethodPost, apiRoot + "/logout"}
	routeMe     = route{http.MethodGet, apiRoot + "/me"}

	routeApps      = route{http.MethodGet, tenantScope + "/apps"}
	routeCreateApp = route{http.MethodPost, tenantScope + "/apps"}
	routeDeleteApp = route{http.MethodDelete, tenantScope + "/apps/{app}"}
	routeBuild     = route{http.MethodPost, tenantScope + "/apps/{app}/builds"}

	routeEvidence  = route{http.MethodGet, envScope + "/evidence"}
	routeHistory   = route{http.MethodGet, envScope + "/history"}
	routeVerify    = route{http.MethodGet, envScope + "/verify"}
	routeRetention = route{http.MethodGet, envScope + "/retention"}
	routeBackup    = route{http.MethodGet, envScope + "/backup"}
	routeExport    = route{http.MethodGet, envScope + "/export"}
	routeDeploy    = route{http.MethodPost, envScope + "/deploys"}
)

// routes is the whole set, and it is a table for the same reason
// server.tenantRoutes is one: so that something can walk it.
//
// Two things walk it. do() refuses any route that is not in here, so a
// convenience endpoint added for the CLI alone fails at the first call rather
// than shipping. And the test starts a real control plane and issues every row,
// so a route this CLI believes in and the server does not have is a failure
// with the path in its message rather than a 404 a user finds.
var routes = []route{
	routeLogin, routeLogout, routeMe,
	routeApps, routeCreateApp, routeDeleteApp, routeBuild,
	routeEvidence, routeHistory, routeVerify, routeRetention,
	routeBackup, routeExport, routeDeploy,
}

// known is the set form of routes, built once.
var known = func() map[route]bool {
	m := make(map[route]bool, len(routes))
	for _, r := range routes {
		m[r] = true
	}
	return m
}()

// target is what fills the path variables.
type target struct{ tenant, app, env string }

// fill substitutes the variables and escapes each one.
//
// Escaped because a tenant id, an app name or an environment ends up inside a
// URL path, and the one that is not a DNS label is the tenant id — the server
// mints it with an underscore prefix and it is never checked against a name
// rule. A missing value is refused here rather than sent, because a path with
// an empty segment collapses into a different route: .../apps//builds is not
// the builds endpoint, it is a 404 that reads as a server problem.
func (t target) fill(pattern string) (string, error) {
	for _, v := range []struct{ name, value, what string }{
		{"{tenant}", t.tenant, "tenant"},
		{"{app}", t.app, "app name"},
		{"{env}", t.env, "environment"},
	} {
		if !strings.Contains(pattern, v.name) {
			continue
		}
		if v.value == "" {
			return "", fmt.Errorf("%w: this command needs the %s", errUsage, v.what)
		}
		pattern = strings.ReplaceAll(pattern, v.name, url.PathEscape(v.value))
	}
	return pattern, nil
}

// Sentinel failures, so main can turn them into exit codes without reading
// messages that belong to the server.
var (
	errUsage       = errors.New("bad usage")
	errNotSignedIn = errors.New("not signed in: run `damga-cli login` first")
	errChainBroken = errors.New("the evidence chain does not verify")
)

// apiError is what the control plane said, kept whole.
//
// The detail is the server's own words and is never reworded here. It is
// deliberately the same sentence for a tenant you may not read and a tenant
// that does not exist, and for a wrong password and an address that has no
// account — improving the wording locally is how a client undoes that.
type apiError struct {
	status int
	detail string
	route  route
}

func (e *apiError) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("%s: HTTP %d", e.route, e.status)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.detail, e.status)
}

// client is one control plane and one session.
type client struct {
	base   *url.URL
	http   *http.Client
	cookie *http.Cookie
}

// call is one request. A struct rather than eight parameters, because the
// alternative grew a new argument every time an endpoint took a query string.
type call struct {
	route  route
	target target
	query  url.Values
	// body is marshalled as JSON when it is not nil.
	body any
	// out receives the decoded response. Ignored when stream is set.
	out any
	// cookies receives the response's Set-Cookie headers.
	//
	// login is the only caller, because login is the only place a session
	// comes from. The name is read back rather than compiled in, so this
	// client cannot hold a second spelling of a constant the server owns.
	cookies *[]*http.Cookie

	// stream takes the raw response body instead of decoding it.
	//
	// This is how --json and `export` both work, and for export it is not a
	// convenience: the export is the store's own encoding because that is the
	// form the hash chain was computed over, so a CLI that decoded and
	// re-encoded it would hand back a file that no longer verifies.
	stream io.Writer
}

// do issues one request and returns what the server said.
func (c *client) do(ctx context.Context, cl call) error {
	if !known[cl.route] {
		// The structural half of "the CLI calls the same API the panel calls".
		// A route reaches this map by being added to the table above, and the
		// table is what the test walks against a real control plane — so an
		// endpoint invented for the CLI alone cannot quietly work.
		return fmt.Errorf("damga-cli: %s is not an endpoint this CLI may call", cl.route)
	}
	path, err := cl.target.fill(cl.route.pattern)
	if err != nil {
		return err
	}

	var payload io.Reader
	if cl.body != nil {
		encoded, mErr := json.Marshal(cl.body)
		if mErr != nil {
			return fmt.Errorf("encoding the request: %w", mErr)
		}
		payload = bytes.NewReader(encoded)
	}

	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	if len(cl.query) > 0 {
		u.RawQuery = cl.query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, cl.route.method, u.String(), payload)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if cl.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		// No Origin and no Sec-Fetch-Site header, which is what makes this
		// work at all: the server's CSRF control is origin-scoped and
		// deliberately allows a request carrying neither, so a terminal client
		// needs no token of its own. Adding an Origin here would start
		// failing every write.
		req.AddCookie(c.cookie)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", cl.route, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return errorFrom(resp, cl.route)
	}
	if cl.cookies != nil {
		*cl.cookies = resp.Cookies()
	}
	switch {
	case cl.stream != nil:
		if _, err := io.Copy(cl.stream, resp.Body); err != nil {
			// Half a file, and said so. A truncated export is detectable —
			// its last record no longer chains — but only if the person who
			// ran the command knows to look.
			return fmt.Errorf("%s: the response was cut short: %w", cl.route, err)
		}
	case cl.out != nil:
		if err := json.NewDecoder(resp.Body).Decode(cl.out); err != nil {
			return fmt.Errorf("%s: the response is not the expected JSON: %w", cl.route, err)
		}
	default:
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return nil
}

// errorFrom reads the server's problem document.
//
// Bounded, because this runs on a response that already said it failed and an
// unbounded read of an error body is a way for a server — or whatever is
// answering in its place — to spend the client's memory.
func errorFrom(resp *http.Response, rt route) error {
	var problem struct {
		Detail string `json:"detail"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(body, &problem)
	if problem.Detail == "" && len(bytes.TrimSpace(body)) > 0 &&
		!strings.Contains(resp.Header.Get("Content-Type"), "json") {
		// Something that is not this API answered — a proxy, a login page, an
		// ingress with no backend. Saying "HTTP 502" and nothing else sends
		// the reader to the control plane's logs, which are not where the
		// fault is.
		problem.Detail = "the server did not answer with JSON; is " +
			resp.Request.URL.Host + " the control plane?"
	}
	return &apiError{status: resp.StatusCode, detail: problem.Detail, route: rt}
}

// newClient builds a client for one server and one session.
func newClient(server string, sess session, timeout time.Duration) (*client, error) {
	base, err := parseServer(server)
	if err != nil {
		return nil, err
	}
	c := &client{
		base: base,
		http: &http.Client{
			Timeout: timeout,
			// Redirects are not followed. Every endpoint here answers
			// directly, so a redirect means something else is in the path —
			// and following one would replay the session cookie against
			// whatever host it named.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	if sess.Cookie != "" {
		c.cookie = &http.Cookie{Name: sess.CookieName, Value: sess.Cookie}
	}
	return c, nil
}

// parseServer accepts what a person types.
func parseServer(server string) (*url.URL, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return nil, fmt.Errorf("%w: no server: pass --server or run `damga-cli login --server ...`", errUsage)
	}
	if !strings.Contains(server, "://") {
		// A bare host, which is what people type. http rather than https,
		// because the documented first run is http on localhost and guessing
		// https there fails with a TLS error that names nothing useful.
		server = "http://" + server
	}
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a URL: %v", errUsage, server, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: %q has no host", errUsage, server)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.TrimSuffix(u.Path, "/")}, nil
}
