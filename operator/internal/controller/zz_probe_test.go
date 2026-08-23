package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/alpacino-0/k8s-lab/operator/api/v1alpha1"
)

// countingClient counts Update calls per object kind.
type countingClient struct {
	client.Client
	updates []string
}

func (c *countingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates = append(c.updates, objKind(obj))
	return c.Client.Update(ctx, obj, opts...)
}

func objKind(obj client.Object) string {
	switch obj.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *corev1.Service:
		return "Service"
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	case *networkingv1.NetworkPolicy:
		return "NetworkPolicy"
	case *policyv1.PodDisruptionBudget:
		return "PDB"
	default:
		return "other"
	}
}

var _ = Describe("AUDITPROBE", func() {
	ctx := context.Background()
	ns := "default"

	cleanup := func(name string) {
		app := &platformv1alpha1.Workload{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, app); err == nil {
			_ = k8sClient.Delete(ctx, app)
		}
		for _, obj := range []client.Object{
			&appsv1.Deployment{}, &corev1.Service{}, &corev1.ServiceAccount{},
			&networkingv1.NetworkPolicy{}, &policyv1.PodDisruptionBudget{},
			&networkingv1.Ingress{}, &autoscalingv2.HorizontalPodAutoscaler{},
		} {
			obj.SetName(name)
			obj.SetNamespace(ns)
			_ = k8sClient.Delete(ctx, obj)
		}
	}

	// ---------------- CLAIM 1 ----------------
	It("CLAIM1 foreign ingress and hpa", func() {
		name := "probe-foreign"
		defer cleanup(name)

		pt := networkingv1.PathTypePrefix
		foreignIng := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
				Labels: map[string]string{"owner": "human"}},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{
					Host: "hand-made.example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{{
								Path: "/", PathType: &pt,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: "something-else",
										Port: networkingv1.ServiceBackendPort{Number: 80},
									},
								},
							}},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, foreignIng)).To(Succeed())

		foreignHPA := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
				Labels: map[string]string{"owner": "human"}},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1", Kind: "Deployment", Name: "unrelated",
				},
				MinReplicas: ptr.To(int32(1)),
				MaxReplicas: 4,
			},
		}
		Expect(k8sClient.Create(ctx, foreignHPA)).To(Succeed())

		Expect(k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "ghcr.io/example/app:1.0.0"},
		})).To(Succeed())

		r := &WorkloadReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		ingErr := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &networkingv1.Ingress{})
		hpaErr := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &autoscalingv2.HorizontalPodAutoscaler{})
		GinkgoWriter.Printf("RESULT CLAIM1 foreign-ingress-get-err=%v notfound=%v\n", ingErr, apierrors.IsNotFound(ingErr))
		GinkgoWriter.Printf("RESULT CLAIM1 foreign-hpa-get-err=%v notfound=%v\n", hpaErr, apierrors.IsNotFound(hpaErr))
	})

	// ---------------- CLAIM 2 ----------------
	It("CLAIM2 stale deployment generation", func() {
		name := "probe-stale"
		defer cleanup(name)
		key := types.NamespacedName{Name: name, Namespace: ns}

		Expect(k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "ghcr.io/example/app:1.0.0"},
		})).To(Succeed())

		r := &WorkloadReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status.Replicas = 2
		dep.Status.ReadyReplicas = 2
		dep.Status.ObservedGeneration = dep.Generation
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		app.Spec.Image = "ghcr.io/example/app:2.0.0-broken"
		Expect(k8sClient.Update(ctx, app)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		for _, c := range app.Status.Conditions {
			GinkgoWriter.Printf("RESULT CLAIM2 cond type=%s status=%s reason=%s msg=%q obsGen=%d\n",
				c.Type, c.Status, c.Reason, c.Message, c.ObservedGeneration)
		}
		GinkgoWriter.Printf("RESULT CLAIM2 wl-generation=%d wl-status-observedGeneration=%d\n",
			app.Generation, app.Status.ObservedGeneration)
		GinkgoWriter.Printf("RESULT CLAIM2 dep-generation=%d dep-status-observedGeneration=%d dep-image=%s dep-readyReplicas=%d\n",
			dep.Generation, dep.Status.ObservedGeneration,
			dep.Spec.Template.Spec.Containers[0].Image, dep.Status.ReadyReplicas)
	})

	// ---------------- CLAIM 3 ----------------
	It("CLAIM3 min greater than max", func() {
		name := "probe-hpa"
		defer cleanup(name)
		key := types.NamespacedName{Name: name, Namespace: ns}

		err := k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: platformv1alpha1.WorkloadSpec{
				Image: "ghcr.io/example/app:1.0.0",
				Autoscale: &platformv1alpha1.Autoscale{
					MinReplicas: 5, MaxReplicas: 3, TargetCPUPercent: 60,
				},
			},
		})
		GinkgoWriter.Printf("RESULT CLAIM3 create-workload-err=%v\n", err)
		if err != nil {
			return
		}

		r := &WorkloadReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, rerr := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		GinkgoWriter.Printf("RESULT CLAIM3 reconcile-err=%v\n", rerr)

		depErr := k8sClient.Get(ctx, key, &appsv1.Deployment{})
		GinkgoWriter.Printf("RESULT CLAIM3 deployment-get-err=%v\n", depErr)

		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		for _, c := range app.Status.Conditions {
			GinkgoWriter.Printf("RESULT CLAIM3 cond %s=%s reason=%s msg=%q\n", c.Type, c.Status, c.Reason, c.Message)
		}
	})

	// ---------------- CLAIM 4 ----------------
	It("CLAIM4 pdb at one replica", func() {
		app := &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "x:1", Replicas: ptr.To(int32(1))},
		}
		pdb := desiredPodDisruptionBudget(app)
		dep := desiredDeployment(app)
		GinkgoWriter.Printf("RESULT CLAIM4 replicas=%d minAvailable=%v maxUnavailable=%v\n",
			*dep.Spec.Replicas, pdb.Spec.MinAvailable, pdb.Spec.MaxUnavailable)
	})

	// ---------------- CLAIM 5 ----------------
	It("CLAIM5 probe timings", func() {
		app := &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "x:1", Port: 8080},
		}
		normalise(app)
		c := desiredDeployment(app).Spec.Template.Spec.Containers[0]
		GinkgoWriter.Printf("RESULT CLAIM5 liveness initialDelay=%d period=%d timeout=%d failureThreshold=%d\n",
			c.LivenessProbe.InitialDelaySeconds, c.LivenessProbe.PeriodSeconds,
			c.LivenessProbe.TimeoutSeconds, c.LivenessProbe.FailureThreshold)
		GinkgoWriter.Printf("RESULT CLAIM5 startupProbe-is-nil=%v\n", c.StartupProbe == nil)
	})

	// ---------------- CLAIM 6 ----------------
	It("CLAIM6 long name", func() {
		name := strings.Repeat("a", 70)
		defer cleanup(name)
		key := types.NamespacedName{Name: name, Namespace: ns}

		err := k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "ghcr.io/example/app:1.0.0"},
		})
		GinkgoWriter.Printf("RESULT CLAIM6 create-workload len=%d err=%v\n", len(name), err)
		if err != nil {
			return
		}
		r := &WorkloadReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, rerr := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		GinkgoWriter.Printf("RESULT CLAIM6 reconcile-err=%v\n", rerr)
		saErr := k8sClient.Get(ctx, key, &corev1.ServiceAccount{})
		depErr := k8sClient.Get(ctx, key, &appsv1.Deployment{})
		GinkgoWriter.Printf("RESULT CLAIM6 sa-err=%v dep-err=%v\n", saErr, depErr)
	})

	// ---------------- CLAIM 7 ----------------
	It("CLAIM7 scale subresource", func() {
		name := "probe-scale"
		defer cleanup(name)
		key := types.NamespacedName{Name: name, Namespace: ns}

		Expect(k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "ghcr.io/example/app:1.0.0"},
		})).To(Succeed())

		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		GinkgoWriter.Printf("RESULT CLAIM7 stored-spec-replicas-is-nil=%v\n", app.Spec.Replicas == nil)
		scale := &autoscalingv1.Scale{}
		err := k8sClient.SubResource("scale").Get(ctx, app, scale)
		GinkgoWriter.Printf("RESULT CLAIM7 scale-get-unset-err=%v\n", err)

		app2 := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app2)).To(Succeed())
		app2.Spec.Replicas = ptr.To(int32(3))
		Expect(k8sClient.Update(ctx, app2)).To(Succeed())
		scale2 := &autoscalingv1.Scale{}
		err2 := k8sClient.SubResource("scale").Get(ctx, app2, scale2)
		GinkgoWriter.Printf("RESULT CLAIM7 scale-get-set-err=%v spec=%+v\n", err2, scale2.Spec)
	})

	// ---------------- CLAIM 8 ----------------
	It("CLAIM8 writes per reconcile", func() {
		name := "probe-writes"
		defer cleanup(name)
		key := types.NamespacedName{Name: name, Namespace: ns}

		Expect(k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "ghcr.io/example/app:1.0.0"},
		})).To(Succeed())

		cc := &countingClient{Client: k8sClient}
		r := &WorkloadReconciler{Client: cc, Scheme: k8sClient.Scheme()}
		for i := 1; i <= 4; i++ {
			cc.updates = nil
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			dep := &appsv1.Deployment{}
			_ = k8sClient.Get(ctx, key, dep)
			GinkgoWriter.Printf("RESULT CLAIM8 pass=%d updates=%v dep-rv=%s dep-gen=%d dnsPolicy=%q restartPolicy=%q schedulerName=%q imagePullPolicy=%q\n",
				i, cc.updates, dep.ResourceVersion, dep.Generation,
				dep.Spec.Template.Spec.DNSPolicy, dep.Spec.Template.Spec.RestartPolicy,
				dep.Spec.Template.Spec.SchedulerName, dep.Spec.Template.Spec.Containers[0].ImagePullPolicy)
		}
	})
})
