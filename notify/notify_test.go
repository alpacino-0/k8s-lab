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

// This is an external test on purpose: it reaches the package the way the
// server and a second transport do, so anything it needs that is not exported
// fails to compile here first.
package notify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/damgahq/damga/notify"
)

// The secret half of a webhook URL. Slack and Discord both put the credential
// in the path, so this stands for the part that must never reach a log.
const secretPath = "/services/T00000000/B00000000/verySecretToken"

// The fixture's tenant, and the state it reached. Named because three cases
// assert on each and a typo in one of them is a case that proves nothing.
const (
	someTenant  = "acme"
	someApp     = "api"
	someEnv     = "prod"
	stateFailed = "failed"
)

// urlFile writes a URL the way a mounted Secret presents one — with the
// trailing newline every editor and `echo` adds.
func urlFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "webhook-url")
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// receiver is a stand-in for Slack, Discord or somebody's own endpoint.
type receiver struct {
	*httptest.Server
	status int
	answer string
	bodies [][]byte
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK, answer: "ok"}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		r.bodies = append(r.bodies, body)
		w.WriteHeader(r.status)
		_, _ = io.WriteString(w, r.answer)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) last(t *testing.T) map[string]any {
	t.Helper()
	if len(r.bodies) == 0 {
		t.Fatal("the receiver was never posted to")
	}
	var out map[string]any
	if err := json.Unmarshal(r.bodies[len(r.bodies)-1], &out); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, r.bodies[len(r.bodies)-1])
	}
	return out
}

func anEvent() notify.Event {
	return notify.Event{
		Tenant: someTenant, App: someApp, Env: someEnv,
		State: stateFailed, Seq: 41,
		Image:  "registry.example.test/acme/api@sha256:abc",
		Commit: "5f1e0c1d0f9b7a2c3d4e5f60718293a4b5c6d7e8",
		Actor:  "Orhan Yavuz",
		Reason: "the commit was never pushed: remote rejected",
		At:     time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

// TestEveryFormatCarriesTheAppAndWhatHappened.
//
// The one assertion that has to hold whatever the receiver is: somebody reading
// the message can tell which application it is about and what happened to it.
// A body that is well-formed and says neither is a notification that gets
// muted.
func TestEveryFormatCarriesTheAppAndWhatHappened(t *testing.T) {
	for _, tc := range []struct {
		format notify.Format
		field  string
	}{
		{notify.FormatSlack, "text"},
		{notify.FormatDiscord, "content"},
		{notify.FormatWebhook, "text"},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			rec := newReceiver(t)
			hook, err := notify.NewWebhook(urlFile(t, rec.URL+secretPath), tc.format, time.Second)
			if err != nil {
				t.Fatalf("NewWebhook: %v", err)
			}
			if err := hook.Notify(context.Background(), anEvent()); err != nil {
				t.Fatalf("Notify: %v", err)
			}

			body := rec.last(t)
			line, _ := body[tc.field].(string)
			if line == "" {
				t.Fatalf("the %s body has no %q: %s", tc.format, tc.field, rec.bodies[0])
			}
			for _, want := range []string{someApp, someEnv, stateFailed, "the commit was never pushed"} {
				if !strings.Contains(line, want) {
					t.Errorf("the message does not mention %q: %s", want, line)
				}
			}
		})
	}
}

// TestTheGenericWebhookCarriesFieldsAsWellAsASentence.
//
// A receiver that routes on the app name needs the fields; one that pastes into
// a channel needs the sentence. Sending only the sentence makes every generic
// receiver parse English.
func TestTheGenericWebhookCarriesFieldsAsWellAsASentence(t *testing.T) {
	rec := newReceiver(t)
	hook, err := notify.NewWebhook(urlFile(t, rec.URL+secretPath), notify.FormatWebhook, time.Second)
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	if err := hook.Notify(context.Background(), anEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	event, ok := rec.last(t)["event"].(map[string]any)
	if !ok {
		t.Fatalf("no event object in the body: %s", rec.bodies[0])
	}
	for field, want := range map[string]any{
		"app": someApp, "env": someEnv, "tenant": someTenant, "state": stateFailed,
		"seq": float64(41), "commit": "5f1e0c1d0f9b7a2c3d4e5f60718293a4b5c6d7e8",
	} {
		if event[field] != want {
			t.Errorf("event.%s = %v, want %v", field, event[field], want)
		}
	}
}

// TestTheShapeIsReadOffTheHost, so one setting cannot disagree with the URL.
func TestTheShapeIsReadOffTheHost(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want notify.Format
	}{
		{"https://hooks.slack.com/services/T0/B0/xxx", notify.FormatSlack},
		{"https://discord.com/api/webhooks/1/xxx", notify.FormatDiscord},
		{"https://discordapp.com/api/webhooks/1/xxx", notify.FormatDiscord},
		{"https://alerts.example.test/hook", notify.FormatWebhook},
	} {
		hook, err := notify.NewWebhook(urlFile(t, tc.url), notify.FormatAuto, time.Second)
		if err != nil {
			t.Fatalf("NewWebhook(%s): %v", tc.url, err)
		}
		if got := hook.Format(); got != tc.want {
			t.Errorf("%s detected as %s, want %s", tc.url, got, tc.want)
		}
	}
}

