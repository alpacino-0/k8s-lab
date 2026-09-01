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

package compose

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// Result is everything one template becomes, and everything it could not.
type Result struct {
	Workloads []platformv1alpha1.Workload
	Databases []platformv1alpha1.Database

	// Primary indexes Workloads at the service this template puts in front:
	// the one carrying the template's published port, or the only one, or the
	// first alphabetically. See primaryService for how it is decided and why
	// that is a guess.
	//
	// Reported rather than left to be inferred, and that is the whole reason
	// this field exists. The answer was computed here and thrown away, and the
	// caller that needed it recovered it by asking for a placeholder domain and
	// seeing which workload the domain landed on — reading an answer out of a
	// side effect. Taking the first workload instead would be wrong for a
	// measured fraction of the corpus: kibana rather than elasticsearch, the
	// proxy rather than the backend. catalog/corpus_test.go computes how many.
	//
	// An index and not a name, because the caller's question is which element
	// of Workloads is the app — it takes the placement's name and the fixed
	// filename every later deploy reads, and its own name is replaced in the
	// process. Always valid: Convert refuses a template with no workloads at
	// all, and when the front-door service became a Database this is 0.
	Primary int

	// Generated names the environment variables the template expects the
	// platform to invent — a password, a user, an API key. They are returned
	// rather than filled in because a value written here would be a credential
	// this package chose, and the caller is the only thing that knows where
	// credentials are allowed to live.
	Generated []Generated

	// DatabaseSources are the generated variables declared by a service that
	// became a Database.
	//
	// Named separately because they must not be minted. A Database publishes
	// its own credentials in its own Secret, and a second value produced under
	// the same name is a password the application holds and the server has
	// never heard of — which fails at the first connection and looks like a
	// configuration mistake in the application.
	//
	// Anything else naming one of these is asking for a value it cannot be
	// given. 109 of the 369 templates that parse convert a service into a
	// Database — computed, with the rest, by
	// TestTheCorpusCountsBehindTheDocComments — and this is how a caller tells
	// that case apart from a credential it may freely invent.
	DatabaseSources []Source

	// Notes is what did not convert. Never empty for a non-trivial template,
	// and the reason this type exists: a conversion that reports nothing is
	// indistinguishable from one that understood everything.
	Notes []Note
}

// Generated is one environment variable the platform has to fill in.
//
// It is a request and not a value: this package mints nothing. What it carries
// is enough for whatever does to produce the same variable twice and get the
// same answer, which is the part that is easy to get wrong — see Sources.
type Generated struct {
	Service string

	// Key is the environment variable the container reads.
	Key string

	// Sources are the variables whose values have to be minted, in the order
	// they appear in Value and without repeats.
	//
	// The name is what identifies a value, not the key. Two keys naming one
	// source must receive one value: n8n declares N8N_RUNNERS_AUTH_TOKEN twice,
	// once in each of its two services, both as ${SERVICE_PASSWORD_N8N}, and a
	// caller that mints per key gives the task runner a token the broker does
	// not accept. 64 of the 369 templates that parse share a source between two
	// services this way.
	//
	// This said 111 until the count was computed rather than remembered, and
	// 111 is still a true number about the same corpus — it is what you get
	// once a service that became a Database is counted as one of the two, and
	// those are in DatabaseSources below rather than here, because their value
	// must not be minted at all. The two fields split a set that one sentence
	// used to describe. Both counts are computed by
	// TestTheCorpusCountsBehindTheDocComments.
	Sources []Source

	// Value is what Key becomes once every source has a value: the template's
	// text with each source left in place as ${NAME} and everything else
	// already resolved.
	//
	// Usually just "${NAME}". It is a whole string for the 51 templates that
	// build one around a credential — counted with the rest by
	// TestTheCorpusCountsBehindTheDocComments — a connection string is a user and a
	// password inside a URL — and those are the reason this is a template
	// rather than a bare source name. Resolving them to their defaults, which
	// is what happens to every other interpolation here, writes the empty
	// string where the password goes and produces a workload that starts,
	// serves, and cannot authenticate.
	Value string
}

