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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/damgahq/damga/catalog"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// The four shapes the endpoint has to tell apart, as templates rather than as
// mocks. They go through compose.Parse and compose.Convert exactly as the real
// corpus does, so a refusal here is one the real corpus would get too.
var testTemplates = fstest.MapFS{
	// Installs: one service, an image the API accepts, nothing to invent.
	"tidy.yaml": &fstest.MapFile{Data: []byte(`# slogan: A tidy little service
# category: testing
# tags: tidy, small
# logo: svgs/tidy.svg
# port: 8080

services:
  app:
    image: example.test/tidy:1.4.2
    environment:
      - LOG_LEVEL=info
`)},
	// Refused: asks the platform to invent a value, and nothing mints one.
	"secretive.yaml": &fstest.MapFile{Data: []byte(`# slogan: Wants a password
# category: testing

services:
  app:
    image: example.test/secretive:2.0.0
    environment:
      - API_TOKEN=${SERVICE_PASSWORD_SECRETIVE}
`)},
	// Refused: two workloads, and an app environment holds one.
	"pair.yaml": &fstest.MapFile{Data: []byte(`# slogan: Two of everything
# category: testing

services:
  web:
    image: example.test/web:1.0.0
  worker:
    image: example.test/worker:1.0.0
`)},
	// Refused by the catalogue itself: an image the Workload API rejects.
	"floating.yaml": &fstest.MapFile{Data: []byte(`# slogan: Rolls under you
# category: testing

services:
  app:
    image: example.test/floating:latest
`)},
}

func testCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load(testTemplates)
	if err != nil {
		t.Fatalf("loading the test catalogue: %v", err)
	}
	return c
}

// recorder wraps a catalogue and remembers the last query it was asked.
//
// The handler's whole job on the list route is to turn a URL into a Query, and
// asserting on the entries that come back would pass just as well if it threw
// the filter away and the corpus happened to be small.
type recorder struct {
	CatalogSource
	last catalog.Query
}

func (r *recorder) Find(q catalog.Query) []catalog.Entry {
	r.last = q
	return r.CatalogSource.Find(q)
}

