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

package registry_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damgahq/damga/registry"
)

// The path a one-segment Docker Hub name expands to, asserted from more than
// one case because the expansion is the part that is easy to lose.
const libraryPostgres = "/v2/library/postgres/manifests/16"

const testDigest = "sha256:" + "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

// toServer sends every https request this package makes to one test server.
//
// The resolver builds https URLs because that is what a registry speaks, and a
// test server is plain http on a port the kernel picked. Rewriting in a
// transport is what keeps the production code free of a scheme switch that
// exists only for tests — and it is what makes "this suite never reaches the
// network" a property of the fixture rather than a hope.
type toServer struct {
	base *url.URL
	seen *atomic.Int64
}

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.seen != nil {
		t.seen.Add(1)
	}
	out := r.Clone(r.Context())
	out.URL.Scheme = t.base.Scheme
	out.URL.Host = t.base.Host
	return http.DefaultTransport.RoundTrip(out)
}

// fakeRegistry answers the two requests the flow makes. want401 turns on the
// bearer challenge, so one server covers both an open registry and one that
// makes a client fetch a token first.
type fakeRegistry struct {
	want401  bool
	manifest atomic.Int64
	tokens   atomic.Int64
	lastPath string
	lastAuth string
	scope    string
}

func (f *fakeRegistry) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokens.Add(1)
		f.scope = r.URL.Query().Get("scope")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"a-token"}`))
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		f.manifest.Add(1)
		f.lastPath = r.URL.Path
		f.lastAuth = r.Header.Get("Authorization")
		if f.want401 && f.lastAuth == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="https://auth.example.test/token",service="registry.example.test",`+
					`scope="repository:library/postgres:pull,push"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "manifest.list") {
			// A request that does not accept an index gets whichever single
			// architecture the registry chose, which is not what the kubelet
			// resolves. Refused here so a client that stops asking is caught.
			http.Error(w, "no index accepted", http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Docker-Content-Digest", testDigest)
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func newFake(t *testing.T, f *fakeRegistry) (*registry.Resolver, *atomic.Int64) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var calls atomic.Int64
	return &registry.Resolver{
		Client: &http.Client{Transport: toServer{base: base, seen: &calls}},
	}, &calls
}

// The ordinary case: a tag becomes the digest the tag points at, with the tag
// kept beside it.
//
// Kept rather than replaced because a reader of the committed manifest should
// still be able to see which tag the platform resolved. Kubernetes pulls by the
// digest and ignores the tag, and WorkloadSpec.Image accepts the pair — it
// checks for an '@' before anything else.
func TestATagBecomesADigestAndKeepsTheTag(t *testing.T) {
	f := &fakeRegistry{}
	r, _ := newFake(t, f)

	got, err := r.Pin("postgres:16")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if want := "postgres:16@" + testDigest; got != want {
		t.Errorf("Pin = %q, want %q", got, want)
	}
	// A one-segment Docker Hub name is library/, and asking without it is a
	// 401 that reads like a permission problem.
	if f.lastPath != libraryPostgres {
		t.Errorf("asked for %q, want %q", f.lastPath, libraryPostgres)
	}
}

// A reference that already names a digest is returned untouched and costs no
// request. Asking anyway would turn something resolvable offline into a network
// call that can fail.
func TestADigestIsLeftAloneAndCostsNoRequest(t *testing.T) {
	f := &fakeRegistry{}
	r, calls := newFake(t, f)

	for _, image := range []string{
		"postgres@" + testDigest,
		"postgres:16@" + testDigest,
		"ghcr.io/damgahq/damga@" + testDigest,
	} {
		got, err := r.Pin(image)
		if err != nil {
			t.Fatalf("Pin(%s): %v", image, err)
		}
		if got != image {
			t.Errorf("Pin(%s) = %q, want it untouched", image, got)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("a reference that already carries a digest made %d requests, want 0", n)
	}
}

// The anonymous bearer flow: 401 with a challenge, a token from the realm it
// named, then the manifest again with the token.
func TestAChallengeIsAnsweredWithAnAnonymousToken(t *testing.T) {
	f := &fakeRegistry{want401: true}
	r, _ := newFake(t, f)

	got, err := r.Pin("postgres:16")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if want := "postgres:16@" + testDigest; got != want {
		t.Errorf("Pin = %q, want %q", got, want)
	}
	if n := f.tokens.Load(); n != 1 {
		t.Errorf("asked for %d tokens, want 1", n)
	}
	if f.lastAuth != "Bearer a-token" {
		t.Errorf("the retry sent %q, want the token", f.lastAuth)
	}
	// A scope carries commas. Splitting the challenge on every comma keeps
	// "repository:library/postgres:pull" and loses ",push", and the registry
	// then issues a token for a scope the client did not mean to ask for.
	if want := "repository:library/postgres:pull,push"; f.scope != want {
		t.Errorf("scope = %q, want %q", f.scope, want)
	}
}

// Every failure says which one it is. A message that is true of all of them
// sends a reader to the wrong place, and this repository has lost rounds to
// exactly that.
func TestEachRefusalNamesItsOwnCause(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   func(w http.ResponseWriter)
		want   string
	}{
		{"a tag that no longer exists", http.StatusNotFound, nil, "does not exist"},
		{"rate limiting", http.StatusTooManyRequests, nil, "rate limiting"},
		{"anything else", http.StatusInternalServerError, nil, "answered 500"},
		{"a 200 with no digest header", http.StatusOK, nil, "without a Docker-Content-Digest"},
		{"a refusal with no way to authenticate", http.StatusUnauthorized, nil, "offered no way to authenticate"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
			}))
			t.Cleanup(srv.Close)
			base, _ := url.Parse(srv.URL)
			r := &registry.Resolver{Client: &http.Client{Transport: toServer{base: base}}}

			_, err := r.Pin("postgres:16")
			if err == nil {
				t.Fatal("resolved anyway")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name %q", err, c.want)
			}
		})
	}
}

// A repository the anonymous token does not open is a credential problem, and
// it says so instead of repeating "unauthorized" — no credential this platform
// holds will change the answer.
func TestAPrivateRepositorySaysACredentialIsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"anonymous"}`))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example.test/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	r := &registry.Resolver{Client: &http.Client{Transport: toServer{base: base}}}

	_, err := r.Pin("ghcr.io/acme/private:1")
	if err == nil {
		t.Fatal("resolved a repository the token did not open")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("error = %q, want it to name the missing credential", err)
	}
}