// Source is one value to mint, and what kind of value it is.
type Source struct {
	Name string // SERVICE_PASSWORD_N8N — the identity of the value
	Kind string // "password" | "user" | "hex" | "base64" | "unknown"
}

// Note is one thing compose said that this platform cannot say back.
type Note struct {
	Service string
	Field   string
	Detail  string
}

func (n Note) String() string {
	if n.Service == "" {
		return fmt.Sprintf("%s: %s", n.Field, n.Detail)
	}
	return fmt.Sprintf("%s.%s: %s", n.Service, n.Field, n.Detail)
}

// Options are the decisions compose does not carry and somebody has to make.
type Options struct {
	// Namespace every produced object lands in.
	Namespace string

	// VolumeSize is what a named volume is claimed at.
	//
	// Invented, and that is the point of it being a field: compose has no size
	// at all — a Docker volume grows into the host's disk — so every converted
	// volume is a number this platform chose. A note says so for each one,
	// because a claim cannot be shrunk afterwards.
	VolumeSize resource.Quantity

	// Domain, if the app should be published. Empty keeps it cluster-internal.
	Domain string
}

// databaseImages are the images this platform runs as a Database rather than as
// a Workload. Prefix match on the repository, so `postgres:16-alpine` and
// `docker.io/library/postgres:16` both land.
var databaseImages = []string{"postgres", "postgis/postgis", "pgvector/pgvector"}

// managedByCompose marks what came from a conversion, so a later import can
// tell what it is allowed to replace.
const managedByCompose = "damga.co/from-compose"

// ServiceAnnotation records which compose service an object came from.
//
// An annotation and not a label because a compose service name is free text and
// a label value is not. It exists because the object's name is derived — the
// template's name, the service's, and a DNS cleanup — and the derivation cannot
// be run backwards. Anything holding a Generated, whose Service is the compose
// name, needs this to find the object it belongs to.
const ServiceAnnotation = "damga.co/compose-service"

var (
	// SERVICE_PASSWORD_FOO, SERVICE_USER_FOO — Coolify's convention for "the
	// platform generates this". Recognised rather than passed through: sending
	// the literal string SERVICE_PASSWORD_FOO as a password is a working
	// deployment with a known credential.
	magicVar = regexp.MustCompile(`^SERVICE_(PASSWORD|USER|BASE64|REALBASE64|HEX|KEY|ENCRYPTION|ROLE)(_[A-Z0-9_]+)?$`)
	// SERVICE_URL_FOO / SERVICE_FQDN_FOO — where the app will be reachable.
	// Supplied by the platform once a domain is known.
	urlVar = regexp.MustCompile(`^SERVICE_(URL|FQDN)(_[A-Z0-9_]+)?$`)
	// ${VAR:-default} and ${VAR}
	interpolation = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}`)
	// A healthcheck that is an HTTP GET, which is the only kind a probe path
	// can be recovered from.
	healthURL  = regexp.MustCompile(`https?://[^/\s"']+(/[^\s"']*)`)
	notDNS     = regexp.MustCompile(`[^a-z0-9-]+`)
	notPGIdent = regexp.MustCompile(`[^a-z0-9_]+`)
)

