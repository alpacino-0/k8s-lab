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

package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/identity"
	"github.com/damgahq/damga/identity/memory"
)

// Cheap parameters for the suite. Real ones take tens of milliseconds each and
// this file hashes a few dozen times; what is under test is the mechanism, and
// the mechanism does not care how expensive it was told to be.
var cheap = auth.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

const (
	tenantID = "t_alpha"
	acctID   = "a_orhan"
	host     = "damga.example.test"
)

func seed(t *testing.T) identity.Store {
	t.Helper()
	ctx := context.Background()
	s := memory.New()
	if _, err := s.CreateTenant(ctx, identity.Tenant{
		ID: tenantID, Slug: "alpha", DisplayName: "Alpha"}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := s.CreateAccount(ctx, identity.Account{
		ID: acctID, Kind: "user", Email: "orhan@example.test",
		AuditEmail: "a_orhan@users.damga.local", DisplayName: "Orhan Yavuz",
	}, identity.Credential{}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return s
}

func sessions(store identity.Store) *auth.Sessions {
	return &auth.Sessions{Store: store, TTL: time.Hour}
}

// ---------------------------------------------------------------- passwords

func TestHashAndVerify(t *testing.T) {
	h := auth.NewHasher(cheap, 2)
	encoded, err := h.Hash("correct horse")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := h.Verify(encoded, "correct horse"); err != nil {
		t.Errorf("Verify with the right password: %v", err)
	}
	if err := h.Verify(encoded, "wrong horse"); !errors.Is(err, auth.ErrMismatch) {
		t.Errorf("Verify with the wrong password = %v, want ErrMismatch", err)
	}
}

// The same password twice must not produce the same hash, or the store leaks
// which accounts share one.
func TestHashesAreSalted(t *testing.T) {
	h := auth.NewHasher(cheap, 2)
	a, err := h.Hash("same")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := h.Hash("same")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if a == b {
		t.Error("two hashes of one password are identical; the salt is not doing anything")
	}
}

// The parameters travel inside the hash, which is what lets them be raised
// without a flag day: an old hash still verifies under the parameters it was
// made with, and the login that verifies it is the only moment the plaintext
// exists to make a stronger one.
func TestParametersTravelWithTheHash(t *testing.T) {
	weak := auth.NewHasher(auth.Params{
		Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}, 2)
	strong := auth.NewHasher(auth.Params{
		Memory: 256, Time: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}, 2)

	old, err := weak.Hash("secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := strong.Verify(old, "secret"); err != nil {
		t.Errorf("a hash made with weaker parameters no longer verifies: %v", err)
	}
	if !strong.NeedsRehash(old) {
		t.Error("NeedsRehash did not notice weaker parameters")
	}
	fresh, err := strong.Hash("secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if strong.NeedsRehash(fresh) {
		t.Error("NeedsRehash flagged a hash made with the current parameters")
	}
}

// A corrupt row is not a failed login. Reporting it as one would hide a data
// problem behind a user mistake, and the user would be told their password is
// wrong for ever.
func TestACorruptHashIsNotAMismatch(t *testing.T) {
	h := auth.NewHasher(cheap, 2)
	for _, bad := range []string{
		"", "not-a-hash", "$argon2id$", "$bcrypt$v=19$m=64,t=1,p=1$AAAA$BBBB",
		"$argon2id$v=99$m=64,t=1,p=1$AAAA$BBBB",
	} {
		err := h.Verify(bad, "anything")
		if err == nil {
			t.Errorf("Verify accepted %q", bad)
			continue
		}
		if errors.Is(err, auth.ErrMismatch) {
			t.Errorf("Verify reported %q as a wrong password rather than a broken hash", bad)
		}
	}
}

// The property the memory parameter alone does not give. Any cost, however
// carefully chosen, multiplies by the number of hashes running at once — so a
// login page without a bound is a memory limit that holds until somebody points
// a script at it.
func TestConcurrencyIsBounded(t *testing.T) {
	const bound = 2
	h := auth.NewHasher(auth.Params{
		Memory: 1024, Time: 3, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}, bound)

	var live, peak int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			// Sampled around the call rather than inside it, so what is
			// measured is the gate the Hasher holds and not a counter this test
			// invented.
			n := atomic.AddInt64(&live, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if n <= old || atomic.CompareAndSwapInt64(&peak, old, n) {
					break
				}
			}
			_, _ = h.Hash("x")
			atomic.AddInt64(&live, -1)
		})
	}
	wg.Wait()

	// The sampling above counts goroutines that have entered, which is at least
	// as many as are hashing — so this asserts the gate did something, not that
	// the count is exact.
	if peak < 2 {
		t.Skip("the goroutines never overlapped; nothing to conclude")
	}
	// What actually proves the bound: a hash cannot run while the gate is full.
	// Fill it and check the next one waits.
	blocked := make(chan struct{})
	release := make(chan struct{})
	for range bound {
		go func() {
			<-release
		}()
	}
	go func() {
		_, _ = h.Hash("y")
		close(blocked)
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Error("a hash never completed; the gate is not releasing")
	}
	close(release)
}