// TestAURLThisCannotSendToIsRefusedAtStartup.
//
// Every one of these is found when the process starts rather than at the first
// failed deploy — which is the moment the notification exists for, and the
// worst possible moment to discover the setting was wrong.
func TestAURLThisCannotSendToIsRefusedAtStartup(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"empty file", "", "is empty"},
		{"a scheme nothing is posted over", "ftp://example.test/hook", "only http and https"},
		{"no host at all", "https:///hook", "has no host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := notify.NewWebhook(urlFile(t, tc.body), notify.FormatAuto, time.Second)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q: %v", tc.want, err)
			}
		})
	}

	if _, err := notify.NewWebhook(filepath.Join(t.TempDir(), "absent"), notify.FormatAuto, time.Second); err == nil {
		t.Error("a missing file was accepted")
	}

	if _, err := notify.NewWebhook(urlFile(t, "https://example.test/hook"), "carrier-pigeon", time.Second); err == nil {
		t.Error("a format nothing can send was accepted")
	} else if !strings.Contains(err.Error(), "slack, discord, webhook or auto") {
		t.Errorf("the refusal does not list what is allowed: %v", err)
	}
}

// TestTheURLIsNeverQuotedBackInAnError.
//
// The whole credential is in that string — for Slack and Discord it is the
// path — and an error message is the one place a secret reliably outlives the
// process that held it, because it is written to a log somebody else can read.
// Every refusal names the file instead.
func TestTheURLIsNeverQuotedBackInAnError(t *testing.T) {
	const token = "verySecretToken"
	for _, body := range []string{
		"ftp://hooks.slack.com/services/T0/B0/" + token,
		"https:///services/T0/B0/" + token,
		"ht tp://hooks.slack.com/" + token,
	} {
		_, err := notify.NewWebhook(urlFile(t, body), notify.FormatAuto, time.Second)
		if err == nil {
			t.Fatalf("%q was accepted", body)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("the refusal quotes the credential: %v", err)
		}
	}

	// And at send time. The host is allowed — it is not the secret and it is
	// what tells you which receiver went quiet — but the path is not.
	hook, err := notify.NewWebhook(urlFile(t, "http://127.0.0.1:1"+secretPath), notify.FormatWebhook, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	err = hook.Notify(context.Background(), anEvent())
	if err == nil {
		t.Fatal("a refused connection reported success")
	}
	if strings.Contains(err.Error(), "verySecretToken") {
		t.Errorf("the delivery failure quotes the credential: %v", err)
	}
}

// TestADeliveryFailureSaysWhichFailureItWas.
//
// "The notification was not sent" is true of a network that never carried it
// and of a receiver that refused the body, and those are fixed in two different
// places. This repository has lost rounds to a message that was true of four
// causes; this is the same rule applied before it costs one.
func TestADeliveryFailureSaysWhichFailureItWas(t *testing.T) {
	// Nothing is listening on port 1.
	unreachable, err := notify.NewWebhook(
		urlFile(t, "http://127.0.0.1:1/hook"), notify.FormatWebhook, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	err = unreachable.Notify(context.Background(), anEvent())
	if err == nil || !strings.Contains(err.Error(), "never answered") {
		t.Errorf("a refused connection reported %v, want it named as never answering", err)
	}

	rec := newReceiver(t)
	rec.status = http.StatusBadRequest
	rec.answer = `{"error":"invalid_payload"}`
	refusing, err := notify.NewWebhook(urlFile(t, rec.URL+"/hook"), notify.FormatSlack, time.Second)
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	err = refusing.Notify(context.Background(), anEvent())
	switch {
	case err == nil:
		t.Fatal("a 400 reported success")
	case !strings.Contains(err.Error(), "refused"):
		t.Errorf("a receiver that answered is not named as one: %v", err)
	case !strings.Contains(err.Error(), "invalid_payload"):
		// Its own words matter: a bare 400 reads as "damga sent something
		// wrong" without saying what.
		t.Errorf("the receiver's own words are missing: %v", err)
	case !strings.Contains(err.Error(), "slack"):
		t.Errorf("the message does not say which body shape was refused: %v", err)
	}
}

// ---------------------------------------------------------------- the store

// fakeStore is an evidence.Store that only answers Transition. The embedded
// interface is nil on purpose: anything else this test provokes a call to
// panics rather than quietly returning a zero value.
type fakeStore struct {
	evidence.Store
	give evidence.Record
	err  error
}

func (f *fakeStore) Transition(
	context.Context, evidence.ID, evidence.Transition,
) (evidence.Record, error) {
	return f.give, f.err
}

// counter records what it was asked to send.
type counter struct {
	events []notify.Event
	err    error
}

func (c *counter) Notify(_ context.Context, e notify.Event) error {
	c.events = append(c.events, e)
	return c.err
}

func recordIn(state evidence.State) evidence.Record {
	return evidence.Record{
		ID: "r-1", Seq: 41, State: state,
		Ref:   evidence.Ref{TenantID: someTenant, App: someApp, Env: someEnv},
		Actor: evidence.Actor{DisplayName: "Orhan Yavuz"},
		Image: evidence.Image{RequestedRef: "registry.example.test/acme/api:1"},
		Transitions: []evidence.Event{
			{To: state, Reason: "because the cluster said so"},
		},
	}
}

// TestOnlyTheStatesWorthWakingSomeoneForAreSent.
//
// Nine states exist and four are sent. Sending all nine is the same as sending
// none, because the first thing anybody does with a channel that reports every
// intermediate step is mute it — and then the one message that mattered is
// muted too.
func TestOnlyTheStatesWorthWakingSomeoneForAreSent(t *testing.T) {
	for _, tc := range []struct {
		state evidence.State
		want  bool
	}{
		{evidence.StateRunning, true},
		{evidence.StateFailed, true},
		{evidence.StateRejected, true},
		{evidence.StateUnknown, true},
		{evidence.StatePending, false},
		{evidence.StateSyncing, false},
		{evidence.StateApplied, false},
		{evidence.StateSuperseded, false},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			sent := &counter{}
			store := notify.Wrap(&fakeStore{give: recordIn(tc.state)}, sent, quiet())
			if _, err := store.Transition(context.Background(), "r-1", evidence.Transition{}); err != nil {
				t.Fatalf("Transition: %v", err)
			}
			if got := len(sent.events) == 1; got != tc.want {
				t.Errorf("%s sent %d notifications, want %d",
					tc.state, len(sent.events), map[bool]int{true: 1, false: 0}[tc.want])
			}
		})
	}
}

