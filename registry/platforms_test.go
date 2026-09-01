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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"

	"github.com/damgahq/damga/registry"
)

const (
	configDigest = "sha256:" + "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"

	// The architecture every case here is about, and the one this machine runs.
	arm64 = "arm64"
	linux = "linux"
)

// platformServer answers a manifest request with whatever body a case supplies,
// and a blob request with whatever config it supplies.
//
// Two handlers rather than one, because the second request is the whole point
// of the single-architecture path: an image with no index says nothing about
// its architecture, and a client that stops after the manifest has to guess.
type platformServer struct {
	manifest string
	config   string
	status   int
	blobs    int
}

func newPlatformResolver(t *testing.T, s *platformServer) *registry.Resolver {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if s.status != 0 {
			http.Error(w, "refused", s.status)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			s.blobs++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(s.config))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.manifest))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &registry.Resolver{Client: &http.Client{Transport: toServer{base: base}}}
}

func platformNames(got []registry.Platform) string {
	out := make([]string, 0, len(got))
	for _, p := range got {
		out = append(out, p.String())
	}
	return strings.Join(out, " ")
}

// An index says what it runs on directly, and the attestation entries buildx
// writes into the same index are not architectures.
func TestAnIndexNamesItsPlatformsAndAttestationsAreNotOne(t *testing.T) {
	r := newPlatformResolver(t, &platformServer{manifest: `{
	  "mediaType": "application/vnd.oci.image.index.v1+json",
	  "manifests": [
	    {"platform": {"os": "linux", "architecture": "amd64"}},
	    {"platform": {"os": "linux", "architecture": "arm64", "variant": "v8"}},
	    {"platform": {"os": "unknown", "architecture": "unknown"}}
	  ]}`})

	got, err := r.Platforms(context.Background(), "example.test/app:1")
	if err != nil {
		t.Fatalf("Platforms: %v", err)
	}
	if names := platformNames(got); names != "linux/amd64 linux/arm64/v8" {
		t.Errorf("platforms = %q; an attestation reported as a platform tells the reader "+
			"something runs on unknown/unknown", names)
	}
}

// The case a guess gets wrong. "No index means amd64" is the plausible shape,
// and it is wrong for exactly the images this exists to find.
func TestASingleManifestIsReadFromItsConfigRatherThanAssumed(t *testing.T) {
	s := &platformServer{
		manifest: `{"mediaType":"application/vnd.oci.image.manifest.v1+json",
		            "config":{"digest":"` + configDigest + `"}}`,
		config: `{"os":"linux","architecture":"arm64","variant":"v8"}`,
	}
	r := newPlatformResolver(t, s)

	got, err := r.Platforms(context.Background(), "example.test/app:1")
	if err != nil {
		t.Fatalf("Platforms: %v", err)
	}
	if s.blobs == 0 {
		t.Error("the config blob was never fetched, so the architecture was assumed rather " +
			"than read; every arm64-only image would be reported as amd64")
	}
	if names := platformNames(got); names != "linux/arm64/v8" {
		t.Errorf("platforms = %q, want linux/arm64/v8", names)
	}
}

// The distinction the whole measurement rests on. "This image has no arm64
// build" is a fact about the image; "nobody would tell us" is a fact about the
// afternoon, and counting them together produces a number that is partly about
// each.
func TestAnUnansweredQuestionIsNotAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"a rate limit", http.StatusTooManyRequests},
		{"a tag that is gone", http.StatusNotFound},
		{"a refusal with no challenge", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPlatformResolver(t, &platformServer{status: tc.status})
			got, err := r.Platforms(context.Background(), "example.test/app:1")
			if !errors.Is(err, registry.ErrUnavailable) {
				t.Fatalf("Platforms returned (%v, %v), want ErrUnavailable; anything else "+
					"lands this image in the same count as one that genuinely has no arm64 "+
					"build", got, err)
			}
			if got != nil {
				t.Errorf("platforms = %v alongside an error", got)
			}
		})
	}
}