// Convert turns one template into the objects that run it.
func Convert(t Template, o Options) (Result, error) {
	if o.Namespace == "" {
		return Result{}, fmt.Errorf("compose: a namespace is required")
	}
	if o.VolumeSize.IsZero() {
		o.VolumeSize = resource.MustParse("1Gi")
	}

	var res Result

	// Sorted, so the same template converts to the same bytes every time. A
	// converter whose output depends on map order produces a different git
	// commit for an unchanged input, and every diff becomes noise.
	names := make([]string, 0, len(t.Services))
	for name := range t.Services {
		names = append(names, name)
	}
	slices.Sort(names)

	primary := primaryService(t, names)

	front := -1
	for _, name := range names {
		svc := t.Services[name]
		// The image is resolved once, here, so the three places that read it —
		// the database match, the Database's image and the Workload's — cannot
		// disagree about what this service runs.
		if resolved, hollow := imageDefaults(svc.Image); resolved != svc.Image {
			if len(hollow) > 0 {
				res.Notes = append(res.Notes, Note{
					Service: name, Field: "image",
					Detail: fmt.Sprintf("%s names %s, which compose has no value for; "+
						"what is left is not a usable reference",
						svc.Image, strings.Join(hollow, ", ")),
				})
			}
			svc.Image = resolved
		}
		if db, notes, ok := asDatabase(t, name, svc, o); ok {
			res.Databases = append(res.Databases, db)
			res.DatabaseSources = append(res.DatabaseSources, declaredSources(svc)...)
			res.Notes = append(res.Notes, Note{
				Service: name, Field: "image",
				Detail: "converted to a Database, which brings backups and the restore " +
					"rehearsal; its compose settings other than the image are not carried over",
			})
			res.Notes = append(res.Notes, notes...)
			continue
		}
		w, notes, gen := asWorkload(t, name, svc, o, primary)
		if name == primary {
			front = len(res.Workloads)
		}
		res.Workloads = append(res.Workloads, w)
		res.Notes = append(res.Notes, notes...)
		res.Generated = append(res.Generated, gen...)
	}

	if len(res.Workloads) == 0 {
		return Result{}, fmt.Errorf("compose: %s: every service is a database; nothing to run", t.Name)
	}
	if front < 0 {
		// The template's front door is the database itself. Rare, and the
		// remaining services still have to name it.
		front = 0
	}
	res.Primary = front
	linkDatabase(&res, front)
	return res, nil
}

// linkDatabase points one workload at the Database, which is the whole coupling
// between the two: a name, and the Secret the Database publishes.
//
// Without it the conversion produces both objects and nothing joins them — the
// state every one of the 109 templates with a postgres service was in. The app
// then has no credentials at all, which at least fails loudly.
//
// Only when there is exactly one. Compose says nothing about which service
// talks to which database, and a template with two of them is a guess this
// package will not make silently.
func linkDatabase(res *Result, front int) {
	switch len(res.Databases) {
	case 0:
		return
	case 1:
		res.Workloads[front].Spec.Database = res.Databases[0].Name
		res.Notes = append(res.Notes, Note{
			Service: res.Workloads[front].Name, Field: "database",
			Detail: fmt.Sprintf(
				"pointed at %s, so it receives POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB, "+
					"DB_HOST and DB_PORT. A template that builds its own connection string from "+
					"those has to be given one built from these names instead",
				res.Databases[0].Name),
		})
	default:
		names := make([]string, 0, len(res.Databases))
		for _, db := range res.Databases {
			names = append(names, db.Name)
		}
		res.Notes = append(res.Notes, Note{
			Field: "database",
			Detail: "more than one database (" + strings.Join(names, ", ") +
				"); compose does not say which service uses which, so none was attached",
		})
	}
}

// primaryService is the one that gets the domain: the service the template's
// port belongs to, or the only one, or the first alphabetically.
//
// A guess, and noted as one by the caller. Compose has no notion of a primary
// service and a template with six of them does not say which is the front door.
func primaryService(t Template, sorted []string) string {
	if len(sorted) == 1 {
		return sorted[0]
	}
	if t.Port > 0 {
		want := strconv.Itoa(int(t.Port))
		for _, name := range sorted {
			for _, p := range t.Services[name].Ports {
				if strings.Contains(p, want) {
					return name
				}
			}
			if hc := t.Services[name].HealthCheck; hc != nil {
				if strings.Contains(strings.Join(hc.Test, " "), want) {
					return name
				}
			}
		}
	}
	return sorted[0]
}

