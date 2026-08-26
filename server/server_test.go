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

// This file is an external test on purpose. It imports the server package the
// way a second repository does and touches nothing else, so anything it needs
// that is not exported would fail to compile here before it failed to compile
// for a paying customer.
package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/server"
)

// ---------------------------------------------------------------- fakes
//
// These stand in for damga-ee. They are deliberately not clever: what is under
// test is that the seams are reachable and consulted, not that a plausible
// enterprise implementation could be written.

// refusingAuthorizer refuses everything and says so, which is how the test can
// tell that the substituted authorizer was the one that answered rather than
// the free one that would have allowed the read.
type refusingAuthorizer struct{ calls int }

func (a *refusingAuthorizer) Authorize(context.Context, authz.Subject, authz.Action, authz.Target,
) (authz.Decision, error) {
	a.calls++
	return authz.Decision{Allow: false, Reason: "refused by the enterprise policy engine"}, nil
}

// countingStore wraps a real store so the test can assert it was read through
// rather than bypassed.
type countingStore struct {
	evidence.Store
	reads int
}

func (s *countingStore) Current(ctx context.Context, ref evidence.Ref) (evidence.Record, error) {
	s.reads++
	return s.Store.Current(ctx, ref)
}

// ---------------------------------------------------------------- helpers

// start runs the server on a port the kernel picks and returns its base URL.
// Port zero rather than a fixed number, because two tests racing for 8080 is a
// flake that only appears on a busy machine.
func start(t *testing.T, o server.Options) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	o.Config.ListenAddr = "127.0.0.1:0"
	addrCh := make(chan string, 1)
	o.Ready = func(addr string) { addrCh <- addr }

	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(ctx, o) }()

	var addr string
	select {
	case addr = <-addrCh:
	case err := <-runErr:
		cancel()
		t.Fatalf("Run returned before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Run never became ready")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			// A clean shutdown is part of the contract: a control plane that
			// returns an error on SIGTERM makes every rollout look failed.
			if err != nil {
				t.Errorf("Run returned %v on shutdown, want nil", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	})
	return "http://" + addr
}

func get(t *testing.T, url string, header map[string][]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func subjectHeader(tenant string, groups ...string) map[string][]string {
	h := map[string][]string{
		"X-Damga-Insecure-Subject": {"u-1"},
		"X-Damga-Insecure-Tenant":  {tenant},
	}
	if len(groups) > 0 {
		h["X-Damga-Insecure-Group"] = groups
	}
	return h
}

func seed(t *testing.T, store evidence.Store, ref evidence.Ref) {
	t.Helper()
	ctx := context.Background()
	rec, err := store.Append(ctx, evidence.Record{
		IdempotencyKey: "commit:" + ref.App,
		Ref:            ref,
		Tier:           evidence.TierFree,
		Source:         evidence.Source{CommitSHA: "abc123", RepoURL: "https://example.test/r.git"},
	})
	if err != nil {
		t.Fatalf("seeding Append: %v", err)
	}
	for _, to := range []evidence.State{evidence.StateApplied, evidence.StateRunning} {
		from := evidence.StatePending
		if to == evidence.StateRunning {
			from = evidence.StateApplied
		}
		if _, err := store.Transition(ctx, rec.ID, evidence.Transition{
			From: []evidence.State{from}, To: to, At: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seeding Transition to %s: %v", to, err)
		}
	}
}

// ---------------------------------------------------------------- cases

// The claim in the package doc, under test: the zero value is a complete
// installation. If this ever needs a field set, the free tier has acquired a
// gap that the paid build fills, which is the arrangement the plan rejected.
func TestZeroOptionsIsACompleteInstallation(t *testing.T) {
	base := start(t, server.Options{})

	status, body := get(t, base+"/healthz", nil)
	if status != http.StatusOK {
		t.Errorf("GET /healthz = %d %q, want 200", status, body)
	}
}

// The free authorizer is wired by default and actually decides: a viewer may
// read the evidence page, and a subject from another tenant may not.
func TestFreeAuthorizerDecidesByDefault(t *testing.T) {
	store := memory.New(0)
	ref := evidence.Ref{TenantID: "tenant-a", App: "api", Env: "prod"}
	seed(t, store, ref)

	base := start(t, server.Options{Evidence: store})
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base, ref.TenantID, ref.App, ref.Env)

	status, body := get(t, url, subjectHeader("tenant-a", "viewer"))
	if status != http.StatusOK {
		t.Fatalf("a viewer reading its own tenant = %d %q, want 200", status, body)
	}
	var got evidence.Record
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	if got.Source.CommitSHA != "abc123" {
		t.Errorf("returned commit %q, want the seeded one", got.Source.CommitSHA)
	}

	status, _ = get(t, url, subjectHeader("tenant-b", "owner"))
	if status != http.StatusForbidden {
		t.Errorf("an owner of another tenant = %d, want 403", status)
	}

	status, _ = get(t, url, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("no subject on the request = %d, want 401", status)
	}
}

// The substitution the whole arrangement exists for. A second repository hands
// in its own authorizer and its own store, and both are the ones consulted —
// asserted by their side effects, not by the response alone, because a handler
// that ignored the seam and answered from the free implementation would look
// identical from outside.
func TestEnterpriseCanSubstituteBothSeams(t *testing.T) {
	auth := &refusingAuthorizer{}
	store := &countingStore{Store: memory.New(0)}
	ref := evidence.Ref{TenantID: "tenant-a", App: "api", Env: "prod"}
	seed(t, store.Store, ref)

	base := start(t, server.Options{Authorizer: auth, Evidence: store})
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base, ref.TenantID, ref.App, ref.Env)

	status, body := get(t, url, subjectHeader("tenant-a", "owner"))
	if status != http.StatusForbidden {
		t.Fatalf("= %d %q, want 403: the substituted authorizer refused", status, body)
	}
	if auth.calls != 1 {
		t.Errorf("the substituted authorizer was called %d times, want 1", auth.calls)
	}
	// A refusal must not read the store. Reading first and then deciding is how
	// an authorization bug becomes a data leak in the logs.
	if store.reads != 0 {
		t.Errorf("the store was read %d times on a refused request, want 0", store.reads)
	}
	if !strings.Contains(body, "enterprise policy engine") {
		t.Errorf("the refusal did not carry the authorizer's own reason: %q", body)
	}

	// And when it allows, the substituted store is the one read.
	auth2 := allowAll{}
	store2 := &countingStore{Store: memory.New(0)}
	seed(t, store2.Store, ref)
	base2 := start(t, server.Options{Authorizer: auth2, Evidence: store2})
	url2 := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base2, ref.TenantID, ref.App, ref.Env)

	if status, body := get(t, url2, subjectHeader("tenant-a", "viewer")); status != http.StatusOK {
		t.Fatalf("= %d %q, want 200", status, body)
	}
	if store2.reads != 1 {
		t.Errorf("the substituted store was read %d times, want 1", store2.reads)
	}
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, authz.Subject, authz.Action, authz.Target,
) (authz.Decision, error) {
	return authz.Decision{Allow: true, Reason: "test"}, nil
}

