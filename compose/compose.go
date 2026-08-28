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

// Package compose turns a Docker Compose service definition into the resources
// this platform runs.
//
// It exists twice over. A user migrating from Coolify or Dokploy arrives with a
// compose file and should not have to learn what a Deployment is to bring it;
// and the catalogue — the one-click n8n, Ghost, Plausible — is a directory of
// compose files. Both are the same conversion, so it is written once.
//
// # What it refuses to do
//
// Compose describes a process on a host and Kubernetes describes a workload in
// a cluster. Some of the gap is mechanical and some of it does not close, and
// the difference matters more than the coverage: a converter that drops
// `cap_add` silently emits something that looks like the application and is
// not. Everything this package cannot express is returned in Result.Notes
// rather than omitted, and the caller is expected to show them.
package compose

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// Template is one catalogue entry: a compose file plus the metadata that a
// catalogue page needs and compose has nowhere to put.
//
// The metadata lives in leading comments, which is Coolify's convention and the
// reason this parser reads the file twice — once as YAML for the services, once
// as text for the header. Anything else would need a second file per entry.
type Template struct {
	// Name is the catalogue identifier, taken from the filename.
	Name string

	Slogan        string
	Documentation string
	Category      string
	Tags          []string
	Logo          string

	// Port is the port the catalogue entry says to publish. Compose can declare
	// several and this says which one a browser should reach.
	Port int32

	Services map[string]Service
}

// Service is the subset of a compose service this platform can act on.
//
// Fields compose has and this does not are not silently dropped: Parse records
// each one it saw and could not use, so Convert can report them. The list is
// therefore a statement about what is supported today rather than about what
// compose contains.
type Service struct {
	Image       string         `json:"image"`
	Environment StringOrMap    `json:"environment"`
	Volumes     []VolumeSpec   `json:"volumes"`
	Ports       []string       `json:"ports"`
	HealthCheck *HealthCheck   `json:"healthcheck"`
	DependsOn   StringOrMap    `json:"depends_on"`
	Command     StringOrList   `json:"command"`
	Entrypoint  StringOrList   `json:"entrypoint"`
	User        string         `json:"user"`
	CapAdd      []string       `json:"cap_add"`
	NetworkMode string         `json:"network_mode"`
	SecurityOpt []string       `json:"security_opt"`
	Ulimits     map[string]any `json:"ulimits"`
	Restart     string         `json:"restart"`
	Labels      StringOrMap    `json:"labels"`
	Extra       map[string]any `json:"-"`
}

// VolumeSpec is one mount, in either of the two forms compose accepts.
//
// The short one is "source:target[:ro]". The long one is a map with type,
// source and target — which the corpus uses for exactly the case this platform
// has to refuse, a host bind mount, so recognising it is not optional. Found by
// the first run of the tests against the real files.
type VolumeSpec struct {
	Source string
	Target string
	// Bind is a mount from the host filesystem. There is no host.
	Bind bool
}

func (v *VolumeSpec) UnmarshalJSON(b []byte) error {
	var short string
	if err := yaml.Unmarshal(b, &short); err == nil {
		source, target, ok := strings.Cut(short, ":")
		if !ok {
			// An anonymous volume: "/var/lib/app" with no source. Docker
			// invents a name and throws it away with the container, so the
			// honest reading is a mount with nothing behind it.
			return fmt.Errorf("compose: %q has no source", short)
		}
		if t, _, found := strings.Cut(target, ":"); found {
			target = t // a trailing :ro or :rw
		}
		*v = VolumeSpec{
			Source: source, Target: target,
			Bind: strings.HasPrefix(source, "/") || strings.HasPrefix(source, "."),
		}
		return nil
	}
	var long struct {
		Type   string `json:"type"`
		Source string `json:"source"`
		Target string `json:"target"`
	}
	if err := yaml.Unmarshal(b, &long); err != nil {
		return err
	}
	*v = VolumeSpec{
		Source: long.Source, Target: long.Target,
		Bind: long.Type == "bind" || strings.HasPrefix(long.Source, "/") || strings.HasPrefix(long.Source, "."),
	}
	return nil
}

// HealthCheck is compose's, which is a command and not a URL. Converting one is
// guesswork and is treated as such — see httpPath.
type HealthCheck struct {
	Test     StringOrList `json:"test"`
	Interval string       `json:"interval"`
	Timeout  string       `json:"timeout"`
	Retries  int          `json:"retries"`
}

// StringOrList is compose's habit of accepting either form for the same field.
type StringOrList []string

