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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

var _ = Describe("Workload Controller", func() {
	const (
		name      = "test-app"
		namespace = "default"
	)

	const dataVol = "data"

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
		// The claim is not owned, so it is not in the list above and would
		// survive into the next spec.
		gen := &corev1.Secret{}
		gen.SetName(name + "-generated")
		gen.SetNamespace(namespace)
		_ = k8sClient.Delete(ctx, gen)

		pvc := &corev1.PersistentVolumeClaim{}
		pvc.SetName(name + "-data")
		pvc.SetNamespace(namespace)
		_ = k8sClient.Delete(ctx, pvc)
	})

	Describe("generated secrets", func() {
		secrets := []platformv1alpha1.GeneratedSecret{
			{Name: "APP_KEY", Kind: platformv1alpha1.GeneratedPassword},
			{Name: "SIGNING_HEX", Kind: platformv1alpha1.GeneratedHex},
		}
		secretKey := func() types.NamespacedName {
			return types.NamespacedName{Name: name + "-generated", Namespace: namespace}
		}

		It("mints what was asked for and mounts it before the tenant's own", func() {
			create(platformv1alpha1.WorkloadSpec{Image: testImage, Secrets: secrets})
			reconcileNow()

			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretKey(), sec)).To(Succeed())
			Expect(sec.Data).To(HaveKey("APP_KEY"))
			Expect(sec.Data).To(HaveKey("SIGNING_HEX"))
			// Hex means hex. A signing key that expects 64 hex characters does
			// not accept base64, which is the whole reason Kind exists.
			Expect(string(sec.Data["SIGNING_HEX"])).To(MatchRegexp(`^[0-9a-f]+$`))
			Expect(string(sec.Data["APP_KEY"])).NotTo(BeEmpty())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers[0].EnvFrom[0].SecretRef.Name).
				To(Equal(name + "-generated"))
		})

		// The failure this guards against is total and silent: a reconcile that
		// mints fresh values every time hands the application credentials its
		// own stored data no longer matches, and nothing reports an error.
		It("keeps what it already minted across reconciles", func() {
			create(platformv1alpha1.WorkloadSpec{Image: testImage, Secrets: secrets})
			reconcileNow()

			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretKey(), sec)).To(Succeed())
			first := string(sec.Data["APP_KEY"])

			reconcileNow()
			reconcileNow()

			Expect(k8sClient.Get(ctx, secretKey(), sec)).To(Succeed())
			Expect(string(sec.Data["APP_KEY"])).To(Equal(first), "the password was rotated under a running app")
		})

		// Adding a name mints one value. Rotating the others would be a silent
		// outage for every one of them.
		It("mints only the new name when one is added", func() {
			create(platformv1alpha1.WorkloadSpec{Image: testImage, Secrets: secrets[:1]})
			reconcileNow()
			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretKey(), sec)).To(Succeed())
			first := string(sec.Data["APP_KEY"])

			app := &platformv1alpha1.Workload{}
			Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
			app.Spec.Secrets = secrets
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
			reconcileNow()

			Expect(k8sClient.Get(ctx, secretKey(), sec)).To(Succeed())
			Expect(sec.Data).To(HaveKey("SIGNING_HEX"))
			Expect(string(sec.Data["APP_KEY"])).To(Equal(first))
		})

		// The same call the volumes make, for the same reason: deleting an app
		// to recreate it must not destroy the only copy of a password.
		It("leaves the secret behind when the workload is deleted", func() {
			create(platformv1alpha1.WorkloadSpec{Image: testImage, Secrets: secrets})
			reconcileNow()
			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretKey(), sec)).To(Succeed())
			Expect(sec.OwnerReferences).To(BeEmpty(),
				"an owner reference here deletes the tenant's credentials on the next redeploy")

			app := &platformv1alpha1.Workload{}
			Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())
			Expect(k8sClient.Get(ctx, secretKey(), sec)).To(Succeed())
		})
	})

	Describe("volumes", func() {
		volumes := []platformv1alpha1.Volume{{
			Name: dataVol, Path: "/var/lib/app",
			Size: resource.MustParse("2Gi"),
		}}

		It("claims the storage, mounts it, and stops rolling", func() {
			create(platformv1alpha1.WorkloadSpec{Image: testImage, Volumes: volumes})
			reconcileNow()

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: name + "-data", Namespace: namespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}))
			Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("2Gi"))

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())

			// Recreate, not RollingUpdate. Both pods would want the same
			// ReadWriteOnce claim and the new one would never schedule.
			Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
			Expect(dep.Spec.Strategy.RollingUpdate).To(BeNil())
			Expect(*dep.Spec.Replicas).To(BeNumerically("==", 1))

			pod := dep.Spec.Template.Spec
			Expect(pod.Volumes).To(HaveLen(2)) // tmp + data
			var claimed string
			for _, v := range pod.Volumes {
				if v.PersistentVolumeClaim != nil {
					claimed = v.PersistentVolumeClaim.ClaimName
				}
			}
			Expect(claimed).To(Equal(name + "-data"))
			Expect(pod.Containers[0].VolumeMounts).To(ContainElement(
				corev1.VolumeMount{Name: dataVol, MountPath: "/var/lib/app"}))
		})

		// The reason the claim carries no owner reference. Written as a test
		// because the failure it prevents is silent and total: deleting an app
		// to recreate it would take its data with it, and nothing would say so.
		It("leaves the claim behind when the workload is deleted", func() {
			create(platformv1alpha1.WorkloadSpec{Image: testImage, Volumes: volumes})
			reconcileNow()

			pvcKey := types.NamespacedName{Name: name + "-data", Namespace: namespace}
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
			Expect(pvc.OwnerReferences).To(BeEmpty(),
				"an owner reference here deletes the tenant's data on the next redeploy")

			app := &platformv1alpha1.Workload{}
			Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())
			Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
		})

		// Enforced by the API server rather than by the controller, so a
		// manifest applied with kubectl is refused too.
		It("refuses to autoscale a workload that has volumes", func() {
			err := k8sClient.Create(ctx, &platformv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: platformv1alpha1.WorkloadSpec{
					Image: testImage, Volumes: volumes,
					Autoscale: &platformv1alpha1.Autoscale{MinReplicas: 1, MaxReplicas: 3},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ReadWriteOnce"))
		})

		It("refuses a volume mounted over the emptyDir at /tmp", func() {
			err := k8sClient.Create(ctx, &platformv1alpha1.Workload{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: platformv1alpha1.WorkloadSpec{
					Image: testImage,
					Volumes: []platformv1alpha1.Volume{{
						Name: dataVol, Path: "/tmp", Size: resource.MustParse("1Gi"),
					}},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("emptyDir"))
		})
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

	// Deleting by name alone would destroy an object that merely shares a name
	// with this Workload — one that could be carrying live traffic and that this
	// operator never created.
	// The join between the git write path and the observer, across an update.
	//
	// The rollout id changes on every deploy by definition, so a Deployment
	// that keeps the id it was created with is an observer permanently
	// attaching new deploys to the oldest record — and once that record is
	// closed, to nothing at all. Every deploy would then sit pending until the
	// sweep marked it unknown: the platform reporting it cannot tell what
	// happened to its own deploy.
	It("carries a new rollout id onto a Deployment that already exists", func() {
		Expect(k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace,
				Annotations: map[string]string{rolloutAnnotation: "rollout-1"},
			},
			Spec: platformv1alpha1.WorkloadSpec{Image: testImage, Port: 3000},
		})).To(Succeed())
		reconcileNow()

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deploy)).To(Succeed())
		Expect(deploy.Annotations).To(HaveKeyWithValue(rolloutAnnotation, "rollout-1"))

		By("deploying again")
		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		app.Annotations[rolloutAnnotation] = "rollout-2"
		app.Spec.Image = "ghcr.io/example/app:2.0.0"
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		reconcileNow()

		Expect(k8sClient.Get(ctx, key, deploy)).To(Succeed())
		Expect(deploy.Annotations).To(HaveKeyWithValue(rolloutAnnotation, "rollout-2"),
			"the observer would attach this deploy to the previous deploy's record")
		// And the pod template still does not carry it, or every deploy would
		// roll the pods a second time on a value that is not part of the app.
		Expect(deploy.Spec.Template.Annotations).NotTo(HaveKey(rolloutAnnotation))
	})

	// The Deployment's annotation map has other writers, and the reconcile has to
	// share it. The deployment controller owns deployment.kubernetes.io/revision
	// and rewrites it on every sync; kubectl and Argo CD own theirs. Assigning
	// the map instead of merging into it deletes the revision annotation, the
	// deployment controller writes it back, that write wakes this controller
	// through Owns(&appsv1.Deployment{}), and the two rewrite one object without
	// pause — with the deployment controller's own status write as the casualty.
	// That is a Deployment stuck at READY 0/2 while every pod is Ready and its
	// ReplicaSet reports 2, which is how this arrived: a green e2e on one commit
	// and a 180-second timeout on the next.
	//
	// envtest runs no kube-controller-manager, so this spec plays its part and
	// writes the annotation by hand. Without that seeded key the spec is
	// vacuous: every annotation on the Deployment is then one the operator put
	// there itself, so replacing the map loses nothing and the bug passes.
	It("shares the annotation map with the controllers that also write to it", func() {
		Expect(k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace,
				Annotations: map[string]string{rolloutAnnotation: "rollout-1"},
			},
			Spec: platformv1alpha1.WorkloadSpec{Image: testImage, Port: 3000},
		})).To(Succeed())
		reconcileNow()

		By("standing in for the deployment controller and kubectl")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Annotations[revisionAnnotation] = "1"
		dep.Annotations["kubectl.kubernetes.io/last-applied-configuration"] = "{}"
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		settled := dep.ResourceVersion

		reconcileNow()

		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKeyWithValue(revisionAnnotation, "1"),
			"the operator deleted an annotation the deployment controller owns; that "+
				"controller writes it back, each write wakes the other, and the "+
				"Deployment's status never converges")
		Expect(dep.Annotations).To(HaveKeyWithValue("kubectl.kubernetes.io/last-applied-configuration", "{}"))
		Expect(dep.ResourceVersion).To(Equal(settled),
			"a reconcile with nothing to change still wrote to the Deployment, so every "+
				"pass emits a watch event and wakes every controller watching this object")

		By("carrying a new rollout id without disturbing the rest")
		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		app.Annotations[rolloutAnnotation] = "rollout-2"
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		reconcileNow()

		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKeyWithValue(rolloutAnnotation, "rollout-2"))
		Expect(dep.Annotations).To(HaveKeyWithValue(revisionAnnotation, "1"))

		By("retracting one the Workload no longer carries")
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		delete(app.Annotations, rolloutAnnotation)
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		reconcileNow()

		// Merging without ever deleting would be the other half of the same bug:
		// the operator could carry an annotation but never take one back.
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(rolloutAnnotation),
			"an annotation removed from the Workload can never be retracted")
		Expect(dep.Annotations).To(HaveKeyWithValue(revisionAnnotation, "1"))
	})

	// The write path's half of the ownership rule, and the one that was missing.
	// A tenant who may create Workloads and nothing else — no create or patch on
	// services, no get on secrets — names one after an object somebody else made
	// by hand, and the reconcile rewrites it, adopts it, and takes it down with
	// the Workload. It was demonstrated against an administrator's Service: the
	// selector moved to the attacker's pods, the labels were erased, and
	// deleting the Workload deleted the Service.
	//
	// The assertions are written as the victim would state them. Every field
	// checked here is one the attack was observed to change.
	It("leaves an object it does not own exactly as it found it", func() {
		// Quoted from the recorded attack rather than invented.
		victimSelector := map[string]string{containerName: "payments-backend"}
		foreign := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace,
				Labels: map[string]string{"owner": "platform-admin", "tier": "critical"},
			},
			Spec: corev1.ServiceSpec{
				Selector: victimSelector,
				Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(9000)}},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, foreign) }()

		create(platformv1alpha1.WorkloadSpec{Image: testImage, Port: 3000})

		By("reconciling, which must not succeed quietly")
		_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred(),
			"the reconcile adopted an object belonging to somebody else and said nothing")

		By("the victim being untouched in every way the attack changed it")
		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Spec.Selector).To(Equal(victimSelector),
			"the selector was rewritten, which sends this service's traffic to the workload's pods")
		Expect(got.Labels).To(HaveKeyWithValue("owner", "platform-admin"),
			"the owner's labels were erased")
		Expect(got.Labels).To(HaveKeyWithValue("tier", "critical"))
		Expect(got.OwnerReferences).To(BeEmpty(),
			"an owner reference was stamped on it, so deleting the workload now deletes this service too")

		By("the workload saying why, in terms the person who named it can act on")
		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		cond := meta.FindStatusCondition(app.Status.Conditions, "Ready")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Conflict"),
			"reported as a platform failure rather than as the name collision it is")
		Expect(cond.Message).To(ContainSubstring("Service"))
		Expect(cond.Message).To(ContainSubstring(name))
	})

	// The same guard must not refuse the objects this Workload made itself,
	// which is the way a check like this usually breaks: written against the
	// attack, never run against the ordinary second reconcile.
	It("keeps reconciling the objects it does own", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Port: 3000})
		reconcileNow()
		reconcileNow()

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.OwnerReferences).To(HaveLen(1))

		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		Expect(meta.FindStatusCondition(app.Status.Conditions, "Ready").Reason).NotTo(Equal("Conflict"))
	})

	// The helpers have their own unit tests, and those pass whether or not the
	// mutate actually calls them. This is the spec that binds the call site: it
	// seeds a label no rendered object carries and checks it is still there
	// after a pass, which is only true if the merge is wired in.
	It("keeps labels on its own objects that it did not write", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Port: 3000})
		reconcileNow()

		By("standing in for Argo CD and for whoever labels things by cost centre")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		svc.Labels["argocd.argoproj.io/instance"] = "storefront"
		svc.Labels["custom.example.com/cost-centre"] = "eng-42"
		Expect(k8sClient.Update(ctx, svc)).To(Succeed())

		reconcileNow()

		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.Labels).To(HaveKeyWithValue("argocd.argoproj.io/instance", "storefront"),
			"Argo CD reads its own tracking label to decide what it owns, and this deleted it")
		Expect(svc.Labels).To(HaveKeyWithValue("custom.example.com/cost-centre", "eng-42"))
		Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "damga-platform"),
			"the operator stopped writing its own labels")
	})

	It("refuses to delete an object it does not own", func() {
		foreign := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{Host: "someone-elses.example.com"}},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		// No domain, so the reconciler wants no Ingress and reaches for the
		// delete path.
		create(platformv1alpha1.WorkloadSpec{Image: testImage})
		reconcileNow()

		survivor := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, key, survivor)).To(Succeed(),
			"the operator deleted an Ingress it never created")
		Expect(survivor.Spec.Rules[0].Host).To(Equal("someone-elses.example.com"))
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
