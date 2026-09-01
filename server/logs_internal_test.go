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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/damgahq/damga/placement"
)

const (
	// What a container says when it comes up, spelled once because three cases
	// assert on it and a fourth reads it out of a timestamped line.
	firstLine = "listening on :8080"

	podAPI1      = "api-1"
	containerApp = "app"

	// The header a browser sends and the CLI does not. It is the whole of what
	// this endpoint's refusal is decided on, so it is named rather than typed
	// out at each of the four cases that turn on it.
	secFetchSite = "Sec-Fetch-Site"
)

// fakeLogs is the cluster, minus the cluster. It records the selector it was
// given, because half of what this endpoint does is turn a path and a query
// into one.
type fakeLogs struct {
	lines []LogLine
	err   error
	got   LogSelector
}

func (f *fakeLogs) Stream(_ context.Context, sel LogSelector, emit func(LogLine) error) error {
	f.got = sel
	for _, l := range f.lines {
		if err := emit(l); err != nil {
			return err
		}
	}
	return f.err
}

func fixedSource(src LogSource) func() (LogSource, error) {
	return func() (LogSource, error) { return src, nil }
}

// placeAPIProd gives the app somewhere to be. Without a placement there is no
// namespace, and every case below would be answered by the same 404.
func (h *harness) placeAPIProd() {
	h.t.Helper()
	if _, err := h.places.Put(context.Background(), placement.Placement{
		TenantID: tenantHome, App: appAPI, Env: envProd,
		RepoURL: homeRepo, Branch: branchMain, Path: pathAPIProd,
		Namespace: nsHomeProd,
	}); err != nil {
		h.t.Fatalf("Put: %v", err)
	}
}

// logsCall drives the route through a mux, so {tenant}, {app} and {env} are set
// the way the router sets them. The shared harness helper substitutes two of
// the three, and this endpoint is the first that needs the environment.
func (h *harness) logsCall(
	open func() (LogSource, error), query, account string, headers http.Header,
) *httptest.ResponseRecorder {
	h.t.Helper()
	const suffix = "/apps/{app}/envs/{env}/logs"

	mux := http.NewServeMux()
	mux.Handle(http.MethodGet+" "+tenantScope+suffix, logStream(h.guard, h.stores, open))

	target := strings.NewReplacer(
		"{tenant}", tenantHome, "{app}", appAPI, "{env}", envProd,
	).Replace(tenantScope + suffix)
	req := httptest.NewRequest(http.MethodGet, target+query, nil)
	req.Host = testHost
	for name, values := range headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if account != "" {
		req.AddCookie(h.cookies[account])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

type sseFrame struct{ event, data string }

// frames parses the stream the way an EventSource does, which is the only
// reading of it that counts. A comment frame — the heartbeat — is skipped, as
// a reader skips it.
func frames(t *testing.T, body string) []sseFrame {
	t.Helper()
	var out []sseFrame
	for block := range strings.SplitSeq(strings.TrimSpace(body), "\n\n") {
		if block == "" || strings.HasPrefix(block, ":") {
			continue
		}
		var f sseFrame
		for line := range strings.SplitSeq(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
		out = append(out, f)
	}
	return out
}

func (f sseFrame) decode(t *testing.T) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(f.data), &got); err != nil {
		t.Fatalf("event %q carried data that is not JSON: %v (%q)", f.event, err, f.data)
	}
	return got
}

// The trap this endpoint was written around.
//
// run.go wraps everything in http.CrossOriginProtection, and that control
// allows every GET — deliberately, because a safe method changes nothing. A log
// stream is a GET that returns a tenant's output, so any page anywhere that can
// make a browser open it with the session cookie attached reads that output.
// Nothing above this handler will stop that, and the comment in run.go says so.
func TestLogStreamRefusesACrossOriginBrowser(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers http.Header
		want    int
	}{
		{"a page on another site", http.Header{secFetchSite: {"cross-site"}}, http.StatusForbidden},
		// The case SameSite cookies would not have caught, and the reason
		// run.go chose an origin-scoped control over the cookie attribute: a
		// sibling subdomain is same-site, and on an internal platform there
		// are usually several.
		{"a sibling subdomain", http.Header{secFetchSite: {"same-site"}}, http.StatusForbidden},
		{"an Origin that is not this host",
			http.Header{"Origin": {"https://evil.example.test"}}, http.StatusForbidden},

		{"the panel itself", http.Header{secFetchSite: {"same-origin"}}, http.StatusOK},
		{"the panel, by Origin", http.Header{"Origin": {"http://" + testHost}}, http.StatusOK},
		// Neither header is sent by anything that is not a browser, and the
		// CLI is not a browser. Refusing these would refuse curl.
		{"a command line client", nil, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.placeAPIProd()
			src := &fakeLogs{lines: []LogLine{{Pod: podAPI1, Container: containerApp, Text: "secret"}}}

			rec := h.logsCall(fixedSource(src), "", accOwner, c.headers)

			if rec.Code != c.want {
				t.Errorf("%s = %d, want %d — the CSRF control in run.go allows every GET, "+
					"so this endpoint has to make this decision itself", c.name, rec.Code, c.want)
			}
			if c.want == http.StatusForbidden && strings.Contains(rec.Body.String(), "secret") {
				t.Error("a cross-origin caller was refused and read the log lines anyway")
			}
		})
	}
}