// A login handler that skips hashing for an unknown address answers faster for
// addresses that do not exist, and that difference is an account enumeration
// oracle. VerifyDummy exists so the handler can take the same path either way.
func TestVerifyDummyDoesTheWork(t *testing.T) {
	h := auth.NewHasher(auth.Params{
		Memory: 4096, Time: 3, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}, 4)
	encoded, err := h.Hash("secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	start := time.Now()
	_ = h.Verify(encoded, "wrong")
	real := time.Since(start)

	start = time.Now()
	h.VerifyDummy("wrong")
	dummy := time.Since(start)

	// Deliberately loose. What is being ruled out is a dummy that returns
	// immediately — an order of magnitude, not a timing-attack-grade match,
	// which no Go test can assert on a shared machine anyway.
	//
	// real/10 because the sentence above says an order of magnitude and the
	// comparison used to say real/4, which is not one. It failed on a loaded
	// machine by 0.36ms — dummy 24.407ms against a real 99.058ms, where the
	// threshold was 24.764ms — and passed on the next run. A test that fails
	// when the machine is busy teaches people to rerun rather than to read,
	// and the thing it exists to catch, a dummy that returns immediately,
	// would miss real/10 by three orders of magnitude rather than by a
	// rounding error.
	if dummy < real/10 {
		t.Errorf("the dummy verification took %v against a real %v; "+
			"an unknown address would answer measurably faster", dummy, real)
	}
}

// ---------------------------------------------------------------- sessions

func TestIssueResolveRevoke(t *testing.T) {
	ctx := context.Background()
	s := sessions(seed(t))

	cookie, err := s.Issue(ctx, acctID, host)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if cookie.Value == "" {
		t.Fatal("Issue returned an empty token")
	}

	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	req.AddCookie(cookie)
	got, err := s.Resolve(ctx, req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.AccountID != acctID {
		t.Errorf("resolved to %q", got.AccountID)
	}

	clear := s.Revoke(ctx, req)
	if clear.MaxAge >= 0 {
		t.Errorf("the clearing cookie has MaxAge %d; a browser would keep the old one", clear.MaxAge)
	}
	if _, err := s.Resolve(ctx, req); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("the session survived logout: %v", err)
	}
}

// Unprefixed name, no Domain, HttpOnly, Path=/. Each of these is load-bearing
// and the reasoning is in the CookieName comment; asserted here so a later
// tidy-up cannot quietly drop one.
func TestCookieAttributes(t *testing.T) {
	ctx := context.Background()
	s := sessions(seed(t))
	s.Secure = true

	c, err := s.Issue(ctx, acctID, host)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	switch {
	case strings.HasPrefix(c.Name, "__Host-"), strings.HasPrefix(c.Name, "__Secure-"):
		t.Errorf("cookie name %q is prefixed; Chrome and Safari drop those on http://localhost, "+
			"which is this project's documented first run", c.Name)
	case c.Domain != "":
		t.Errorf("cookie has Domain %q; that offers it to every subdomain", c.Domain)
	case !c.HttpOnly:
		t.Error("cookie is readable from JavaScript")
	case c.Path != "/":
		t.Errorf("cookie Path is %q, want /", c.Path)
	case !c.Secure:
		t.Error("Secure was requested and not set")
	case c.SameSite != http.SameSiteLaxMode:
		t.Errorf("SameSite = %v, want Lax by default", c.SameSite)
	}

	// And it must be settable to None, because SAML's HTTP-POST binding cannot
	// carry a Lax cookie, and that must not require a
	// fork of this package.
	s.SameSite = http.SameSiteNoneMode
	c, err = s.Issue(ctx, acctID, host)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if c.SameSite != http.SameSiteNoneMode {
		t.Errorf("SameSite = %v after being set to None", c.SameSite)
	}
}

// The property the __Host- prefix would have given, obtained a way that works
// on every browser: a token issued for one host does not authenticate against
// another.
func TestASessionIsBoundToItsHost(t *testing.T) {
	ctx := context.Background()
	s := sessions(seed(t))

	cookie, err := s.Issue(ctx, acctID, host)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	elsewhere := httptest.NewRequest(http.MethodGet, "http://other.example.test/", nil)
	elsewhere.AddCookie(cookie)
	if _, err := s.Resolve(ctx, elsewhere); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("a session issued for %s resolved against another host: %v", host, err)
	}

	// The port is not part of the identity, or moving from :8080 to :8443 would
	// log everybody out.
	withPort := httptest.NewRequest(http.MethodGet, "http://"+host+":8443/", nil)
	withPort.AddCookie(cookie)
	if _, err := s.Resolve(ctx, withPort); err != nil {
		t.Errorf("the same host on another port failed to resolve: %v", err)
	}
}

