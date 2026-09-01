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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// session is what `login` leaves behind for the next command.
//
// It holds a bearer credential — the session cookie is the whole authority the
// account has — so the file it lives in is 0600 and this package refuses to
// read one that is not. See loadSession.
type session struct {
	// Server is the URL the session was issued by, and it is stored rather
	// than only remembered because of the host binding: the control plane ties
	// a session to the host it was issued for and refuses it from any other,
	// with the same "not signed in" it gives an expired one. Without this
	// field, logging in at 127.0.0.1 and then using localhost is a login that
	// silently stops working, and the message says nothing about why.
	Server string `json:"server"`

	// The cookie, by the name the server used. The name is read back from the
	// response rather than compiled in, so the client and the server cannot
	// hold two spellings of one string — this repository has already paid for
	// that mistake twice, with a registry host and with a build's home
	// directory.
	CookieName string `json:"cookieName"`
	Cookie     string `json:"cookie"`

	// Tenant is the default for every tenant-scoped command. Chosen at login
	// from the membership list the API returns, so the common install — one
	// person, one tenant — needs no --tenant on any command.
	Tenant string `json:"tenant,omitempty"`

	// Email is printed by `whoami` when there is no server to ask. It is the
	// login address, and it is here rather than the audit alias because it is
	// what the person typed and what they will type again.
	Email string `json:"email,omitempty"`
}

// errNoSessionFile is a file that is not there. Its own error so that "you have
// never logged in" is not reported as "your session file is unreadable".
var errNoSessionFile = errors.New("no session file")

// sessionPath is where the session lives when nothing overrides it.
//
// os.UserConfigDir rather than a hand-rolled XDG lookup: it already honours
// XDG_CONFIG_HOME where that is the convention and returns the right place on
// the other platforms, and a second implementation of it here would be one more
// thing that disagrees with the rest of the machine.
func sessionPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if fromEnv := strings.TrimSpace(os.Getenv("DAMGA_SESSION_FILE")); fromEnv != "" {
		return fromEnv, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the config directory: %w "+
			"(pass --session-file or set DAMGA_SESSION_FILE)", err)
	}
	return filepath.Join(dir, "damga", "session.json"), nil
}

// loadSession reads the session and refuses one anybody else on the machine can
// read.
//
// The refusal is the point. A session cookie is the account: whoever holds it
// can deploy, delete an app's registration and export the whole evidence log,
// and none of that shows up as a different actor because the server resolves
// the actor from this token. A world-readable file on a shared build host hands
// that over silently, and the failure it produces later — somebody else's
// deploys attributed to you — is not one anybody traces back to a file mode. So
// it is checked on every read, and refused rather than repaired: fixing it here
// would hide that it had been wrong, and it may have been wrong for a while.
func loadSession(path string) (session, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return session{}, errNoSessionFile
	case err != nil:
		return session{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return session{}, fmt.Errorf(
			"%s is readable by other accounts on this machine (mode %04o) and holds a session "+
				"token; run `chmod 600 %s`, or `damga-cli logout` and sign in again", path, mode, path)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return session{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var s session
	if err := json.Unmarshal(body, &s); err != nil {
		return session{}, fmt.Errorf("%s is not a session file this version wrote: %w", path, err)
	}
	return s, nil
}

// saveSession writes the session, creating its directory.
//
// Written to a temporary file in the same directory and renamed, so that an
// interrupted write leaves the previous session intact rather than a truncated
// file that reads as corruption. The mode is set on the temporary file before
// anything is written to it, so the token is never briefly on disk at 0644.
func saveSession(path string, s session) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp) // A no-op once the rename below has succeeded.
	}()

	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("setting the mode on %s: %w", tmp, err)
	}
	if err := json.NewEncoder(f).Encode(s); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving %s: %w", path, err)
	}
	return nil
}

// clearSession removes the file. A file that is already gone is not an error:
// the caller asked to be logged out and they are.
func clearSession(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// sameHost answers whether a session issued for one URL will be accepted at
// another.
//
// The port is dropped and the case folded, because that is exactly what the
// server does before comparing — a session issued to :8080 works on :9090 of
// the same host, and 127.0.0.1 and localhost are two hosts however much they
// are one machine. Getting this wrong in the lenient direction would produce
// the failure this function exists to explain, so it is deliberately no more
// forgiving than the server is.
func sameHost(a, b string) bool {
	return canonicalHost(a) == canonicalHost(b)
}

func canonicalHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(h, ":"); i > -1 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}