// And the other side of it: a reference this package cannot address is not the
// registry's doing either.
func TestAReferenceThatIsNotOneSaysSo(t *testing.T) {
	r := newPlatformResolver(t, &platformServer{})
	if _, err := r.Platforms(context.Background(), ""); !errors.Is(err, registry.ErrReference) {
		t.Errorf("an empty reference returned %v, want ErrReference", err)
	}
}

// An index that names nothing is not an image with no platforms. Reported as
// unanswered, because the document was not what it said it was.
func TestAnIndexNamingNothingIsNotAnAnswerEither(t *testing.T) {
	r := newPlatformResolver(t, &platformServer{manifest: `{
	  "mediaType": "application/vnd.oci.image.index.v1+json",
	  "manifests": [{"platform": {"os": "unknown", "architecture": "unknown"}}]}`})

	got, err := r.Platforms(context.Background(), "example.test/app:1")
	if !errors.Is(err, registry.ErrUnavailable) {
		t.Fatalf("Platforms returned (%v, %v), want ErrUnavailable", got, err)
	}
}

// The variant is compared only when it was asked for. A node reports
// kubernetes.io/arch=arm64 and nothing finer, so an index offering
// linux/arm64/v8 runs there — and treating the two as different architectures
// would report "no arm64 build" about images that run perfectly well.
func TestAVariantDoesNotMakeADifferentArchitecture(t *testing.T) {
	node := registry.Platform{OS: linux, Architecture: arm64}
	for _, tc := range []struct {
		have registry.Platform
		want bool
	}{
		{registry.Platform{OS: linux, Architecture: arm64, Variant: "v8"}, true},
		{registry.Platform{OS: linux, Architecture: arm64}, true},
		{registry.Platform{OS: linux, Architecture: "amd64"}, false},
		{registry.Platform{OS: "windows", Architecture: arm64}, false},
		{registry.Platform{OS: linux, Architecture: "arm", Variant: "v7"}, false},
	} {
		if got := tc.have.Matches(node); got != tc.want {
			t.Errorf("%s.Matches(%s) = %v, want %v", tc.have, node, got, tc.want)
		}
	}
}

