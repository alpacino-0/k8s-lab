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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// The phase a pod reports while its container is up, which four cases here
	// assert on — including the one that matters most, where a pod is Running
	// and not Ready at the same time.
	phaseRunning = "Running"

	// What a browser sends when another origin made it ask.
	crossSite = "cross-site"
)

// fakeMetrics is the cluster, minus the cluster. It records the namespace and
// app it was asked for, because half of what this endpoint does is turn a path
// into those two through a placement.
type fakeMetrics struct {
	out       AppMetrics
	err       error
	namespace string
	app       string
}

func (f *fakeMetrics) ForApp(_ context.Context, namespace, app string) (AppMetrics, error) {
	f.namespace, f.app = namespace, app
	return f.out, f.err
}

func fixedMetrics(src MetricsSource) func() (MetricsSource, error) {
	return func() (MetricsSource, error) { return src, nil }
}

// metricsCall drives the route through a mux, so {tenant}, {app} and {env} are
// set the way the router sets them.
func (h *harness) metricsCall(
	open func() (MetricsSource, error), account string, headers http.Header,
) *httptest.ResponseRecorder {
	h.t.Helper()
	const suffix = "/apps/{app}/envs/{env}/metrics"

	mux := http.NewServeMux()
	mux.Handle(http.MethodGet+" "+tenantScope+suffix, appMetricsFrom(h.guard, h.stores, open))

	target := strings.NewReplacer(
		"{tenant}", tenantHome, "{app}", appAPI, "{env}", envProd,
	).Replace(tenantScope + suffix)
	req := httptest.NewRequest(http.MethodGet, target, nil)
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

func decodeMetrics(t *testing.T, rec *httptest.ResponseRecorder) wireMetrics {
	t.Helper()
	var got wireMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the body is not the expected JSON: %v (%s)", err, rec.Body.String())
	}
	return got
}

// The whole point of the endpoint: a user seeing that their application is
// being killed, and what killed it.
func TestTheAnswerSaysWhyAPodWasKilled(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()
	src := &fakeMetrics{out: AppMetrics{
		UsageSource: "metrics.k8s.io",
		Pods: []PodSample{{
			Name: podAPI1, Phase: phaseRunning, Ready: true, Restarts: 7,
			LastTerminated: &Termination{
				Reason: "OOMKilled", ExitCode: 137,
				At: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
			},
			Memory: Sample{
				Usage:   resource.NewQuantity(500*1024*1024, resource.BinarySI),
				Request: resource.NewQuantity(128*1024*1024, resource.BinarySI),
				Limit:   resource.NewQuantity(512*1024*1024, resource.BinarySI),
			},
		}},
	}}

	rec := h.metricsCall(fixedMetrics(src), accViewer, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeMetrics(t, rec)

	if src.namespace != nsHomeProd || src.app != appAPI {
		t.Errorf("asked the cluster for %s/%s, want %s/%s", src.namespace, src.app, nsHomeProd, appAPI)
	}
	if len(got.Pods) != 1 {
		t.Fatalf("pods = %d", len(got.Pods))
	}
	pod := got.Pods[0]
	if pod.Restarts != 7 {
		t.Errorf("restarts = %d, want 7", pod.Restarts)
	}
	if pod.LastTerminated == nil || pod.LastTerminated.Reason != "OOMKilled" {
		t.Fatalf("lastTerminated = %+v, want the OOMKill that explains the restarts", pod.LastTerminated)
	}
	if pod.LastTerminated.ExitCode != 137 {
		t.Errorf("exitCode = %d, want 137", pod.LastTerminated.ExitCode)
	}
	// Usage without the limit beside it is a number nobody can act on: 500Mi
	// is fine against 2Gi and an outage against 512Mi.
	if pod.Memory.Usage == "" || pod.Memory.Limit == "" {
		t.Errorf("memory = %+v, want usage and the limit it is measured against", pod.Memory)
	}
	if pod.Memory.OfLimit == nil || *pod.Memory.OfLimit != 97 {
		t.Errorf("ofLimitPercent = %v, want 97 (500Mi of 512Mi)", pod.Memory.OfLimit)
	}
}

// Usage comes from a component an install may not have, and the pod facts come
// from the API server, which it always has. Losing the second to the first
// would turn "no metrics-server here" into "this app has no pods".
func TestPodFactsSurviveMetricsServerBeingAbsent(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()
	src := &fakeMetrics{out: AppMetrics{
		UsageNote: "live CPU and memory need metrics-server, and metrics.k8s.io is not answering in this cluster",
		Pods: []PodSample{{
			Name: podAPI1, Phase: phaseRunning, Ready: true, Restarts: 2,
			Memory: Sample{Request: resource.NewQuantity(128*1024*1024, resource.BinarySI)},
		}},
	}}

	rec := h.metricsCall(fixedMetrics(src), accViewer, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d, want the pod facts to be served anyway: %s", rec.Code, rec.Body.String())
	}
	got := decodeMetrics(t, rec)

	if len(got.Pods) != 1 || got.Pods[0].Restarts != 2 {
		t.Errorf("pods = %+v, want the facts the API server could answer", got.Pods)
	}
	if got.Usage.Source != "" {
		t.Errorf("usage.source = %q, want it empty when nothing answered", got.Usage.Source)
	}
	if !strings.Contains(got.Usage.Note, "metrics-server") {
		t.Errorf("usage.note = %q, want it to name the component that is missing", got.Usage.Note)
	}
	// A zero would be a number, and a wrong one.
	if got.Pods[0].Memory.Usage != "" || got.Pods[0].Memory.OfLimit != nil {
		t.Errorf("memory = %+v, want no usage rather than a zero", got.Pods[0].Memory)
	}
}

// A panel drawing a latency chart from a field that is simply absent draws a
// flat line at zero. Saying what is not answered is cheaper than the bug report.
func TestTheAnswerSaysWhatItDoesNotAnswer(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()

	rec := h.metricsCall(fixedMetrics(&fakeMetrics{}), accViewer, nil)
	got := decodeMetrics(t, rec)

	joined := strings.Join(got.Limits, "\n")
	for _, want := range []string{"latency", "history"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the answer does not say that %s is missing:\n%s", want, joined)
		}
	}
}

