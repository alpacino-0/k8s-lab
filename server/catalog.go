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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/catalog"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// CatalogSource is the loaded catalogue, as this package needs it.
//
// An interface rather than *catalog.Catalog so that where the templates come
// from stays a decision outside this package — and so a test can hand over four
// entries instead of a directory of 371 files.
type CatalogSource interface {
	Entries() []catalog.Entry
	Find(catalog.Query) []catalog.Entry
	Categories() []string
	Plan(name string, o catalog.Options) (catalog.Plan, error)
}

// catalogSource resolves the seam, then the flag, then nothing.
//
// Nothing is a supported answer and an empty catalogue is not: an install with
// no templates mounted answers 503 and names the flag, because the alternative
// — an empty list — looks exactly like a filter that matched nothing, and the
// person reading it is the one who would have to guess which.
func (o Options) catalogSource() (CatalogSource, error) {
	if o.Catalog != nil {
		return o.Catalog, nil
	}
	if o.Config.CatalogDir == "" {
		return nil, nil
	}
	// os.DirFS and not embed. The catalogue changes far faster than damga does
	// — upstream adds entries weekly — and a built-in bundle makes updating the
	// list a version upgrade of the control plane. A directory is what a
	// ConfigMap or a volume already is, and catalog.Load takes any fs.FS, so
	// pulling the same files as an OCI artifact later changes this function
	// and nothing else.
	c, err := catalog.Load(os.DirFS(o.Config.CatalogDir))
	if err != nil {
		// At startup rather than at the first page load, and fatal rather than
		// logged: a mount that is not there is a misconfiguration, and finding
		// out from a user who clicked Catalogue means finding out late.
		return nil, fmt.Errorf("reading the catalogue from %s: %w", o.Config.CatalogDir, err)
	}
	return c, nil
}

// Two strings this file would otherwise be the third user of in the package.
//
// Named rather than repeated because that is what the linter counts, and the
// other two uses of each are in files this change does not own — so the choice
// was a constant here or an edit there, and a constant here touches nothing
// somebody else is holding.
const (
	fieldNamespace = "namespace"
	keyApp         = "app"
)

// wireEntry is one catalogue entry on the wire.
//
// Its own type for the reason wire.go gives at length: what an entry is in the
// catalogue is one concern and what a panel renders is another. It carries no
// plan — planning 341 entries to draw a list would be 341 conversions per page
// load, and the answer a user needs is about the one they clicked.
type wireEntry struct {
	Name          string   `json:"name"`
	Slogan        string   `json:"slogan"`
	Category      string   `json:"category"`
	Tags          []string `json:"tags,omitempty"`
	Logo          string   `json:"logo,omitempty"`
	Documentation string   `json:"documentation,omitempty"`

	// Services is how many containers the entry runs, and it is on the list
	// rather than only in the plan because it is the one number that predicts
	// the refusal below: an entry with more than one is one this platform
	// cannot yet install, and a user is better served by seeing that before
	// they choose than after.
	Services int `json:"services"`
}

func toWireEntry(e catalog.Entry) wireEntry {
	return wireEntry{
		Name: e.Name, Slogan: e.Slogan, Category: e.Category, Tags: e.Tags,
		Logo: e.Logo, Documentation: e.Documentation, Services: e.Services,
	}
}

// wirePlan is what a dry run answers: what installing this entry would produce,
// and every reason it would be refused.
type wirePlan struct {
	Entry     wireEntry `json:"entry"`
	Namespace string    `json:"namespace"`

	Workloads []string `json:"workloads"`
	Databases []string `json:"databases,omitempty"`

	// Generated names the values the template asks the platform to invent. The
	// names only: this endpoint never holds a value, and the reason it does not
	// is that nothing yet mints one.
	Generated []string `json:"generated,omitempty"`

	// Notes are what did not convert, for a person to read.
	Notes []string `json:"notes,omitempty"`

	// Refusals is empty exactly when the install would go ahead.
	Refusals []string `json:"refusals,omitempty"`

	Installable bool `json:"installable"`
}