// callEnv drives a handler whose route names an environment, through a mux, so
// the path values the guard reads are set the way the router sets them.
func (h *harness) callEnv(
	handler func(guard, stores) http.Handler,
	method, suffix, tenant, app, env, account, body string,
) (int, string) {
	h.t.Helper()
	mux := http.NewServeMux()
	mux.Handle(method+" "+tenantScope+suffix, handler(h.guard, h.stores))

	target := strings.NewReplacer(
		"{tenant}", tenant, "{app}", app, "{env}", env,
	).Replace(tenantScope + suffix)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = testHost
	if account != "" {
		req.AddCookie(h.cookies[account])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (h *harness) install(app, env, account, body string) (int, string) {
	h.t.Helper()
	return h.callEnv(installFromCatalog, http.MethodPost,
		"/apps/{app}/envs/{env}/from-catalog", tenantHome, app, env, account, body)
}

// The filter the user typed has to reach the catalogue. A handler that reads
// the wrong parameter names answers a full list, which looks like a catalogue
// that ignores search rather than like a bug.
func TestTheCatalogueListsWhatWasAskedFor(t *testing.T) {
	h := newHarness(t)
	src := &recorder{CatalogSource: testCatalog(t)}
	h.stores.catalog = src

	mux := http.NewServeMux()
	mux.Handle(http.MethodGet+" "+tenantScope+"/catalog", catalogList(h.guard, h.stores))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/tenants/"+tenantHome+"/catalog?q=tidy&category=testing&tag=small&tag=tidy", nil)
	req.Host = testHost
	req.AddCookie(h.cookies[accViewer])
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /catalog = %d: %s", rec.Code, rec.Body.String())
	}
	switch {
	case src.last.Text != "tidy":
		t.Errorf("the search text reached the catalogue as %q; nothing the user typed was used", src.last.Text)
	case src.last.Category != "testing":
		t.Errorf("the category reached the catalogue as %q", src.last.Category)
	case len(src.last.Tags) != 2:
		t.Errorf("tags reached the catalogue as %v; a repeated parameter is two tags, not one", src.last.Tags)
	}

	var got struct {
		Entries    []wireEntry `json:"entries"`
		Categories []string    `json:"categories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Name != "tidy" {
		t.Fatalf("the filtered list is %+v, want the one entry that matches", got.Entries)
	}
	if got.Entries[0].Services != 1 || got.Entries[0].Slogan == "" {
		t.Errorf("the entry lost what a person picks by: %+v", got.Entries[0])
	}
	// The categories are of the catalogue and not of the result, or filtering
	// to one category deletes every other option from the page that offered it.
	if len(got.Categories) == 0 {
		t.Error("the categories came back empty while a category filter was applied")
	}
}

// An install with no templates mounted has to say so. An empty list is the same
// bytes as a filter that matched nothing, and the person reading the page is
// the one who cannot tell which.
func TestNoCatalogueSaysSoRatherThanAnsweringAnEmptyList(t *testing.T) {
	h := newHarness(t)
	code, body := h.callEnv(catalogList, http.MethodGet, "/catalog",
		tenantHome, "", "", accViewer, "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET /catalog with nothing mounted = %d: %s", code, body)
	}
	if !strings.Contains(body, "-catalog-dir") {
		t.Errorf("the refusal does not name the flag that fixes it: %s", body)
	}
}

// Every refusal is checked before anything is written. A 422 that has already
// created the placement leaves an app registered for an install that never
// happened, and the next attempt answers 409 about a row nobody made.
func TestAnInstallThatCannotWorkWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		template string
		app      string
		wants    string
	}{
		{"secretive", "wants-a-password", "SERVICE_PASSWORD_SECRETIVE"},
		{"pair", "two-workloads", "2 workloads"},
		{"floating", "moving-tag", ":latest"},
	} {
		t.Run(tc.template, func(t *testing.T) {
			h := newHarness(t)
			h.stores.catalog = testCatalog(t)

			code, body := h.install(tc.app, envProd, accMember, `{
				"template": "`+tc.template+`",
				"repoUrl": "`+homeRepo+`", "branch": "main", "path": "apps/x",
				"namespace": "`+nsHomeProd+`"
			}`)
			// Errorf and not Fatalf: what this case is really about is the
			// row below, and stopping here would hide a refusal that answered
			// the right status after writing.
			if code != http.StatusUnprocessableEntity {
				t.Errorf("installing %s = %d, want 422: %s", tc.template, code, body)
			}
			if !strings.Contains(body, tc.wants) {
				t.Errorf("the refusal does not say what stopped it (%q): %s", tc.wants, body)
			}

			_, err := h.places.Get(context.Background(), tenantHome, tc.app, envProd)
			if !errors.Is(err, placement.ErrNotFound) {
				t.Fatalf("a refused install registered the app anyway (%v), so the next attempt "+
					"answers 409 about a row nothing installed", err)
			}
		})
	}
}

// A dry run is how a page greys an entry out with the reason attached. It must
// answer the same refusals and write nothing at all.
func TestADryRunAnswersThePlanAndWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.stores.catalog = testCatalog(t)

	code, body := h.install("preview", envProd, accMember, `{
		"template": "secretive", "dryRun": true,
		"repoUrl": "`+homeRepo+`", "path": "apps/x", "namespace": "`+nsHomeProd+`"
	}`)
	if code != http.StatusOK {
		t.Fatalf("a dry run = %d, want 200: %s", code, body)
	}

	var plan wirePlan
	if err := json.Unmarshal([]byte(body), &plan); err != nil {
		t.Fatalf("the plan is not JSON: %v", err)
	}
	switch {
	case plan.Installable:
		t.Error("a template nothing can supply values for was reported as installable")
	case len(plan.Refusals) == 0:
		t.Error("the plan carries no reason, so a page can grey the entry out and say nothing")
	case len(plan.Generated) != 1 || plan.Generated[0] != "SERVICE_PASSWORD_SECRETIVE":
		t.Errorf("the values the template asks for are %v; the names are what a user checks "+
			"the platform read correctly", plan.Generated)
	}

	if _, err := h.places.Get(context.Background(), tenantHome, "preview", envProd); !errors.Is(err, placement.ErrNotFound) {
		t.Fatal("a dry run registered the app; the word means it does not")
	}
}

// The whole point, end to end: a template becomes a placement and a committed
// manifest, in one request.
func TestInstallingCommitsOneManifestNamedAfterTheApp(t *testing.T) {
	h := newHarness(t)
	h.stores.catalog = testCatalog(t)
	h.stores.writer = &gitwrite.Writer{Evidence: h.records}
	// The harness carries noAuth, which is the answer for an install with no
	// token configured; a local path needs none.
	h.stores.gitAuth = pathAuth{}
	repo := testBareRepo(t)

	code, body := h.install("tidy-app", envProd, accMember, `{
		"template": "tidy",
		"repoUrl": "`+repo+`", "branch": "main", "path": "apps/tidy/prod",
		"namespace": "`+nsHomeProd+`"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("installing tidy = %d, want 201: %s", code, body)
	}

	got, err := h.places.Get(context.Background(), tenantHome, "tidy-app", envProd)
	if err != nil {
		t.Fatalf("the app was not registered: %v", err)
	}
	if got.Namespace != nsHomeProd || got.RepoURL != repo {
		t.Errorf("the placement is not what was asked for: %+v", got)
	}

	committed := committedManifest(t, repo, "apps/tidy/prod/"+manifest.File)
	app, err := manifest.Parse([]byte(committed))
	if err != nil {
		t.Fatalf("what was committed is not a manifest this build can read: %v", err)
	}
	switch {
	case app.Name != "tidy-app":
		t.Errorf("the manifest is named %q; identity comes from the placement, not from the "+
			"compose service, or nothing can deploy to it later", app.Name)
	case app.Namespace != nsHomeProd:
		t.Errorf("the manifest landed in namespace %q", app.Namespace)
	case app.Spec.Image != "example.test/tidy:1.4.2":
		t.Errorf("the image is %q; the template's image is what makes this an install of "+
			"that template", app.Spec.Image)
	}
}

// An install claims the app is new. A directory that already holds a manifest
// is one where that claim is false, and writing over it replaces an app that
// exists with one somebody picked off a list.
func TestInstallingOverAnExistingManifestIsRefused(t *testing.T) {
	c := testCatalog(t)
	plan, err := c.Plan("tidy", catalog.Options{Namespace: nsHomeProd})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	render := renderInstall(placement.Placement{
		App: "tidy-app", Namespace: nsHomeProd, Path: "apps/tidy/prod",
	}, plan)

	if _, err := render("r1", map[string][]byte{manifest.File: []byte("kind: Workload\n")}); err == nil {
		t.Fatal("an install wrote over a manifest that was already committed")
	}
	files, err := render("r1", nil)
	if err != nil {
		t.Fatalf("installing into an empty directory failed: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("an install committed %d files; a later deploy renders one and would leave "+
			"the rest to drift", len(files))
	}
}

// A mounted directory with nothing in it is a misconfiguration, and it has to
// fail where somebody is looking: at startup, naming the directory.
func TestAnEmptyCatalogueDirectoryIsAnErrorAndNotAnEmptyCatalogue(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not a template\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := Options{Config: Config{CatalogDir: dir}}
	if _, err := o.catalogSource(); err == nil {
		t.Fatal("a directory with no templates loaded as a catalogue, so the panel would show " +
			"an empty list and nothing would say the mount is wrong")
	} else if !strings.Contains(err.Error(), dir) {
		t.Errorf("the error does not name the directory it read: %v", err)
	}

	// And no directory at all is not an error: an install without a catalogue
	// is a legitimate install, and the endpoints say so themselves.
	src, err := (Options{}).catalogSource()
	if err != nil || src != nil {
		t.Errorf("an install with no catalogue configured = (%v, %v), want (nil, nil)", src, err)
	}
}

// pathAuth is what an install supplies for a repository that needs no
// credentials: a local path, or an in-cluster git server on a private network.
type pathAuth struct{}

func (pathAuth) For(string) (transport.AuthMethod, error) { return nil, nil }

// testBareRepo makes a repository with one commit on main and returns its path.
//
// A second copy of server_test.go's helper, because that file is in the
// external test package and this one has to be internal: it reaches stores and
// renderInstall, which are unexported and are what there is to test.
func testBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "state.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init: %v", err)
	}
	// An empty bare repository cannot be cloned, so the first commit is made
	// beside it and pushed.
	work := filepath.Join(dir, "seed")
	seed, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatalf("seed init: %v", err)
	}
	if _, err := seed.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin", URLs: []string{bare},
	}); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	tree, err := seed.Worktree()
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("state\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := tree.Add("README.md"); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	who := &object.Signature{Name: "seed", Email: "seed@example.test", When: time.Now()}
	if _, err := tree.Commit("seed", &git.CommitOptions{Author: who, Committer: who}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	head, err := seed.Head()
	if err != nil {
		t.Fatalf("seed head: %v", err)
	}
	if err := seed.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head.Hash()),
	); err != nil {
		t.Fatalf("seed branch: %v", err)
	}
	if err := seed.Push(&git.PushOptions{RefSpecs: []gitconfig.RefSpec{
		gitconfig.RefSpec("refs/heads/main:refs/heads/main"),
	}}); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	return bare
}

// committedManifest reads one path out of the remote's main branch.
func committedManifest(t *testing.T, repoPath, name string) string {
	t.Helper()
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		t.Fatalf("opening %s: %v", repoPath, err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("resolving main: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("reading the commit: %v", err)
	}
	file, err := commit.File(name)
	if err != nil {
		t.Fatalf("%s was never committed: %v", name, err)
	}
	body, err := file.Contents()
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return body
}
