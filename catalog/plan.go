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

package catalog

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/compose"
)

// SecretSuffix is appended to a workload's name to name the Secret holding the
// values the platform had to invent for it.
//
// One Secret per workload rather than one per install, and the reason is not
// tidiness. Two services in one template can want the same variable name with
// different values — a template with two components each holding
// ${SERVICE_PASSWORD_A} and ${SERVICE_PASSWORD_B} under the key PASSWORD — and
// a single Secret has one key of that name. The install would succeed, one of
// the two would silently get the other's value, and the symptom would appear in
// the component that was configured correctly.
//
// It costs a copy of a shared value in two Secrets. That is only safe because
// the value is minted once per source and written into each — see Plan.Mint.
const SecretSuffix = "-env"

// Options are the decisions the template does not carry.
type Options struct {
	// Namespace every produced object lands in. Required.
	Namespace string

	// Domain publishes the entry's front door. Empty keeps it cluster-internal.
	Domain string

	// VolumeSize is what each named volume is claimed at. Compose declares no
	// size, so this is a number somebody has to choose; compose.Convert
	// defaults it and says so in a Note for every volume.
	VolumeSize resource.Quantity

	// Pin resolves an image reference to one the Workload API accepts, in
	// practice by reading a digest from the registry.
	//
	// A seam rather than a policy, and left empty on purpose until the decision
	// behind it is made. Measured against the upstream corpus: 193 of the 341
	// entries that are offered name at least one image with no tag or with
	// :latest, and with nothing in this position 119 of them install as they
	// stand against 280 with it. What goes here — a registry client, a mirror,
	// a lockfile committed beside the templates — does not change this package.
	//
	// An error is a Blocker naming the image, not a failed plan: one
	// unresolvable image should say which one, rather than making the whole
	// entry disappear.
	Pin func(image string) (string, error)

	// Ports reports the TCP ports an image says it listens on, for a service
	// whose compose file declared none.
	//
	// A seam for the same reason Pin is, and it closes the same kind of hole
	// from the other end. Compose leaves Workload.Spec.Port unset when nothing
	// declared a port; the CRD then defaults it to 8080 and the operator puts
	// an HTTP probe there. Measured on a cluster on 2026-09-01: a mongo
	// listening on 27017 never became Ready — "dial tcp 10.244.1.3:8080:
	// connect: connection refused" — its Service published no endpoints, and
	// the application that named it could not connect. The entry was offered as
	// installable throughout, and the converter's own note said the default
	// "is probably wrong".
	//
	// Empty means nothing asks. An entry with a service that declared no port
	// is then blocked rather than guessed at: see resolvePorts.
	Ports func(image string) ([]int32, error)
}

// Plan is everything installing one entry produces, and every reason it should
// not be installed.
type Plan struct {
	Entry     Entry
	Namespace string

	Workloads []platformv1alpha1.Workload
	Databases []platformv1alpha1.Database

	// Primary indexes Workloads at the entry's front door — the object that
	// becomes the app, taking the placement's name and the fixed filename a
	// later deploy addresses.
	//
	// Carried through from the conversion rather than worked out again here.
	// Compose has no notion of a primary service, so this is the converter's
	// guess and the only informed one available: it is the service the
	// template's own port belongs to. What it replaces is worse than a guess —
	// the caller used to request a placeholder domain and read back which
	// workload it was attached to, which made an API gap into a mechanism.
	Primary int

	// Secrets are the values the platform has to invent, arranged by the
	// workload that reads them. Keys carry a template and never a value: see
	// Key.
	Secrets []Secret

	// Mint is every value that has to be produced, once each, in name order.
	//
	// The deduplication is the point of this field. A source names a value, and
	// two keys naming one source have to receive the same one — across services
	// as well as within them. Minting per key instead is a plan that installs
	// cleanly and does not work, and it is the common case rather than the
	// exotic one. Counted, and the count is computed on every run rather than
	// written here: see compose/corpus_test.go, which logs how many of the
	// templates that parse share a source between two services and how many do
	// so only once a service that became a Database is counted as one of them.
	// The two numbers differ, and a single figure in this position used to mean
	// both.
	Mint []compose.Source

	// Notes is what did not convert, for a person to read.
	Notes []compose.Note

	// Blockers are facts about the objects, not advice. A plan with any is not
	// installable and the caller is expected to refuse rather than to warn.
	Blockers []Blocker
}

// Secret is one Secret to create, described rather than built.
//
// There is no corev1.Secret here and that is deliberate. Filling one in means
// holding the values, and this package does not have them and must not: see the
// package comment.
type Secret struct {
	Name      string
	Namespace string

	// Keys are in name order, so the same entry plans to the same bytes twice.
	Keys []Key
}

// Key is one entry in a Secret.
type Key struct {
	// Name is the environment variable the container reads.
	Name string

	// Template is the value with each source left in place as ${NAME}.
	// Substituting the minted values into it is the last step and the only one
	// that touches a credential.
	Template string

	// Sources are the values that have to be substituted into Template.
	Sources []compose.Source
}

