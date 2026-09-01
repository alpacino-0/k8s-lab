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

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorder is a stand-in control plane that remembers what it was asked.
//
// It exists for the handful of assertions that are about the request this CLI
// sends rather than about the answer it gets — the bytes of a deploy body, the
// headers, a response copied through untouched. Everything about which
// endpoints exist is tested against the real server instead; a fake would
// happily serve an endpoint the product does not have.
type recorder struct {
	*httptest.Server
	method  string
	path    string
	origin  string
	cookie  string
	body    []byte
	answer  string
	status  int
	content string
	hits    int
}

func newRecorder(t *testing.T, answer string) *recorder {
	t.Helper()
	r := &recorder{answer: answer, status: http.StatusOK, content: "application/json"}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits++
		r.method, r.path, r.origin = req.Method, req.URL.Path, req.Header.Get("Origin")
		if c, err := req.Cookie("damga_session"); err == nil {
			r.cookie = c.Value
		}
		r.body, _ = io.ReadAll(io.LimitReader(req.Body, 1<<20))
		w.Header().Set("Content-Type", r.content)
		w.WriteHeader(r.status)
		_, _ = io.WriteString(w, r.answer)
	}))
	t.Cleanup(r.Close)
	return r
}

// seedSession writes a session for a server, the way a successful login would.
func seedSession(t *testing.T, path, server string) {
	t.Helper()
	if err := saveSession(path, session{
		Server: server, CookieName: "damga_session", Cookie: "a-token",
		Tenant: testTenant, Email: testEmail,
	}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}
}

// TestARouteOutsideTheTableIsRefusedBeforeAnythingIsSent.
//
// The structural half of "there is no endpoint here the panel cannot reach".
// The table in client.go is what the test against the real server walks, so a
// route that bypasses the table would be a capability nothing checks — and the
// refusal happens before the request is built, which is why the recorder is
// asserted to have seen nothing.
func TestARouteOutsideTheTableIsRefusedBeforeAnythingIsSent(t *testing.T) {
	rec := newRecorder(t, `{}`)
	c, err := newClient(rec.URL, session{}, time.Second)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	invented := route{http.MethodGet, envScope + "/logs"}
	err = c.do(context.Background(), call{
		route:  invented,
		target: target{tenant: testTenant, app: testApp, env: testEnv},
	})
	if err == nil {
		t.Fatal("a route that is not in the table was allowed")
	}
	if !strings.Contains(err.Error(), "not an endpoint this CLI may call") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if rec.hits != 0 {
		t.Errorf("the request was sent anyway: %d hits", rec.hits)
	}
}

// fieldReplicas is the field the pointer semantics exist for: its zero value is
// a request somebody might genuinely mean.
const fieldReplicas = "replicas"

// TestDeploySendsOnlyTheFieldsThatWereAskedFor.
//
// The server treats a field that is absent as "keep what is committed" and a
// field that is present as "set it to this", which is why its request type uses
// pointers. A client that sent every flag's zero value would collapse that
// distinction, and the collapse has one very specific consequence: no
// --replicas would become replicas: 0, which is "take the app down".
func TestDeploySendsOnlyTheFieldsThatWereAskedFor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    map[string]any
		absent  []string
		present []string
	}{
		{
			name:   "only an image",
			args:   []string{"--image", "example.test/api@sha256:abc"},
			absent: []string{"port", fieldReplicas, "domain", "note"},
		},
		{
			name:    "zero replicas is a request, not an omission",
			args:    []string{"--image", "example.test/api@sha256:abc", "--" + fieldReplicas, "0"},
			want:    map[string]any{fieldReplicas: float64(0)},
			present: []string{fieldReplicas},
			absent:  []string{"port", "domain"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t, `{"seq":1,"state":"pending","image":{"requestedRef":"x"}}`)
			rec.status = http.StatusAccepted
			c := newCLI(t)
			seedSession(t, c.sessionFile, rec.URL)

			args := append([]string{"deploy", testApp, testEnv}, tc.args...)
			c.mustRun("", args...)

			var sent map[string]any
			if err := json.Unmarshal(rec.body, &sent); err != nil {
				t.Fatalf("the body is not JSON: %v (%s)", err, rec.body)
			}
			for _, key := range tc.absent {
				if _, found := sent[key]; found {
					t.Errorf("%q was sent although no flag asked for it: %s", key, rec.body)
				}
			}
			for _, key := range tc.present {
				if _, found := sent[key]; !found {
					t.Errorf("%q was asked for and not sent: %s", key, rec.body)
				}
			}
			for key, want := range tc.want {
				if sent[key] != want {
					t.Errorf("%q is %v, want %v", key, sent[key], want)
				}
			}
		})
	}
}

// TestTheRequestCarriesTheSessionAndNoOrigin.
//
// The server's CSRF control is origin-scoped and deliberately allows a request
// that carries neither Sec-Fetch-Site nor Origin, which is what lets a terminal
// client work without a token of its own. A well-meaning Origin header added
// here would start failing every write, and it would fail them at the server
// with a message about cross-origin requests that names nothing about this
// client.
func TestTheRequestCarriesTheSessionAndNoOrigin(t *testing.T) {
	rec := newRecorder(t, `{"apps":[]}`)
	c := newCLI(t)
	seedSession(t, c.sessionFile, rec.URL)

	c.mustRun("", "apps")

	if rec.cookie != "a-token" {
		t.Errorf("the session cookie did not travel: %q", rec.cookie)
	}
	if rec.origin != "" {
		t.Errorf("an Origin header was sent (%q); the server rejects same-site writes", rec.origin)
	}
	if rec.path != "/api/v1/tenants/"+testTenant+"/apps" {
		t.Errorf("apps asked for %s", rec.path)
	}
}