// catalogList is the list a user picks from.
//
// Inside the tenant scope rather than beside it, although the catalogue is
// install-wide and identical for every tenant. The scope is what puts a route
// in the table TestEveryTenantRouteIsGuarded walks, and a route registered next
// to the table is the one that arrangement cannot protect. The cost is a path
// segment that does not narrow the answer; the alternative is a new
// unauthenticated surface, which is the kind of thing that is added once and
// noticed later.
func catalogList(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// app:view, the weakest read this platform has. A viewer may see what
		// is installed, and the list of what could be installed says nothing
		// about this tenant at all.
		if _, _, ok := g.admit(w, r, authz.ActionAppView); !ok {
			return
		}
		src, ok := requireCatalog(w, st)
		if !ok {
			return
		}

		q := catalog.Query{
			Text:     r.URL.Query().Get("q"),
			Category: r.URL.Query().Get("category"),
			Tags:     r.URL.Query()["tag"],
		}
		found := src.Find(q)
		entries := make([]wireEntry, 0, len(found))
		for _, e := range found {
			entries = append(entries, toWireEntry(e))
		}
		// The categories are of the whole catalogue and not of the result, so
		// a filter that narrows to one category does not also delete every
		// other option from the page that offered it.
		writeJSON(w, map[string]any{"entries": entries, "categories": src.Categories()})
	})
}

// installRequest installs one catalogue entry as one app environment.
//
// The app and the environment come from the path and never from here, because
// the guard authorized against them: an endpoint that names its target in the
// body has authorized one thing and done another the day an authorizer looks
// at more than the tenant.
type installRequest struct {
	Template string `json:"template"`

	RepoURL string `json:"repoUrl"`
	Branch  string `json:"branch"`
	Path    string `json:"path"`

	Namespace string `json:"namespace"`
	Domain    string `json:"domain,omitempty"`

	// VolumeSize is what each named volume is claimed at. Compose declares no
	// size, so somebody has to choose; empty leaves the converter's default,
	// which it records in a note for every volume.
	VolumeSize string `json:"volumeSize,omitempty"`

	// DryRun plans and answers without writing anything. It is how a page
	// greys out an entry with the reason attached rather than with a shrug.
	DryRun bool `json:"dryRun,omitempty"`
}

