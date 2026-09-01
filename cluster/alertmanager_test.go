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

// Package cluster holds the tests for the manifests and values this repository
// ships to a cluster.
//
// A Go file for a YAML file, and the reason is the gate: `go test ./...` is
// what CI runs, and a rule proved by something nothing runs is decoration.
//
// # What is under test, and what a cheaper shape would have missed
//
// The alerting path was built and proved as far as Alertmanager's own API —
// scripts/alert-test.sh breaks the application, waits, and reads
// /api/v2/alerts — and it stopped there because the receiver was "null" and
// there was nothing to deliver to. That gap is what this closes.
//
// The obvious way to close it is to install kube-prometheus-stack in CI, break
// something and wait for a real alert to travel the whole way. It was costed
// and rejected, and the cost is mostly waiting: the fastest rule in
// chart/templates/prometheusrule.yaml is `for: 1m`, group_wait is 30s, and a
// scrape and an evaluation cycle sit on top — which is why alert-test.sh
// budgets four minutes for the arrival alone. What that buys is proof of an
// opt-in receiver that no default install uses.
//
// This runs a real Alertmanager as a local process and posts to the same
// /api/v2/alerts the second half of alert-test.sh already posts to, which skips
// Prometheus, the rule and the scrape entirely and leaves exactly the part that
// was never proved: does the receiver configuration this repository ships
// actually deliver, and does what arrives say anything useful.
//
// # What it deliberately does not prove
//
// That an alert produced by a real rule, scraped from a real service, travels
// this path in a real cluster. Nothing here starts Prometheus. That is a real
// gap and it is recorded as one rather than implied away by a green test.
//
// It also does not assert the inhibition at delivery. scripts/alert-test.sh
// checks it through the API, which is where it can be checked without a race:
// with group_wait at 0s both alerts enter the notification pipeline at once and
// which of them is evaluated first is timing, so an assertion here would be a
// flake wearing the clothes of a guarantee.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The receiver this repository defines and does not route to, and the two
// labels an alert has to arrive carrying.
//
// Named here so that renaming the receiver in the values file fails this test
// by name rather than by a delivery that quietly stops happening.
const (
	webhookReceiver = "damga-webhook"
	nullReceiver    = "null"

	probeAlert = "NoReadyReplicas"
	probeJob   = "app-damga-app"
)

// alertmanagerBinary finds the binary, or says how to get one.
//
// Skipped and not failed when it is missing, which is the arrangement the
// PostgreSQL conformance suite already uses: `go test ./...` stays useful on a
// laptop that has downloaded nothing, and the gate that stops the skip from
// becoming permanent is `make -f Makefile.operator test`, which depends on the
// download target.
func alertmanagerBinary(t *testing.T) string {
	t.Helper()
	if named := os.Getenv("ALERTMANAGER"); named != "" {
		if _, err := os.Stat(named); err == nil {
			return named
		}
	}
	if local, err := filepath.Abs(filepath.Join("..", "bin", "alertmanager")); err == nil {
		if _, err := os.Stat(local); err == nil {
			return local
		}
	}
	if onPath, err := exec.LookPath("alertmanager"); err == nil {
		return onPath
	}
	t.Skip("no alertmanager binary: run `make -f Makefile.operator alertmanager`, " +
		"or set ALERTMANAGER to one. `make -f Makefile.operator test` does it for you.")
	return ""
}

// shippedConfig is the alertmanager.config block out of the values file this
// repository installs with.
//
// Read from that file and never restated here. A second copy of the
// configuration beside the test is a copy that passes while the shipped one is
// broken, which is the entire failure this case exists to rule out.
func shippedConfig(t *testing.T) map[string]any {
	t.Helper()
	body, err := os.ReadFile("monitoring-values.yaml")
	if err != nil {
		t.Fatalf("reading the values this repository installs with: %v", err)
	}
	var values struct {
		Alertmanager struct {
			Config map[string]any `yaml:"config"`
		} `yaml:"alertmanager"`
	}
	if err := yaml.Unmarshal(body, &values); err != nil {
		t.Fatalf("monitoring-values.yaml does not parse: %v", err)
	}
	if len(values.Alertmanager.Config) == 0 {
		t.Fatal("monitoring-values.yaml carries no alertmanager.config; this case has nothing to test")
	}
	return values.Alertmanager.Config
}