// asDatabase recognises a service this platform would rather run as a Database.
func asDatabase(t Template, name string, svc Service, o Options) (platformv1alpha1.Database, []Note, bool) {
	repo := imageRepo(svc.Image)
	matched := false
	for _, prefix := range databaseImages {
		if repo == prefix || strings.HasSuffix(repo, "/"+prefix) {
			matched = true
			break
		}
	}
	if !matched {
		return platformv1alpha1.Database{}, nil, false
	}
	var notes []Note
	note := func(field, detail string) { notes = append(notes, Note{Service: name, Field: field, Detail: detail}) }

	db := platformv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:        objectName(t.Name + "-" + name),
			Namespace:   o.Namespace,
			Labels:      map[string]string{managedByCompose: t.Name},
			Annotations: map[string]string{ServiceAnnotation: name},
		},
		Spec: platformv1alpha1.DatabaseSpec{
			Image:   svc.Image,
			Storage: o.VolumeSize,
		},
	}
	// Both fields are PostgreSQL identifiers and the API refuses anything else,
	// so a value that cannot be one is left out and defaulted rather than
	// written. Measured: of the 369 templates that parse, 61 name a database
	// the API would reject — `plausible-db`, because a hyphen is legal in
	// compose and not in an unquoted identifier — and 75 name a user that is a
	// generated variable, which resolved to the empty string.
	db.Spec.Database = identifier(svc.Environment["POSTGRES_DB"], "database", note)
	db.Spec.Username = identifier(svc.Environment["POSTGRES_USER"], "username", note)
	return db, notes, true
}

