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

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// The two failures a caller has to be able to tell apart, and the reason this
// file has sentinels where the rest of the package has sentences.
//
// "This image has no arm64 build" and "nobody would tell us" are different
// facts about different things: the first is about the image, the second is
// about the registry, the network, or a rate limit that goes away by waiting.
// Counted together they produce a number that looks like a measurement of the
// catalogue and is partly a measurement of the afternoon.
//
// A third case is not here because it is not this package's to name: a
// reference that was never a reference — an unexpanded compose variable, say.
// parse takes it, the registry answers 404, and it would arrive as
// ErrUnavailable. Whoever holds the list has to sort that out before asking,
// and the corpus measurement does.
var (
	// ErrReference is a reference this package cannot address at all.
	ErrReference = errors.New("registry: the reference cannot be read")

	// ErrUnavailable is everything that stopped the question being answered:
	// a refusal, a rate limit, a repository that needs a credential, a tag
	// that no longer exists, a network that did not reply.
	ErrUnavailable = errors.New("registry: the registry did not answer")
)

// Platform is one operating system and architecture an image is published for.
type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

// String is the spelling every container tool uses: linux/arm64, linux/arm/v7.
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Matches reports whether this platform would run where want does.
//
// The variant is compared only when the caller asked for one. An index that
// offers linux/arm64/v8 satisfies a node reporting linux/arm64, and a node is
// what kubernetes.io/arch reports — so requiring the variants to be equal
// would answer "no arm64 build" about images that run perfectly well.
func (p Platform) Matches(want Platform) bool {
	if p.OS != want.OS || p.Architecture != want.Architecture {
		return false
	}
	return want.Variant == "" || p.Variant == want.Variant
}

// manifest is the part of a manifest, an index or an image config that says
// what a thing runs on. One struct for three documents, because the three
// fields wanted from them do not overlap and a response is only ever one.
type manifest struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		Platform Platform `json:"platform"`
		// Digest is what Ports follows to reach one image's config. Platforms
		// does not need it: an index states every platform in the index itself.
		Digest string `json:"digest"`
	} `json:"manifests"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`

	// Present on an image config blob rather than on a manifest.
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}

// unknownPlatform is what buildx writes on an attestation, which rides in the
// same index as the images and is not one.
const unknownPlatform = "unknown"

// Platforms reports every platform a reference is published for.
//
// An index answers directly. A single manifest does not: what it carries is a
// pointer to a config blob, and the architecture is in the blob — so a
// single-architecture image costs a second request. That is worth paying rather
// than guessing, because guessing has exactly one plausible shape ("no index
// means amd64") and it is wrong for every arm64-only image, which is the answer
// this exists to find.
//
// The result is never empty without an error. An index with nothing in it that
// runs anywhere is reported as ErrUnavailable rather than as "no platforms",
// because it means the document was not what it claimed to be.
func (r *Resolver) Platforms(ctx context.Context, image string) ([]Platform, error) {
	// The digest comes off first, and it has to: parse looks for the tag after
	// the last colon, and in name@sha256:abc… that colon is inside the digest.
	// The reference then addresses the repository "name@sha256" and the tag
	// "abc…", and the registry answers 404 — which arrives as ErrUnavailable
	// and lands a perfectly good image in the same count as a rate limit.
	//
	// Found by running this against the corpus rather than by reading: one
	// upstream template pins by digest, and the failure it produced named a URL
	// with @sha256 in the path. Pin() never meets this because it returns a
	// digest-bearing reference untouched — there is nothing to look up — but
	// this asks a question that still has an answer.
	name, digest, pinned := strings.Cut(image, "@")
	if pinned && !strings.Contains(digest, ":") {
		return nil, fmt.Errorf("%w: %s: %q is not a digest", ErrReference, image, digest)
	}
	ref, err := parse(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrReference, image, err)
	}
	target := ref.tag
	if pinned {
		target = digest
	}

	body, err := r.manifestBody(ctx, ref, target)
	if err != nil {
		return nil, err
	}
	var doc manifest
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s sent a manifest that is not JSON: %w",
			ErrUnavailable, ref.host, err)
	}

	if len(doc.Manifests) > 0 {
		out := make([]Platform, 0, len(doc.Manifests))
		for _, m := range doc.Manifests {
			// Attestations ride in the same index as the images and are not
			// images: buildx writes them with platform unknown/unknown. Kept
			// out rather than reported, because an index whose only "extra"
			// platform is unknown/unknown is a single-architecture image with
			// provenance attached, and listing it invites the reader to think
			// something runs there.
			if m.Platform.Architecture == unknownPlatform || m.Platform.OS == unknownPlatform {
				continue
			}
			if m.Platform.OS == "" && m.Platform.Architecture == "" {
				continue
			}
			out = append(out, m.Platform)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: %s offered an index naming no platform",
				ErrUnavailable, image)
		}
		return out, nil
	}

	// A single manifest. The architecture is one blob further down.
	if doc.Config.Digest == "" {
		return nil, fmt.Errorf("%w: %s is neither an index nor an image manifest",
			ErrUnavailable, image)
	}
	cfg, err := r.blob(ctx, ref, doc.Config.Digest)
	if err != nil {
		return nil, err
	}
	var conf manifest
	if err := json.Unmarshal(cfg, &conf); err != nil {
		return nil, fmt.Errorf("%w: %s sent a config that is not JSON: %w",
			ErrUnavailable, ref.host, err)
	}
	if conf.OS == "" || conf.Architecture == "" {
		return nil, fmt.Errorf("%w: the config for %s names no platform", ErrUnavailable, image)
	}
	return []Platform{{OS: conf.OS, Architecture: conf.Architecture, Variant: conf.Variant}}, nil
}

