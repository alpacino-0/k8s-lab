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
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// The three keys of a Kubernetes condition and one certificate field, spelled
// as they appear inside an object this package has no Go type for.
const (
	conditionType    = "type"
	conditionStatus  = "status"
	conditionMessage = "message"

	// A field on a Certificate that this operator does not render, standing in
	// for whatever cert-manager or an administrator puts there.
	certUsage = "digital signature"

	// The issuer a kind cluster has; cluster/issuers.yaml installs it, and CI
	// issues the platform's own certificate from it.
	localIssuer = "selfsigned-ca"

	certGroup = "cert-manager.io"
)

// published is a Workload with a domain, which is the only thing that makes any
// of this render.
func published() *platformv1alpha1.Workload {
	return app(func(a *platformv1alpha1.Workload) { a.Spec.Domain = testDomain })
}

func certSpec(t *testing.T, cert *unstructured.Unstructured) map[string]any {
	t.Helper()
	spec, ok := cert.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("the certificate has no spec: %v", cert.Object)
	}
	return spec
}

func TestNoDomainMeansNoCertificate(t *testing.T) {
	if cert := desiredCertificate(app(), defaultClusterIssuer); cert != nil {
		t.Error("a certificate was rendered for a workload that is not published, " +
			"so a cluster-internal service asks a public CA for a name it does not have")
	}
}

// The chart's Certificate, absorbed. Every assertion here is a line of
// chart/templates/certificate.yaml, and the reason each line is in that file is
// written beside it there.
func TestCertificateCarriesWhatTheChartSpecified(t *testing.T) {
	cert := desiredCertificate(published(), "letsencrypt-staging")
	if cert == nil {
		t.Fatal("no certificate was rendered for a workload with a domain")
	}
	if got := cert.GetAPIVersion() + " " + cert.GetKind(); got != "cert-manager.io/v1 Certificate" {
		t.Errorf("the object is a %q, which cert-manager will never look at", got)
	}
	spec := certSpec(t, cert)

	if got := spec["secretName"]; got != "blog-tls" {
		t.Errorf("secretName = %v, want blog-tls", got)
	}
	if got := spec["dnsNames"]; !equalStrings(got, testDomain) {
		t.Errorf("dnsNames = %v, want [%s]; a certificate for another name is no certificate", got, testDomain)
	}

	issuer, _ := spec["issuerRef"].(map[string]any)
	if issuer["name"] != "letsencrypt-staging" {
		t.Errorf("issuerRef.name = %v, so the installation's issuer was ignored", issuer["name"])
	}
	if issuer["kind"] != clusterIssuerKind {
		t.Errorf("issuerRef.kind = %v; a namespaced Issuer would let a tenant sign their own hostname",
			issuer["kind"])
	}

	key, _ := spec["privateKey"].(map[string]any)
	if key["algorithm"] != certificateKeyAlgorithm || key["size"] != certificateKeySize {
		t.Errorf("private key = %v/%v, want %s/%d", key["algorithm"], key["size"],
			certificateKeyAlgorithm, certificateKeySize)
	}
	if key["rotationPolicy"] != certificateKeyRotation {
		t.Errorf("rotationPolicy = %v, so a renewal reuses the key it was supposed to replace",
			key["rotationPolicy"])
	}

	if spec["duration"] != certificateDuration || spec["renewBefore"] != certificateRenewBefore {
		t.Errorf("duration/renewBefore = %v/%v, want %s/%s",
			spec["duration"], spec["renewBefore"], certificateDuration, certificateRenewBefore)
	}
}

// The issuer is a property of the installation. Hard-coded, every kind cluster
// and every private CA would be asking Let's Encrypt for a name it cannot
// validate, and the certificate would never arrive.
func TestTheIssuerComesFromTheInstallation(t *testing.T) {
	r := &WorkloadReconciler{}
	if got := r.clusterIssuer(); got != defaultClusterIssuer {
		t.Errorf("unset issuer = %q, want %q", got, defaultClusterIssuer)
	}
	r.ClusterIssuer = localIssuer
	if got := r.clusterIssuer(); got != localIssuer {
		t.Errorf("configured issuer = %q, want %q", got, localIssuer)
	}
}