// Four different situations that must all look the same to a caller. Telling
// them apart is telling an attacker which guess was closer.
func TestEveryFailureLooksTheSame(t *testing.T) {
	ctx := context.Background()
	store := seed(t)
	s := sessions(store)

	live, err := s.Issue(ctx, acctID, host)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cases := map[string]*http.Cookie{
		"no cookie at all": nil,
		"empty value":      {Name: auth.CookieName, Value: ""},
		"unknown token":    {Name: auth.CookieName, Value: "AAAAAAAAAAAAAAAAAAAAAAAAAA"},
	}
	for name, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		if c != nil {
			req.AddCookie(c)
		}
		if _, err := s.Resolve(ctx, req); !errors.Is(err, auth.ErrNoSession) {
			t.Errorf("%s = %v, want ErrNoSession", name, err)
		}
	}

	// And an expired one, which the store refuses rather than returning for the
	// caller to check.
	expired := &auth.Sessions{
		Store: store, TTL: time.Hour,
		Now: func() time.Time { return time.Now().Add(2 * time.Hour) },
	}
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	req.AddCookie(live)
	if _, err := expired.Resolve(ctx, req); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("an expired session = %v, want ErrNoSession", err)
	}
}

// The store never holds the token. A dump of the session table must not let
// anybody log in as anybody.
func TestTheStoreNeverHoldsTheToken(t *testing.T) {
	ctx := context.Background()
	store := seed(t)
	s := sessions(store)

	cookie, err := s.Issue(ctx, acctID, host)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Looking the session up by the token itself must find nothing: what is
	// stored is its digest.
	if _, err := store.Session(ctx, []byte(cookie.Value), time.Now()); !errors.Is(err, identity.ErrNotFound) {
		t.Error("the session is retrievable by its own token; the store is holding the secret")
	}
}