// The same exception logs documents. This is a GET carrying one tenant's own
// operational state, so the CSRF control's blanket allowance for safe methods
// is wrong here and is not inherited.
func TestTheMetricsEndpointRefusesACrossOriginRead(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()

	rec := h.metricsCall(fixedMetrics(&fakeMetrics{}), accViewer,
		http.Header{secFetchSite: []string{crossSite}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a cross-site read = %d, want 403", rec.Code)
	}
	// Refused before the session is resolved, so the answer cannot be used to
	// tell a good cookie from a bad one.
	anonymous := h.metricsCall(fixedMetrics(&fakeMetrics{}), "",
		http.Header{secFetchSite: []string{crossSite}})
	if anonymous.Code != rec.Code {
		t.Errorf("cross-site with no cookie = %d and with one = %d; the difference is an oracle",
			anonymous.Code, rec.Code)
	}
}

// An install whose control plane is not in a cluster cannot read a pod, and
// saying so is different from reporting an application with no replicas.
func TestNoClusterSaysSoRatherThanReportingNoPods(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()

	rec := h.metricsCall(func() (MetricsSource, error) {
		return nil, errors.New("reading pod state needs the cluster this pod is in")
	}, accViewer, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("= %d, want 501: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "\"pods\"") {
		t.Errorf("the refusal carries a pod list: %s", rec.Body.String())
	}
}

// An app nobody has placed has no namespace, and guessing one would read
// whatever happens to be running under that name.
func TestAnAppWithNoPlacementIsNotFound(t *testing.T) {
	h := newHarness(t)

	rec := h.metricsCall(fixedMetrics(&fakeMetrics{}), accViewer, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("= %d, want 404", rec.Code)
	}
}

// samplePod is where two Kubernetes shapes become one, and both halves have a
// trap in them: restarts are per container and have to be summed, and a limit
// that is absent means unbounded while a limit of zero means the opposite.
func TestSamplePodReadsBothHalvesOfAPod(t *testing.T) {
	started := metav1.NewTime(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podAPI1},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: containerApp,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		}}},
		Status: corev1.PodStatus{
			Phase:     corev1.PodRunning,
			StartTime: &started,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: 3},
				// A sidecar that has also been restarted. Reporting only the
				// first container would under-report a pod that is thrashing.
				{RestartCount: 4, LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
				}},
			},
		},
	}

	got := samplePod(pod)
	if got.Restarts != 7 {
		t.Errorf("restarts = %d, want 3+4 summed across containers", got.Restarts)
	}
	if !got.Ready || got.Phase != phaseRunning {
		t.Errorf("ready=%v phase=%q", got.Ready, got.Phase)
	}
	if got.LastTerminated == nil || got.LastTerminated.Reason != "Error" {
		t.Errorf("lastTerminated = %+v", got.LastTerminated)
	}
	if got.Memory.Limit == nil || got.Memory.Limit.String() != "512Mi" {
		t.Errorf("memory limit = %v", got.Memory.Limit)
	}
	if got.CPU.Limit != nil {
		// The Workload API sets no CPU limit on purpose — throttling is worse
		// than letting a container finish — so absent has to stay absent
		// rather than becoming a zero that reads as "no CPU allowed".
		t.Errorf("cpu limit = %v, want nil when none is set", got.CPU.Limit)
	}
}

