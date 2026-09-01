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