// Blocker is a reason this entry cannot be installed as it stands.
type Blocker struct {
	// Object is the workload or database it concerns, empty for the entry as a
	// whole.
	Object string
	Field  string
	Detail string
}

func (b Blocker) String() string {
	if b.Object == "" {
		return fmt.Sprintf("%s: %s", b.Field, b.Detail)
	}
	return fmt.Sprintf("%s.%s: %s", b.Object, b.Field, b.Detail)
}

// Installable says whether the plan can be applied.
func (p Plan) Installable() bool { return len(p.Blockers) == 0 }

// Plan works out what installing one entry would produce.
//
// It returns an error only when there is no plan to describe — an unknown entry
// or a template that does not convert at all. Everything else that is wrong
// with the result is in Blockers, because a caller needs to be able to show a
// user why an entry is greyed out.
func (c *Catalog) Plan(name string, o Options) (Plan, error) {
	entry, ok := c.Get(name)
	if !ok {
		return Plan{}, fmt.Errorf("catalog: no entry named %q", name)
	}
	if o.Namespace == "" {
		return Plan{}, fmt.Errorf("catalog: a namespace is required")
	}

	res, err := compose.Convert(entry.template, compose.Options{
		Namespace:  o.Namespace,
		Domain:     o.Domain,
		VolumeSize: o.VolumeSize,
	})
	if err != nil {
		return Plan{}, fmt.Errorf("catalog: %s: %w", name, err)
	}

	p := Plan{
		Entry:     entry,
		Namespace: o.Namespace,
		Workloads: res.Workloads,
		Databases: res.Databases,
		Primary:   res.Primary,
		Notes:     res.Notes,
	}
	p.attachSecrets(res)
	p.pinImages(o.Pin)
	p.resolvePorts(o.Ports)
	return p, nil
}

// attachSecrets turns the conversion's requests into one Secret per workload
// that needs one, and points that workload at it.
func (p *Plan) attachSecrets(res compose.Result) {
	// The compose service name is what a request is keyed by; the object's name
	// is derived from it and the derivation does not run backwards, which is
	// what the annotation is for.
	byService := map[string][]compose.Generated{}
	for _, g := range res.Generated {
		byService[g.Service] = append(byService[g.Service], g)
	}

	mint := map[string]compose.Source{}
	for i := range p.Workloads {
		w := &p.Workloads[i]
		asked := byService[w.Annotations[compose.ServiceAnnotation]]
		if len(asked) == 0 {
			continue
		}

		secret := Secret{Name: w.Name + SecretSuffix, Namespace: w.Namespace}
		for _, g := range asked {
			if owned := ownedByDatabase(g.Sources, res.DatabaseSources); len(owned) > 0 {
				// The Database publishes its own credentials under its own
				// names. Minting a second value here would hand the
				// application a password the server does not have.
				p.Blockers = append(p.Blockers, Blocker{
					Object: w.Name, Field: g.Key,
					Detail: fmt.Sprintf(
						"is built from %s, which belong to the database this template runs. "+
							"The platform publishes those as POSTGRES_USER, POSTGRES_PASSWORD, "+
							"POSTGRES_DB, DB_HOST and DB_PORT, and cannot assemble them into "+
							"one string for an application that only reads one",
						strings.Join(owned, " and ")),
				})
				continue
			}
			secret.Keys = append(secret.Keys, Key{Name: g.Key, Template: g.Value, Sources: g.Sources})
			for _, s := range g.Sources {
				mint[s.Name] = s
			}
		}
		if len(secret.Keys) == 0 {
			continue
		}
		slices.SortFunc(secret.Keys, func(a, b Key) int { return strings.Compare(a.Name, b.Name) })
		p.Secrets = append(p.Secrets, secret)
		w.Spec.EnvFrom = append(w.Spec.EnvFrom, secret.Name)
	}

	p.Mint = make([]compose.Source, 0, len(mint))
	for _, s := range mint {
		p.Mint = append(p.Mint, s)
	}
	slices.SortFunc(p.Mint, func(a, b compose.Source) int { return strings.Compare(a.Name, b.Name) })
}

func ownedByDatabase(want, owned []compose.Source) []string {
	var out []string
	for _, w := range want {
		if slices.ContainsFunc(owned, func(o compose.Source) bool { return o.Name == w.Name }) {
			out = append(out, w.Name)
		}
	}
	return out
}