// TestNothingIsSentForATransitionThatDidNotHappen.
//
// After the store, never before: a message about a state change that then
// failed to persist cannot be reconciled with the page it points at.
func TestNothingIsSentForATransitionThatDidNotHappen(t *testing.T) {
	sent := &counter{}
	store := notify.Wrap(
		&fakeStore{give: recordIn(evidence.StateFailed), err: evidence.ErrConflict},
		sent, quiet())

	_, err := store.Transition(context.Background(), "r-1", evidence.Transition{})
	if !errors.Is(err, evidence.ErrConflict) {
		t.Fatalf("Transition returned %v, want ErrConflict", err)
	}
	if len(sent.events) != 0 {
		t.Errorf("a rejected transition still notified: %+v", sent.events)
	}
}

// TestANotificationNobodyReceivedDoesNotFailTheDeploy — and is not silent.
func TestANotificationNobodyReceivedDoesNotFailTheDeploy(t *testing.T) {
	var log bytes.Buffer
	store := notify.Wrap(
		&fakeStore{give: recordIn(evidence.StateFailed)},
		&counter{err: errors.New("hooks.example.test never answered")},
		slog.New(slog.NewTextHandler(&log, nil)))

	rec, err := store.Transition(context.Background(), "r-1", evidence.Transition{})
	if err != nil {
		t.Fatalf("a webhook being down failed the transition: %v", err)
	}
	if rec.State != evidence.StateFailed {
		t.Errorf("the record came back as %s", rec.State)
	}
	written := log.String()
	for _, want := range []string{"not delivered", someApp, "never answered"} {
		if !strings.Contains(written, want) {
			t.Errorf("the log does not mention %q:\n%s", want, written)
		}
	}
}

// TestNoNotifierMeansNoWrapper, so an install that configured nothing pays
// nothing and reads nothing.
func TestNoNotifierMeansNoWrapper(t *testing.T) {
	inner := &fakeStore{give: recordIn(evidence.StateFailed)}
	if got := notify.Wrap(inner, nil, quiet()); got != evidence.Store(inner) {
		t.Errorf("Wrap with no notifier returned %T, want the store it was given", got)
	}
}

// TestALongReasonIsMarkedWhereItWasCut, because Slack refuses a body over 4000
// characters and a sentence that merely stops is one somebody debugs as if it
// were whole.
func TestALongReasonIsMarkedWhereItWasCut(t *testing.T) {
	rec := recordIn(evidence.StateFailed)
	rec.Transitions[0].Reason = strings.Repeat("x", 4096)

	e := notify.EventFrom(rec)
	if len(e.Reason) > 700 {
		t.Errorf("the reason is %d characters; nothing bounded it", len(e.Reason))
	}
	if !strings.Contains(e.Reason, "truncated") {
		t.Errorf("the cut is not marked: %q", e.Reason[max(0, len(e.Reason)-80):])
	}
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }
