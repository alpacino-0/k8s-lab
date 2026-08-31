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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

var _ = Describe("Build Controller", func() {
	const (
		namespace = "default"
		rev       = "0123456789abcdef0123456789abcdef01234567"
		digest    = "sha256:" + "ab12cd34" + "ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34"
	)

	// A stand-in image for pods this suite writes by hand; envtest runs no
	// kubelet, so nothing is ever pulled.
	const stubImage = "busybox:1"

	ctx := context.Background()

	// A name per spec rather than one shared name that is cleaned up between
	// them. envtest runs no garbage collector, so a deleted Job with a
	// finalizer never actually goes — and the next spec then finds the previous
	// one, which the API server refuses to move from Complete to Failed with an
	// error that reads as a bug in the controller. Unique names make the specs
	// independent instead of making the cleanup work.
	var name string
	var key types.NamespacedName
	seq := 0
	BeforeEach(func() {
		seq++
		name = fmt.Sprintf("test-build-%d", seq)
		key = types.NamespacedName{Name: name, Namespace: namespace}
	})

	reconciler := func() *BuildReconciler {
		return &BuildReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}
	reconcileNow := func() {
		GinkgoHelper()
		_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	spec := platformv1alpha1.BuildSpec{
		Repo:     "https://github.com/example/app.git",
		Revision: rev,
		// Named rather than left to detect, and the reason is what this suite
		// asserts: detect renders two containers — an init that clones and
		// decides, and a main one carrying the buildpack lifecycle — so
		// Containers[0] is no longer the thing whose environment and
		// termination path the specs below read. Naming dockerfile keeps those
		// assertions pointed at the container they were written about.
		Builder: platformv1alpha1.BuildDockerfile,
		Image:   "registry.damga.svc/tenant-a/app",
	}

	create := func(s platformv1alpha1.BuildSpec) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &platformv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       s,
		})).To(Succeed())
	}

	// The job's pods are what carry the result, and envtest runs no kubelet, so
	// the pod and its terminated container state are written by hand — which is
	// the only way to test the reading at all.
	// finishWithReason drives the Job to a terminal condition carrying a reason
	// of its own, which is what happens when no pod ever ran.
	finishWithReason := func(jobCondition batchv1.JobConditionType, reason, jobMessage string) {
		GinkgoHelper()
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{
				Type: jobCondition, Status: corev1.ConditionTrue, LastTransitionTime: now,
				Reason: reason, Message: jobMessage,
			},
		}
		job.Status.Failed = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	finish := func(jobCondition batchv1.JobConditionType, message string) {
		GinkgoHelper()
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		// The API server enforces the Job's own status invariants, which a fake
		// client would not: Complete needs SuccessCriteriaMet and a completion
		// time, and Failed needs FailureTarget. Writing them by hand here is
		// what makes this a test of the reading rather than of a struct.
		switch jobCondition {
		case batchv1.JobComplete:
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
			}
			job.Status.CompletionTime = &now
			job.Status.Succeeded = 1
		default:
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now},
			}
			job.Status.Failed = 1
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		if message == "" {
			return
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name + "-pod", Namespace: namespace,
				Labels: map[string]string{
					instanceLabel: name, componentLabel: buildComponent,
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: buildContainer, Image: stubImage,
			}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: buildContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: message,
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	It("creates a job that clones the revision and pushes a tagged image", func() {
		create(spec)
		reconcileNow()

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())

		c := job.Spec.Template.Spec.Containers[0]
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		Expect(env["REVISION"]).To(Equal(rev))
		// The tag names the commit. A build whose output is called :latest
		// cannot answer which commit is running.
		Expect(env["IMAGE"]).To(HaveSuffix(":" + rev))

		// The channel between a job with no token and the control plane.
		Expect(c.TerminationMessagePath).To(Equal(BuildResultPath))
		Expect(c.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageReadFile))
		Expect(job.Spec.Template.Spec.AutomountServiceAccountToken).To(Equal(ptrFalse()))

		// One attempt. A compile error is not transient and retrying it three
		// times produces the same message three times more slowly.
		Expect(*job.Spec.BackoffLimit).To(BeNumerically("==", 0))

		b := &platformv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
		Expect(b.Status.Phase).To(Equal(platformv1alpha1.BuildRunning))
	})

	It("records the digest and which method actually ran", func() {
		create(spec)
		reconcileNow()
		finish(batchv1.JobComplete, `{"digest":"`+digest+`","method":"dockerfile"}`)
		reconcileNow()

		b := &platformv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
		Expect(b.Status.Phase).To(Equal(platformv1alpha1.BuildSucceeded))
		Expect(b.Status.Digest).To(Equal(digest))
		// "detect" was asked for; knowing it resolved to dockerfile is the
		// difference between a reproducible record and a guess.
		Expect(b.Status.Method).To(Equal(platformv1alpha1.BuildDockerfile))
	})

	// The case that is worse than a failure: the job exits zero and says
	// nothing. An image may exist and nothing can name it, and an empty digest
	// written into a Workload is a reference that resolves to nothing.
	It("treats a successful job with no digest as a failure", func() {
		create(spec)
		reconcileNow()
		finish(batchv1.JobComplete, "")
		reconcileNow()

		b := &platformv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
		Expect(b.Status.Phase).To(Equal(platformv1alpha1.BuildFailed))
		Expect(b.Status.Digest).To(BeEmpty())
		Expect(b.Status.Message).To(ContainSubstring("no digest"))
	})

	// A build that needs two images runs the first as an init container, and an
	// init container that dies means the main one never starts. Reading only
	// ContainerStatuses would find nothing, and the workaround anybody reaches
	// for — exit zero, leave the error in a shared file — hides a hard death
	// behind a success.
	It("reads a message left by an init container", func() {
		create(spec)
		reconcileNow()

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		job.Status.Failed = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name + "-pod", Namespace: namespace,
				Labels: map[string]string{instanceLabel: name, componentLabel: buildComponent},
			},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "prepare", Image: stubImage}},
				Containers:     []corev1.Container{{Name: buildContainer, Image: stubImage}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		// The main container never ran, which is what an init failure means.
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "prepare",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: `{"method":"buildpack","message":"no buildpack matched this source"}`,
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		reconcileNow()

		b := &platformv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
		Expect(b.Status.Phase).To(Equal(platformv1alpha1.BuildFailed))
		Expect(b.Status.Message).To(Equal("no buildpack matched this source"))
	})

	It("quotes the builder's own message on failure", func() {
		create(spec)
		reconcileNow()
		finish(batchv1.JobFailed, `{"method":"dockerfile","message":"npm ERR! missing script: build"}`)
		reconcileNow()

		b := &platformv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
		Expect(b.Status.Phase).To(Equal(platformv1alpha1.BuildFailed))
		// Quoted rather than summarised. A build fails for reasons that belong
		// to the user's code, and a platform that paraphrases the compiler is
		// one nobody can debug against.
		Expect(b.Status.Message).To(Equal("npm ERR! missing script: build"))
	})

	// The case that cost a CI round: admission refused the pod template, so
	// there was never a container to write anything. The first version of this
	// said "check the job's pod" — and there was no pod, which sent the search
	// to the wrong place. The Job knows why, so the Job is quoted.
	It("quotes the job when there was never a pod to speak", func() {
		create(spec)
		reconcileNow()
		finishWithReason(batchv1.JobFailed, "DeadlineExceeded", "Job was active longer than specified deadline")
		reconcileNow()

		b := &platformv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
		Expect(b.Status.Phase).To(Equal(platformv1alpha1.BuildFailed))
		Expect(b.Status.Message).To(ContainSubstring("DeadlineExceeded"))
		Expect(b.Status.Message).To(ContainSubstring("longer than specified deadline"))
	})

	// A recorded digest must survive a controller restart. Reconciling a
	// finished build would look again at a job whose pods the history limit has
	// since deleted, and overwrite the answer with "no digest".
	It("never revisits a finished build", func() {
		create(spec)
		reconcileNow()
		finish(batchv1.JobComplete, `{"digest":"`+digest+`","method":"buildpack"}`)
		reconcileNow()

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: namespace}}
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		reconcileNow()

		b := &platformv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
		Expect(b.Status.Phase).To(Equal(platformv1alpha1.BuildSucceeded))
		Expect(b.Status.Digest).To(Equal(digest))
	})

	Describe("what the API refuses", func() {
		It("refuses a branch name where a commit belongs", func() {
			err := k8sClient.Create(ctx, &platformv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-a", Namespace: namespace},
				Spec: platformv1alpha1.BuildSpec{
					Repo: spec.Repo, Revision: "main", Image: spec.Image,
				},
			})
			// A branch moves, and a record that says "built main" cannot answer
			// which main — the only question anybody asks of it later.
			Expect(err).To(HaveOccurred())
		})

		// The registry this platform installs carries a port, and the first
		// rule written here read the colon in it as a tag — refusing every
		// build on the first run against a real cluster.
		It("accepts a registry that carries a port", func() {
			Expect(k8sClient.Create(ctx, &platformv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-port", Namespace: namespace},
				Spec: platformv1alpha1.BuildSpec{
					Repo: spec.Repo, Revision: rev,
					Image: "registry.damga-registry.svc:5000/ci/app",
				},
			})).To(Succeed())
		})

		It("refuses an image reference that already carries a tag", func() {
			err := k8sClient.Create(ctx, &platformv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-b", Namespace: namespace},
				Spec: platformv1alpha1.BuildSpec{
					Repo: spec.Repo, Revision: rev, Image: spec.Image + ":v1",
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must not carry a tag"))
		})

		It("refuses a path that climbs out of the repository", func() {
			err := k8sClient.Create(ctx, &platformv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-c", Namespace: namespace},
				Spec: platformv1alpha1.BuildSpec{
					Repo: spec.Repo, Revision: rev, Image: spec.Image,
					Path: "../../etc",
				},
			})
			Expect(err).To(HaveOccurred())
		})

		// A Build is a record of one commit. If it can be edited, it cannot
		// answer "what produced the image that is running".
		It("refuses an edit to a build that already exists", func() {
			create(spec)
			b := &platformv1alpha1.Build{}
			Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
			b.Spec.Revision = "ffffffffffffffffffffffffffffffffffffffff"
			err := k8sClient.Update(ctx, b)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("new Build"))
		})
	})
})

func ptrFalse() *bool { f := false; return &f }
