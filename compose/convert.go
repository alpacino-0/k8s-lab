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

	// Generated names the environment variables the template expects the
	// platform to invent — a password, a user, an API key. They are returned
	// rather than filled in because a value written here would be a credential
	// this package chose, and the caller is the only thing that knows where
	// credentials are allowed to live.
	Generated []Generated

	// Notes is what did not convert. Never empty for a non-trivial template,
	// and the reason this type exists: a conversion that reports nothing is
	// indistinguishable from one that understood everything.
	Notes []Note
}

// Generated is one value the platform has to produce for the workload to start.
type Generated struct {
	Service string
	Key     string // the environment variable name
	Kind    string // "password" | "user" | "hex" | "base64" | "unknown"
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
	healthURL = regexp.MustCompile(`https?://[^/\s"']+(/[^\s"']*)`)
	notDNS    = regexp.MustCompile(`[^a-z0-9-]+`)
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

	for _, name := range names {
		svc := t.Services[name]
		if db, ok := asDatabase(t, name, svc, o); ok {
			res.Databases = append(res.Databases, db)
			res.Notes = append(res.Notes, Note{
				Service: name, Field: "image",
				Detail: "converted to a Database, which brings backups and the restore " +
					"rehearsal; its compose settings other than the image are not carried over",
			})
			continue
		}
		w, notes, gen := asWorkload(t, name, svc, o, name == primary)
		res.Workloads = append(res.Workloads, w)
		res.Notes = append(res.Notes, notes...)
		res.Generated = append(res.Generated, gen...)
	}

	if len(res.Workloads) == 0 {
		return Result{}, fmt.Errorf("compose: %s: every service is a database; nothing to run", t.Name)
	}
	return res, nil
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
func asDatabase(t Template, name string, svc Service, o Options) (platformv1alpha1.Database, bool) {
	repo := imageRepo(svc.Image)
	matched := false
	for _, prefix := range databaseImages {
		if repo == prefix || strings.HasSuffix(repo, "/"+prefix) {
			matched = true
			break
		}
	}
	if !matched {
		return platformv1alpha1.Database{}, false
	}
	db := platformv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectName(t.Name + "-" + name),
			Namespace: o.Namespace,
			Labels:    map[string]string{managedByCompose: t.Name},
		},
		Spec: platformv1alpha1.DatabaseSpec{
			Image:   svc.Image,
			Storage: o.VolumeSize,
		},
	}
	if v, ok := svc.Environment["POSTGRES_DB"]; ok {
		db.Spec.Database = literal(v)
	}
	if v, ok := svc.Environment["POSTGRES_USER"]; ok {
		db.Spec.Username = literal(v)
	}
	return db, true
}

// asWorkload is the main conversion, and most of it is deciding what to say
// about the parts that do not fit.
func asWorkload(
	t Template, name string, svc Service, o Options, isPrimary bool,
) (platformv1alpha1.Workload, []Note, []Generated) {
	var notes []Note
	var generated []Generated
	note := func(field, detail string) { notes = append(notes, Note{Service: name, Field: field, Detail: detail}) }

	w := platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectName(t.Name + "-" + name),
			Namespace: o.Namespace,
			Labels:    map[string]string{managedByCompose: t.Name},
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
		switch {
		case magicVar.MatchString(k) && v == "":
			// The bare form: the key itself is the request. Coolify writes
			// `- SERVICE_PASSWORD_N8N` with no value at all.
			generated = append(generated, Generated{Service: name, Key: k, Kind: magicKind(k)})
			continue
		}
		if ref := magicVar.FindString(strings.Trim(literalRefs(v), "${}")); ref != "" {
			generated = append(generated, Generated{Service: name, Key: k, Kind: magicKind(ref)})
			continue
		}
		if urlVar.MatchString(strings.Trim(literalRefs(v), "${}")) || urlVar.MatchString(k) {
			note(k, "expects the platform to supply its own public URL; set it once the domain is known")
			continue
		}
		w.Spec.Env = append(w.Spec.Env, platformv1alpha1.EnvVar{Name: k, Value: literal(v)})
	}

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

// literal resolves ${VAR:-default} to its default, which is the only value
// available without an environment to read.
func literal(v string) string {
	return interpolation.ReplaceAllStringFunc(v, func(m string) string {
		parts := interpolation.FindStringSubmatch(m)
		return parts[2]
	})
}

// literalRefs returns the variable a value refers to, if it is only a
// reference. `${SERVICE_PASSWORD_N8N}` is a request; `pg://x:${...}@y` is not,
// and treating it as one would drop a connection string.
func literalRefs(v string) string {
	m := interpolation.FindStringSubmatch(v)
	if m == nil || m[0] != v {
		return ""
	}
	return m[1]
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
