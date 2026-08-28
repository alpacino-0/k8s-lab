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

package deploywatch_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/internal/deploywatch"
)

// envtest runs kube-apiserver and etcd and nothing else. There is no
// kube-controller-manager, so a Deployment created here never gets a ReplicaSet,
// never gets Pods, and never has its .status written by anything — measured.
//
// That is what makes it usable rather than what makes it useless. The status
// subresource accepts and keeps whatever is written to it precisely because
// nothing else is competing to own it, so a test can put the object in the exact
// shape the reconciler has to read. The shapes below are copied from live
// objects on a real cluster rather than invented, because the alternative is a
// test that agrees with the reconciler about a fiction.
//
// The one rule the apiserver does enforce: updatedReplicas, readyReplicas and
// availableReplicas must each be no greater than status.replicas.

var testCfg *rest.Config

func TestMain(m *testing.M) {
	env := &envtest.Environment{}
	if dir := envtestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		// Skipping rather than failing: this suite needs binaries that
		// `make -f Makefile.operator test` fetches, and a bare `go test ./...`
		// should not be a failure because of that.
		println("deploywatch: envtest unavailable, skipping:", err.Error())
		os.Exit(0)
	}
	testCfg = cfg
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// envtestBinaryDir mirrors the operator suite's lookup so both find the same
// binaries.
func envtestBinaryDir() string {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	_, file, _, _ := runtime.Caller(0)
	base := filepath.Join(filepath.Dir(file), "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "1.") {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// deployName is the object every case creates. One name, because the cases
// differ in what the object says and never in what it is called.
const deployName = "api"

type fixture struct {
	c      client.Client
	store  evidence.Store
	r      *deploywatch.Reconciler
	ns     string
	record evidence.Record
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c, err := client.New(testCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ns := "dw-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	if len(ns) > 60 {
		// Trimmed of trailing hyphens after the cut, not just cut. A name that
		// happens to be sliced mid-word leaves one, and a namespace may not end
		// in a hyphen — so a long enough test function failed on its own name
		// with an error about RFC 1123 rather than about anything it asserted.
		ns = strings.TrimRight(ns[:60], "-")
	}
	if err := c.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	store := memory.New(0)
	rec, err := store.Append(context.Background(), evidence.Record{
		IdempotencyKey: "commit:" + ns,
		Ref:            testRef,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return &fixture{
		c: c, store: store, ns: ns, record: rec,
		r: &deploywatch.Reconciler{Client: c, Evidence: store},
	}
}

// deploy creates a Deployment carrying the annotations Damga puts on
// it, and returns it. Status is written separately: the apiserver strips any
// status supplied on create.
func (f *fixture) deploy(t *testing.T, annotations map[string]string) *appsv1.Deployment {
	const name = deployName
	t.Helper()
	two := int32(2)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Replicas: &two,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "c", Image: "ghcr.io/damgahq/damga:1.0.0",
				}}},
			},
		},
	}
	if err := f.c.Create(context.Background(), d); err != nil {
		t.Fatalf("creating the deployment: %v", err)
	}
	return d
}

// settled puts the Deployment in the shape a finished rollout has, copied from
// a live cluster: observedGeneration caught up, every replica updated and ready,
// and the two conditions the deployment controller actually writes.
func (f *fixture) settled(t *testing.T, d *appsv1.Deployment, ready int32) {
	t.Helper()
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: d.Generation,
		Replicas:           2,
		UpdatedReplicas:    2,
		ReadyReplicas:      ready,
		AvailableReplicas:  ready,
		Conditions: []appsv1.DeploymentCondition{
			{
				Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
				Reason: "MinimumReplicasAvailable", LastUpdateTime: metav1.Now(),
			},
			{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue,
				Reason: "NewReplicaSetAvailable", LastUpdateTime: metav1.Now(),
			},
		},
	}
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("writing the deployment status: %v", err)
	}
}

func (f *fixture) reconcile(t *testing.T) {
	const name = deployName
	t.Helper()
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: f.ns, Name: name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (f *fixture) state(t *testing.T) evidence.Record {
	t.Helper()
	got, err := f.store.Get(context.Background(), f.record.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got
}

// The happy path, and the two things it has to carry: the record moves to
// running, and it learns the image reference that is actually running.
//
// The reference and not a digest, and the difference is recorded rather than
// papered over: this is what the Deployment's own spec says, which is what git
// wrote. A tag means "whatever that name pointed at when the kubelet pulled
// it", and claiming a digest here would be claiming something nothing checked.
func TestFinishedRolloutBecomesRunning(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: string(f.record.ID),
	})
	f.settled(t, d, 2)
	f.reconcile(t)

	got := f.state(t)
	if got.State != evidence.StateRunning {
		t.Fatalf("state = %q, want running", got.State)
	}
	if got.Image.RequestedRef != "ghcr.io/damgahq/damga:1.0.0" {
		t.Errorf("image = %+v, want the reference the Deployment carries", got.Image)
	}
}

