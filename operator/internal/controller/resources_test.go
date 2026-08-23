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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/alpacino-0/k8s-lab/operator/api/v1alpha1"
)

func app(mutate ...func(*platformv1alpha1.Application)) *platformv1alpha1.Application {
	a := &platformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "team-a"},
		Spec: platformv1alpha1.ApplicationSpec{
			Image: "ghcr.io/example/blog:1.2.3",
		},
	}
	for _, m := range mutate {
		m(a)
	}
	normalise(a)
	return a
}

// The whole premise of the platform is that these settings are not reachable
// from the API. If a spec could ever produce a pod without them, the promise is
// a default rather than a guarantee — so this runs the hardening assertions
// against every spec shape the type allows, including the emptiest one.
func TestHardeningSurvivesEverySpec(t *testing.T) {
	cases := map[string]*platformv1alpha1.Application{
		"minimal": app(),
		"fixed replicas": app(func(a *platformv1alpha1.Application) {
			a.Spec.Replicas = ptr.To(int32(7))
		}),
		"autoscaled": app(func(a *platformv1alpha1.Application) {
			a.Spec.Autoscale = &platformv1alpha1.Autoscale{MinReplicas: 3, MaxReplicas: 9, TargetCPUPercent: 70}
		}),
		"published": app(func(a *platformv1alpha1.Application) {
			a.Spec.Domain = "blog.example.com"
		}),
		"with env and secrets": app(func(a *platformv1alpha1.Application) {
			a.Spec.Env = []platformv1alpha1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}
			a.Spec.EnvFrom = []string{"blog-secrets"}
		}),
	}

	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			pod := desiredDeployment(a).Spec.Template.Spec
			c := pod.Containers[0]

			if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
				t.Error("pod may run as root")
			}
			if pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != runAsUID {
				t.Errorf("uid = %v, want %d", pod.SecurityContext.RunAsUser, runAsUID)
			}
			if pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Error("seccomp is not RuntimeDefault")
			}
			if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
				t.Error("a service-account token is mounted")
			}
			if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
				t.Error("root filesystem is writable")
			}
			if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
				t.Error("privilege escalation is allowed")
			}
			if len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
				t.Errorf("capabilities dropped = %v, want [ALL]", c.SecurityContext.Capabilities.Drop)
			}
			if c.Resources.Requests.Cpu().IsZero() || c.Resources.Requests.Memory().IsZero() {
				t.Error("a resource request is missing, which the admission policy rejects")
			}
			if c.Resources.Limits.Memory().IsZero() {
				t.Error("the memory limit is missing, which the admission policy rejects")
			}
			if c.LivenessProbe == nil || c.ReadinessProbe == nil {
				t.Error("a probe is missing")
			}
			if c.Lifecycle == nil || c.Lifecycle.PreStop == nil {
				t.Error("preStop is missing, so a drain will drop in-flight requests")
			}
		})
	}
}

// A CPU limit is deliberately absent: throttling a container produces a latency
// cliff, and the failure it prevents is one that memory limits already cover.
func TestNoCPULimit(t *testing.T) {
	c := desiredDeployment(app()).Spec.Template.Spec.Containers[0]
	if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Error("a CPU limit was set")
	}
}

func TestRolloutNeverRemovesAReplicaFirst(t *testing.T) {
	strategy := desiredDeployment(app()).Spec.Strategy
	if strategy.RollingUpdate.MaxUnavailable.IntValue() != 0 {
		t.Errorf("maxUnavailable = %v, want 0", strategy.RollingUpdate.MaxUnavailable)
	}
}

// The autoscaler owns the replica count once it exists. Writing one into the
// Deployment as well makes the operator and the autoscaler overwrite each other
// on every pass.
func TestAutoscalingOwnsTheReplicaCount(t *testing.T) {
	withHPA := app(func(a *platformv1alpha1.Application) {
		a.Spec.Autoscale = &platformv1alpha1.Autoscale{MinReplicas: 2, MaxReplicas: 8, TargetCPUPercent: 60}
	})
	if r := desiredDeployment(withHPA).Spec.Replicas; r != nil {
		t.Errorf("replicas = %d, want nil while autoscaling", *r)
	}
	if desiredHPA(withHPA) == nil {
		t.Fatal("no autoscaler was rendered")
	}

	fixed := app()
	if r := desiredDeployment(fixed).Spec.Replicas; r == nil || *r != 2 {
		t.Errorf("replicas = %v, want the default of 2", r)
	}
	if desiredHPA(fixed) != nil {
		t.Error("an autoscaler was rendered without one being asked for")
	}
}

func TestNetworkPolicyDeniesByDefaultAndBlocksMetadata(t *testing.T) {
	np := desiredNetworkPolicy(app())

	if len(np.Spec.PolicyTypes) != 2 {
		t.Fatalf("policy types = %v, want both Ingress and Egress — one alone leaves the other direction open",
			np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 {
		t.Fatal("ingress should be allowed from exactly one source")
	}
	if ns := np.Spec.Ingress[0].From[0].NamespaceSelector; ns == nil ||
		ns.MatchLabels["kubernetes.io/metadata.name"] != ingressNamespace {
		t.Error("ingress is not restricted to the ingress controller's namespace")
	}

	var dnsAllowed, metadataBlocked bool
	for _, rule := range np.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntValue() == 53 {
				dnsAllowed = true
			}
		}
		for _, to := range rule.To {
			if to.IPBlock == nil {
				continue
			}
			for _, ex := range to.IPBlock.Except {
				if ex == metadataCIDR {
					metadataBlocked = true
				}
			}
		}
	}
	if !dnsAllowed {
		t.Error("DNS egress is not allowed, so every outbound call will time out")
	}
	if !metadataBlocked {
		t.Errorf("%s is reachable, so a request-forgery bug reaches cloud credentials", metadataCIDR)
	}
}

func TestIngressOnlyExistsWithADomainAndAlwaysForcesTLS(t *testing.T) {
	if desiredIngress(app()) != nil {
		t.Fatal("an ingress was rendered for an application with no domain")
	}

	ing := desiredIngress(app(func(a *platformv1alpha1.Application) {
		a.Spec.Domain = "blog.example.com"
	}))
	if ing == nil {
		t.Fatal("no ingress was rendered for an application with a domain")
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].Hosts[0] != "blog.example.com" {
		t.Error("TLS is not configured for the domain")
	}
	if ing.Annotations["nginx.ingress.kubernetes.io/force-ssl-redirect"] != "true" {
		t.Error("plaintext is served rather than redirected")
	}
}

func TestServiceAccountCarriesNoToken(t *testing.T) {
	sa := desiredServiceAccount(app())
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("the service account hands out a token to anything that uses it")
	}
}

// A fixed minAvailable becomes a permanent eviction block the moment the app
// scales down to it, which turns a disruption budget into an outage during node
// maintenance.
func TestDisruptionBudgetIsProportional(t *testing.T) {
	pdb := desiredPodDisruptionBudget(app())
	if pdb.Spec.MinAvailable.Type != 1 {
		t.Errorf("minAvailable = %v, want a percentage", pdb.Spec.MinAvailable)
	}
}