// pinImages resolves the images the Workload API would refuse, and blocks the
// ones that are still refused afterwards.
//
// The rule is restated here rather than left to admission. An entry whose image
// the API rejects is an entry that is offered, clicked, committed, and then
// fails somewhere the user is not looking — and the catalogue knew before the
// click.
//
// Only the refused ones, and that is the whole shape of it. This ran Pin over
// every image first, which made installing anything depend on a registry
// answering: an entry naming an explicit tag installs today with Pin empty, and
// with a resolver in place it stopped installing whenever the registry was
// unreachable or rate limiting. Measured against the upstream corpus of 371
// files, 341 offered, 119 install with Pin empty — those 119 are exactly the
// ones a resolver must not be able to take away.
//
// The cost is that an explicit tag is left as a tag. `postgres:16.1` is
// something the API accepts and something that can still be re-pushed, so this
// is a rescue for references the platform would otherwise refuse rather than a
// provenance pass over all of them. Pinning everything is a stronger claim and
// a different decision; it is not this one.
// fieldPort names the blocker field the four refusals below share.
const fieldPort = "port"

// resolvePorts fills in the port for a workload whose compose file declared
// none, and blocks the entry when it cannot.
//
// Blocking is the point. Before this, an unset port became the CRD's 8080, the
// operator probed http://:8080/healthz, and a service listening anywhere else
// never became Ready — so the entry installed, reported itself installable, and
// did not work. The platform was guessing and saying so in a note nobody could
// act on.
//
// Exactly one distinct port is an answer. Several is not: choosing between 80
// and 443 is the same guess with better odds, and this exists to stop guessing.
// Both cases name what was found, because "this image exposes two ports and the
// platform cannot tell which one your application serves" is something a person
// can resolve and "not installable" alone is not.
func (p *Plan) resolvePorts(ports func(string) ([]int32, error)) {
	for i := range p.Workloads {
		w := &p.Workloads[i]
		if w.Spec.Port != 0 {
			continue
		}
		if ports == nil {
			p.Blockers = append(p.Blockers, Blocker{
				Object: w.Name, Field: fieldPort,
				Detail: "no port is declared anywhere in the template and this installation " +
					"cannot ask the registry, so the port it listens on is not known",
			})
			continue
		}
		found, err := ports(w.Spec.Image)
		switch {
		case err != nil:
			p.Blockers = append(p.Blockers, Blocker{
				Object: w.Name, Field: fieldPort,
				Detail: fmt.Sprintf("no port is declared anywhere in the template and %s "+
					"could not be asked: %v", w.Spec.Image, err),
			})
		case len(found) == 0:
			p.Blockers = append(p.Blockers, Blocker{
				Object: w.Name, Field: fieldPort,
				Detail: fmt.Sprintf("no port is declared anywhere in the template and %s "+
					"exposes none, so nothing says what it listens on", w.Spec.Image),
			})
		case len(found) > 1:
			p.Blockers = append(p.Blockers, Blocker{
				Object: w.Name, Field: fieldPort,
				Detail: fmt.Sprintf("no port is declared anywhere in the template and %s "+
					"exposes %v, so which one serves this application is a guess", w.Spec.Image, found),
			})
		default:
			w.Spec.Port = found[0]
		}
	}
}

func (p *Plan) pinImages(pin func(string) (string, error)) {
	images := make([]*string, 0, len(p.Workloads)+len(p.Databases))
	objects := make([]string, 0, cap(images))
	for i := range p.Workloads {
		images = append(images, &p.Workloads[i].Spec.Image)
		objects = append(objects, p.Workloads[i].Name)
	}
	for i := range p.Databases {
		images = append(images, &p.Databases[i].Spec.Image)
		objects = append(objects, p.Databases[i].Name)
	}

	refuse := func(i int, detail string) {
		p.Blockers = append(p.Blockers, Blocker{
			Object: objects[i], Field: "image", Detail: detail,
		})
	}
	for i, image := range images {
		reason, ok := refusedImage(*image)
		if ok {
			// Already something the API takes. Left alone on purpose; see above.
			continue
		}
		refusal := fmt.Sprintf("%s %s, and the API refuses it: a rollback to a tag that moved "+
			"restores something other than what was rolled back from", *image, reason)
		if pin == nil {
			refuse(i, refusal)
			continue
		}

		resolved, err := pin(*image)
		if err != nil {
			// The registry's own words, because they are the difference
			// between an image nobody published, a rate limit that goes away
			// by waiting, and a repository that needs a credential.
			refuse(i, fmt.Sprintf("%s could not be resolved: %v", *image, err))
			continue
		}
		*image = resolved
		if _, ok := refusedImage(*image); !ok {
			// A resolver that answered without fixing anything — one that
			// hands the reference back unchanged, say. Blocked rather than
			// trusted: the check is on the value that would be committed.
			refuse(i, refusal)
		}
	}
}

// refusedImage mirrors the two rules on WorkloadSpec.Image.
//
// The tag has to be looked for in the last path segment: the colon in
// `registry.local:5000/team-a/app` is a port, there is no tag at all, and a
// check against the whole string would let through precisely what the rule
// exists to forbid.
func refusedImage(image string) (string, bool) {
	if strings.HasSuffix(image, ":latest") {
		return "uses the :latest tag", false
	}
	if strings.Contains(image, "@") {
		return "", true
	}
	segments := strings.Split(image, "/")
	if !strings.Contains(segments[len(segments)-1], ":") {
		return "carries no tag or digest", false
	}
	return "", true
}