// clusterMetrics has three answers about usage and they must stay three: the
// API is absent, the API answered with a sample, and the API answered and had
// nothing for these pods. The third was found by running this against a
// crash-looping application — every pod came back with no usage under a source
// that said it had answered, which reads as "this app uses no memory".
func TestAnAnsweringMetricsAPIWithNoSampleSaysSo(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()
	src := &fakeMetrics{out: AppMetrics{
		// What ForApp produces when the API answered and matched no pod.
		UsageSource: usageSourceName,
		UsageNote: "metrics.k8s.io answered but has no sample for these pods yet: a container " +
			"that has just started, or is restarting faster than the scrape interval, is never scraped",
		Pods: []PodSample{{Name: podAPI1, Phase: phaseRunning, Restarts: 6}},
	}}

	got := decodeMetrics(t, h.metricsCall(fixedMetrics(src), accViewer, nil))
	if got.Usage.Source != usageSourceName {
		t.Errorf("usage.source = %q, want the API that did answer", got.Usage.Source)
	}
	if !strings.Contains(got.Usage.Note, "no sample") {
		t.Errorf("usage.note = %q, want it to separate an empty answer from a missing API", got.Usage.Note)
	}
	if got.Pods[0].Memory.Usage != "" {
		t.Errorf("memory usage = %q, want it absent rather than zero", got.Pods[0].Memory.Usage)
	}
}

// metrics.k8s.io reports CPU in nanocores, and an idle application's honest
// answer is "518738n" — measured against a real cluster, beside a `kubectl top
// pod` that said "1m" for the same container. Both are correct and no reader
// would believe they were the same number, so usage is rendered in the unit
// every other CPU figure in this product uses.
func TestCPUUsageIsRenderedInTheUnitTheRestOfTheProductUses(t *testing.T) {
	h := newHarness(t)
	h.placeAPIProd()
	nanocores := resource.NewQuantity(518738, resource.DecimalSI)
	nanocores.Format = resource.DecimalSI
	src := &fakeMetrics{out: AppMetrics{
		UsageSource: usageSourceName,
		Pods: []PodSample{{
			Name: podAPI1,
			CPU: Sample{
				// 518738 nanocores, which is how metrics.k8s.io says "half a
				// milli-core".
				Usage:   resource.NewScaledQuantity(518738, resource.Nano),
				Request: ptrQuantity(resource.MustParse("100m")),
			},
		}},
	}}

	got := decodeMetrics(t, h.metricsCall(fixedMetrics(src), accViewer, nil))
	if strings.HasSuffix(got.Pods[0].CPU.Usage, "n") {
		t.Errorf("cpu usage = %q, want milli-cores rather than nanocores", got.Pods[0].CPU.Usage)
	}
	// The request is what the tenant wrote and must survive untouched.
	if got.Pods[0].CPU.Request != "100m" {
		t.Errorf("cpu request = %q, want the 100m the workload asked for", got.Pods[0].CPU.Request)
	}
}

func ptrQuantity(q resource.Quantity) *resource.Quantity { return &q }
