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

package gitwrite_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/internal/gitwrite"
)

// One directory, three files: the app, a second service, and something this
// platform did not write.
const (
	secondPath   = "workload-worker.yaml"
	databasePath = "database-db.yaml"
	foreignPath  = "kustomization.yaml"
)

// ours stands in for manifest.Owns, which this package must not import: the
// question "is this file mine" belongs to whoever writes the files, and the
// point of the field is that this package never answers it. The test predicate
// is the same shape as the real one — it reads the content, not the name.
func ours(_ string, body []byte) bool {
	return bytes.Contains(body, []byte("apiVersion: platform.damga.co/v1alpha1"))
}

func manifestBody(kind, image string) []byte {
	return []byte("apiVersion: platform.damga.co/v1alpha1\nkind: " + kind +
		"\nspec:\n  image: " + image + "\n")
}

// owning is request() with the deletion rule switched on.
func owning(target string, render func(string, map[string][]byte) (map[string][]byte, error)) gitwrite.Request {
	req := request(target, render)
	req.Owns = ours
	return req
}

func renders(files map[string][]byte) func(string, map[string][]byte) (map[string][]byte, error) {
	return func(string, map[string][]byte) (map[string][]byte, error) { return files, nil }
}

// namesAt lists the files committed under the request's directory.
func namesAt(t *testing.T, url string) []string {
	t.Helper()
	commit := head(t, url)
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("reading the tree: %v", err)
	}
	var out []string
	if err := tree.Files().ForEach(func(f *object.File) error {
		if len(f.Name) > len("apps/api/") && f.Name[:len("apps/api/")] == "apps/api/" {
			out = append(out, f.Name[len("apps/api/"):])
		}
		return nil
	}); err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	slices.Sort(out)
	return out
}

// An app is not always one object. A catalogue entry with a worker and a
// database is three manifests in one directory, and the write path has to be
// able to carry them or the entry cannot be installed at all — 117 of the 341
// entries the upstream corpus offers are out of reach for exactly this reason
// (measured through the shipping code, docs/DURUM.md).
func TestARenderWithSeveralObjectsWritesAFilePerObject(t *testing.T) {
	url := remote(t)
	w := newWriter(memory.New(0))

	if _, err := w.Deploy(context.Background(), owning(url, renders(map[string][]byte{
		manifestPath: manifestBody("Workload", "app:1"),
		secondPath:   manifestBody("Workload", "worker:1"),
		databasePath: manifestBody("Database", "postgres:17"),
	}))); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	got := namesAt(t, url)
	want := []string{databasePath, secondPath, manifestPath}
	slices.Sort(want)
	if len(got) != len(want) {
		t.Fatalf("the directory holds %v; a render that returns three objects has to commit "+
			"three files, or the objects it did not write are objects nothing creates", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the directory holds %v, want %v", got, want)
		}
	}
}

// The half that was missing, and the reason it matters is not tidiness: a
// service dropped from a template keeps its file, Argo CD goes on applying it,
// and the object the control plane believes it retracted goes on running.
func TestAnObjectTheRenderStoppedProducingIsRemoved(t *testing.T) {
	url := remote(t)
	w := newWriter(memory.New(0))
	ctx := context.Background()

	if _, err := w.Deploy(ctx, owning(url, renders(map[string][]byte{
		manifestPath: manifestBody("Workload", "app:1"),
		secondPath:   manifestBody("Workload", "worker:1"),
	}))); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	// The worker is gone from the template.
	if _, err := w.Deploy(ctx, owning(url, renders(map[string][]byte{
		manifestPath: manifestBody("Workload", "app:2"),
	}))); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	for _, name := range namesAt(t, url) {
		if name == secondPath {
			t.Fatal("the dropped object's manifest is still committed, so Argo CD is still " +
				"applying an object the platform believes it removed")
		}
	}
}

// And the guard on that deletion. This directory belongs to damga today, but
// the day anything else commits into it, a rule of "delete what the render
// omitted" removes work from a repository whose owner cannot push to it — and
// leaves no diff that explains why.
func TestAFileThisPlatformDidNotWriteIsNeverRemoved(t *testing.T) {
	url := remote(t)
	w := newWriter(memory.New(0))
	ctx := context.Background()

	foreign := []byte("resources:\n  - workload.yaml\n")
	if _, err := w.Deploy(ctx, owning(url, renders(map[string][]byte{
		manifestPath: manifestBody("Workload", "app:1"),
		foreignPath:  foreign,
	}))); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	// A render that knows nothing about the foreign file — which is every
	// render this platform has.
	if _, err := w.Deploy(ctx, owning(url, renders(map[string][]byte{
		manifestPath: manifestBody("Workload", "app:2"),
	}))); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	var found bool
	for _, name := range namesAt(t, url) {
		if name == foreignPath {
			found = true
		}
	}
	if !found {
		t.Fatal("a file this platform did not write was deleted because a render did not " +
			"return it; ownership is decided by what is in the file, and nothing in that " +
			"one says damga wrote it")
	}
}

// Deletion must not invent a commit either. A redeploy of an identical set is
// a legitimate request and there is nothing to record.
func TestAnIdenticalSetOfObjectsIsNotACommit(t *testing.T) {
	url := remote(t)
	w := newWriter(memory.New(0))
	ctx := context.Background()

	files := map[string][]byte{
		manifestPath: manifestBody("Workload", "app:1"),
		secondPath:   manifestBody("Workload", "worker:1"),
	}
	if _, err := w.Deploy(ctx, owning(url, renders(files))); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	before := head(t, url).Hash

	_, err := w.Deploy(ctx, owning(url, renders(files)))
	if !errors.Is(err, gitwrite.ErrNoChange) {
		t.Fatalf("redeploying the same set = %v, want ErrNoChange; a commit that changes "+
			"nothing puts a deploy in the history that did nothing", err)
	}
	if after := head(t, url).Hash; after != before {
		t.Fatalf("the branch moved from %s to %s without a change", before, after)
	}
}

// With no ownership rule, nothing is removed — which is what every caller
// written before this field existed depends on.
func TestWithoutAnOwnershipRuleNothingIsRemoved(t *testing.T) {
	url := remote(t)
	w := newWriter(memory.New(0))
	ctx := context.Background()

	if _, err := w.Deploy(ctx, request(url, renders(map[string][]byte{
		manifestPath: manifestBody("Workload", "app:1"),
		secondPath:   manifestBody("Workload", "worker:1"),
	}))); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	if _, err := w.Deploy(ctx, request(url, renders(map[string][]byte{
		manifestPath: manifestBody("Workload", "app:2"),
	}))); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	var found bool
	for _, name := range namesAt(t, url) {
		if name == secondPath {
			found = true
		}
	}
	if !found {
		t.Fatal("a request with no Owns removed a file anyway; a caller that has not said " +
			"which files are its own must keep the behaviour it was written against")
	}
}
