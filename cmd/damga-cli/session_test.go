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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestALoginWritesTheSessionSoOnlyThisAccountCanReadIt.
//
// The cookie in that file is the account: whoever holds it can deploy, delete
// an app's registration and export the whole evidence log, and the server
// resolves the actor from the token, so none of it looks like anyone else. A
// default umask on a shared build host is not a decision anybody made about
// that, so the mode is set rather than inherited.
func TestALoginWritesTheSessionSoOnlyThisAccountCanReadIt(t *testing.T) {
	base := startControlPlane(t)
	c := newCLI(t)
	c.login(base)

	info, err := os.Stat(c.sessionFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the session file is mode %04o, want 0600", mode)
	}
	// The directory too, and it is this one that saveSession created — a file
	// nobody can read inside a directory anybody can list is still a directory
	// anybody can replace the file in.
	dir, err := os.Stat(filepath.Dir(c.sessionFile))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dir.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the session directory is mode %04o and others can reach into it", mode)
	}
}

// TestASessionFileOtherAccountsCanReadIsRefused.
//
// Refused and not repaired. Repairing it would hide that it had been wrong, and
// it may have been wrong for a while — the window is what matters here, not the
// current mode, and a chmod at read time erases the only evidence of it.
func TestASessionFileOtherAccountsCanReadIsRefused(t *testing.T) {
	c := newCLI(t)
	seedSession(t, c.sessionFile, "http://127.0.0.1:1")
	if err := os.Chmod(c.sessionFile, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := loadSession(c.sessionFile)
	if err == nil {
		t.Fatal("a world-readable session file was accepted")
	}
	if !strings.Contains(err.Error(), "readable by other accounts") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}

	// And the whole program refuses, not just the reader.
	if _, _, err := c.run("", "apps"); err == nil {
		t.Fatal("a command ran with a world-readable session file")
	}
}

// TestNoSessionFileIsNotAnUnreadableOne.
//
// "You have never logged in" and "your session file is broken" send a reader to
// two different places, and only one of them is where the fix is.
func TestNoSessionFileIsNotAnUnreadableOne(t *testing.T) {
	if _, err := loadSession(filepath.Join(t.TempDir(), "absent.json")); err != errNoSessionFile {
		t.Errorf("a missing file reported %v, want errNoSessionFile", err)
	}
}

// TestSameHostIsNoMoreForgivingThanTheServer.
//
// The server strips the port and folds the case before comparing, and this
// check exists to explain that refusal rather than to make a different one. A
// version of it that were more lenient would let the command through to the
// failure it was written to describe.
func TestSameHostIsNoMoreForgivingThanTheServer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"damga.example.test:8443", "damga.example.test", true},
		{"DAMGA.example.test", "damga.example.test", true},
		{"[::1]:8080", "[::1]:9090", true},
		{"127.0.0.1:8080", "localhost:8080", false},
		{"demo.example.test", "other.example.test", false},
	} {
		if got := sameHost(tc.a, tc.b); got != tc.want {
			t.Errorf("sameHost(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestAnInterruptedSaveKeepsThePreviousSession.
//
// saveSession writes a temporary file and renames it, so a crash between the
// two leaves the old session rather than a truncated file — which would read as
// corruption and send somebody looking for a bug that is not there.
func TestAnInterruptedSaveKeepsThePreviousSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	seedSession(t, path, "http://first.example.test")

	// A directory where the file should go is the cheapest way to make the
	// rename fail after the temporary file has been written in full.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "session.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := saveSession(filepath.Join(blocked, "session.json"), session{Server: "x"}); err == nil {
		t.Fatal("saving over a directory reported success")
	}

	got, err := loadSession(path)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if got.Server != "http://first.example.test" {
		t.Errorf("the previous session was lost: %+v", got)
	}

	// And the failed save left nothing behind for the next read to trip over.
	entries, err := os.ReadDir(blocked)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".session-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}