// The measured trap, and the reason the Certificate is rendered at all.
//
// With cert-manager.io/cluster-issuer on the Ingress, cert-manager's
// ingress-shim creates a Certificate of its own and ties it to that Ingress.
// Measured on the chart before the operator existed: two Ingresses over one
// hostname and one secret, and the second one refused with "certificate
// resource is not owned by this object / refusing to update non-owned
// certificate resource" — then the owning Ingress was deleted, the Certificate
// was collected with it, and the other Ingress quietly lost its certificate.
//
// Beside a Certificate this operator owns, the annotation is worse than that:
// two objects issuing into one secret, taking turns.
func TestTheIngressDoesNotAskTheShimForACertificate(t *testing.T) {
	ing := desiredIngress(published())
	if ing == nil {
		t.Fatal("no ingress was rendered for a workload with a domain")
	}
	if _, present := ing.Annotations[shimAnnotation]; present {
		t.Errorf("the ingress still carries %s, so cert-manager's ingress-shim creates a "+
			"second Certificate over the same secret as the one the operator owns", shimAnnotation)
	}
	if ing.Annotations["nginx.ingress.kubernetes.io/force-ssl-redirect"] != annotationTrue {
		t.Error("removing the cert-manager annotation took the redirect with it")
	}
}

// The join. A Certificate that writes one Secret and an Ingress that reads
// another is two objects that are each individually correct and together serve
// nothing.
func TestTheIngressReadsTheSecretTheCertificateWrites(t *testing.T) {
	a := published()
	ing := desiredIngress(a)
	cert := desiredCertificate(a, defaultClusterIssuer)

	if len(ing.Spec.TLS) != 1 {
		t.Fatalf("the ingress declares %d TLS blocks, want 1", len(ing.Spec.TLS))
	}
	if got, want := ing.Spec.TLS[0].SecretName, certSpec(t, cert)["secretName"]; got != want {
		t.Errorf("the ingress reads secret %q and the certificate writes %q, so the "+
			"hostname is served with whatever the ingress controller has instead", got, want)
	}
}

// The condition has to say which of four different things happened, because a
// different person acts on each: install cert-manager, wait, read what
// cert-manager said, or fix the operator's own access.
func TestTheTLSConditionSaysWhichWayItFailed(t *testing.T) {
	noMatch := &apimeta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: certGroup, Kind: certificateKind},
	}
	notFound := apierrors.NewNotFound(
		schema.GroupResource{Group: certGroup, Resource: "certificates"}, "blog")
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: certGroup, Resource: "certificates"}, "blog", nil)

	cases := []struct {
		name    string
		cert    *unstructured.Unstructured
		err     error
		status  metav1.ConditionStatus
		reason  string
		message string
	}{
		{
			name: "cert-manager is not installed", err: noMatch,
			status: metav1.ConditionFalse, reason: reasonCertManagerAbsent,
			message: "cert-manager is not installed",
		},
		{
			name: "nothing has been created yet", err: notFound,
			status: metav1.ConditionFalse, reason: reasonAwaitingCert,
		},
		{
			name: "the operator may not read it", err: forbidden,
			status: metav1.ConditionFalse, reason: reasonUnreadable,
		},
		{
			name: "cert-manager has not finished",
			cert: certificateWithReady(metav1.ConditionFalse, "no solver for challenge"),
			// cert-manager's own sentence, kept, because it is the only part of
			// this that says what is actually wrong.
			status: metav1.ConditionFalse, reason: reasonPending,
			message: "no solver for challenge",
		},
		{
			name:   "there is no status at all yet",
			cert:   &unstructured.Unstructured{Object: map[string]any{}},
			status: metav1.ConditionFalse, reason: reasonPending,
		},
		{
			name:   "issued",
			cert:   certificateWithReady(metav1.ConditionTrue, "Certificate is up to date"),
			status: metav1.ConditionTrue, reason: reasonIssued,
			message: testDomain,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := certificateCondition(testDomain, tc.cert, tc.err)
			if got.Type != tlsCondition {
				t.Errorf("condition type = %q, want %q", got.Type, tlsCondition)
			}
			if got.Status != tc.status {
				t.Errorf("status = %q, want %q", got.Status, tc.status)
			}
			if got.Reason != tc.reason {
				t.Errorf("reason = %q, want %q; the wrong person is sent to fix it",
					got.Reason, tc.reason)
			}
			if got.Message == "" {
				t.Error("the condition carries no message, so the status says something is " +
					"wrong without saying what")
			}
			if tc.message != "" && !strings.Contains(got.Message, tc.message) {
				t.Errorf("message = %q, want it to carry %q", got.Message, tc.message)
			}
		})
	}
}