// A reference that already names a digest still has an answer, and asking for
// it must not mangle the reference on the way.
//
// parse looks for a tag after the last colon, and in name@sha256:abc… that
// colon belongs to the digest — so without splitting first the request goes to
// the repository "name@sha256" for the tag "abc…". Found against the corpus:
// one upstream template pins by digest, and it arrived as a 404, which is
// indistinguishable from a rate limit in the count that matters.
func TestADigestReferenceIsAskedAboutByDigest(t *testing.T) {
	var asked string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"manifests":[{"platform":{"os":"linux","architecture":"arm64"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := &registry.Resolver{Client: &http.Client{Transport: toServer{base: base}}}

	got, err := r.Platforms(context.Background(), "example.test/app:1@"+configDigest)
	if err != nil {
		t.Fatalf("Platforms: %v", err)
	}
	if want := "/v2/app/manifests/" + configDigest; asked != want {
		t.Errorf("asked for %q, want %q; the digest was read as a tag and the repository "+
			"grew an @sha256 on the end, which the registry answers 404 to", asked, want)
	}
	if names := platformNames(got); names != "linux/arm64" {
		t.Errorf("platforms = %q", names)
	}
}

// And one that carries an @ with nothing usable after it is a bad reference,
// not a registry that would not answer.
func TestAnAtWithNoDigestIsABadReference(t *testing.T) {
	r := newPlatformResolver(t, &platformServer{})
	if _, err := r.Platforms(context.Background(), "example.test/app@notadigest"); !errors.Is(err, registry.ErrReference) {
		t.Errorf("Platforms returned %v, want ErrReference", err)
	}
}

// What an image says it listens on, read from its config blob.
//
// The reason this is not a guess: before it existed, a service whose compose
// file declared no ports left Workload.Spec.Port unset, the CRD defaulted it to
// 8080, and the operator put an HTTP probe there. Measured on a cluster on
// 2026-09-01, a mongo on 27017 never became Ready — "dial tcp
// 10.244.1.3:8080: connect: connection refused" — while its own log said
// "Ready to accept connections tcp".
func TestAnImageIsAskedWhatItListensOn(t *testing.T) {
	// A manifest that points at a config blob, which is what every case here
	// needs before the ports can be read out of it.
	const configPointer = `{"config":{"digest":"sha256:cfg"}}`

	for _, c := range []struct {
		name     string
		manifest string
		config   string
		want     []int32
	}{
		{
			name:     "a single manifest",
			manifest: configPointer,
			config:   `{"config":{"ExposedPorts":{"27017/tcp":{}}}}`,
			want:     []int32{27017},
		}, {
			// Older images write the number with no protocol.
			name:     "a bare port with no protocol",
			manifest: configPointer,
			config:   `{"config":{"ExposedPorts":{"6379":{}}}}`,
			want:     []int32{6379},
		}, {
			// A Service this platform renders is TCP, so a port it cannot serve
			// is not an answer — returning it would move the same silent
			// mismatch one layer along.
			name:     "udp is not an answer",
			manifest: configPointer,
			config:   `{"config":{"ExposedPorts":{"53/udp":{},"53/tcp":{}}}}`,
			want:     []int32{53},
		}, {
			name:     "several, sorted, for the caller to refuse",
			manifest: configPointer,
			config:   `{"config":{"ExposedPorts":{"443/tcp":{},"80/tcp":{}}}}`,
			want:     []int32{80, 443},
		}, {
			name:     "an image that exposes nothing",
			manifest: configPointer,
			config:   `{"config":{}}`,
			want:     nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPlatformResolver(t, &platformServer{manifest: c.manifest, config: c.config})
			got, err := r.Ports(context.Background(), "example/app:1")
			if err != nil {
				t.Fatalf("Ports: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("Ports = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("Ports = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// A registry that will not answer is not an image with no ports. The caller
// refuses in both cases and says which, so "declare a port" and "wait and try
// again" do not arrive as one sentence.
func TestAPortQuestionThatCannotBeAnsweredIsAnError(t *testing.T) {
	r := newPlatformResolver(t, &platformServer{status: http.StatusTooManyRequests})
	if _, err := r.Ports(context.Background(), "example/app:1"); err == nil {
		t.Fatal("a rate-limited registry answered a port")
	}
}

// An index has to be followed to one of its members: the ports are in an
// image's config and an index has none of its own.
//
// Its own server rather than platformServer, which answers every manifest
// request with the same body — enough for Platforms, which stops at the index,
// and not for this, which asks a second question whose answer is different.
func TestPortsFollowAnIndexToAnImage(t *testing.T) {
	const (
		index = `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
			`{"platform":{"os":"unknown","architecture":"unknown"},"digest":"sha256:att"},` +
			`{"platform":{"os":"linux","architecture":"arm64"},"digest":"sha256:img"}]}`
		image = `{"config":{"digest":"sha256:cfg"}}`
		cfg   = `{"config":{"ExposedPorts":{"9200/tcp":{}}}}`
	)
	var askedFor []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		askedFor = append(askedFor, path.Base(r.URL.Path))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/blobs/"):
			_, _ = w.Write([]byte(cfg))
		case strings.HasSuffix(r.URL.Path, "/manifests/1"):
			_, _ = w.Write([]byte(index))
		default:
			_, _ = w.Write([]byte(image))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := &registry.Resolver{Client: &http.Client{Transport: toServer{base: base}}}

	got, err := r.Ports(context.Background(), "example/app:1")
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if len(got) != 1 || got[0] != 9200 {
		t.Errorf("Ports = %v, want [9200]", got)
	}
	// The attestation is skipped rather than followed: it carries no config,
	// and a client that took the first entry would ask for a blob that is not
	// there and report the image as unanswerable.
	if len(askedFor) != 3 || askedFor[1] != "sha256:img" {
		t.Errorf("asked for %v, want the index, the image manifest, then its config", askedFor)
	}
}