// declaredSources lists every generated variable a service's environment names,
// in key order and without repeats.
func declaredSources(svc Service) []Source {
	keys := make([]string, 0, len(svc.Environment))
	for k := range svc.Environment {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var out []Source
	add := func(s Source) {
		if !slices.ContainsFunc(out, func(o Source) bool { return o.Name == s.Name }) {
			out = append(out, s)
		}
	}
	for _, k := range keys {
		v := svc.Environment[k]
		if v == "" && magicVar.MatchString(k) {
			add(Source{Name: k, Kind: magicKind(k)})
			continue
		}
		for _, s := range sources(v) {
			add(s)
		}
	}
	return out
}

// identifier turns a compose value into something the Database API accepts, or
// into nothing, which lets the CRD's own default stand.
func identifier(v, field string, note func(field, detail string)) string {
	if v == "" {
		return ""
	}
	if len(sources(v)) > 0 {
		// The platform mints this database's own credentials and publishes them
		// in its Secret. A generated name from the template would be a second
		// answer to a question already answered.
		note(field, "names a generated value; the platform mints this database's credentials itself")
		return ""
	}
	resolved := literal(v)
	fixed := strings.Trim(notPGIdent.ReplaceAllString(strings.ToLower(resolved), "_"), "_")
	if fixed == "" || fixed[0] < 'a' || fixed[0] > 'z' {
		note(field, fmt.Sprintf(
			"%q is not a PostgreSQL identifier and could not be made into one; the default stands", resolved))
		return ""
	}
	if fixed != resolved {
		note(field, fmt.Sprintf("%q is not a PostgreSQL identifier; using %q", resolved, fixed))
	}
	return fixed
}

// asWorkload is the main conversion, and most of it is deciding what to say
// about the parts that do not fit.
func asWorkload(
	t Template, name string, svc Service, o Options, primary string,
) (platformv1alpha1.Workload, []Note, []Generated) {
	// The name and not a bool, because rewriting a sibling address needs to
	// know which service is the application: a reference to that one is the
	// single case that cannot be repointed here. See rewriteSiblings.
	isPrimary := name == primary
	var notes []Note
	var generated []Generated
	note := func(field, detail string) { notes = append(notes, Note{Service: name, Field: field, Detail: detail}) }

	w := platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:        objectName(t.Name + "-" + name),
			Namespace:   o.Namespace,
			Labels:      map[string]string{managedByCompose: t.Name},
			Annotations: map[string]string{ServiceAnnotation: name},
		},
		Spec: platformv1alpha1.WorkloadSpec{Image: svc.Image},
	}
	if len(t.Services) == 1 {
		w.Name = objectName(t.Name)
	}

	// Port. The template's declaration first, then the published port, then the
	// healthcheck. Nothing invents 8080: the CRD defaults to it, and a wrong
	// port produces a Service that resolves and never answers.
	switch {
	case isPrimary && t.Port > 0:
		w.Spec.Port = t.Port
	default:
		if p, ok := containerPort(svc.Ports); ok {
			w.Spec.Port = p
		} else if p, ok := healthPort(svc.HealthCheck); ok {
			w.Spec.Port = p
			note("port", fmt.Sprintf("no published port; %d taken from the healthcheck URL", p))
		} else {
			note("port", "no port declared anywhere; the API's default of 8080 will be used and is probably wrong")
		}
	}

	if isPrimary && o.Domain != "" {
		w.Spec.Domain = o.Domain
	}

	// Environment.
	keys := make([]string, 0, len(svc.Environment))
	for k := range svc.Environment {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := svc.Environment[k]
		if v == "" && magicVar.MatchString(k) {
			// The bare form: the key itself is the request. Coolify writes
			// `- SERVICE_PASSWORD_N8N` with no value at all.
			generated = append(generated, Generated{
				Service: name, Key: k,
				Sources: []Source{{Name: k, Kind: magicKind(k)}},
				Value:   "${" + k + "}",
			})
			continue
		}
		if urlVar.MatchString(k) || refers(v, urlVar) {
			note(k, "expects the platform to supply its own public URL; set it once the domain is known")
			continue
		}
		// Anywhere in the value, not only as the whole of it. Checking for the
		// whole of it was the earlier behaviour and it dropped the 51 templates
		// that put a credential inside a connection string: the reference fell
		// through to literal(), which resolves an interpolation to its default,
		// and `postgres://${SERVICE_USER_PG}:${SERVICE_PASSWORD_PG}@db/x`
		// reached the workload as `postgres://:@db/x`.
		if srcs := sources(v); len(srcs) > 0 {
			generated = append(generated, Generated{
				Service: name, Key: k, Sources: srcs, Value: literalExceptSources(v),
			})
			continue
		}
		w.Spec.Env = append(w.Spec.Env, platformv1alpha1.EnvVar{Name: k, Value: literal(v)})
	}

	notes = append(notes, repointSiblings(t, name, primary, w.Spec.Env, generated)...)

	// Volumes.
	for _, v := range svc.Volumes {
		if v.Bind {
			note("volumes", fmt.Sprintf(
				"%s is a bind mount from the host; there is no host to bind to. "+
					"Put the contents in the image, or mount a volume and write them there",
				v.Source))
			continue
		}
		if v.Target == "" {
			note("volumes", fmt.Sprintf("%s has no target path", v.Source))
			continue
		}
		if len(w.Spec.Volumes) >= 8 {
			note("volumes", fmt.Sprintf("%s dropped: a workload may declare eight volumes", v.Source))
			continue
		}
		w.Spec.Volumes = append(w.Spec.Volumes, platformv1alpha1.Volume{
			Name: objectName(v.Source), Path: v.Target, Size: o.VolumeSize,
		})
		note("volumes", fmt.Sprintf(
			"%s claimed at %s — compose declares no size, so this is a number this platform chose and a claim cannot be shrunk",
			v.Source, o.VolumeSize.String()))
	}

	// Health. Only an HTTP check converts; the rest is a command in a container
	// and this platform has no field for one.
	if hc := svc.HealthCheck; hc != nil {
		if path, ok := httpPath(hc); ok {
			w.Spec.Health.LivenessPath = path
			w.Spec.Health.ReadinessPath = path
		} else {
			note("healthcheck", "not an HTTP request, so no probe path could be taken from it; the API's defaults will be used")
		}
	}

	// Everything compose said that has nowhere to go.
	for _, u := range []struct {
		field   string
		present bool
	}{
		{fieldCommand, len(svc.Command) > 0},
		{fieldEntrypoint, len(svc.Entrypoint) > 0},
		{fieldUser, svc.User != ""},
		{fieldCapAdd, len(svc.CapAdd) > 0},
		{fieldNetworkMode, svc.NetworkMode != ""},
		{fieldSecurityOpt, len(svc.SecurityOpt) > 0},
		{fieldUlimits, len(svc.Ulimits) > 0},
		{fieldDependsOn, len(svc.DependsOn) > 0},
	} {
		if u.present {
			notes = append(notes, Note{Service: name, Field: u.field, Detail: unsupported[u.field]})
		}
	}
	slices.SortFunc(notes, func(a, b Note) int { return strings.Compare(a.String(), b.String()) })

	return w, notes, generated
}

