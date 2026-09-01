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

// Its own file rather than more cases in catalog_internal_test.go: what is
// under test here is the seam between this package and catalog.Options.Pin,
// and that file belongs to the catalogue endpoint.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// pinnedDigest is a plausible-looking digest and never a real one. Nothing in
// this file reaches a registry; the resolver's own suite covers the wire.
const pinnedDigest = "@sha256:" +
	"1111111111111111111111111111111111111111111111111111111111111111"

// An entry upstream names by moving tag installs once its images are resolved,
// and does not otherwise.
//
// This is the whole point of the change and the pair is what makes it a test:
// the same entry, the same request, one with a resolver in the seam and one
// without. Measured against the upstream corpus of 371 files, 341 offered: 119
// install with the seam empty and 280 with a resolver that answers for every
// image.
func TestAMovingTagInstallsOnlyOnceItIsPinned(t *testing.T) {
	// A dry run, which is what a page greys an entry out from, so the pair is
	// the same request twice with one thing different.
	const dryRun = `{"template": "floating", "dryRun": true,
		"repoUrl": "%s", "path": "apps/x", "namespace": "%s"}`

	t.Run("without a resolver", func(t *testing.T) {
		h := newHarness(t)
		h.stores.catalog = testCatalog(t)

		code, body := h.install("moving-tag", accMember, fmt.Sprintf(dryRun, homeRepo, nsHomeProd))
		if code != http.StatusOK {
			t.Fatalf("the dry run itself failed: %d %s", code, body)
		}
		if !strings.Contains(body, `"installable":false`) {
			t.Errorf("floating is installable with no resolver: %s", body)
		}
		if !strings.Contains(body, ":latest") {
			t.Errorf("the refusal does not name the tag: %s", body)
		}
	})

	t.Run("with one", func(t *testing.T) {
		h := newHarness(t)
		h.stores.catalog = testCatalog(t)
		h.stores.pin = func(image string) (string, error) { return image + pinnedDigest, nil }

		code, body := h.install("moving-tag", accMember, fmt.Sprintf(dryRun, homeRepo, nsHomeProd))
		if code != http.StatusOK {
			t.Fatalf("the dry run failed: %d %s", code, body)
		}
		if !strings.Contains(body, `"installable":true`) {
			t.Errorf("floating is still not installable with a resolver: %s", body)
		}
	})

	// And the digest reaches the manifest, not only the decision. A plan that
	// answers installable and then commits the tag has moved the failure to
	// the cluster, where the API server refuses it and nobody is looking.
	t.Run("and the digest is what gets committed", func(t *testing.T) {
		h := newHarness(t)
		h.stores.catalog = testCatalog(t)
		h.stores.writer = &gitwrite.Writer{Evidence: h.records}
		h.stores.gitAuth = pathAuth{}
		h.stores.pin = func(image string) (string, error) { return image + pinnedDigest, nil }
		repo := testBareRepo(t)

		code, body := h.install("moving-tag", accMember, `{
			"template": "floating",
			"repoUrl": "`+repo+`", "branch": "main", "path": "apps/x",
			"namespace": "`+nsHomeProd+`"
		}`)
		if code != http.StatusCreated {
			t.Fatalf("installing floating = %d, want 201: %s", code, body)
		}
		committed := committedManifest(t, repo, "apps/x/"+manifest.File)
		app, err := manifest.Parse([]byte(committed))
		if err != nil {
			t.Fatalf("what was committed is not a manifest: %v", err)
		}
		if want := "example.test/floating:latest" + pinnedDigest; app.Spec.Image != want {
			t.Errorf("the committed image is %q, want %q", app.Spec.Image, want)
		}
	})
}

// An image that cannot be resolved is refused by name, with the registry's own
// reason, and nothing is written.
//
// The alternative is the failure class this repository has paid for more than
// once: leaving the tag in place and installing anyway. That succeeds, commits
// a moving tag, and the platform's claim that git says exactly what runs is
// false for that app with nothing anywhere saying so.
func TestAnUnresolvableImageIsRefusedByNameAndNotSilently(t *testing.T) {
	h := newHarness(t)
	h.stores.catalog = testCatalog(t)
	h.stores.pin = func(string) (string, error) {
		return "", errors.New("registry-1.docker.io is rate limiting this address")
	}

	code, body := h.install("unresolvable", accMember, `{
		"template": "floating",
		"repoUrl": "`+homeRepo+`", "branch": "main", "path": "apps/x",
		"namespace": "`+nsHomeProd+`"
	}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("an unresolvable image installed anyway: %d %s", code, body)
	}
	for _, want := range []string{"floating", "rate limiting"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not say %q: %s", want, body)
		}
	}
	if _, err := h.places.Get(context.Background(), tenantHome, "unresolvable", envProd); !errors.Is(err, placement.ErrNotFound) {
		t.Errorf("a refused install registered the app anyway: %v", err)
	}
}

// The switch, and both directions of it.
//
// On by default, which it can only be because catalog.pinImages resolves the
// refused images and no others: a resolver may rescue an entry and may not take
// one away. Off has to stay reachable for an install that will not make
// outbound calls at install time at all.
func TestImagesArePinnedUnlessTheInstallSaysNot(t *testing.T) {
	if o := (Options{}).withDefaults(); o.Pin == nil {
		t.Error("the zero Options installed no resolver, so a fresh install pins nothing")
	}
	if o := (Options{Config: Config{NoImagePinning: true}}).withDefaults(); o.Pin != nil {
		t.Error("pinning was turned off and a resolver was installed anyway")
	}
	// A caller that supplied its own — a mirror, a lockfile — keeps it.
	mine := func(image string) (string, error) { return image, nil }
	if o := (Options{Pin: mine}).withDefaults(); o.Pin == nil {
		t.Error("a supplied resolver was dropped")
	}

	// The flag and the zero value have to say the same thing. They did not when
	// this was a positive PinImages defaulting true in the flag and false in
	// the struct, which is why the field is named the way it is.
	if got := bind(t).NoImagePinning; got != (Options{}).withDefaults().Config.NoImagePinning {
		t.Errorf("-no-image-pinning defaults to %v and the zero Config to %v",
			got, (Options{}).withDefaults().Config.NoImagePinning)
	}
	if !bind(t, "-no-image-pinning").NoImagePinning {
		t.Error("-no-image-pinning did not turn it off")
	}
}

// The resolver has to reach the handler, not merely exist on Options. Deleting
// the one line in run.go that copies it into stores leaves every case above
// passing, because they set stores.pin directly.
func TestTheHandlerIsGivenTheResolver(t *testing.T) {
	h := newHarness(t)
	called := 0
	o := Options{
		Config:    Config{Registry: testRegistry},
		Evidence:  h.records,
		Identity:  h.idStore,
		Placement: h.places,
		Catalog:   testCatalog(t),
		GitAuth:   noAuth{},
		Pin: func(image string) (string, error) {
			called++
			return image + pinnedDigest, nil
		},
	}.withDefaults()

	handler, err := o.handler(h.records, h.idStore)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/tenants/"+tenantHome+"/apps/moving-tag/envs/"+envProd+"/from-catalog",
		strings.NewReader(`{"template":"floating","dryRun":true,`+
			`"repoUrl":"`+homeRepo+`","path":"apps/x","namespace":"`+nsHomeProd+`"}`))
	req.Host = testHost
	req.AddCookie(h.cookies[accMember])
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("the install refused a pinnable entry: %d %s", rec.Code, rec.Body.String())
	}
	if called == 0 {
		t.Error("Options.Pin never reached the handler: the seam is not wired into stores")
	}
}