// The reconciler runs constantly. Ten passes over an unchanged object must
// produce one transition, or the record's history becomes a log of how often the
// controller woke up.
func TestRepeatedReconcilesWriteOnce(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: string(f.record.ID),
	})
	f.settled(t, d, 2)
	for range 10 {
		f.reconcile(t)
	}

	if n := len(f.state(t).Transitions); n != 1 {
		t.Errorf("transitions = %d after ten reconciles, want 1", n)
	}
}

// A rollout in flight is not a state. Writing "applied" here would be a guess,
// and writing "running" would be a lie; the reconciler waits.
func TestMidRolloutIsNotYetRunning(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: string(f.record.ID),
	})
	f.settled(t, d, 1) // one of two ready
	f.reconcile(t)

	if got := f.state(t); got.State != evidence.StateApplied {
		t.Errorf("state = %q, want applied while the rollout is still moving", got.State)
	}
}

// Status that describes the previous spec must be ignored. This is the signal
// that has a generation stamp on it, which is why the reconciler uses it and not
// the Available condition.
func TestStaleStatusIsIgnored(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: string(f.record.ID),
	})
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: d.Generation - 1,
		Replicas:           2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2,
	}
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("status: %v", err)
	}
	f.reconcile(t)

	if got := f.state(t); got.State != evidence.StatePending {
		t.Errorf("state = %q, want it untouched at pending — the status describes an older spec",
			got.State)
	}
}

// A rollout that ran out of time is the one failure a Deployment reports about
// itself.
func TestProgressDeadlineBecomesFailed(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: string(f.record.ID),
	})
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: d.Generation,
		Replicas:           2, UpdatedReplicas: 1, ReadyReplicas: 0, AvailableReplicas: 0,
		Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded", LastUpdateTime: metav1.Now(),
		}},
	}
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("status: %v", err)
	}
	f.reconcile(t)

	if got := f.state(t); got.State != evidence.StateFailed {
		t.Errorf("state = %q, want failed", got.State)
	}
}

// Running is terminal for a rollout id. A Deployment's progress deadline is ten
// minutes, so admitting running -> failed would admit a failure written ten
// minutes after the fact and open unbounded alternation between the two. The
// record answers "did this deploy land", not "is this app healthy now".
func TestRunningIsTerminal(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: string(f.record.ID),
	})
	f.settled(t, d, 2)
	f.reconcile(t)
	if got := f.state(t); got.State != evidence.StateRunning {
		t.Fatalf("setup: state = %q, want running", got.State)
	}

	// Now it falls over.
	d.Status.ReadyReplicas = 0
	d.Status.AvailableReplicas = 0
	d.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
		Reason: "ProgressDeadlineExceeded", LastUpdateTime: metav1.Now(),
	}}
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("status: %v", err)
	}
	f.reconcile(t)

	got := f.state(t)
	if got.State != evidence.StateRunning {
		t.Errorf("state = %q, want it left at running", got.State)
	}
	if n := len(got.Transitions); n != 1 {
		t.Errorf("transitions = %d, want 1 — the record must not flap", n)
	}
}

// An object nobody stamped is not this reconciler's business. Creating a record
// for it would be the observer inventing a deploy nobody asked for.
func TestUnstampedDeploymentIsIgnored(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, nil)
	f.settled(t, d, 2)
	f.reconcile(t)

	if got := f.state(t); got.State != evidence.StatePending {
		t.Errorf("state = %q, want the record untouched", got.State)
	}
}

// A stamp naming a record the store does not have is what a restored cluster or
// a wiped store looks like. It must not be an error, and it must not requeue for
// ever: retrying cannot conjure the record.
func TestUnknownRecordIsNotAnError(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: "no-such-record",
	})
	f.settled(t, d, 2)
	f.reconcile(t) // Reconcile fatals the test if it errors
}

func TestDeletedDeploymentLeavesTheRecordAlone(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: string(f.record.ID),
	})
	f.settled(t, d, 2)
	f.reconcile(t)

	if err := f.c.Delete(context.Background(), d); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// envtest runs no garbage collector, so nothing else disappears with it.
	f.reconcile(t)

	got := f.state(t)
	if got.State != evidence.StateRunning {
		t.Errorf("state = %q after the deployment was deleted, want running", got.State)
	}
	if n := len(got.Transitions); n != 1 {
		t.Errorf("transitions = %d, want 1", n)
	}
}