// The hooks: an extra route, a wrapper, and a bundle. All three are what an
// enterprise build needs in order not to fork this package.
func TestHooksAreReachable(t *testing.T) {
	const marker = "X-Enterprise"

	base := start(t, server.Options{
		Authorizer: allowAll{},
		Evidence:   memory.New(0),
		Panel: fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte("<title>enterprise</title>")},
		},
		Routes: func(mux *http.ServeMux) {
			mux.HandleFunc("GET /auth/sso/callback", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("sso"))
			})
		},
		Middleware: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(marker, "1")
				next.ServeHTTP(w, r)
			})
		},
	})

	if status, body := get(t, base+"/auth/sso/callback", nil); status != http.StatusOK || body != "sso" {
		t.Errorf("the Routes hook did not mount: %d %q", status, body)
	}
	if status, body := get(t, base+"/", nil); status != http.StatusOK || !strings.Contains(body, "enterprise") {
		t.Errorf("the substituted panel was not served: %d %q", status, body)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/healthz", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get(marker) == "" {
		t.Error("the Middleware hook did not wrap the core routes")
	}
}

// A middleware that returns nil is a programming error in the enterprise build,
// and it has to fail at startup. Serving nil panics on the first request
// instead, which would be discovered by a customer.
func TestNilMiddlewareIsRefusedAtStartup(t *testing.T) {
	err := server.Run(context.Background(), server.Options{
		Config:     server.Config{ListenAddr: "127.0.0.1:0"},
		Evidence:   memory.New(0),
		Middleware: func(http.Handler) http.Handler { return nil },
	})
	if err == nil {
		t.Fatal("Run accepted a Middleware that returns nil")
	}
	if !strings.Contains(err.Error(), "nil handler") {
		t.Errorf("Run failed with %v, which does not name the cause", err)
	}
}

// An app that has never been deployed is a normal state, not a server error.
// The page has to be able to render it, so it must be distinguishable.
func TestNothingDeployedIsNotAnError(t *testing.T) {
	base := start(t, server.Options{Authorizer: allowAll{}, Evidence: memory.New(0)})
	status, _ := get(t,
		base+"/api/v1/tenants/tenant-a/apps/never/envs/prod/evidence",
		subjectHeader("tenant-a", "viewer"))
	if status != http.StatusNotFound {
		t.Errorf("= %d, want 404 for an app with no deploys", status)
	}
}

// The interface assertions damga-ee depends on. Written as a compile-time check
// rather than prose, because "implements the interface" is exactly the kind of
// claim that stops being true without anyone noticing.
var (
	_ authz.Authorizer = allowAll{}
	_ authz.Authorizer = (*refusingAuthorizer)(nil)
	_ evidence.Store   = (*countingStore)(nil)
)