// TestTheShippedAlertmanagerConfigStillDefaultsToDeliveringNowhere.
//
// The decision this file has carried from the start: where an alert should land
// is a property of whoever runs the cluster. Defining a receiver does not change
// that, and this is what says so — routing the default at the webhook would send
// every install's alerts at a URL file that install never created, and the
// symptom is a notification failure in a log nobody reads rather than an alert.
func TestTheShippedAlertmanagerConfigStillDefaultsToDeliveringNowhere(t *testing.T) {
	cfg := shippedConfig(t)

	route, _ := cfg["route"].(map[string]any)
	if got := route["receiver"]; got != nullReceiver {
		t.Errorf("the default route delivers to %v; it has to stay %q until an operator "+
			"chooses somewhere", got, nullReceiver)
	}
}

// TestTheWebhookReceiverCarriesAFileAndNotACredential.
//
// A values file is committed. The URL is the whole credential — whoever holds a
// Slack or Discord webhook URL can post into that channel as you — so the one
// thing this block must never grow is a `url`.
func TestTheWebhookReceiverCarriesAFileAndNotACredential(t *testing.T) {
	hook := webhookConfig(t, shippedConfig(t))

	if _, embedded := hook["url"]; embedded {
		t.Error("the webhook receiver names a url; a URL in a committed file is a credential " +
			"in a committed file, and url_file is why this line exists")
	}
	file, _ := hook["url_file"].(string)
	if file == "" {
		t.Fatal("the webhook receiver names no url_file, so nothing tells it where to post")
	}
	if !strings.HasPrefix(file, "/etc/alertmanager/secrets/") {
		// Where the kube-prometheus-stack chart mounts what
		// alertmanagerSpec.secrets names. A path outside it is one no Secret
		// ever appears at, and the failure is at the first alert.
		t.Errorf("url_file is %q; the stack mounts secrets under "+
			"/etc/alertmanager/secrets/<name>/, so nothing would appear there", file)
	}
}

// webhookConfig digs out the one webhook_configs entry, failing by name.
func webhookConfig(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	receivers, _ := cfg["receivers"].([]any)
	for _, r := range receivers {
		entry, _ := r.(map[string]any)
		if entry["name"] != webhookReceiver {
			continue
		}
		hooks, _ := entry["webhook_configs"].([]any)
		if len(hooks) != 1 {
			t.Fatalf("%s has %d webhook_configs; this case reads one", webhookReceiver, len(hooks))
		}
		out, _ := hooks[0].(map[string]any)
		return out
	}
	t.Fatalf("monitoring-values.yaml defines no receiver named %q", webhookReceiver)
	return nil
}