// The sweep and the observer race by design: one gives up, the other answers.
// A record given up on must still be correctable when the workload is finally
// seen, or an observer outage is permanent.
func TestObserverCanCorrectASweptRecord(t *testing.T) {
	f := newFixture(t)
	sweep := &deploywatch.Sweep{
		Evidence: f.store, After: time.Minute,
		Now: func() time.Time { return time.Now().Add(time.Hour) },
	}
	if err := sweep.Once(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := f.state(t); got.State != evidence.StateUnknown {
		t.Fatalf("setup: state = %q, want unknown", got.State)
	}

	d := f.deploy(t, map[string]string{
		deploywatch.RolloutAnnotation: string(f.record.ID),
	})
	f.settled(t, d, 2)
	f.reconcile(t)

	if got := f.state(t); got.State != evidence.StateRunning {
		t.Errorf("state = %q, want running — an observer outage must not be permanent", got.State)
	}
}

func (f *fixture) refused(t *testing.T, d *appsv1.Deployment, reason, message string) {
	t.Helper()
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: d.Generation,
		Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue,
			Reason: reason, Message: message, LastUpdateTime: metav1.Now(),
		}},
	}
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("status: %v", err)
	}
}

// The state nothing could ever reach, and the question the product exists to
// answer.
//
// Admission refusing a deploy is the single most likely reason somebody opens
// this page — an unsigned image, a policy violation — and until now the record
// sat at "applied" until the sweep gave up and marked it unknown. The page
// could not say why, on a page whose entire reason to exist is saying why.
func TestARefusedDeployIsRecordedAsRejectedWithTheReason(t *testing.T) {
	f := newFixture(t)
	const msg = `admission webhook "validation.example.com" denied ` +
		`the request: image ghcr.io/acme/api:1.0.0 is not signed`

	d := f.deploy(t, map[string]string{deploywatch.RolloutAnnotation: string(f.record.ID)})
	f.refused(t, d, "FailedCreate", msg)
	f.reconcile(t)

	got := f.state(t)
	if got.State != evidence.StateRejected {
		t.Fatalf("state = %q, want rejected — a deploy admission refused sat at %q, "+
			"and the sweep would eventually call it unknown", got.State, got.State)
	}
	if !got.Admission.Allowed == false && got.Admission.Message == "" {
		t.Error("the outcome carries no message")
	}
	if got.Admission.Allowed {
		t.Error("a refused deploy is recorded as admitted")
	}
	// Quotable to the person who asked. A reason of "rejected" tells them
	// nothing they did not already see on the page.
	if !strings.Contains(got.Admission.Message, "is not signed") {
		t.Errorf("the webhook's own sentence did not reach the record: %q", got.Admission.Message)
	}
}

// The other half, and the one whose absence made the page call every deploy
// refused: a record has to be able to say it was admitted.
func TestAnAdmittedDeploySaysSo(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{deploywatch.RolloutAnnotation: string(f.record.ID)})
	f.settled(t, d, 2)
	f.reconcile(t)

	got := f.state(t)
	if got.State != evidence.StateRunning {
		t.Fatalf("state = %q, want running", got.State)
	}
	if !got.Admission.Allowed {
		t.Error("a deploy whose pods are running is recorded as not admitted; every " +
			"record ever written carried this and the page called them all refused")
	}
}

// A refusal that has cleared is not a refusal. The condition is not removed
// when the problem goes away — it flips to False — so a reader that only checks
// the type reports a deploy as refused because an earlier one was.
func TestAClearedRefusalIsNotARefusal(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{deploywatch.RolloutAnnotation: string(f.record.ID)})

	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: d.Generation,
		Replicas:           2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2,
		Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionFalse,
			Reason: "SlowStartFailure", Message: "an earlier attempt could not create pods",
			LastUpdateTime: metav1.Now(),
		}},
	}
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("status: %v", err)
	}
	f.reconcile(t)

	got := f.state(t)
	if got.State == evidence.StateRejected {
		t.Error("a condition that had flipped to False was read as a refusal")
	}
	if !got.Admission.Allowed {
		t.Error("the deploy is running and is not recorded as admitted")
	}
}

// A deploy that ran cannot become refused. Admission acts before an object
// exists, so a later refusal belongs to a different deploy with a record of its
// own — and moving this one back would rewrite the history of something that
// demonstrably ran.
func TestRunningIsNotRewrittenByALaterRefusal(t *testing.T) {
	f := newFixture(t)
	d := f.deploy(t, map[string]string{deploywatch.RolloutAnnotation: string(f.record.ID)})
	f.settled(t, d, 2)
	f.reconcile(t)
	if f.state(t).State != evidence.StateRunning {
		t.Fatal("the fixture did not reach running")
	}

	f.refused(t, d, "FailedCreate", "denied later")
	f.reconcile(t)

	if got := f.state(t); got.State != evidence.StateRunning {
		t.Errorf("state = %q; a record that reached running was rewritten", got.State)
	}
}