// siblingHost matches a compose service name used as a host: after a scheme or
// the credentials of a URL, or anywhere with a port attached.
//
// Deliberately narrow, and narrowed by a test rather than by review. A value of
// exactly `db` matches everything a host does and is as often a database name —
// the first version of this reported `DB_NAME=db`, and a note that fires on
// those is a note people stop reading. The cost is silence on a bare
// `SOME_HOST=db`, which is the better of the two mistakes.
var siblingHost = regexp.MustCompile(
	`(?://|@)([a-zA-Z0-9][a-zA-Z0-9._-]*)(?::[0-9]+)?(?:[/?]|$)` +
		`|(?:^|=)([a-zA-Z0-9][a-zA-Z0-9._-]*):[0-9]+(?:[/?]|$)`)

// repointSiblings rewrites every address that names another service in this
// template, and says what it did and what it could not.
//
// Compose puts every service on one network under its own name, so `db:5432`
// resolves. Here each service becomes its own object under a derived name —
// the template's, then the service's — and `db` resolves to nothing. The
// application starts, retries, and reports that its database is down. Measured
// on a real cluster on 2026-09-01: cryptgeon's log is "cannot reach redis", and
// grafana-with-postgresql dies on "dial tcp: lookup postgresql ... no such
// host" while its Service is grafana-with-postgresql-postgresql. Every entry
// that brings a Database has this shape.
//
// This wrote a note and changed nothing until 2026-09-01. The note was right
// about every particular — the old name, the new one, the field — which is what
// made rewriting possible without widening anything: the information was
// already in hand.
//
// # The one it still cannot fix
//
// A reference to the template's own primary service. That workload is renamed
// again when it is installed, to the app name the person chose, and this
// package is not told what that will be — server/catalog.go's renderInstall
// picks it. Writing the convert-time name here would produce a value that
// resolves to nothing while looking repaired, which is worse than leaving the
// compose name and saying so. Those keep a note.
//
// What is deliberately NOT done is widening siblingHost. Its comment records
// the measurement: a bare `db` matches everything a host does and is as often a
// database name, and the first version reported `DB_NAME=db`. Rewriting that
// one would put a Service name where a database name belongs — turning an
// application that says its dependency is down into one that connects and
// cannot find its data. Silence on `SOME_HOST=db` is still the better of the
// two mistakes, and it stays silence.
func repointSiblings(
	t Template, self, primary string, env []platformv1alpha1.EnvVar, gen []Generated,
) []Note {
	var (
		notes   []Note
		fixed   = map[string]string{}
		unfixed = map[string]bool{}
	)
	// Env and Generated in one pass over one list of pointers, so a value that
	// carries a credential is repointed exactly like one that does not. A
	// Generated value is a template with ${NAME} left in it; only the host is
	// touched, so the placeholders survive.
	type slot struct {
		field string
		value *string
	}
	slots := make([]slot, 0, len(env)+len(gen))
	for i := range env {
		slots = append(slots, slot{field: env[i].Name, value: &env[i].Value})
	}
	for i := range gen {
		slots = append(slots, slot{field: gen[i].Key, value: &gen[i].Value})
	}

	for _, sl := range slots {
		rewritten, found := rewriteSiblings(t, self, primary, *sl.value)
		*sl.value = rewritten
		for _, f := range found {
			if f.to == "" {
				if !unfixed[f.host] {
					unfixed[f.host] = true
					notes = append(notes, Note{
						Service: self, Field: sl.field,
						Detail: fmt.Sprintf(
							"addresses %q, which is this template's own application. What that "+
								"runs under is chosen when it is installed, not here, so this one "+
								"reference is left as compose wrote it and does not resolve",
							f.host),
					})
				}
				continue
			}
			if _, seen := fixed[f.host]; !seen {
				fixed[f.host] = f.to
				notes = append(notes, Note{
					Service: self, Field: sl.field,
					Detail: fmt.Sprintf(
						"addresses %q, which is what compose calls that service and not what it "+
							"is called here; it has been repointed at %s",
						f.host, f.to),
				})
			}
		}
	}
	return notes
}