// installFromCatalog turns one catalogue entry into one app environment.
//
// Two writes in one request, in this order and no other: the placement first,
// then the commit. A placement is a row saying where a deploy would go, so a
// commit that fails leaves an app registered with nothing committed — which is
// recoverable by asking again. The other order leaves manifests in a repository
// that Argo CD will apply and the control plane has no record of, which is not.
//
// The environment is in the path rather than in the body, and that is the one
// place this endpoint differs from createApp beside it. createApp takes its
// environment from the body because it only writes a row; this one also commits
// a manifest, which is what app:deploy authorizes — and the guard builds its
// target from the path. An environment the guard never saw is one an
// environment-scoped authorizer would never have refused.
func installFromCatalog(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// app:deploy and not env:create, although it does both. This endpoint
		// ends with a commit that Argo CD applies, and the stronger of the two
		// rights is the honest one: somebody who may register an app but not
		// ship to it must not be able to install n8n by another door.
		sub, ref, ok := g.admit(w, r, authz.ActionAppDeploy)
		if !ok {
			return
		}
		src, ok := requireCatalog(w, st)
		if !ok {
			return
		}

		var req installRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "the request body is not the expected JSON")
			return
		}
		req.Template = strings.TrimSpace(req.Template)
		req.Namespace = strings.TrimSpace(req.Namespace)
		req.RepoURL, req.Branch = strings.TrimSpace(req.RepoURL), strings.TrimSpace(req.Branch)
		req.Path, req.Domain = strings.TrimSpace(req.Path), strings.TrimSpace(req.Domain)

		if req.Template == "" {
			problem(w, http.StatusBadRequest, "an install needs the name of a template")
			return
		}
		// The same three shapes createApp checks, for the same reason: an app
		// called "My API" is accepted by every layer between here and the
		// cluster and refused by the API server, where the failure is a
		// rollout that never appears rather than a sentence anybody can read.
		for _, f := range []struct{ what, value string }{
			{"app name", ref.App},
			{"environment", ref.Env},
			{fieldNamespace, req.Namespace},
		} {
			if f.value == "" {
				problem(w, http.StatusBadRequest, "an install needs an app name, an environment and a namespace")
				return
			}
			if msgs := validation.IsDNS1123Label(f.value); len(msgs) > 0 {
				problem(w, http.StatusBadRequest, "the "+f.what+" is not usable in Kubernetes: "+msgs[0])
				return
			}
		}
		if req.RepoURL == "" {
			problem(w, http.StatusBadRequest, "an install needs a repository to write its manifests to")
			return
		}

		opts := catalog.Options{Namespace: req.Namespace, Domain: req.Domain}
		if req.VolumeSize != "" {
			size, err := resource.ParseQuantity(req.VolumeSize)
			if err != nil {
				problem(w, http.StatusBadRequest, "volumeSize is not a quantity: "+err.Error())
				return
			}
			opts.VolumeSize = size
		}
		// Options.Pin is deliberately not set, and it is the largest single
		// reason an entry is refused below. Resolving an image to a digest
		// means asking a registry at install time, and what asks — a client
		// here, a mirror, a lockfile beside the templates — is an open
		// decision. Measured against the upstream corpus: 119 of the 341
		// entries offered are installable with this empty, and 280 with it
		// filled in.
		plan, err := src.Plan(req.Template, opts)
		if err != nil {
			// The only errors Plan returns are an unknown entry and a template
			// that does not convert at all. Everything else it knows is wrong
			// arrives as a blocker, which is what refusals below are made of.
			problem(w, http.StatusNotFound, err.Error())
			return
		}

		secrets, _ := plannedSecrets(plan)
		refusals := whyRefused(plan)
		if req.DryRun {
			writeJSON(w, toWirePlan(plan, refusals))
			return
		}
		if len(refusals) > 0 {
			// 422 and not 400: the request is well formed and the template is
			// real. What cannot be done is the install, and every reason is
			// listed rather than only the first — a user who fixes one and is
			// then told about the next has been sent round the loop by the
			// endpoint rather than by their choice.
			w.WriteHeader(http.StatusUnprocessableEntity)
			writeJSON(w, toWirePlan(plan, refusals))
			return
		}

		// Advisory, exactly as in createApp: two installs racing each other
		// both see nothing here. What it buys is the ordinary case — an app
		// that already exists is a 409 rather than a live app silently
		// repointed at a template.
		if _, err := st.placement.Get(r.Context(), ref.TenantID, ref.App, ref.Env); err == nil {
			problem(w, http.StatusConflict, "this app and environment already exist")
			return
		} else if !errors.Is(err, placement.ErrNotFound) {
			problem(w, http.StatusInternalServerError, "reading the placement failed")
			return
		}

		place, err := st.placement.Put(r.Context(), placement.Placement{
			TenantID:  ref.TenantID,
			App:       ref.App,
			Env:       ref.Env,
			RepoURL:   req.RepoURL,
			Branch:    req.Branch,
			Path:      req.Path,
			Namespace: req.Namespace,
		})
		switch {
		case errors.Is(err, placement.ErrInvalid), errors.Is(err, placement.ErrConflict):
			// The store's own words, which name the field and quote only
			// values this caller sent.
			problem(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "writing the placement failed")
			return
		}

		method, err := st.gitAuth.For(place.RepoURL)
		if err != nil {
			problem(w, http.StatusInternalServerError, err.Error())
			return
		}
		result, err := st.writer.Deploy(r.Context(), gitwrite.Request{
			Target: gitwrite.Target{
				RepoURL: place.RepoURL, Branch: place.Branch, Dir: place.Path, Auth: method,
			},
			Author:  gitwrite.Author{ID: sub.ID, Name: sub.ID, Email: sub.Email},
			Ref:     ref,
			Message: fmt.Sprintf("install %s as %s/%s", plan.Entry.Name, ref.App, ref.Env),
			Render:  renderInstall(place, plan, secrets),
		})
		if err != nil {
			// The placement is written and the commit is not. Said plainly,
			// because the caller is the only party in a position to know that
			// asking again is what fixes it — and because "the install failed"
			// would suggest there is nothing to clean up, while the app is
			// registered and its next deploy would go somewhere real.
			problem(w, http.StatusBadGateway,
				"the app was registered but its manifests were not committed, so nothing is "+
					"running yet: "+err.Error())
			return
		}

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{
			keyApp:   toWirePlacement(place),
			"plan":   toWirePlan(plan, nil),
			"deploy": toWireRecord(result.Record),
		})
	})
}

// envName mirrors the pattern the Workload API puts on a generated value's
// name. Checked here so a template that names a variable Kubernetes will refuse
// is refused before the commit rather than after it — an API server rejecting a
// manifest that is already pushed is a failure with nobody watching.
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// maxGeneratedSecrets mirrors MaxItems on the same field, for the same reason.
const maxGeneratedSecrets = 16

