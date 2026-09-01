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

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// BuildReconciler runs one Build to completion and records what happened.
type BuildReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// KeptPerApp and KeptInNamespace bound how many finished Build records
	// survive; zero means the constants collectBuilds documents. Injected for
	// the tests, the way gitwrite.Writer.Now is: the namespace ceiling is 150,
	// and a test that had to create 151 objects to reach it would be a test
	// nobody runs twice.
	KeptPerApp      int
	KeptInNamespace int
}

// buildResult is what the build writes to its termination log.
//
// A shape shared with a shell script, which is a joint nothing type-checks
// across: a field renamed on one side reads as a zero on the other. These names
// are the names in buildScript, and the test that parses a literal copy of what
// that script prints is what keeps the two honest.
type buildResult struct {
	Digest  string                       `json:"digest"`
	Method  platformv1alpha1.BuildMethod `json:"method"`
	Message string                       `json:"message"`
}

// +kubebuilder:rbac:groups=platform.damga.co,resources=builds,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=platform.damga.co,resources=builds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *BuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var build platformv1alpha1.Build
	if err := r.Get(ctx, req.NamespacedName, &build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal and never revisited. A Build describes one commit and the answer
	// does not change; reconciling a finished one would mean a controller
	// restart could overwrite a recorded digest with a fresh look at a job
	// whose pods the history limit has since deleted.
	if build.Status.Phase == platformv1alpha1.BuildSucceeded ||
		build.Status.Phase == platformv1alpha1.BuildFailed {
		return ctrl.Result{}, nil
	}

	job := desiredBuildJob(&build)
	if err := controllerutil.SetControllerReference(&build, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	var have batchv1.Job
	err := r.Get(ctx, client.ObjectKeyFromObject(job), &have)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("creating the build job: %w", err)
		}
		return ctrl.Result{}, r.setPhase(ctx, &build, platformv1alpha1.BuildRunning, "the build job was created")
	case err != nil:
		return ctrl.Result{}, err
	}
	// No update pass. A Job's pod template is immutable, and the spec that
	// produced it cannot change either — the API refuses an edited Build.

	return ctrl.Result{}, r.observe(ctx, &build, &have)
}

// observe reads the finished job and writes down what it said.
func (r *BuildReconciler) observe(
	ctx context.Context, build *platformv1alpha1.Build, job *batchv1.Job,
) error {
	done, failed := jobFinished(job)
	if !done {
		return r.setPhase(ctx, build, platformv1alpha1.BuildRunning, "the build is running")
	}

	res, found := r.resultFromPods(ctx, build)

	patch := client.MergeFrom(build.DeepCopy())
	build.Status.FinishedAt = ptrTime(metav1.Now())
	if job.Status.StartTime != nil {
		build.Status.StartedAt = job.Status.StartTime
	}
	build.Status.ObservedGeneration = build.Generation

	switch {
	case !failed && found && res.Digest != "":
		build.Status.Phase = platformv1alpha1.BuildSucceeded
		build.Status.Digest = res.Digest
		build.Status.Method = res.Method
		build.Status.Message = ""
		setBuildCondition(build, metav1.ConditionTrue, "Built", "the image was pushed")
	case !failed:
		// The job succeeded and said nothing, which is worse than failing: the
		// image may exist and nothing can name it. Recorded as a failure rather
		// than as a success with an empty digest, because an empty digest in a
		// Workload is an image reference that resolves to nothing.
		build.Status.Phase = platformv1alpha1.BuildFailed
		build.Status.Message = "the build reported no digest, so nothing can say which image it produced"
		setBuildCondition(build, metav1.ConditionFalse, "NoDigest", build.Status.Message)
	default:
		build.Status.Phase = platformv1alpha1.BuildFailed
		build.Status.Method = res.Method
		build.Status.Message = res.Message
		if build.Status.Message == "" {
			// No message from the container, which usually means there was no
			// container: a pod template admission refused, an image that would
			// not pull, a deadline. The Job knows why and the pod does not
			// exist to be asked, so the Job's own condition is quoted rather
			// than replaced with a sentence about checking a pod that is not
			// there — which is what this said first, and it sent the search to
			// the wrong place.
			build.Status.Message = jobFailureMessage(job)
		}
		setBuildCondition(build, metav1.ConditionFalse, "Failed", build.Status.Message)
	}
	if err := r.Status().Patch(ctx, build, patch); err != nil {
		return err
	}

	// One build finishing is the moment there is one more record to keep, so it
	// is the moment to decide which one stops being kept. Triggered here rather
	// than on a timer because a timer is a second thing to configure, to run on
	// the leader only, and to explain when it has not fired yet — and because a
	// namespace that is filling is a namespace where builds are finishing.
	//
	// The error is returned so a failed sweep is retried, and it comes after
	// the patch so that a sweep that fails cannot cost the record of the build
	// that triggered it.
	return r.collectBuilds(ctx, build.Namespace)
}