// mergeSpec is the reason the certificate's spec is merged rather than
// assigned: cert-manager can default a field, a release can add one, and an
// administrator can set one this operator has no field for. Assigning deletes
// all three on the next pass, on an object another controller is writing to.
func TestMergingTheCertificateSpecKeepsWhatItDoesNotRender(t *testing.T) {
	existing := desiredCertificate(published(), defaultClusterIssuer)
	spec := certSpec(t, existing)
	spec["usages"] = []any{certUsage}
	spec["secretName"] = "stale-tls"

	mergeSpec(existing, desiredCertificate(published(), localIssuer))

	got := certSpec(t, existing)
	if _, kept := got["usages"]; !kept {
		t.Error("a field the operator does not render was deleted; cert-manager will " +
			"write it back and the two will rewrite one object without pause")
	}
	if got["secretName"] != "blog-tls" {
		t.Errorf("secretName = %v; a field the operator does render was not corrected", got["secretName"])
	}
	issuer, _ := got["issuerRef"].(map[string]any)
	if issuer["name"] != localIssuer {
		t.Errorf("issuerRef.name = %v; changing the installation's issuer changed nothing",
			issuer["name"])
	}
}

// readyConditions is a cert-manager status as cert-manager writes one: more
// than one condition, with Ready somewhere among them rather than first.
func readyConditions(status metav1.ConditionStatus, message string) []any {
	return []any{
		map[string]any{conditionType: "Approved", conditionStatus: string(metav1.ConditionTrue)},
		map[string]any{
			conditionType:    readyCondition,
			conditionStatus:  string(status),
			conditionMessage: message,
		},
	}
}

func certificateWithReady(status metav1.ConditionStatus, message string) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{Object: map[string]any{}}
	if err := unstructured.SetNestedSlice(
		cert.Object, readyConditions(status, message), "status", "conditions"); err != nil {
		panic(err)
	}
	return cert
}

func equalStrings(got any, want ...string) bool {
	list, ok := got.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i, w := range want {
		if list[i] != w {
			return false
		}
	}
	return true
}

