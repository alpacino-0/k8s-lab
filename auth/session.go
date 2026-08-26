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

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/damgahq/damga/identity"
)

// CookieName is the session cookie.
//
// Unprefixed, and that is a decision rather than an oversight. The __Host-
// prefix would give a useful property — a sibling subdomain cannot overwrite
// the cookie — but Chrome and Safari refuse a prefixed cookie on
// http://localhost, which is exactly this project's documented first run. A
// login that silently never sticks, with nothing in the logs, is a wall at the
// front door for the one person the free tier exists to reach.
//
// The property is obtained instead by omitting Domain, which already stops a
// parent from targeting this cookie by name, and by binding each session to the
// host it was issued for and checking that on every use. That works on every
// browser, and it survives the eventual move from http to https without
// renaming the cookie and logging everyone out.
const CookieName = "damga_session"

// ErrNoSession means the request carried no usable session.
var ErrNoSession = errors.New("auth: no session")

// Sessions mints, resolves and revokes logins.
type Sessions struct {
	Store identity.Store

	// TTL is how long a session lives. Absolute, not sliding: a sliding window
	// means a stolen cookie never expires as long as it is being used.
	TTL time.Duration

	// Secure sets the cookie's Secure attribute. It is configuration rather
	// than a constant because the documented first run is plain http on
	// localhost, and the alternative to a flag is a cookie that does not work
	// there — see CookieName.
	Secure bool

	// SameSite defaults to Lax. It is a field rather than a constant because
	// SAML's HTTP-POST binding cannot carry a Lax cookie, and SAML is a paid
	// feature that must not require forking this package to enable.
	//
	// It is defence in depth and not the CSRF control; that is
	// net/http.CrossOriginProtection, wired in the server's middleware. Lax is
	// site-scoped rather than origin-scoped, and a sibling subdomain is
	// same-site — ordinary for an internal platform, and wide open.
	SameSite http.SameSite

	// Now is injected for tests.
	Now func() time.Time
}

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sessions) ttl() time.Duration {
	if s.TTL <= 0 {
		return 12 * time.Hour
	}
	return s.TTL
}

// Issue creates a session for an account and returns the cookie to set.
//
// The token is returned only here, in the cookie. The store keeps its SHA-256
// digest and never the token itself, which is what makes a leaked database dump
// useless for impersonation.
func (s *Sessions) Issue(ctx context.Context, accountID, host string) (*http.Cookie, error) {
	if accountID == "" {
		return nil, errors.New("auth: no account")
	}
	// crypto/rand.Text is 128+ bits of entropy in an unambiguous alphabet,
	// which is both more than a session needs and free of the encoding
	// questions a hand-rolled generator invites.
	token := rand.Text()
	sum := sha256.Sum256([]byte(token))

	now := s.now()
	if err := s.Store.CreateSession(ctx, identity.Session{
		Digest:    sum[:],
		AccountID: accountID,
		IssuedFor: canonicalHost(host),
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl()),
	}); err != nil {
		return nil, err
	}

	sameSite := s.SameSite
	if sameSite == 0 {
		sameSite = http.SameSiteLaxMode
	}
	return &http.Cookie{
		Name:  CookieName,
		Value: token,
		Path:  "/",
		// No Domain, deliberately. Setting one would offer the cookie to every
		// subdomain, which is the opposite of what the __Host- prefix is for.
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: sameSite,
		MaxAge:   int(s.ttl().Seconds()),
	}, nil
}

// Resolve returns the account a request is authenticated as.
//
// It returns ErrNoSession for a missing, unknown, expired or host-mismatched
// cookie — four different situations that must all look the same to a caller,
// because telling them apart is telling an attacker which of their guesses was
// closer.
func (s *Sessions) Resolve(ctx context.Context, r *http.Request) (identity.Session, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return identity.Session{}, ErrNoSession
	}
	sum := sha256.Sum256([]byte(c.Value))
	sess, err := s.Store.Session(ctx, sum[:], s.now())
	if err != nil {
		// Including ErrNotFound, which covers both "no such session" and
		// "expired" — the store refuses an expired one rather than returning it
		// for the caller to check, because a caller that forgets is an
		// authentication bypass.
		return identity.Session{}, ErrNoSession
	}
	// The host binding. A cookie planted by a sibling subdomain arrives with a
	// digest this server never issued, but one lifted from a session that was
	// issued for a different host would otherwise work here.
	if sess.IssuedFor != "" && sess.IssuedFor != canonicalHost(r.Host) {
		return identity.Session{}, ErrNoSession
	}
	return sess, nil
}

// Revoke is logout. It removes the session and returns the cookie that clears
// it, so a browser does not keep presenting a token the server has forgotten.
func (s *Sessions) Revoke(ctx context.Context, r *http.Request) *http.Cookie {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		sum := sha256.Sum256([]byte(c.Value))
		// A session that was already gone is not an error worth reporting: the
		// user asked to be logged out and they are.
		_ = s.Store.DeleteSession(ctx, sum[:])
	}
	sameSite := s.SameSite
	if sameSite == 0 {
		sameSite = http.SameSiteLaxMode
	}
	return &http.Cookie{
		Name: CookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: s.Secure, SameSite: sameSite,
		MaxAge: -1,
	}
}

// canonicalHost strips the port and lowercases, so a session issued to
// damga.example.test:8443 resolves against damga.example.test and a mixed-case
// Host header does not look like a different site.
func canonicalHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(h, ":"); i > -1 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}