// TestExportIsCopiedThroughByteForByte.
//
// The export is the store's own encoding because that is the form the hash
// chain was computed over. Decoding and re-encoding it — even correctly, even
// into identical-looking JSON — produces a file that no longer verifies, and it
// does so silently. So the bytes are copied, and this is the test that says so.
func TestExportIsCopiedThroughByteForByte(t *testing.T) {
	// Two records, with the key order and the spacing a re-encode would
	// normalise away.
	const jsonl = `{"seq":1,"hash":"aa","ref":{"App":"api"}}` + "\n" +
		`{"seq":2,  "hash":"bb","ref":{"App":"api"}}` + "\n"

	rec := newRecorder(t, jsonl)
	rec.content = "application/jsonl"
	c := newCLI(t)
	seedSession(t, c.sessionFile, rec.URL)

	got := c.mustRun("", "export", testApp, testEnv)
	if got != jsonl {
		t.Errorf("the export was rewritten on the way past:\n got %q\nwant %q", got, jsonl)
	}
}

// TestJSONPrintsTheServersOwnBytes, for the same reason as the export: the
// point of asking for JSON is to hand it to something else.
func TestJSONPrintsTheServersOwnBytes(t *testing.T) {
	const answer = `{"apps":[{"app":"api","env":"prod","state":"deployed"}]}`
	rec := newRecorder(t, answer)
	c := newCLI(t)
	seedSession(t, c.sessionFile, rec.URL)

	got := strings.TrimSpace(c.mustRun("", "--json", "apps"))
	if got != answer {
		t.Errorf("--json rewrote the answer:\n got %s\nwant %s", got, answer)
	}
}

// TestVerifyFailsTheScriptWhenTheChainDoes.
//
// A verification that reports a broken chain and exits 0 is worse than no
// verification: every script that runs it treats "the history has been
// rewritten" as a normal day. The exit code is its own number so that a caller
// does not have to read the output to find out.
func TestVerifyFailsTheScriptWhenTheChainDoes(t *testing.T) {
	rec := newRecorder(t, `{"valid":false,"brokenAt":7,"records":9,"fromSeq":1,"toSeq":9}`)
	c := newCLI(t)
	seedSession(t, c.sessionFile, rec.URL)

	out, _, err := c.run("", "verify", testApp, testEnv)
	if err == nil {
		t.Fatal("a broken chain exited 0")
	}
	if got := exitFor(err); got != exitChainBroken {
		t.Errorf("exit code %d, want %d", got, exitChainBroken)
	}
	if !strings.Contains(out, "BROKEN") || !strings.Contains(out, "seq 7") {
		t.Errorf("the output does not say where it broke:\n%s", out)
	}

	// And in --json, where the bytes are handed to a script rather than read.
	// The verdict must not depend on which mode was asked for.
	_, _, err = c.run("", "--json", "verify", testApp, testEnv)
	if got := exitFor(err); got != exitChainBroken {
		t.Errorf("--json exit code %d, want %d", got, exitChainBroken)
	}
}

// TestSomethingThatIsNotTheControlPlaneIsNamed.
//
// An ingress with no backend, a proxy, a login page in front of the API: all of
// them answer HTML with a status this client would otherwise report as a bare
// number, sending the reader to the control plane's logs, which is not where
// the fault is.
func TestSomethingThatIsNotTheControlPlaneIsNamed(t *testing.T) {
	rec := newRecorder(t, "<html><body>502 Bad Gateway</body></html>")
	rec.content = "text/html"
	rec.status = http.StatusBadGateway
	c := newCLI(t)
	seedSession(t, c.sessionFile, rec.URL)

	_, _, err := c.run("", "apps")
	if err == nil {
		t.Fatal("an HTML error page was accepted")
	}
	if !strings.Contains(err.Error(), "did not answer with JSON") {
		t.Errorf("the message does not say what answered: %v", err)
	}
}

// TestAnExpiredSessionExitsAsNotSignedIn, which is the code a wrapper script
// keys on to run login again.
func TestAnExpiredSessionExitsAsNotSignedIn(t *testing.T) {
	rec := newRecorder(t, `{"status":401,"detail":"not signed in"}`)
	rec.status = http.StatusUnauthorized
	c := newCLI(t)
	seedSession(t, c.sessionFile, rec.URL)

	_, _, err := c.run("", "apps")
	if err == nil {
		t.Fatal("a 401 was not reported as a failure")
	}
	if got := exitFor(err); got != exitNotSignedIn {
		t.Errorf("exit code %d, want %d", got, exitNotSignedIn)
	}
}

// TestAPathVariableIsEscaped, because a tenant id is not a DNS label: the
// control plane mints it with an underscore and nothing checks it against a
// name rule, so it is the value most likely to carry something a URL cares
// about.
func TestAPathVariableIsEscaped(t *testing.T) {
	got, err := target{tenant: "t_a b/../c", app: testApp, env: testEnv}.fill(envScope + "/evidence")
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	const want = "/api/v1/tenants/t_a%20b%2F..%2Fc/apps/api/envs/prod/evidence"
	if got != want {
		t.Errorf("fill gave %q, want %q", got, want)
	}
}

// TestAMissingPathVariableIsRefusedRatherThanSent.
//
// An empty segment collapses the path into a different route: .../apps//builds
// is not the builds endpoint, it is a 404 that reads as the server being
// broken.
func TestAMissingPathVariableIsRefusedRatherThanSent(t *testing.T) {
	_, err := target{tenant: testTenant, app: ""}.fill(tenantScope + "/apps/{app}/builds")
	if err == nil {
		t.Fatal("a path with an empty segment was built")
	}
	if !strings.Contains(err.Error(), "needs the app name") {
		t.Errorf("the message does not name what is missing: %v", err)
	}
}