// The same reference asked for twice costs one round trip. Without this a
// catalogue page of templates that all name postgres is hundreds of requests
// for one answer.
func TestTheSecondAskIsCached(t *testing.T) {
	f := &fakeRegistry{}
	r, calls := newFake(t, f)

	for range 5 {
		if _, err := r.Pin("postgres:16"); err != nil {
			t.Fatalf("Pin: %v", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("five asks made %d requests, want 1", n)
	}

	// And a negative TTL is no cache at all, which is what a caller that must
	// see a moved tag asks for.
	fresh := &registry.Resolver{Client: r.Client, TTL: -1}
	for range 3 {
		_, _ = fresh.Pin("postgres:16")
	}
	if n := calls.Load(); n != 4 {
		t.Errorf("three uncached asks brought the total to %d, want 4", n)
	}
}

// A stale entry is dropped rather than returned for ever: a digest is immutable
// so a stale one is never a wrong image, but somebody who has just pushed
// should not have to restart the process to see it.
func TestACachedDigestExpires(t *testing.T) {
	f := &fakeRegistry{}
	r, calls := newFake(t, f)
	r.TTL = time.Nanosecond

	for range 3 {
		if _, err := r.Pin("postgres:16"); err != nil {
			t.Fatalf("Pin: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("three asks past the TTL made %d requests, want 3", n)
	}
}

// Where a reference points, which is the half that never touches the network.
//
// The port case is the one this repository has already been caught by twice —
// in WorkloadSpec.Image and again in BuildSpec.Image, in opposite directions —
// so it is asserted rather than assumed.
func TestAReferenceIsSplitTheWayARuntimeSplitsIt(t *testing.T) {
	for _, c := range []struct{ image, path string }{
		{"postgres:16", libraryPostgres},
		{"postgres", "/v2/library/postgres/manifests/latest"},
		{"n8nio/n8n:latest", "/v2/n8nio/n8n/manifests/latest"},
		{"docker.io/library/postgres:16", libraryPostgres},
		{"ghcr.io/damgahq/damga:1.0.0", "/v2/damgahq/damga/manifests/1.0.0"},
		{"quay.io/prometheus/node-exporter:v1", "/v2/prometheus/node-exporter/manifests/v1"},
		// The colon is a port and there is no tag at all.
		{"registry.local:5000/team-a/app", "/v2/team-a/app/manifests/latest"},
		{"registry.local:5000/team-a/app:2", "/v2/team-a/app/manifests/2"},
		// A host with a port and no dot in it, which is what a registry on a
		// LAN looks like. It is here because it is the only input that tells a
		// port apart from a name: drop the colon from the host test and
		// `registry.local:5000` is still a host because of its dot, while this
		// one silently becomes a Docker Hub repository called "build:5000".
		{"build:5000/team/app:2", "/v2/team/app/manifests/2"},
		{"localhost:5000/app:2", "/v2/app/manifests/2"},
		// A one-segment name that is not a host stays on Docker Hub.
		{"damgahq/damga:1", "/v2/damgahq/damga/manifests/1"},
	} {
		t.Run(c.image, func(t *testing.T) {
			f := &fakeRegistry{}
			r, _ := newFake(t, f)
			if _, err := r.Pin(c.image); err != nil {
				t.Fatalf("Pin: %v", err)
			}
			if f.lastPath != c.path {
				t.Errorf("asked for %q, want %q", f.lastPath, c.path)
			}
		})
	}
}

func TestAnUnusableReferenceIsRefusedBeforeAnyRequest(t *testing.T) {
	f := &fakeRegistry{}
	r, calls := newFake(t, f)
	for _, image := range []string{"", ":", "postgres:"} {
		if _, err := r.Pin(image); err == nil {
			t.Errorf("Pin(%q) was accepted", image)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("an unusable reference made %d requests, want 0", n)
	}
}