// manifestBody GETs a manifest, taking an anonymous token if one is offered.
//
// GET and not HEAD, which is the difference from head(): that one wants the
// digest and the digest is in a header, this one wants the document.
func (r *Resolver) manifestBody(ctx context.Context, ref reference, tag string) ([]byte, error) {
	return r.fetch(ctx, ref, "https://"+ref.host+"/v2/"+ref.repo+"/manifests/"+tag, acceptManifest)
}

// blob GETs one blob by digest, which is where an image config lives.
func (r *Resolver) blob(ctx context.Context, ref reference, digest string) ([]byte, error) {
	return r.fetch(ctx, ref, "https://"+ref.host+"/v2/"+ref.repo+"/blobs/"+digest, "*/*")
}

// fetch performs the two-step the registry protocol asks for: try, and if the
// answer is a challenge, take the anonymous token and try once more.
//
// Once more and not in a loop. A registry that answers 401 to a request already
// carrying the token it just issued is saying the credential is not enough, and
// asking again produces the same 401 at the cost of another round trip.
func (r *Resolver) fetch(ctx context.Context, ref reference, endpoint, accept string) ([]byte, error) {
	body, challenge, err := r.get(ctx, endpoint, accept, "")
	switch {
	case err != nil:
		return nil, err
	case body != nil:
		return body, nil
	case challenge == "":
		return nil, fmt.Errorf("%w: %s refused the request and offered no way to authenticate",
			ErrUnavailable, ref.host)
	}

	token, err := r.token(ctx, challenge)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, err)
	}
	body, _, err = r.get(ctx, endpoint, accept, token)
	switch {
	case err != nil:
		return nil, err
	case body == nil:
		return nil, fmt.Errorf("%w: %s needs a credential this platform does not have",
			ErrUnavailable, ref.repo)
	}
	return body, nil
}

// get returns the body, or the challenge to answer, or an error. Exactly one of
// the three, and a nil body with no challenge and no error cannot happen.
func (r *Resolver) get(ctx context.Context, endpoint, accept, token string) (body []byte, challenge string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req.Header.Set("Accept", accept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.client().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: asking %s: %w", ErrUnavailable, hostOf(endpoint), err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, resp.Header.Get("WWW-Authenticate"), nil
	case resp.StatusCode == http.StatusNotFound:
		// The same distinction head() draws, and it matters more here: for an
		// upstream template a 404 usually means the template outlived the
		// image, which is a fact about the catalogue and not about arm64.
		return nil, "", fmt.Errorf("%w: %s does not exist", ErrUnavailable, endpoint)
	case resp.StatusCode == http.StatusTooManyRequests:
		// The one failure that is nobody's fault and goes away by waiting. It
		// has to stay distinguishable all the way up: a run that hit a rate
		// limit and a run that found no arm64 build must not produce the same
		// number.
		return nil, "", fmt.Errorf("%w: %s is rate limiting this address",
			ErrUnavailable, hostOf(endpoint))
	case resp.StatusCode != http.StatusOK:
		return nil, "", fmt.Errorf("%w: %s answered %s", ErrUnavailable, hostOf(endpoint), resp.Status)
	}

	// Bounded. A manifest is kilobytes and a config blob is smaller; a body
	// that is neither is a registry doing something this cannot use anyway,
	// and reading it into memory is how one bad answer takes the process down.
	got, err := io.ReadAll(io.LimitReader(resp.Body, manifestLimit))
	if err != nil {
		return nil, "", fmt.Errorf("%w: reading from %s: %w", ErrUnavailable, hostOf(endpoint), err)
	}
	return got, "", nil
}