// The order matters as much as the answer. Refusing after the session is
// resolved answers "not signed in" to a cross-origin probe, which tells it
// whether the cookie it borrowed is any good.
func TestLogStreamRefusesCrossOriginBeforeItReadsTheSession(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()

	rec := h.logsCall(fixedSource(&fakeLogs{}), "", "",
		http.Header{secFetchSite: {"cross-site"}})

	if rec.Code != http.StatusForbidden {
		t.Errorf("a cross-origin request with no cookie = %d, want 403: the origin is refused "+
			"before the session is looked at, so the answer cannot report on the cookie",
			rec.Code)
	}
}

func TestLogStreamSendsWhatTheContainersWrote(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()
	at := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	src := &fakeLogs{lines: []LogLine{
		{Pod: podAPI1, Container: containerApp, At: at, Text: firstLine},
		{Pod: podAPI1, Container: "sidecar", Text: "no timestamp here"},
	}}

	rec := h.logsCall(fixedSource(src), "?tail=50&follow=false", accOwner, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	// nginx buffers a proxied response by default, and a buffered stream
	// arrives in one piece when it ends — which for a followed log is never.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	got := frames(t, rec.Body.String())
	if len(got) != 3 {
		t.Fatalf("got %d events, want two lines and an end: %q", len(got), rec.Body.String())
	}
	first := got[0].decode(t)
	if got[0].event != "line" || first["text"] != firstLine {
		t.Errorf("first event = %+v, want the first line the container wrote", got[0])
	}
	if first["pod"] != podAPI1 || first["container"] != containerApp {
		t.Errorf("a line that does not say which container wrote it: %+v", first)
	}
	if first["at"] != "2026-09-01T10:30:00Z" {
		t.Errorf("at = %v, want the time the cluster recorded", first["at"])
	}
	// A line with no timestamp is a line. The kubelet adds the prefix and a
	// container is free to write something that is not one.
	if second := got[1].decode(t); second["at"] != "" {
		t.Errorf("a line with no timestamp reported one: %v", second["at"])
	}
	if got[2].event != "end" {
		t.Errorf("last event = %q, want end: a stream that stops without saying so "+
			"cannot be told from one that was cut", got[2].event)
	}
	if lines := got[2].decode(t)["lines"]; lines != float64(2) {
		t.Errorf("end reported %v lines, want 2", lines)
	}

	// The path and the query become one selector, and the namespace is not
	// part of the query: it comes from the placement row.
	want := LogSelector{Namespace: nsHomeProd, App: appAPI, Tail: 50, Follow: false}
	if src.got != want {
		t.Errorf("selector = %+v, want %+v", src.got, want)
	}
}

// Scaled to zero is not a failure, and an error frame would make the page say
// something broke.
func TestLogStreamReportsNothingRunningAsAnEnd(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()

	rec := h.logsCall(fixedSource(&fakeLogs{err: ErrNoPods}), "", accOwner, nil)

	got := frames(t, rec.Body.String())
	if len(got) != 1 || got[0].event != "end" {
		t.Fatalf("events = %+v, want a single end", got)
	}
	if reason, _ := got[0].decode(t)["reason"].(string); reason == "" {
		t.Error("an app with no pods ended with no reason, so the page cannot say which " +
			"silence this is: nothing running, or nothing written")
	}
}

// The status line is spent by the time anything goes wrong, and a stream that
// answers a mid-flight failure by stopping is one the reader cannot tell from
// an application that went quiet.
func TestLogStreamReportsAFailureItCannotStillStatusCode(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()
	src := &fakeLogs{
		lines: []LogLine{{Pod: podAPI1, Container: containerApp, Text: "still here"}},
		err:   errors.New("the API server hung up"),
	}

	rec := h.logsCall(fixedSource(src), "", accOwner, nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d: the failure happened after the headers, and 200 is "+
			"already on the wire", rec.Code)
	}
	got := frames(t, rec.Body.String())
	if len(got) != 2 {
		t.Fatalf("events = %+v, want the line that arrived and then the failure", got)
	}
	if got[0].event != "line" || !strings.Contains(got[0].data, "still here") {
		t.Errorf("the line delivered before the failure was dropped: %+v", got[0])
	}
	if got[1].event != "error" {
		t.Errorf("last event = %q, want error", got[1].event)
	}
	// The cluster's own words stay in the process log. They name the
	// ServiceAccount and the resource it was refused.
	if strings.Contains(got[1].data, "hung up") {
		t.Errorf("the source's error was handed to the caller: %s", got[1].data)
	}
}