// plannedSecrets turns the plan's requests into the field the operator mints,
// and reports every request it cannot express.
//
// The field mints one value per name. The plan is richer than that on purpose —
// a Key carries a template and the sources to substitute into it, because 51 of
// the upstream templates build a connection string around a credential and 111
// share one value between two services — and this function is where that
// richness meets an API that has none of it. Everything it cannot say is
// refused by name rather than approximated, because the approximations are all
// the same shape: an application that starts, holds a credential nobody else
// has, and fails at its first connection.
//
// Measured through this function: 159 of the 341 entries ask for a generated
// value and 62 of them ask only in ways this field can carry. Counting the 97
// that do not by the first reason each hits — 61 want a kind the API does not
// have, 17 name one value twice, 15 build a string around a credential, and 4
// use a variable name the API refuses.
func plannedSecrets(p catalog.Plan) ([]platformv1alpha1.GeneratedSecret, []string) {
	var out []platformv1alpha1.GeneratedSecret
	var refusals []string
	claimed := map[string]string{}

	for _, secret := range p.Secrets {
		for _, key := range secret.Keys {
			// A value that is exactly one source and nothing else. Anything
			// longer — postgres://${USER}:${PASSWORD}@db/x — needs a
			// substitution, and the field has no template: the operator would
			// mint a password and hand it over as the whole URL.
			if len(key.Sources) != 1 || key.Template != "${"+key.Sources[0].Name+"}" {
				refusals = append(refusals, fmt.Sprintf(
					"%s is built around %d generated value(s) rather than being one, and the "+
						"platform mints values rather than assembling strings: %s",
					key.Name, len(key.Sources), key.Template))
				continue
			}
			src := key.Sources[0]

			// One source, two keys. The template means them to be the same
			// value and the field would mint two — which is the n8n failure
			// the converter already measured: the task runner gets a token the
			// broker does not accept, and it reads as a bug in the runner.
			if first, taken := claimed[src.Name]; taken {
				refusals = append(refusals, fmt.Sprintf(
					"%s and %s are both %s and have to receive the same value; the platform "+
						"mints one value per variable, so they would receive two",
					first, key.Name, src.Name))
				continue
			}

			kind, ok := generatedKind(src.Kind)
			if !ok {
				refusals = append(refusals, fmt.Sprintf(
					"%s asks for a %q value, and the platform makes passwords, hex and base64",
					key.Name, src.Kind))
				continue
			}
			if !envName.MatchString(key.Name) || len(key.Name) > 64 {
				refusals = append(refusals, fmt.Sprintf(
					"%s is not a name the Workload API accepts for a generated value; it takes "+
						"an upper-case environment variable name", key.Name))
				continue
			}

			claimed[src.Name] = key.Name
			out = append(out, platformv1alpha1.GeneratedSecret{Name: key.Name, Kind: kind})
		}
	}

	if len(out) > maxGeneratedSecrets {
		refusals = append(refusals, fmt.Sprintf(
			"this template asks for %d generated values and a workload holds %d",
			len(out), maxGeneratedSecrets))
	}
	return out, refusals
}

// generatedKind maps the converter's own spelling onto the API's three.
//
// "user" and "unknown" are deliberately not mapped. A username is not a
// password with a different label — it is read back by a person and typed into
// a form — and minting one from the password generator would produce an account
// name nobody can use. Refusing says so; mapping it would hide it.
//
// It is also the largest thing standing between this endpoint and the rest of
// the catalogue: 61 entries are refused for this reason first, more than the
// other three limits together. What would fix it is a fourth kind on the
// Workload API, not a mapping here.
func generatedKind(kind string) (platformv1alpha1.GeneratedKind, bool) {
	switch kind {
	case "password":
		return platformv1alpha1.GeneratedPassword, true
	case "hex":
		return platformv1alpha1.GeneratedHex, true
	case "base64":
		return platformv1alpha1.GeneratedBase64, true
	default:
		return "", false
	}
}

// whyRefused is every reason this platform will not install this plan, in the
// order a person would want to read them.
//
// Three refusals, and only the first belongs to the template. The other two are
// this platform's own limits, which is why they are spelled out rather than
// folded into "cannot be installed": a user who is told n8n is unsupported will
// go and look for a different n8n, and a user who is told the write path holds
// one workload per environment has been told something true.
//
// Measured by running this function over the upstream corpus of 341 offered
// entries: 59 install as they stand, and 146 once images resolve to digests —
// Options.Pin is the difference and it is empty. 117 of the 341 are stopped by
// the object count alone, which is the write path's limit rather than the
// converter's.
func whyRefused(p catalog.Plan) []string {
	var out []string
	for _, b := range p.Blockers {
		out = append(out, b.String())
	}

	_, unmintable := plannedSecrets(p)
	out = append(out, unmintable...)

	if objects := len(p.Workloads) + len(p.Databases); objects != 1 || len(p.Workloads) != 1 {
		// The limit is the write path's, not the converter's: a placement owns
		// one directory holding one workload.yaml, and every later deploy
		// renders that one file. Committing the rest beside it would install
		// something that runs and is then managed by half — the next deploy
		// would update one service of six, and nothing would say so.
		out = append(out, fmt.Sprintf(
			"this template becomes %d objects (%d workloads, %d databases) and an app "+
				"environment holds one workload, so the others would be committed once and "+
				"never updated again",
			objects, len(p.Workloads), len(p.Databases)))
	}
	return out
}