// resultFromPods reads the termination message off whichever pod the job left.
//
// Init containers are read as well as the main one, and the reason is a design
// this would otherwise have forced: a build that needs two images — a builder
// to produce the image and a second to push it — runs the first as an init
// container, and an init container that dies means the main container never
// starts. Reading only ContainerStatuses would then find nothing at all, and
// the alternative anybody reaches for is making the init container exit zero
// and leave its error in a shared file. That hides a hard death — OOM, an image
// that would not pull — behind a success, which is the failure this repository
// has already paid four rounds to learn.
//
// Init containers first, because they run first: when both have something to
// say, the earlier failure is the one that explains the later.
func (r *BuildReconciler) resultFromPods(
	ctx context.Context, build *platformv1alpha1.Build,
) (buildResult, bool) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(build.Namespace),
		client.MatchingLabels{instanceLabel: build.Name, componentLabel: buildComponent},
	); err != nil {
		return buildResult{}, false
	}
	for i := range pods.Items {
		statuses := append(
			append([]corev1.ContainerStatus{}, pods.Items[i].Status.InitContainerStatuses...),
			pods.Items[i].Status.ContainerStatuses...)
		for _, cs := range statuses {
			if cs.State.Terminated == nil || cs.State.Terminated.Message == "" {
				continue
			}
			var res buildResult
			if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &res); err != nil {
				// Not JSON. A container that died mid-write leaves whatever it
				// had, and reading that as a result would invent one.
				continue
			}
			return res, true
		}
	}
	return buildResult{}, false
}

// jobFailureMessage is what the Job says about itself when no pod could say
// anything.
func jobFailureMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type != batchv1.JobFailed || c.Status != corev1.ConditionTrue {
			continue
		}
		switch {
		case c.Message != "" && c.Reason != "":
			return c.Reason + ": " + c.Message
		case c.Message != "":
			return c.Message
		case c.Reason != "":
			return c.Reason
		}
	}
	return "the build produced no output and its job gave no reason"
}

func jobFinished(job *batchv1.Job) (done, failed bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false
		case batchv1.JobFailed:
			return true, true
		}
	}
	return false, false
}

func (r *BuildReconciler) setPhase(
	ctx context.Context, build *platformv1alpha1.Build,
	phase platformv1alpha1.BuildPhase, message string,
) error {
	if build.Status.Phase == phase {
		return nil
	}
	patch := client.MergeFrom(build.DeepCopy())
	build.Status.Phase = phase
	build.Status.ObservedGeneration = build.Generation
	setBuildCondition(build, metav1.ConditionUnknown, string(phase), message)
	return r.Status().Patch(ctx, build, patch)
}

func setBuildCondition(
	build *platformv1alpha1.Build, status metav1.ConditionStatus, reason, message string,
) {
	meta := metav1.Condition{
		Type: readyCondition, Status: status, Reason: reason, Message: message,
		ObservedGeneration: build.Generation,
	}
	for i := range build.Status.Conditions {
		if build.Status.Conditions[i].Type == meta.Type {
			if build.Status.Conditions[i].Status != status {
				meta.LastTransitionTime = metav1.Now()
			} else {
				meta.LastTransitionTime = build.Status.Conditions[i].LastTransitionTime
			}
			build.Status.Conditions[i] = meta
			return
		}
	}
	meta.LastTransitionTime = metav1.Now()
	build.Status.Conditions = append(build.Status.Conditions, meta)
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

// SetupWithManager registers the reconciler.
func (r *BuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Build{}).
		Owns(&batchv1.Job{}).
		Named("build").
		Complete(r)
}
