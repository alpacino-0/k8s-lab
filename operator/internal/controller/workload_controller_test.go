/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/alpacino-0/k8s-lab/operator/api/v1alpha1"
)

var _ = Describe("Workload Controller", func() {
	const (
		name      = "test-app"
		namespace = "default"
	)

	const testImage = "ghcr.io/example/app:1.0.0"

	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: namespace}

	reconciler := func() *WorkloadReconciler {
		return &WorkloadReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	reconcileNow := func() {
		GinkgoHelper()
		_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	create := func(spec platformv1alpha1.WorkloadSpec) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       spec,
		})).To(Succeed())
	}

	AfterEach(func() {
		app := &platformv1alpha1.Workload{}
		if err := k8sClient.Get(ctx, key, app); err == nil {
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())
		}
		// envtest runs no garbage collector, so owner references do not cascade
		// here. Removing the children by hand keeps each spec independent.
		for _, obj := range []client.Object{
			&appsv1.Deployment{}, &corev1.Service{}, &corev1.ServiceAccount{},
			&networkingv1.NetworkPolicy{}, &policyv1.PodDisruptionBudget{},
			&networkingv1.Ingress{}, &autoscalingv2.HorizontalPodAutoscaler{},
		} {
			obj.SetName(name)
			obj.SetNamespace(namespace)
			_ = k8sClient.Delete(ctx, obj)
		}
	})

	It("renders the full set of objects a production workload needs", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Port: 3000})
		reconcileNow()

		for _, obj := range []client.Object{
			&appsv1.Deployment{}, &corev1.Service{}, &corev1.ServiceAccount{},
			&networkingv1.NetworkPolicy{}, &policyv1.PodDisruptionBudget{},
		} {
			Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			Expect(obj.GetOwnerReferences()).To(HaveLen(1),
				"without an owner reference, deleting the Workload would orphan this object")
			Expect(obj.GetOwnerReferences()[0].Kind).To(Equal("Workload"))
		}

		By("leaving out what was not asked for")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &networkingv1.Ingress{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{}))).To(BeTrue())
	})

	It("adds and then removes the ingress as the domain comes and goes", func() {
		create(platformv1alpha1.WorkloadSpec{
			Image:  testImage,
			Domain: "app.example.com",
		})
		reconcileNow()

		ing := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, key, ing)).To(Succeed())
		Expect(ing.Spec.Rules[0].Host).To(Equal("app.example.com"))

		By("dropping the domain again")
		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		app.Spec.Domain = ""
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		reconcileNow()

		// Only ever creating conditional objects would leave an Ingress serving
		// a hostname the user has removed from the spec.
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &networkingv1.Ingress{}))).To(BeTrue())
	})

	It("hands the replica count to the autoscaler and takes it back", func() {
		create(platformv1alpha1.WorkloadSpec{
			Image:     testImage,
			Autoscale: &platformv1alpha1.Autoscale{MinReplicas: 3, MaxReplicas: 9, TargetCPUPercent: 70},
		})
		reconcileNow()

		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		Expect(k8sClient.Get(ctx, key, hpa)).To(Succeed())
		Expect(*hpa.Spec.MinReplicas).To(Equal(int32(3)))

		By("scaling the deployment away from the spec, as the autoscaler would")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Spec.Replicas = ptrInt32(6)
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())

		reconcileNow()

		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(6)),
			"reconciling undid the autoscaler's decision, so the two will fight forever")
	})

	It("reports what it observed rather than what it was asked for", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage})
		reconcileNow()

		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())

		// No kubelet runs in envtest, so nothing can become ready. A status that
		// said otherwise would be echoing the spec.
		cond := findCondition(app.Status.Conditions, "Ready")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(app.Status.ObservedGeneration).To(Equal(app.Generation))
	})
})

func ptrInt32(v int32) *int32 { return &v }

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