// TestAnAlertReachesTheWebhookSayingWhichAppAndWhichAlert.
//
// The whole path below Prometheus, with a real Alertmanager reading the
// configuration this repository ships.
//
// Three values are substituted and each is one an operator supplies rather than
// one under test: the route's receiver, which is the single line the comment in
// the values file tells them to change; url_file, which names a path that
// exists only inside a cluster; and group_wait, which is 30s in the shipped
// file and is the delay this shape exists to avoid paying on every run. The
// receiver definition, the routing, the body Alertmanager builds and the labels
// it carries are all the shipped ones.
func TestAnAlertReachesTheWebhookSayingWhichAppAndWhichAlert(t *testing.T) {
	binary := alertmanagerBinary(t)
	cfg := shippedConfig(t)

	delivered := make(chan []byte, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		delivered <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.Close)

	dir := t.TempDir()
	urlFile := filepath.Join(dir, "url")
	if err := os.WriteFile(urlFile, []byte(receiver.URL+"/hook\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	route, _ := cfg["route"].(map[string]any)
	route["receiver"] = webhookReceiver
	route["group_wait"] = "0s"
	webhookConfig(t, cfg)["url_file"] = urlFile

	rendered, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("re-encoding the shipped config: %v", err)
	}
	configFile := filepath.Join(dir, "alertmanager.yml")
	if err := os.WriteFile(configFile, rendered, 0o600); err != nil {
		t.Fatal(err)
	}

	addr := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// No clustering. Left on, Alertmanager binds 9094 as well, and two tests or
	// two checkouts on one machine then collide on a port neither asked for.
	am := exec.CommandContext(ctx, binary,
		"--config.file="+configFile,
		"--storage.path="+filepath.Join(dir, "data"),
		"--web.listen-address="+addr,
		"--cluster.listen-address=")
	var said strings.Builder
	am.Stdout, am.Stderr = &said, &said
	if err := am.Start(); err != nil {
		t.Fatalf("starting alertmanager: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = am.Wait()
		if t.Failed() {
			t.Logf("alertmanager said:\n%s", said.String())
		}
	})

	base := "http://" + addr
	waitReady(t, base, &said)

	// The shape a rule in chart/templates/prometheusrule.yaml produces: an
	// alertname, a severity, and the job label the route groups by.
	body := fmt.Sprintf(`[{"labels":{"alertname":%q,"severity":"critical","job":%q},`+
		`"annotations":{"summary":"%s has no ready replicas"},"endsAt":%q}]`,
		probeAlert, probeJob, probeJob,
		time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339))
	post(t, base+"/api/v2/alerts", body)

	select {
	case got := <-delivered:
		// Not "something arrived". A receiver that answers 200 to an empty body
		// would satisfy that, and so would a payload about a different
		// application — which is the failure a notification has to not have,
		// because the person reading it is deciding where to look.
		var payload struct {
			Receiver string `json:"receiver"`
			Status   string `json:"status"`
			Alerts   []struct {
				Labels map[string]string `json:"labels"`
			} `json:"alerts"`
		}
		if err := json.Unmarshal(got, &payload); err != nil {
			t.Fatalf("what arrived is not the JSON a webhook receiver parses: %v\n%s", err, got)
		}
		if payload.Receiver != webhookReceiver {
			t.Errorf("delivered to receiver %q, want %q", payload.Receiver, webhookReceiver)
		}
		if payload.Status != "firing" {
			t.Errorf("delivered with status %q, want firing", payload.Status)
		}
		if len(payload.Alerts) != 1 {
			t.Fatalf("delivered %d alerts, want 1: %s", len(payload.Alerts), got)
		}
		labels := payload.Alerts[0].Labels
		if labels["alertname"] != probeAlert {
			t.Errorf("the body names alert %q, want %q — a notification that cannot say which "+
				"alert fired is one somebody has to go and look up", labels["alertname"], probeAlert)
		}
		if labels["job"] != probeJob {
			t.Errorf("the body names job %q, want %q — without it nobody knows which "+
				"application is down", labels["job"], probeJob)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the alert never reached the webhook. alertmanager said:\n%s", said.String())
	}
}

// freeAddress asks the kernel for a port and gives it back.
//
// Racy in principle and the alternative is worse: a fixed port makes two
// checkouts on one machine fail for a reason that has nothing to do with the
// code, and this repository already writes that down where it binds one.
func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitReady(t *testing.T, base string, said *strings.Builder) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodGet, base+"/-/ready", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("alertmanager never became ready on %s. It said:\n%s", base, said.String())
}

func post(t *testing.T, url, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url,
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		said, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		t.Fatalf("POST %s answered %d: %s", url, resp.StatusCode, said)
	}
}
