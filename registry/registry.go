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

// Package registry resolves an image tag to the digest it points at right now.
//
// It exists to fill catalog.Options.Pin, which has been a seam with nothing in
// it. The seam's doc says what goes there is an open decision — a client here, a
// mirror, a lockfile beside the templates — and this is the first of those: a
// client that asks the registry at install time.
//
// # Why the catalogue needs it at all
//
// Upstream compose templates name images by moving tag: postgres:16,
// n8nio/n8n:latest. WorkloadSpec.Image refuses both, and the reason is written
// on the field — a rollback to a tag that moved restores something other than
// what was rolled back from. So the catalogue offers an entry, the user clicks
// it, and catalog.Plan blocks it. Measured against the upstream corpus of 371
// files: 341 are offered and 119 of them install as they stand; of the 222 that
// do not, 162 are blocked by :latest and 37 by an image with no tag at all.
//
// # What this does not do
//
// It authenticates anonymously and nothing else. A registry that needs a real
// credential — a private Docker Hub repository, ghcr.io for a private package,
// anything behind a paid plan — answers 401 to the token request and the image
// is reported unresolvable by name. There is no place to put a credential here
// on purpose: the one that would go in is per-install configuration, and
// inventing the field before somebody needs it would be inventing its rotation
// story too.
//
// It speaks https and only https. The platform's own in-cluster registry is
// served over plain HTTP at registry.damga-registry.svc:5000, so a reference
// into it cannot be resolved here. That has not mattered yet — catalogue images
// are upstream — and it is written down rather than guessed at because the day
// it matters the failure is a timeout, not a refusal.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The media types a manifest request has to declare it accepts.
//
// All four, and the two index types are the ones that matter: an image that is
// published for several architectures answers with an index, and a request that
// does not accept one gets either a 404 or — worse — a digest for whichever
// single architecture the registry decided to show, which is not the digest the
// kubelet will resolve.
var acceptManifest = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

const (
	// defaultTimeout bounds one resolution. An install waits on this, and a
	// registry that has stopped answering must not hold the request open.
	defaultTimeout = 15 * time.Second

	// defaultTTL is how long a resolved digest is reused.
	//
	// A cache is not an optimisation here, it is what makes a corpus-wide run
	// finish: the upstream templates name postgres and redis over and over, and
	// without one a single catalogue page is hundreds of round trips to the
	// same three references.
	//
	// It expires because a tag moves. A digest is immutable, so a stale entry
	// is never a wrong image — it is an older one the tag really did point at —
	// but somebody who has just pushed and is reinstalling should not have to
	// restart the process to see it.
	defaultTTL = 5 * time.Minute
)

// Resolver turns a reference into a digest-pinned reference.
//
// The zero value works and talks to the real internet. A test supplies Client
// with a transport of its own; nothing in this package reaches the network
// except through it, which is what keeps the test suite offline.
type Resolver struct {
	// Client is how requests are made. nil is a client with defaultTimeout.
	Client *http.Client

	// TTL overrides how long a resolved digest is reused. Zero is defaultTTL,
	// negative disables the cache.
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	ref string
	err error
	at  time.Time
}

// Pin resolves one reference, and its signature is exactly catalog.Options.Pin.
//
// A reference that already carries a digest is returned untouched — including
// one that carries both a tag and a digest. There is nothing to look up, and
// asking anyway would turn an offline-resolvable reference into a network call
// that can fail.
func (r *Resolver) Pin(image string) (string, error) {
	return r.PinContext(context.Background(), image)
}

// PinContext is Pin with a deadline the caller owns.
func (r *Resolver) PinContext(ctx context.Context, image string) (string, error) {
	if strings.Contains(image, "@") {
		return image, nil
	}
	if got, ok := r.lookup(image); ok {
		return got.ref, got.err
	}
	ref, err := r.resolve(ctx, image)
	r.store(image, cached{ref: ref, err: err, at: time.Now()})
	return ref, err
}

func (r *Resolver) ttl() time.Duration {
	if r.TTL == 0 {
		return defaultTTL
	}
	return r.TTL
}

func (r *Resolver) lookup(image string) (cached, bool) {
	if r.ttl() < 0 {
		return cached{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	got, ok := r.cache[image]
	if !ok || time.Since(got.at) > r.ttl() {
		return cached{}, false
	}
	return got, true
}

func (r *Resolver) store(image string, got cached) {
	if r.ttl() < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]cached{}
	}
	r.cache[image] = got
}

func (r *Resolver) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: defaultTimeout}
}

// resolve does the two requests a registry may need: the manifest, and — if it
// answers 401 — a token for the scope it named, then the manifest again.
func (r *Resolver) resolve(ctx context.Context, image string) (string, error) {
	ref, err := parse(image)
	if err != nil {
		return "", err
	}

	digest, challenge, err := r.head(ctx, ref, "")
	switch {
	case err != nil:
		return "", err
	case digest != "":
		return image + "@" + digest, nil
	case challenge == "":
		// Refused with nothing to act on. Named as its own case because
		// "unauthorized" and "unauthorized, and here is where to get a token"
		// send a reader to two different places.
		return "", fmt.Errorf("%s refused the request and offered no way to authenticate", ref.host)
	}

	token, err := r.token(ctx, challenge)
	if err != nil {
		return "", err
	}
	digest, _, err = r.head(ctx, ref, token)
	switch {
	case err != nil:
		return "", err
	case digest == "":
		// The anonymous token was issued and still is not enough. This is the
		// private-repository case and it is worth saying so rather than
		// repeating "unauthorized": no credential this platform holds will
		// change the answer.
		return "", fmt.Errorf("%s needs a credential this platform does not have", ref.repo)
	}
	return image + "@" + digest, nil
}

