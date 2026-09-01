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

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The note the fixture repository keeps out of every commit, which is what
// makes it stand for PLAN.md and the four beside it.
const (
	fixtureNote = "NOTE.md"
	fixtureBody = "the scope boundary is here\n"
)

// fixtureRepo builds a repository with one commit and one ignored note, and
// returns the path to its main working tree.
//
// Its own repository rather than this one: what is under test is what happens
// to a file that is in no commit, and asserting that against the checkout the
// test is running in would make the case depend on the state of somebody's
// working tree.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main", ".")

	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, ".gitignore"), "docs/*\n!docs/PUBLIC.md\n")
	write(t, filepath.Join(root, "docs", "PUBLIC.md"), "committed\n")
	write(t, filepath.Join(root, "docs", fixtureNote), fixtureBody)
	git("add", ".gitignore", "docs/PUBLIC.md")
	git("commit", "-qm", "first")
	return root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// linkDocs runs the script the way somebody standing in a worktree runs it.
//
// It reads the script before running it, and that read is load-bearing rather
// than defensive. `go test` caches a result and invalidates it when a file the
// test process opened changes — a script executed by a child bash is not one of
// those, so editing link-docs.sh and rerunning the gate replays the previous
// PASS. Measured here: a guard was removed and the suite reported ok from
// cache. Opening the file makes it an input the cache knows about.
func linkDocs(t *testing.T, wd string) string {
	t.Helper()
	script, err := filepath.Abs("link-docs.sh")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("the script under test is unreadable: %v", err)
	}
	if !strings.HasPrefix(string(body), "#!") {
		t.Fatalf("%s does not start with a shebang", script)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = wd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("link-docs.sh in %s: %v\n%s", wd, err, out)
	}
	return string(out)
}

func worktree(t *testing.T, main string) string {
	t.Helper()
	linked := filepath.Join(t.TempDir(), "linked")
	cmd := exec.Command("git", "-C", main, "worktree", "add", "-q", "--detach", linked)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", main, "worktree", "remove", "--force", linked).Run()
	})
	return linked
}

// The failure this exists for: a note in no commit is a note in no worktree.
//
// Six sessions were told the scope boundary was in docs/PLAN.md and five of
// them were standing somewhere that file did not exist.
func TestTheNotesReachALinkedWorktree(t *testing.T) {
	main := fixtureRepo(t)
	linked := worktree(t, main)

	if _, err := os.Lstat(filepath.Join(linked, "docs", fixtureNote)); err == nil {
		t.Fatal("the fixture is wrong: the ignored note arrived in the worktree by itself, " +
			"so this test would pass without the script doing anything")
	}

	linkDocs(t, linked)

	body, err := os.ReadFile(filepath.Join(linked, "docs", fixtureNote))
	if err != nil {
		t.Fatalf("the note is still unreadable from the worktree: %v", err)
	}
	if string(body) != fixtureBody {
		t.Fatalf("the note reads %q, want %q", body, fixtureBody)
	}
}

// The half that decides between a link and a copy, and the reason the decision
// is not a preference.
//
// These files are edited as well as read, and they are in no commit — so an
// edit written into a worktree's own copy is in no ref, and no merge will ever
// find it. It is gone when the worktree is removed. A write through a link
// lands in the one copy there is, which is what this asserts: replace the ln
// with a cp and this case fails saying the edit did not survive.
func TestAnEditInTheWorktreeReachesTheOriginal(t *testing.T) {
	main := fixtureRepo(t)
	linked := worktree(t, main)
	linkDocs(t, linked)

	const edited = "someone corrected this from a worktree\n"
	write(t, filepath.Join(linked, "docs", fixtureNote), edited)

	body, err := os.ReadFile(filepath.Join(main, "docs", fixtureNote))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != edited {
		t.Fatalf("the main checkout still reads %q. The worktree got a copy, so the edit is in "+
			"no ref, no checkout and no merge — it is gone with the worktree", body)
	}
}

// A file somebody already has in their worktree is theirs. Replacing it with a
// link would delete work with no diff to show for it, because the path is
// ignored and git never mentions it.
func TestARealFileIsLeftAlone(t *testing.T) {
	main := fixtureRepo(t)
	linked := worktree(t, main)

	const mine = "my own draft\n"
	if err := os.MkdirAll(filepath.Join(linked, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(linked, "docs", fixtureNote), mine)

	out := linkDocs(t, linked)

	body, err := os.ReadFile(filepath.Join(linked, "docs", fixtureNote))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != mine {
		t.Fatalf("a file that was already there was replaced by a link to %q", body)
	}
	if !strings.Contains(out, fixtureNote) {
		t.Errorf("it kept the file and did not say so: %q", out)
	}
}

// A tracked file must not be linked even when it is missing from the worktree,
// which is the only way this can happen: a checked-out file is a real file, so
// the guard against overwriting one covers the ordinary case and this is what
// is left. Somebody deletes docs/DEPLOY.md, runs this, and now a tracked path
// is a symlink into another checkout — git reports a type change, `git add -A`
// commits a link to somebody's home directory, and the file itself is a runbook
// that differs between branches, so it would also be showing the wrong one.
func TestATrackedFileIsNeverLinked(t *testing.T) {
	main := fixtureRepo(t)
	linked := worktree(t, main)

	tracked := filepath.Join(linked, "docs", "PUBLIC.md")
	if err := os.Remove(tracked); err != nil {
		t.Fatal(err)
	}

	linkDocs(t, linked)

	if info, err := os.Lstat(tracked); err == nil && info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("a committed path became a symlink into another checkout; git calls that a " +
			"type change and the next `git add -A` commits it")
	}
}

// Run in the main checkout it must do nothing at all: there is no second copy
// to point at, and a link from a file to itself is a broken file.
func TestInTheMainCheckoutItChangesNothing(t *testing.T) {
	main := fixtureRepo(t)
	out := linkDocs(t, main)

	info, err := os.Lstat(filepath.Join(main, "docs", fixtureNote))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the note in the main checkout was replaced by a symlink, which is where the " +
			"only copy of it used to be")
	}
	if !strings.Contains(out, "main checkout") {
		t.Errorf("it did nothing and did not say why: %q", out)
	}
}