// found is one sibling address, and the name it was repointed at. An empty To
// is one that was left alone.
type found struct {
	host string
	to   string
}

// rewriteSiblings replaces the host part of every sibling address in one value.
//
// The host part and nothing else: `redis://redis:6379/0` becomes
// `redis://cryptgeon-redis:6379/0`, with the scheme, the port and the path
// untouched. That is why this works on the submatch offsets rather than on
// strings.ReplaceAll, which would also rewrite a password that happened to
// equal the service name.
func rewriteSiblings(t Template, self, primary, value string) (string, []found) {
	matches := siblingHost.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value, nil
	}
	var (
		out  strings.Builder
		out2 []found
		last int
	)
	for _, m := range matches {
		// One of the two alternatives matched; the other group is -1.
		lo, hi := m[2], m[3]
		if lo < 0 {
			lo, hi = m[4], m[5]
		}
		if lo < 0 {
			continue
		}
		host := value[lo:hi]
		if host == self {
			continue
		}
		if _, isSibling := t.Services[host]; !isSibling {
			continue
		}
		if host == primary {
			out2 = append(out2, found{host: host})
			continue
		}
		to := objectName(t.Name + "-" + host)
		out.WriteString(value[last:lo])
		out.WriteString(to)
		last = hi
		out2 = append(out2, found{host: host, to: to})
	}
	out.WriteString(value[last:])
	return out.String(), out2
}

// unsupported says what each dropped field would have meant, rather than that
// it was dropped. "command is not supported" sends the reader to the source;
// naming the consequence lets them decide whether it mattered.
// The compose fields this platform has no equivalent for, named once so the
// table below and the check above cannot drift.
const (
	fieldCommand     = "command"
	fieldEntrypoint  = "entrypoint"
	fieldUser        = "user"
	fieldCapAdd      = "cap_add"
	fieldNetworkMode = "network_mode"
	fieldSecurityOpt = "security_opt"
	fieldUlimits     = "ulimits"
	fieldDependsOn   = "depends_on"
)

var unsupported = map[string]string{
	fieldCommand: "overrides the image's command; the platform runs images as built. " +
		"Build an image that does what the command did",
	fieldEntrypoint:  "overrides the image's entrypoint; the platform runs images as built",
	fieldUser:        "runs as a specific uid; every pod here runs as a non-root uid the platform chooses",
	fieldCapAdd:      "asks for Linux capabilities; pods here drop all of them and there is no field to keep one",
	fieldNetworkMode: "shares another container's network; each workload gets its own address",
	fieldSecurityOpt: "changes the container's confinement; the platform sets that and offers no override",
	fieldUlimits: "sets process limits; not expressible, and usually a symptom of " +
		"a container that should be built differently",
	fieldDependsOn: "declares start order; Kubernetes restarts until dependencies " +
		"answer instead. Usually harmless, occasionally a slow first start",
}

func magicKind(k string) string {
	switch {
	case strings.HasPrefix(k, "SERVICE_PASSWORD"):
		return "password"
	case strings.HasPrefix(k, "SERVICE_USER"):
		return "user"
	case strings.HasPrefix(k, "SERVICE_HEX"):
		return "hex"
	case strings.Contains(k, "BASE64"):
		return "base64"
	default:
		return "unknown"
	}
}