// manifestLimit is how much of a manifest or a config is read.
//
// Four megabytes, which is far more than either document is and far less than
// a layer. Chosen rather than measured: what it is protecting against is a
// response that is not the document at all.
const manifestLimit = 4 << 20

// hostOf is the host part of an endpoint, for a message. Errors are reported
// with the URL they came from rather than parsed back out of it.
func hostOf(endpoint string) string {
	rest := strings.TrimPrefix(endpoint, "https://")
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// imageConfig is the half of an image config blob this package reads for ports.
//
// Its own type rather than more fields on manifest: ExposedPorts lives under
// "config", and manifest already reads "config" as the pointer to this blob.
// One struct for both would have two different meanings for one key.
type imageConfig struct {
	Config struct {
		// The keys are "27017/tcp" or, on older images, a bare "27017". The
		// values are always empty objects; the set is the whole content.
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
	} `json:"config"`
}

// Ports reports the TCP ports an image says it listens on, distinct and sorted.
//
// This exists because the platform was guessing. A compose service that
// declares no ports left Workload.Spec.Port unset, the CRD defaulted it to
// 8080, and the operator put an HTTP probe on 8080 — so a mongo listening on
// 27017 never became Ready, its Service published no endpoints, and the
// application that named it could not connect. Measured on a cluster on
// 2026-09-01: "Startup probe failed: dial tcp 10.244.1.3:8080: connect:
// connection refused", against an image whose own log said "Ready to accept
// connections tcp" on 6379.
//
// The image knows. EXPOSE is written into the config blob by whoever built it,
// and mongo says 27017, valkey 6379, elasticsearch 9200. Asking is one request
// more than assuming and is the difference between a declaration and a guess.
//
// UDP-only ports are not reported. A Service this platform renders is TCP, so a
// port it cannot serve is not an answer — and returning it would produce the
// same silent mismatch one layer along.
//
// An index is followed through its first image manifest. Exposed ports are a
// property of what was built rather than of the architecture it was built for,
// and an index whose members disagreed about them would be an image nobody
// could describe in one sentence anyway.
func (r *Resolver) Ports(ctx context.Context, image string) ([]int32, error) {
	name, digest, pinned := strings.Cut(image, "@")
	if pinned && !strings.Contains(digest, ":") {
		return nil, fmt.Errorf("%w: %s: %q is not a digest", ErrReference, image, digest)
	}
	ref, err := parse(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrReference, image, err)
	}
	target := ref.tag
	if pinned {
		target = digest
	}

	body, err := r.manifestBody(ctx, ref, target)
	if err != nil {
		return nil, err
	}
	var doc manifest
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s sent a manifest that is not JSON: %w",
			ErrUnavailable, ref.host, err)
	}

	if len(doc.Manifests) > 0 {
		chosen := ""
		for _, m := range doc.Manifests {
			// Attestations carry no config to read, and buildx writes them
			// into the same index with platform unknown/unknown.
			if m.Platform.Architecture == unknownPlatform || m.Platform.OS == unknownPlatform {
				continue
			}
			if m.Digest != "" {
				chosen = m.Digest
				break
			}
		}
		if chosen == "" {
			return nil, fmt.Errorf("%w: %s offered an index with no image in it",
				ErrUnavailable, image)
		}
		if body, err = r.manifestBody(ctx, ref, chosen); err != nil {
			return nil, err
		}
		doc = manifest{}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("%w: %s sent a manifest that is not JSON: %w",
				ErrUnavailable, ref.host, err)
		}
	}
	if doc.Config.Digest == "" {
		return nil, fmt.Errorf("%w: %s is neither an index nor an image manifest",
			ErrUnavailable, image)
	}

	raw, err := r.blob(ctx, ref, doc.Config.Digest)
	if err != nil {
		return nil, err
	}
	var conf imageConfig
	if err := json.Unmarshal(raw, &conf); err != nil {
		return nil, fmt.Errorf("%w: %s sent a config that is not JSON: %w",
			ErrUnavailable, ref.host, err)
	}

	seen := map[int32]bool{}
	var out []int32
	for key := range conf.Config.ExposedPorts {
		num, proto, hasProto := strings.Cut(key, "/")
		if hasProto && !strings.EqualFold(proto, "tcp") {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		if !seen[int32(n)] {
			seen[int32(n)] = true
			out = append(out, int32(n))
		}
	}
	slices.Sort(out)
	return out, nil
}