func (s *StringOrList) UnmarshalJSON(b []byte) error {
	var one string
	if err := yaml.Unmarshal(b, &one); err == nil {
		*s = StringOrList{one}
		return nil
	}
	var many []string
	if err := yaml.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// StringOrMap is the other one, and it has more shapes than it looks.
//
// `environment` is a map in most files and a list of KEY=VALUE in the rest;
// both are correct compose. A map's values are not always strings either — YAML
// reads `RETRIES: 10` as a number and `DEBUG: true` as a bool, and a
// map[string]string decode fails on both. And `depends_on` is sometimes a map
// whose values are themselves maps (`{db: {condition: service_healthy}}`),
// where only the keys carry meaning here.
//
// Every one of those is in the corpus this package is tested against, and the
// map-of-maps form is what the first run of those tests found. A fixture
// written by hand would have used the shape the code already handled.
type StringOrMap map[string]string

func (m *StringOrMap) UnmarshalJSON(b []byte) error {
	if as := decodeMap(b); as != nil {
		*m = as
		return nil
	}
	var list []any
	if err := yaml.Unmarshal(b, &list); err != nil {
		// One more shape, and it is not valid compose: a bare scalar where a
		// list belongs — `environment: DOCKER_TLS_CERTDIR=/certs`. Docker
		// Compose itself rejects it. Accepted here because it is in the corpus
		// and refusing the file would lose the other eleven services in it,
		// which serves nobody.
		var one string
		if err2 := yaml.Unmarshal(b, &one); err2 != nil {
			return err
		}
		list = []any{one}
	}
	out := map[string]string{}
	for _, item := range list {
		k, v, _ := strings.Cut(scalar(item), "=")
		out[strings.TrimSpace(k)] = v
	}
	*m = out
	return nil
}

func decodeMap(b []byte) map[string]string {
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		// A nested object means the value is a qualifier rather than a value —
		// depends_on's condition, for instance. The key is what matters and an
		// empty string is the honest reading of "there is no scalar here".
		out[k] = scalar(v)
	}
	return out
}

// scalar renders what YAML produced without inventing a format. Integers come
// back from a JSON decode as float64, and %v would print PORT: 8080 as "8080"
// only by luck of magnitude — 1e+06 for a large one.
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}

// composeFile is the top level. `volumes:` and `networks:` are declared here in
// compose and have no equivalent: a named volume becomes a claim sized by this
// platform, and there is one network per namespace.
type composeFile struct {
	Services map[string]Service `json:"services"`
}

var metaLine = regexp.MustCompile(`^#\s*([a-z_]+)\s*:\s*(.*)$`)

// Parse reads one catalogue file: the metadata header and the services.
//
// name is the catalogue identifier and comes from the caller rather than the
// file, because compose has no field for it and a filename is what every
// catalogue in this format uses.
func Parse(name string, data []byte) (Template, error) {
	t := Template{Name: name}

	// The header, read as text. Only the run of comments before the first
	// non-comment line: a `# port:` further down is somebody explaining a
	// service, not declaring the catalogue's port.
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
		m := metaLine.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		key, value := m[1], strings.TrimSpace(m[2])
		switch key {
		case "slogan":
			t.Slogan = value
		case "documentation":
			t.Documentation = value
		case "category":
			t.Category = value
		case "logo":
			t.Logo = value
		case "tags":
			for tag := range strings.SplitSeq(value, ",") {
				if tag = strings.TrimSpace(tag); tag != "" {
					t.Tags = append(t.Tags, tag)
				}
			}
		case "port":
			// The first of several, because a template that publishes two ports
			// still has one a browser goes to.
			first, _, _ := strings.Cut(value, ",")
			if n, err := strconv.Atoi(strings.TrimSpace(first)); err == nil {
				t.Port = int32(n)
			}
		}
	}

	f, err := decodeServices(data)
	if err != nil {
		return Template{}, fmt.Errorf("compose: %s: %w", name, err)
	}
	if len(f.Services) == 0 {
		return Template{}, fmt.Errorf("compose: %s: no services", name)
	}
	t.Services = f.Services
	return t, nil
}

// decodeServices reads the compose document, resolving YAML merge keys on the
// way.
//
// The detour through yaml.v3 is the whole reason this function exists.
// sigs.k8s.io/yaml converts YAML to JSON first, and that conversion does not
// resolve `<<: *anchor` — it fails with "map merge requires map or sequence of
// maps". Compose files use merge keys to share one environment block between
// services, which is common in hand-written files and was 2 of the 368
// templates measured here. yaml.v3 resolves them during decode, so decoding to
// a generic value and re-encoding as JSON gets a document the typed decode can
// read.
func decodeServices(data []byte) (composeFile, error) {
	var raw any
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return composeFile{}, err
	}
	asJSON, err := json.Marshal(raw)
	if err != nil {
		return composeFile{}, err
	}
	var f composeFile
	if err := json.Unmarshal(asJSON, &f); err != nil {
		return composeFile{}, err
	}
	return f, nil
}