func toWirePlan(p catalog.Plan, refusals []string) wirePlan {
	out := wirePlan{
		Entry:       toWireEntry(p.Entry),
		Namespace:   p.Namespace,
		Refusals:    refusals,
		Installable: len(refusals) == 0,
	}
	for _, w := range p.Workloads {
		out.Workloads = append(out.Workloads, w.Name)
	}
	for _, d := range p.Databases {
		out.Databases = append(out.Databases, d.Name)
	}
	for _, s := range p.Mint {
		out.Generated = append(out.Generated, s.Name)
	}
	for _, n := range p.Notes {
		out.Notes = append(out.Notes, n.String())
	}
	return out
}

// renderInstall writes the entry's single workload as the app's manifest.
//
// It refuses to write over anything. Every other render in this package is a
// read-modify-write of what is committed, because a deploy is "the same app, a
// new build"; an install is the opposite claim — that this app is new — and a
// directory that already holds a manifest is one where that claim is false.
func renderInstall(
	place placement.Placement, plan catalog.Plan,
	secrets []platformv1alpha1.GeneratedSecret,
) renderFunc {
	return func(rolloutID string, current map[string][]byte) (map[string][]byte, error) {
		if _, taken := current[manifest.File]; taken {
			return nil, fmt.Errorf(
				"%s already holds a committed manifest; installing over it would replace an "+
					"app that exists", place.Path)
		}

		app := plan.Workloads[0]
		// Identity comes from the placement and never from the template. The
		// template's own name is the compose service — "app", "web", "n8n" —
		// and a manifest naming something the control plane does not know is
		// one nothing can later deploy to.
		app.ObjectMeta = metav1.ObjectMeta{
			Name:      place.App,
			Namespace: place.Namespace,
			// Kept, because they record which compose service this came from,
			// which is the only surviving link between a running object and
			// the catalogue entry it was installed from.
			Annotations: app.Annotations,
		}

		// The values the template asked the platform to invent, as a request
		// rather than as values: the manifest that carries this is committed,
		// and a credential in a commit is a credential for ever.
		app.Spec.Secrets = secrets

		// And the Secret the plan pointed at is dropped, because nothing
		// creates it. catalog.Plan names one per workload for a caller that
		// writes Secrets itself; the operator writes its own, called
		// <app>-generated, and injects it. Leaving the plan's name here would
		// be an envFrom naming a Secret that does not exist, which is a pod
		// that never starts and an event nobody is watching.
		app.Spec.EnvFrom = withoutPlannedSecrets(app.Spec.EnvFrom, plan)

		body, err := manifest.Render(app, rolloutID)
		if err != nil {
			return nil, err
		}
		return map[string][]byte{manifest.File: body}, nil
	}
}

// withoutPlannedSecrets removes the Secret names the plan invented for a caller
// that would have written them itself.
func withoutPlannedSecrets(envFrom []string, plan catalog.Plan) []string {
	if len(envFrom) == 0 || len(plan.Secrets) == 0 {
		return envFrom
	}
	planned := make(map[string]bool, len(plan.Secrets))
	for _, s := range plan.Secrets {
		planned[s.Name] = true
	}
	kept := make([]string, 0, len(envFrom))
	for _, name := range envFrom {
		if !planned[name] {
			kept = append(kept, name)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// requireCatalog answers 503 when this install has no templates mounted.
//
// Not an empty list. An empty list is indistinguishable from a filter that
// matched nothing, and the difference — whether anybody has mounted the
// catalogue at all — is a question about the installation that the person
// looking at the page cannot answer. The flag is named for the same reason.
func requireCatalog(w http.ResponseWriter, st stores) (CatalogSource, bool) {
	if st.catalog == nil {
		problem(w, http.StatusServiceUnavailable,
			"this installation has no catalogue: no templates are mounted and -catalog-dir is unset")
		return nil, false
	}
	return st.catalog, true
}

// The loaded catalogue satisfies the interface the handlers take. Asserted here
// rather than discovered at the one call site that wires them together.
var _ CatalogSource = (*catalog.Catalog)(nil)