// Everything above renders. These run against an API server, because the
// questions left are about objects that already exist: whether the certificate
// is created and owned, whether it is retracted with the domain, whether an
// annotation this operator used to write is taken back, and whether a status
// somebody else wrote reaches the Workload.
//
// The kind is installed here rather than in the suite's own bootstrap: it is a
// stand-in this file owns, and the specs that need it are all in this file.
var _ = Describe("Workload TLS", Ordered, func() {
	const (
		name      = "tls-app"
		namespace = "default"
		domain    = "shop.example.com"
		testImage = "ghcr.io/example/app:1.0.0"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: namespace}

	certificate := func() *unstructured.Unstructured {
		cert := &unstructured.Unstructured{}
		cert.SetAPIVersion(certificateAPIVersion)
		cert.SetKind(certificateKind)
		return cert
	}

	reconciler := &WorkloadReconciler{}
	reconcileNow := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	create := func(spec platformv1alpha1.WorkloadSpec) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &platformv1alpha1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       spec,
		})).To(Succeed())
	}

	BeforeAll(func() {
		_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
			Paths:              []string{filepath.Join("testdata", "certificates.cert-manager.io.yaml")},
			ErrorIfPathMissing: true,
		})
		Expect(err).NotTo(HaveOccurred())
		reconciler = &WorkloadReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			ClusterIssuer: localIssuer,
		}
	})

	AfterEach(func() {
		app := &platformv1alpha1.Workload{}
		if err := k8sClient.Get(ctx, key, app); err == nil {
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())
		}
		// envtest runs no garbage collector, so owner references cascade
		// nowhere here. Every rendered object has to go, not only the two these
		// specs are about: one left behind is owned by a Workload that no
		// longer exists, and the next spec's reconcile refuses it by name.
		for _, obj := range []client.Object{
			&appsv1.Deployment{}, &corev1.Service{}, &corev1.ServiceAccount{},
			&networkingv1.NetworkPolicy{}, &policyv1.PodDisruptionBudget{},
			&networkingv1.Ingress{}, certificate(),
		} {
			obj.SetName(name)
			obj.SetNamespace(namespace)
			_ = k8sClient.Delete(ctx, obj)
		}
	})

	It("renders a certificate for the domain and joins it to the ingress", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Domain: domain})
		reconcileNow()

		cert := certificate()
		Expect(k8sClient.Get(ctx, key, cert)).To(Succeed(),
			"a workload declared a domain and nothing asked for a certificate for it")

		Expect(cert.GetOwnerReferences()).To(HaveLen(1),
			"without an owner reference, deleting the Workload leaves a certificate renewing for ever")
		Expect(cert.GetOwnerReferences()[0].Kind).To(Equal("Workload"))

		spec, _, err := unstructured.NestedMap(cert.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(spec["dnsNames"]).To(Equal([]any{domain}))
		Expect(spec["issuerRef"]).To(HaveKeyWithValue("name", localIssuer),
			"the issuer the installation configured was not the one asked")

		ing := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, key, ing)).To(Succeed())
		Expect(ing.Spec.TLS[0].SecretName).To(Equal(spec["secretName"]),
			"the ingress reads a different secret from the one the certificate writes")
		Expect(ing.Annotations).NotTo(HaveKey(shimAnnotation),
			"cert-manager's ingress-shim will create a second certificate over the same secret")
	})

	// The second pass, which is where a reconcile that looks right becomes a
	// pair of controllers rewriting one object. Certificates have another
	// writer by definition.
	It("writes nothing on a pass with nothing to change", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Domain: domain})
		reconcileNow()

		cert := certificate()
		Expect(k8sClient.Get(ctx, key, cert)).To(Succeed())
		settled := cert.GetResourceVersion()

		reconcileNow()

		Expect(k8sClient.Get(ctx, key, cert)).To(Succeed())
		Expect(cert.GetResourceVersion()).To(Equal(settled),
			"a reconcile with nothing to change still wrote to the certificate, so every "+
				"pass wakes cert-manager and cert-manager's write wakes the next pass")
	})

	It("keeps a field on the certificate that it did not write", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Domain: domain})
		reconcileNow()

		By("standing in for cert-manager's webhook and for whoever set a field we have no name for")
		cert := certificate()
		Expect(k8sClient.Get(ctx, key, cert)).To(Succeed())
		Expect(unstructured.SetNestedStringSlice(
			cert.Object, []string{certUsage}, "spec", "usages")).To(Succeed())
		Expect(k8sClient.Update(ctx, cert)).To(Succeed())

		reconcileNow()

		Expect(k8sClient.Get(ctx, key, cert)).To(Succeed())
		usages, found, err := unstructured.NestedStringSlice(cert.Object, "spec", "usages")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(),
			"the operator deleted a field it does not render; whoever wrote it writes it "+
				"back, and the two rewrite one object without pause")
		Expect(usages).To(Equal([]string{certUsage}))
	})

	It("reports the certificate on the workload, without folding it into Ready", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Domain: domain})
		reconcileNow()

		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		tls := findCondition(app.Status.Conditions, tlsCondition)
		Expect(tls).NotTo(BeNil(),
			"a published workload says nothing about whether it has a certificate")
		Expect(tls.Status).To(Equal(metav1.ConditionFalse))
		Expect(tls.Reason).To(Equal(reasonPending))

		// Ready is about the pods and stays about the pods. envtest runs no
		// kubelet, so it is False here either way — what this asserts is that
		// it is False for its own reason rather than for the certificate's.
		ready := findCondition(app.Status.Conditions, readyCondition)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(BeElementOf("AwaitingDeployment", "NoReadyReplicas"),
			"a pending certificate was folded into Ready, so a workload that is serving "+
				"reports the same failure as one that cannot start")

		By("standing in for cert-manager, which is not in envtest and never will be")
		cert := certificate()
		Expect(k8sClient.Get(ctx, key, cert)).To(Succeed())
		Expect(unstructured.SetNestedSlice(cert.Object,
			readyConditions(metav1.ConditionTrue, "Certificate is up to date and has not expired"),
			"status", "conditions")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cert)).To(Succeed())

		reconcileNow()

		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		tls = findCondition(app.Status.Conditions, tlsCondition)
		Expect(tls.Status).To(Equal(metav1.ConditionTrue),
			"cert-manager issued the certificate and the workload still says it is waiting")
		Expect(tls.Reason).To(Equal(reasonIssued))
		Expect(app.Status.URL).To(Equal("https://" + domain))
	})

	// Without this the controller sleeps until something else wakes it, and
	// nothing else will: no watch on the certificate is possible on a cluster
	// where the kind may be absent. The status would sit at Pending after
	// cert-manager had finished.
	It("comes back to look at a certificate that has not been issued", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Domain: domain})

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(certificatePollInterval),
			"nothing will wake this controller when the certificate is issued, so the "+
				"status stays Pending for ever")

		By("and stopping once there is nothing left to wait for")
		cert := certificate()
		Expect(k8sClient.Get(ctx, key, cert)).To(Succeed())
		Expect(unstructured.SetNestedSlice(cert.Object,
			readyConditions(metav1.ConditionTrue, "issued"),
			"status", "conditions")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cert)).To(Succeed())

		result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero(),
			"an issued certificate is still being polled once every interval, for ever")
	})

	It("takes the certificate and the condition back when the domain goes", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Domain: domain})
		reconcileNow()
		Expect(k8sClient.Get(ctx, key, certificate())).To(Succeed())

		By("dropping the domain")
		app := &platformv1alpha1.Workload{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		app.Spec.Domain = ""
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		reconcileNow()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, certificate()))).To(BeTrue(),
			"a certificate is still being renewed for a hostname the workload no longer serves")

		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		Expect(findCondition(app.Status.Conditions, tlsCondition)).To(BeNil(),
			"the workload still reports TLS for a domain it does not have")
	})

	// The upgrade half. Every Ingress this operator rendered before it owned
	// the Certificate carries the shim annotation, and reconcileAnnotations
	// cannot retract a key outside the damga.co/ prefix — so without the
	// delete by name it would stay there for the life of the object, asking
	// cert-manager for a second certificate over the same secret.
	It("retracts the cert-manager annotation it used to write", func() {
		create(platformv1alpha1.WorkloadSpec{Image: testImage, Domain: domain})
		reconcileNow()

		By("standing in for the operator as it was before it rendered the certificate")
		ing := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, key, ing)).To(Succeed())
		ing.Annotations[shimAnnotation] = "letsencrypt-prod"
		ing.Annotations["nginx.ingress.kubernetes.io/proxy-body-size"] = "64m"
		Expect(k8sClient.Update(ctx, ing)).To(Succeed())

		reconcileNow()

		Expect(k8sClient.Get(ctx, key, ing)).To(Succeed())
		Expect(ing.Annotations).NotTo(HaveKey(shimAnnotation),
			"the ingress-shim still has a certificate of its own over this secret, and the "+
				"two write it in turn")
		Expect(ing.Annotations).To(HaveKeyWithValue("nginx.ingress.kubernetes.io/proxy-body-size", "64m"),
			"retracting one key took an administrator's unrelated annotation with it")
	})

	// The ownership rule, which the certificate reaches through the same apply
	// as everything else. Stated again here because the object is unstructured:
	// the guard reads a UID and an owner reference off an interface, and a kind
	// it has never been run against is exactly where that stops working.
	It("leaves a certificate it does not own exactly as it found it", func() {
		foreign := certificate()
		foreign.SetName(name)
		foreign.SetNamespace(namespace)
		foreign.Object["spec"] = map[string]any{
			"secretName": "payments-tls",
			"dnsNames":   []any{"payments.example.com"},
			"issuerRef":  map[string]any{"name": "letsencrypt-prod", "kind": clusterIssuerKind},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		create(platformv1alpha1.WorkloadSpec{Image: testImage, Domain: domain})
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred(),
			"the reconcile adopted a certificate belonging to somebody else and said nothing")

		got := certificate()
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		spec, _, _ := unstructured.NestedMap(got.Object, "spec")
		Expect(spec["dnsNames"]).To(Equal([]any{"payments.example.com"}),
			"the hostname was rewritten, so the certificate for someone else's domain stops renewing")
		Expect(spec["secretName"]).To(Equal("payments-tls"),
			"the secret was repointed, and whatever reads it is now served the wrong certificate")
		Expect(got.GetOwnerReferences()).To(BeEmpty(),
			"an owner reference was stamped on it, so deleting this workload deletes that certificate")
	})
})
