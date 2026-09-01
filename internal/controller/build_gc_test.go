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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

var _ = Describe("Collecting build records", func() {
	const (
		gcNamespace = "default"
		gcTenant    = "t-acme"
		gcRevision  = "0123456789abcdef0123456789abcdef01234567"
	)

	// The package-level ctx, read inside the closures rather than copied here:
	// BeforeSuite assigns it, and a copy taken while the tree is being built is
	// a nil context.
	seq := 0

	// finishedBuild writes a Build that has already ended, with a finish time
	// that puts it in a known order. envtest runs no controller of ours here:
	// what is under test is the sweep, so the records it sweeps are written
	// rather than produced.
	finished := func(app string, ago time.Duration, phase platformv1alpha1.BuildPhase) string {
		GinkgoHelper()
		seq++
		name := fmt.Sprintf("gc-%s-%d", app, seq)
		b := &platformv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: gcNamespace,
				Labels: map[string]string{
					platformv1alpha1.BuildTenantLabel: gcTenant,
					platformv1alpha1.BuildAppLabel:    app,
				},
			},
			Spec: platformv1alpha1.BuildSpec{
				Repo: "https://github.com/example/app.git", Revision: gcRevision,
				Image: "registry.damga.svc/tenant-a/" + app, Builder: platformv1alpha1.BuildDockerfile,
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		b.Status.Phase = phase
		b.Status.FinishedAt = ptrTime(metav1.NewTime(time.Now().Add(-ago)))
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		return name
	}

	running := func(app string) string {
		GinkgoHelper()
		seq++
		name := fmt.Sprintf("gc-running-%d", seq)
		b := &platformv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: gcNamespace,
				Labels: map[string]string{
					platformv1alpha1.BuildTenantLabel: gcTenant,
					platformv1alpha1.BuildAppLabel:    app,
				},
			},
			Spec: platformv1alpha1.BuildSpec{
				Repo: "https://github.com/example/app.git", Revision: gcRevision,
				Image: "registry.damga.svc/tenant-a/" + app, Builder: platformv1alpha1.BuildDockerfile,
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		b.Status.Phase = platformv1alpha1.BuildRunning
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		return name
	}

	// Every spec starts from an empty namespace, because what these assert is a
	// count and a leftover from the spec before is a different count.
	BeforeEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &platformv1alpha1.Build{},
			client.InNamespace(gcNamespace))).To(Succeed())
		Eventually(func() int { return len(list().Items) }).Should(Equal(0))
	})

	sweeper := func(perApp, ceiling int) *BuildReconciler {
		return &BuildReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			KeptPerApp: perApp, KeptInNamespace: ceiling,
		}
	}

	It("keeps the newest records of an app and removes the rest", func() {
		// Oldest first, so the two that must go are the two named first.
		var oldest []string
		for i := 6; i >= 1; i-- {
			oldest = append(oldest, finished("api", time.Duration(i)*time.Hour, platformv1alpha1.BuildSucceeded))
		}
		// The precondition, asserted rather than assumed: a sweep that collects
		// nothing and a namespace that had nothing to collect leave the same
		// green behind, and this is what tells them apart.
		Expect(list().Items).To(HaveLen(6), "the fixture did not write six records")

		Expect(sweeper(4, 100).collectBuilds(ctx, gcNamespace)).To(Succeed())

		names := namesOf(list())
		Expect(names).To(HaveLen(4))
		Expect(names).NotTo(ContainElement(oldest[0]),
			"the oldest record survived a sweep that was supposed to reach it")
		Expect(names).NotTo(ContainElement(oldest[1]))
		Expect(names).To(ContainElement(oldest[5]), "the newest record was collected")
	})

	It("never removes a build that has not finished", func() {
		for i := 3; i >= 1; i-- {
			finished("api", time.Duration(i)*time.Hour, platformv1alpha1.BuildSucceeded)
		}
		live := running("api")
		Expect(list().Items).To(HaveLen(4))

		Expect(sweeper(1, 100).collectBuilds(ctx, gcNamespace)).To(Succeed())

		// One finished record kept, and the running one, which is not the
		// sweep's to take: deleting it takes the Job with it through the owner
		// reference and kills a build somebody is waiting on.
		names := namesOf(list())
		Expect(names).To(ContainElement(live),
			"a build that was still running was collected, which cancels it")
		Expect(names).To(HaveLen(2))
	})

	// Failures are collected by the same rule as successes, and the spec exists
	// because the opposite is the intuitive choice: keep failures longer, they
	// are what somebody is debugging. A failed build is the provenance of
	// nothing, and a failing repository is what fills this namespace fastest.
	It("collects a failure as readily as a success", func() {
		old := finished("api", 3*time.Hour, platformv1alpha1.BuildFailed)
		finished("api", 2*time.Hour, platformv1alpha1.BuildFailed)
		newest := finished("api", time.Hour, platformv1alpha1.BuildSucceeded)
		Expect(list().Items).To(HaveLen(3))

		Expect(sweeper(2, 100).collectBuilds(ctx, gcNamespace)).To(Succeed())

		names := namesOf(list())
		Expect(names).To(HaveLen(2))
		Expect(names).NotTo(ContainElement(old),
			"the oldest failure outlived the rule; failures are not kept longer here")
		Expect(names).To(ContainElement(newest))
	})

	// The ceiling is the quota's own wall, so it wins over the per-app rule:
	// twenty apps keeping ten each is exactly the 200 the namespace admits.
	It("takes a record its own app would have kept when the namespace is over its ceiling", func() {
		var order []string
		for i := 6; i >= 1; i-- {
			app := "api"
			if i%2 == 0 {
				app = "web"
			}
			order = append(order, finished(app, time.Duration(i)*time.Hour, platformv1alpha1.BuildSucceeded))
		}
		Expect(list().Items).To(HaveLen(6))

		// Per app, every one of these would be kept. The namespace says four.
		Expect(sweeper(10, 4).collectBuilds(ctx, gcNamespace)).To(Succeed())

		names := namesOf(list())
		Expect(names).To(HaveLen(4),
			"the per-app rule was allowed to overrule the namespace ceiling, which is the "+
				"wall the quota actually is")
		Expect(names).NotTo(ContainElement(order[0]))
		Expect(names).NotTo(ContainElement(order[1]))
	})

	// The other half of the same green: a sweep with nothing to collect must
	// leave everything, or the specs above would pass for a collector that
	// deletes on every call.
	It("removes nothing when everything is within the limits", func() {
		var all []string
		for i := 3; i >= 1; i-- {
			all = append(all, finished("api", time.Duration(i)*time.Hour, platformv1alpha1.BuildSucceeded))
		}

		Expect(sweeper(10, 100).collectBuilds(ctx, gcNamespace)).To(Succeed())

		names := namesOf(list())
		Expect(names).To(HaveLen(3), "a sweep collected records that were inside every limit")
		for _, name := range all {
			Expect(names).To(ContainElement(name))
		}
	})
})

func list() *platformv1alpha1.BuildList {
	GinkgoHelper()
	var out platformv1alpha1.BuildList
	Expect(k8sClient.List(ctx, &out, client.InNamespace("default"))).To(Succeed())
	return &out
}

func namesOf(l *platformv1alpha1.BuildList) []string {
	out := make([]string, 0, len(l.Items))
	for _, b := range l.Items {
		out = append(out, b.Name)
	}
	return out
}
