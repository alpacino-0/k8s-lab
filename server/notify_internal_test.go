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

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/memory"
)

// notifyFile writes a URL where a mounted Secret would put one.
func notifyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify-url")
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAConfiguredWebhookIsReachedThroughTheStoreTheServerBuilds.
//
// The whole chain below the HTTP handler, with nothing substituted for the
// parts that can be real: the free evidence store, the real webhook sender and
// a receiver that records what it was posted. What it proves is the thing the
// unit tests cannot — that a Config carrying a URL file produces a store whose
// transitions reach that URL, rather than a notifier nobody wired.
func TestAConfiguredWebhookIsReachedThroughTheStoreTheServerBuilds(t *testing.T) {
	posted := make(chan string, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		posted <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.Close)

	var o Options
	o.Config.NotifyURLFile = notifyFile(t, receiver.URL+"/hook")
	store, err := o.withNotifications(memory.New(0), quietLogger())
	if err != nil {
		t.Fatalf("withNotifications: %v", err)
	}

	ctx := context.Background()
	rec, err := store.Append(ctx, evidence.Record{
		IdempotencyKey: "commit:abc:apps/api",
		Ref:            evidence.Ref{TenantID: "acme", App: appAPI, Env: envProd},
		Actor:          evidence.Actor{ID: "u-1", Kind: kindUser, DisplayName: "Orhan Yavuz"},
		Image:          evidence.Image{RequestedRef: "registry.example.test/acme/api:1"},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Syncing first, which must say nothing: it is damga narrating its own
	// progress, and a channel that carries it is one somebody mutes.
	if _, err := store.Transition(ctx, rec.ID, evidence.Transition{
		From: []evidence.State{evidence.StatePending}, To: evidence.StateSyncing,
		At: time.Now().UTC(), Reason: "pushed as abc",
	}); err != nil {
		t.Fatalf("Transition to syncing: %v", err)
	}
	select {
	case body := <-posted:
		t.Fatalf("syncing was announced: %s", body)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := store.Transition(ctx, rec.ID, evidence.Transition{
		From: []evidence.State{evidence.StateSyncing}, To: evidence.StateFailed,
		At: time.Now().UTC(), Reason: "the rollout never became available",
	}); err != nil {
		t.Fatalf("Transition to failed: %v", err)
	}

	select {
	case body := <-posted:
		for _, want := range []string{appAPI, envProd, "failed", "the rollout never became available"} {
			if !strings.Contains(body, want) {
				t.Errorf("the delivered body does not mention %q: %s", want, body)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a failed deploy reached the store and nothing was posted")
	}
}

// TestAnUnusableNotifyFileStopsTheServerStarting.
//
// Refused at startup and not at the first failed deploy, which is the moment
// the notification exists for. Asserted against Run itself, because the promise
// is about the process and not about a helper: a control plane that starts,
// serves, and then cannot tell anybody the thing it was configured to tell them
// has failed in the one way nobody looks for.
func TestAnUnusableNotifyFileStopsTheServerStarting(t *testing.T) {
	var o Options
	o.Config.ListenAddr = "127.0.0.1:0"
	o.Config.NotifyURLFile = notifyFile(t, "ftp://hooks.example.test/nope")
	o.Ready = func(string) { t.Error("the server bound a listener despite an unusable notify URL") }

	err := Run(context.Background(), o)
	if err == nil {
		t.Fatal("Run started with a URL it cannot post to")
	}
	if !strings.Contains(err.Error(), "only http and https") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	if !strings.Contains(err.Error(), o.Config.NotifyURLFile) {
		// The file and never the URL: the file is where the fix is, and the URL
		// is the credential.
		t.Errorf("the refusal does not name the file to fix: %v", err)
	}
}

// TestNothingConfiguredLeavesTheStoreAlone, so an install that wants no
// notifications pays for none.
func TestNothingConfiguredLeavesTheStoreAlone(t *testing.T) {
	inner := memory.New(0)
	var o Options
	got, err := o.withNotifications(inner, quietLogger())
	if err != nil {
		t.Fatalf("withNotifications: %v", err)
	}
	if got != evidence.Store(inner) {
		t.Errorf("an install with no webhook got %T wrapped around its store", got)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
