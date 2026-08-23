package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/alpacino-0/k8s-lab/operator/api/v1alpha1"
)

var _ = Describe("PROBES", func() {
	pctx := context.Background()

	mkNS := func(n string) {
		GinkgoHelper()
		_ = k8sClient.Create(pctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}
	rec := func() *WorkloadReconciler {
		return &WorkloadReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	It("P1: deleteIfPresent kills a foreign Ingress with the same name", func() {
		ns := "p1"
		mkNS(ns)
		foreign := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: ns,
				Labels: map[string]string{"owner": "someone-else"}},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{Host: "victim.example.com"}},
			},
		}
		Expect(k8sClient.Create(pctx, foreign)).To(Succeed())

		Expect(k8sClient.Create(pctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "ghcr.io/x/a:1"},
		})).To(Succeed())

		_, err := rec().Reconcile(pctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "shop", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		got := &networkingv1.Ingress{}
		gerr := k8sClient.Get(pctx, types.NamespacedName{Name: "shop", Namespace: ns}, got)
		GinkgoWriter.Printf("P1 RESULT: foreign ingress get err = %v\n", gerr)
	})

	It("P2a: CreateOrUpdate adopts an unowned Service and rewrites its selector", func() {
		ns := "p2"
		mkNS(ns)
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: ns},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "billing-real"},
				Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(9000)}},
			},
		}
		Expect(k8sClient.Create(pctx, svc)).To(Succeed())

		Expect(k8sClient.Create(pctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "evil/attacker:1"},
		})).To(Succeed())

		_, err := rec().Reconcile(pctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "billing", Namespace: ns}})
		GinkgoWriter.Printf("P2a reconcile err = %v\n", err)

		got := &corev1.Service{}
		Expect(k8sClient.Get(pctx, types.NamespacedName{Name: "billing", Namespace: ns}, got)).To(Succeed())
		GinkgoWriter.Printf("P2a RESULT selector=%v ownerRefs=%v\n", got.Spec.Selector, got.OwnerReferences)
	})

	It("P2b: adopting a Deployment whose selector differs", func() {
		ns := "p2b"
		mkNS(ns)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "orders"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "orders"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "c", Image: "internal/orders:9.9.9"}}},
				},
			},
		}
		Expect(k8sClient.Create(pctx, dep)).To(Succeed())

		Expect(k8sClient.Create(pctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: ns},
			Spec:       platformv1alpha1.WorkloadSpec{Image: "evil/attacker:1"},
		})).To(Succeed())

		_, err := rec().Reconcile(pctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "orders", Namespace: ns}})
		GinkgoWriter.Printf("P2b RESULT reconcile err = %v\n", err)

		got := &appsv1.Deployment{}
		Expect(k8sClient.Get(pctx, types.NamespacedName{Name: "orders", Namespace: ns}, got)).To(Succeed())
		GinkgoWriter.Printf("P2b RESULT image=%s ownerRefs=%v\n",
			got.Spec.Template.Spec.Containers[0].Image, got.OwnerReferences)
	})

	It("P5: image validation with a ported registry and no tag", func() {
		ns := "p5"
		mkNS(ns)
		for i, img := range []string{
			"registry.local:5000/team-a/app",
			"registry.local:5000/team-a/app:latest",
			"nginx",
			"nginx:latest",
			"nginx:1.2.3",
		} {
			err := k8sClient.Create(pctx, &platformv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("img%d", i), Namespace: ns},
				Spec:       platformv1alpha1.WorkloadSpec{Image: img},
			})
			GinkgoWriter.Printf("P5 RESULT image=%-40q accepted=%v err=%v\n", img, err == nil, err)
		}
	})

	It("P12: min>max autoscale", func() {
		ns := "p12"
		mkNS(ns)
		err := k8sClient.Create(pctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa", Namespace: ns},
			Spec: platformv1alpha1.WorkloadSpec{
				Image:     "ghcr.io/x/a:1",
				Autoscale: &platformv1alpha1.Autoscale{MinReplicas: 9, MaxReplicas: 3, TargetCPUPercent: 60},
			},
		})
		GinkgoWriter.Printf("P12 RESULT crd accepted=%v err=%v\n", err == nil, err)
		if err == nil {
			_, rerr := rec().Reconcile(pctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "hpa", Namespace: ns}})
			GinkgoWriter.Printf("P12 RESULT reconcile err = %v\n", rerr)
			d := &appsv1.Deployment{}
			GinkgoWriter.Printf("P12 RESULT deployment exists=%v\n",
				k8sClient.Get(pctx, types.NamespacedName{Name: "hpa", Namespace: ns}, d) == nil)
			h := &autoscalingv2.HorizontalPodAutoscaler{}
			GinkgoWriter.Printf("P12 RESULT hpa exists=%v\n",
				k8sClient.Get(pctx, types.NamespacedName{Name: "hpa", Namespace: ns}, h) == nil)
			app := &platformv1alpha1.Workload{}
			Expect(k8sClient.Get(pctx, types.NamespacedName{Name: "hpa", Namespace: ns}, app)).To(Succeed())
			GinkgoWriter.Printf("P12 RESULT status conds = %+v\n", app.Status.Conditions)
		}
	})

	It("P13: memoryLimit < memoryRequest", func() {
		ns := "p13"
		mkNS(ns)
		err := k8sClient.Create(pctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: "mem", Namespace: ns},
			Spec: platformv1alpha1.WorkloadSpec{
				Image: "ghcr.io/x/a:1",
				Resources: platformv1alpha1.Resources{
					MemoryRequest: resource.MustParse("2Gi"),
					MemoryLimit:   resource.MustParse("64Mi"),
				},
			},
		})
		GinkgoWriter.Printf("P13 RESULT crd accepted=%v err=%v\n", err == nil, err)
		if err == nil {
			_, rerr := rec().Reconcile(pctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "mem", Namespace: ns}})
			GinkgoWriter.Printf("P13 RESULT reconcile err = %v\n", rerr)
		}
	})

	It("P3: duplicate domain across namespaces", func() {
		for _, ns := range []string{"tenant-a", "tenant-b"} {
			mkNS(ns)
			err := k8sClient.Create(pctx, &platformv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
				Spec:       platformv1alpha1.WorkloadSpec{Image: "ghcr.io/x/a:1", Domain: "bank.example.com"},
			})
			GinkgoWriter.Printf("P3 RESULT ns=%s accepted=%v err=%v\n", ns, err == nil, err)
		}
	})

	It("P9: PDB percentage math at replicas=1", func() {
		for _, n := range []int{1, 2, 3, 4} {
			v, err := intstr.GetScaledValueFromIntOrPercent(ptr.To(intstr.FromString("50%")), n, true)
			GinkgoWriter.Printf("P9 RESULT expected=%d desiredHealthy=%d disruptionsAllowed=%d err=%v\n",
				n, v, n-v, err)
		}
	})

	It("P14: probe fields as rendered", func() {
		app := &platformv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: "pr", Namespace: "default"},
			Spec: platformv1alpha1.WorkloadSpec{Image: "x:1"}}
		normalise(app)
		d := desiredDeployment(app)
		lp := d.Spec.Template.Spec.Containers[0].LivenessProbe
		GinkgoWriter.Printf("P14 RESULT liveness initialDelay=%d period=%d failureThreshold=%d startupProbe=%v\n",
			lp.InitialDelaySeconds, lp.PeriodSeconds, lp.FailureThreshold,
			d.Spec.Template.Spec.Containers[0].StartupProbe)
		GinkgoWriter.Printf("P10 RESULT emptyDir=%+v limits=%+v\n",
			d.Spec.Template.Spec.Volumes[0].VolumeSource.EmptyDir,
			d.Spec.Template.Spec.Containers[0].Resources.Limits)
	})
})