func TestLogStreamNeedsAPlacement(t *testing.T) {
	h := newHarness(t)

	rec := h.logsCall(fixedSource(&fakeLogs{}), "", accOwner, nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("an app with no placement = %d, want 404: there is no namespace to "+
			"read, and guessing one would be reading a name out of an identity", rec.Code)
	}
}

// An install that cannot reach a cluster says so. An empty 200 would be
// indistinguishable from an application that is writing nothing.
func TestLogStreamSaysWhenTheInstallCannotRead(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()
	broken := func() (LogSource, error) { return nil, errors.New("not in a cluster") }

	rec := h.logsCall(broken, "", accOwner, nil)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "not in a cluster") {
		t.Errorf("the internal error reached the caller: %s", rec.Body.String())
	}
}

func TestLogSelectorFrom(t *testing.T) {
	for _, c := range []struct {
		query string
		want  LogSelector
		bad   bool
	}{
		{query: "", want: LogSelector{Tail: defaultTail, Follow: true}},
		{query: "?tail=10", want: LogSelector{Tail: 10, Follow: true}},
		{query: "?follow=false", want: LogSelector{Tail: defaultTail, Follow: false}},
		{query: "?tail=0", want: LogSelector{Tail: 0, Follow: true}},
		// Refused rather than clamped. A page that asked for more than the
		// ceiling and silently received the ceiling shows a gap where the
		// missing lines were, and nothing on screen says why.
		{query: "?tail=100000", bad: true},
		{query: "?tail=-1", bad: true},
		{query: "?tail=lots", bad: true},
		{query: "?follow=maybe", bad: true},
	} {
		t.Run(c.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/logs"+c.query, nil)
			got, err := logSelectorFrom(req)
			switch {
			case c.bad && err == nil:
				t.Fatalf("%q was accepted as %+v", c.query, got)
			case c.bad:
				return
			case err != nil:
				t.Fatalf("%q was refused: %v", c.query, err)
			case got != c.want:
				t.Errorf("%q = %+v, want %+v", c.query, got, c.want)
			}
		})
	}
}

func TestSplitStamp(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 30, 0, 500000000, time.UTC)
	for _, c := range []struct {
		name string
		line string
		want time.Time
		text string
	}{
		{"what the kubelet writes", "2026-09-01T10:30:00.5Z " + firstLine, at, firstLine},
		{"a line with spaces in it", "2026-09-01T10:30:00.5Z one two three", at, "one two three"},
		// Not a tolerance. The prefix is added by the API server when it is
		// asked for, and a container may legitimately write a date first.
		{"a line that only looks like one", "2026-09-01 something happened",
			time.Time{}, "2026-09-01 something happened"},
		{"a single word", "starting", time.Time{}, "starting"},
		{"an empty line", "", time.Time{}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			gotAt, gotText := splitStamp(c.line)
			if !gotAt.Equal(c.want) {
				t.Errorf("time = %v, want %v", gotAt, c.want)
			}
			if gotText != c.text {
				t.Errorf("text = %q, want %q", gotText, c.text)
			}
		})
	}
}

// The one place this package restates something the operator decides. If
// internal/controller/resources.go ever labels a pod differently, this is the
// line that has to change with it — and this is what says so out loud.
func TestPodSelectorIsTheLabelPairTheOperatorWrites(t *testing.T) {
	const want = "app.kubernetes.io/name=api,app.kubernetes.io/instance=api"
	if got := podSelector("api"); got != want {
		t.Errorf("podSelector = %q, want %q — labelsFor in "+
			"internal/controller/resources.go is what this has to match", got, want)
	}
}