// head asks for the manifest and reads the digest off the response.
//
// HEAD and not GET: the digest is in a header, the body is the manifest, and
// pulling a multi-megabyte index to read a header is a cost paid once per image
// per install.
func (r *Resolver) head(ctx context.Context, ref reference, token string) (digest, challenge string, err error) {
	endpoint := "https://" + ref.host + "/v2/" + ref.repo + "/manifests/" + ref.tag
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", acceptManifest)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.client().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("asking %s: %w", ref.host, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", resp.Header.Get("WWW-Authenticate"), nil
	case resp.StatusCode == http.StatusNotFound:
		// Distinct from every other refusal: the tag is gone, which for an
		// upstream template usually means the template outlived the image.
		return "", "", fmt.Errorf("%s:%s does not exist in %s", ref.repo, ref.tag, ref.host)
	case resp.StatusCode == http.StatusTooManyRequests:
		// Named, because it is the one failure that is neither the image's
		// fault nor the platform's and that goes away by waiting.
		return "", "", fmt.Errorf("%s is rate limiting this address", ref.host)
	case resp.StatusCode != http.StatusOK:
		return "", "", fmt.Errorf("%s answered %s", ref.host, resp.Status)
	}

	if got := resp.Header.Get("Docker-Content-Digest"); got != "" {
		return got, "", nil
	}
	// A registry that answers 200 and no digest header. Every conformant one
	// sends it; saying which header is missing is what tells the reader this is
	// the registry's doing and not a parse failure here.
	return "", "", fmt.Errorf("%s answered without a Docker-Content-Digest header", ref.host)
}

// token performs the anonymous half of the bearer flow the registry described.
func (r *Resolver) token(ctx context.Context, challenge string) (string, error) {
	realm, params := parseChallenge(challenge)
	if realm == "" {
		return "", fmt.Errorf("the registry asked for a bearer token without saying where to get one")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("the registry named an unusable token endpoint %q", realm)
	}
	q := u.Query()
	for _, k := range []string{"service", "scope"} {
		if v := params[k]; v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("asking %s for a token: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s refused an anonymous token: %s", u.Host, resp.Status)
	}

	// Two spellings of one field. Docker Hub sends "token"; the OCI
	// distribution spec calls it "access_token" and several registries send
	// only that, so reading one of them works everywhere until it does not.
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("%s returned a token this client cannot read: %w", u.Host, err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("%s returned an empty token", u.Host)
}

// parseChallenge pulls the realm and its parameters out of a WWW-Authenticate
// header. Only the Bearer scheme; Basic is what a credential would be for and
// there is none to send.
func parseChallenge(header string) (realm string, params map[string]string) {
	rest, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", nil
	}
	params = map[string]string{}
	for _, part := range splitParams(rest) {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		params[k] = strings.Trim(v, `"`)
	}
	return params["realm"], params
}

// splitParams splits on commas that are not inside quotes. A scope carries
// commas — `scope="repository:library/postgres:pull,push"` — and splitting on
// every one of them loses half of it.
func splitParams(s string) []string {
	var (
		out    []string
		start  int
		quoted bool
	)
	for i, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// reference is an image split into the three things a manifest request needs.
type reference struct {
	host string
	repo string
	tag  string
}

const (
	// dockerHubAPI is where docker.io actually answers. The name in a reference
	// is not the name of the API host, and using docker.io directly redirects
	// at best.
	dockerHubAPI = "registry-1.docker.io"
	// officialPrefix is what a one-segment name on Docker Hub expands to:
	// postgres is library/postgres, and a request for /v2/postgres/manifests
	// is a 401 that looks like a permission problem.
	officialPrefix = "library/"
)

// parse splits a reference the way a container runtime does.
func parse(image string) (reference, error) {
	if image == "" {
		return reference{}, fmt.Errorf("an empty image reference")
	}
	name := image
	host := ""

	// The first segment is a registry only if it looks like a host: it carries
	// a dot or a port, or it is localhost. Two things ride on this one test.
	// Without it `damgahq/damga` parses as the host `damgahq` and the lookup
	// goes to a machine that does not exist — and, the other way, splitting the
	// host here is also what keeps a registry port from being read as a tag
	// later: by the time the tag is looked for, the `:5000` in
	// `registry.local:5000/team-a/app` is gone. That colon is the trap this
	// repository has already been caught by twice, in WorkloadSpec.Image and
	// again in BuildSpec.Image, in opposite directions.
	if before, after, found := strings.Cut(image, "/"); found {
		if strings.ContainsAny(before, ".:") || before == "localhost" {
			host, name = before, after
		}
	}
	switch host {
	case "", "docker.io", "index.docker.io":
		host = dockerHubAPI
		if !strings.Contains(name, "/") {
			name = officialPrefix + name
		}
	}

	tag := "latest"
	// Only the tag can be left: the host, and any port with it, came off above.
	//
	// This read `i > strings.LastIndex(name, "/")` first, to guard the port —
	// and that guard is unreachable, because nothing with a port in it survives
	// the split above. Removing it changed no test, which is how it was found.
	// A condition no input can fail is not a safeguard, it is a claim that the
	// next reader has to check for themselves.
	if i := strings.LastIndex(name, ":"); i >= 0 {
		tag, name = name[i+1:], name[:i]
	}
	if name == "" || tag == "" {
		return reference{}, fmt.Errorf("%q is not a usable image reference", image)
	}
	return reference{host: host, repo: name, tag: tag}, nil
}