// imageDefaults resolves the interpolations in an image reference, and reports
// the variables it had no value for.
//
// Compose reads ${VAR:-default} against an environment file; there is none
// here, so the default is the only value there is, which is what literal does
// everywhere else in this package. The image was the one field it was not
// applied to, and the cost was measured: of the 37 images the registry client
// could not resolve across the upstream corpus, 22 were this — references that
// reached the registry with the ${...} still in them. The registry then answers
// "does not exist", which is true of a repository nobody published and reads
// exactly like an image upstream withdrew. Same failure, wrong sentence.
//
// A variable with no default at all resolves to nothing, which is again what
// compose does. What is left — `ghcr.io/x/y:` — is refused downstream by the
// rule that wants a tag, and that is the right end for it: substituting
// "latest" here would invent the moving tag this platform exists to refuse.
func imageDefaults(image string) (resolved string, hollow []string) {
	for _, m := range interpolation.FindAllStringSubmatch(image, -1) {
		if m[2] == "" && !slices.Contains(hollow, m[1]) {
			hollow = append(hollow, m[1])
		}
	}
	return literal(image), hollow
}

// literal resolves ${VAR:-default} to its default, which is the only value
// available without an environment to read.
func literal(v string) string {
	return interpolation.ReplaceAllStringFunc(v, func(m string) string {
		parts := interpolation.FindStringSubmatch(m)
		return parts[2]
	})
}

// refers says whether any interpolation in v names a variable matching re.
//
// Anywhere in the value. The earlier version of this asked whether the value
// was *only* the reference, which is true of `${SERVICE_URL_X}` and false of
// `https://${SERVICE_FQDN_X}/hook` — and the second one is the same request
// wearing a path.
func refers(v string, re *regexp.Regexp) bool {
	for _, m := range interpolation.FindAllStringSubmatch(v, -1) {
		if re.MatchString(m[1]) {
			return true
		}
	}
	return false
}

// sources lists the generated values a single environment value names, in order
// and without repeats.
func sources(v string) []Source {
	var out []Source
	for _, m := range interpolation.FindAllStringSubmatch(v, -1) {
		if !magicVar.MatchString(m[1]) {
			continue
		}
		if slices.ContainsFunc(out, func(s Source) bool { return s.Name == m[1] }) {
			continue
		}
		out = append(out, Source{Name: m[1], Kind: magicKind(m[1])})
	}
	return out
}

// literalExceptSources resolves every interpolation to its default the way
// literal does, except the generated ones, which are left as a bare ${NAME}
// for whoever mints them to substitute.
//
// The default is dropped from those on purpose. `${SERVICE_PASSWORD_X:-hunter2}`
// carries a default that is a published credential, and honouring it would be
// the one outcome worse than an empty password.
func literalExceptSources(v string) string {
	return interpolation.ReplaceAllStringFunc(v, func(m string) string {
		parts := interpolation.FindStringSubmatch(m)
		if magicVar.MatchString(parts[1]) {
			return "${" + parts[1] + "}"
		}
		return parts[2]
	})
}

func imageRepo(image string) string {
	repo, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo
}

func containerPort(ports []string) (int32, bool) {
	for _, p := range ports {
		fields := strings.Split(p, ":")
		// The container port is the last field, whether the form is "8080",
		// "80:8080" or "127.0.0.1:80:8080".
		last := fields[len(fields)-1]
		last, _, _ = strings.Cut(last, "/")
		if n, err := strconv.Atoi(last); err == nil && n > 0 && n < 65536 {
			return int32(n), true
		}
	}
	return 0, false
}

func healthPort(hc *HealthCheck) (int32, bool) {
	if hc == nil {
		return 0, false
	}
	m := healthURL.FindString(strings.Join(hc.Test, " "))
	if m == "" {
		return 0, false
	}
	_, hostport, _ := strings.Cut(m, "//")
	hostport, _, _ = strings.Cut(hostport, "/")
	_, port, ok := strings.Cut(hostport, ":")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

// httpPath recovers a probe path from a healthcheck command.
func httpPath(hc *HealthCheck) (string, bool) {
	m := healthURL.FindStringSubmatch(strings.Join(hc.Test, " "))
	if m == nil || m[1] == "" {
		return "", false
	}
	return m[1], true
}

// objectName turns a compose service or volume name into a DNS label.
func objectName(s string) string {
	s = notDNS.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "app"
	}
	return s
}
