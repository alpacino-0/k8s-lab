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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	testDomain = "blog.example.com"

	// Two annotations named side by side, because telling them apart is the
	// whole subject of the tests below: the first is this operator's to write
	// and retract, the second belongs to the deployment controller and is only
	// ever passed through.
	rolloutAnnotation  = annotationPrefix + "rollout"
	revisionAnnotation = "deployment.kubernetes.io/revision"

	// A rollout id and the one that replaces it.
	oldRollout = "t_1"
	newRollout = "t_2"
)

func app(mutate ...func(*platformv1alpha1.Workload)) *platformv1alpha1.Workload {
	a := &platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "team-a"},
		Spec: platformv1alpha1.WorkloadSpec{
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
	cases := map[string]*platformv1alpha1.Workload{
		"minimal": app(),
		"fixed replicas": app(func(a *platformv1alpha1.Workload) {
			a.Spec.Replicas = ptr.To(int32(7))
		}),
		"autoscaled": app(func(a *platformv1alpha1.Workload) {
			a.Spec.Autoscale = &platformv1alpha1.Autoscale{MinReplicas: 3, MaxReplicas: 9, TargetCPUPercent: 70}
		}),
		"published": app(func(a *platformv1alpha1.Workload) {
			a.Spec.Domain = testDomain
		}),
		"with env and secrets": app(func(a *platformv1alpha1.Workload) {
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
	withHPA := app(func(a *platformv1alpha1.Workload) {
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
		t.Fatal("an ingress was rendered for an workload with no domain")
	}

	ing := desiredIngress(app(func(a *platformv1alpha1.Workload) {
		a.Spec.Domain = testDomain
	}))
	if ing == nil {
		t.Fatal("no ingress was rendered for an workload with a domain")
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].Hosts[0] != testDomain {
		t.Error("TLS is not configured for the domain")
	}
	if ing.Annotations["nginx.ingress.kubernetes.io/force-ssl-redirect"] != annotationTrue {
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

// A container that needs longer than liveness allows must still be given the
// chance to start. Without a startup probe the kubelet begins liveness checks
// at once and kills anything slower than periodSeconds * failureThreshold.
func TestSlowStartIsSurvivable(t *testing.T) {
	c := desiredDeployment(app()).Spec.Template.Spec.Containers[0]

	if c.StartupProbe == nil {
		t.Fatal("no startup probe: any image slower than ~15s restarts forever")
	}
	budget := c.StartupProbe.PeriodSeconds * c.StartupProbe.FailureThreshold
	if budget < 120 {
		t.Errorf("startup budget is %ds, too short for a runtime that compiles or migrates on boot", budget)
	}
	if c.StartupProbe.HTTPGet == nil || c.StartupProbe.HTTPGet.Path != c.LivenessProbe.HTTPGet.Path {
		t.Error("the startup probe should check the same endpoint liveness will")
	}
}

// The images this platform is built to run are distroless: no shell, no
// coreutils, no /bin/sleep. An exec hook would fail and the grace period it
// promises would never happen.
func TestPreStopDoesNotDependOnTheImage(t *testing.T) {
	lc := desiredDeployment(app()).Spec.Template.Spec.Containers[0].Lifecycle
	if lc == nil || lc.PreStop == nil {
		t.Fatal("no preStop hook")
	}
	if lc.PreStop.Exec != nil {
		t.Error("preStop execs a binary, which a distroless image does not have")
	}
	if lc.PreStop.Sleep == nil || lc.PreStop.Sleep.Seconds <= 0 {
		t.Error("preStop does not use the kubelet's own sleep")
	}
}

// The rollout annotation has to reach the Deployment, and it has to reach the
// object rather than the pod template.
//
// This is the join between the git write path and the observer, and it was
// missing: objectMeta set a name, a namespace and labels, so a Workload
// carrying damga.co/rollout produced a Deployment carrying nothing. The
// observer reads the annotation off the Deployment and treats its absence as
// "not ours, nothing to move", so every record damga itself opened would have
// stayed pending until the sweep gave up and marked it unknown — the platform
// reporting that it could not tell what happened to its own deploy.
//
// Both halves were broken. desiredDeployment never set it, and the apply's
// mutate function copied labels and not annotations, so even once it was set a
// second deploy's new id would never land on an existing Deployment.
func TestTheRolloutAnnotationReachesTheDeploymentObject(t *testing.T) {
	const rollout = "t_alpha-api-prod-41"
	a := app(func(a *platformv1alpha1.Workload) {
		a.Annotations = map[string]string{
			rolloutAnnotation: rollout,
			// Argo CD's, kubectl's, and anything else that annotates the
			// object on its way through. Copying everything would drag these
			// onto a Deployment they say nothing true about.
			"kubectl.kubernetes.io/last-applied-configuration": "{...}",
			"argocd.argoproj.io/sync-wave":                     "1",
		}
	})

	d := desiredDeployment(a)
	if got := d.Annotations[rolloutAnnotation]; got != rollout {
		t.Errorf("the Deployment's rollout annotation is %q, want %q — the observer cannot "+
			"attach this deploy to the record that opened it", got, rollout)
	}
	for _, k := range []string{
		"kubectl.kubernetes.io/last-applied-configuration",
		"argocd.argoproj.io/sync-wave",
	} {
		if _, copied := d.Annotations[k]; copied {
			t.Errorf("%q was copied onto the Deployment", k)
		}
	}

	// On the object, never on the pod template. The deployment controller
	// hashes .spec.template alone, so a template annotation would roll every
	// pod on a value that changes on every deploy — and the rollout id changes
	// on every deploy by definition.
	if _, onTemplate := d.Spec.Template.Annotations[rolloutAnnotation]; onTemplate {
		t.Error("the rollout annotation is on the pod template, which makes every deploy roll pods twice")
	}
}

// A Workload with nothing of ours on it must not grow an empty annotation map,
// which shows up as a diff Argo CD reports for ever.
//
// This is about the object desiredDeployment renders, not the one on the
// cluster. Reading it as "a managed Deployment carries no annotations" is how
// the outage happened: the mutate assigned this nil over the live map and took
// deployment.kubernetes.io/revision with it. What the operator renders and what
// it may claim on an object it shares are two different questions —
// reconcileAnnotations answers the second.
func TestNoDamgaAnnotationsMeansNoAnnotations(t *testing.T) {
	d := desiredDeployment(app())
	if len(d.Annotations) != 0 {
		t.Errorf("Deployment annotations = %v, want none", d.Annotations)
	}
}

// The nil map is the trap in reconcileAnnotations, and it is worth pinning
// separately: writing into a nil map panics, and allocating one when there is
// nothing to put in it turns every reconcile of an unannotated Workload into a
// diff — an empty map where the live object has none.
func TestReconcileAnnotationsOwnsOnlyItsOwnPrefix(t *testing.T) {
	for _, tc := range []struct {
		name              string
		existing, desired map[string]string
		want              map[string]string
	}{
		{
			name:     "nothing on either side stays nil",
			existing: nil, desired: nil, want: nil,
		},
		{
			name:     "keeps what other controllers own",
			existing: map[string]string{revisionAnnotation: "7"},
			desired:  map[string]string{rolloutAnnotation: newRollout},
			want: map[string]string{
				revisionAnnotation: "7",
				rolloutAnnotation:  newRollout,
			},
		},
		{
			name:     "a new rollout id replaces the old one",
			existing: map[string]string{rolloutAnnotation: oldRollout, "argocd.argoproj.io/tracking-id": "x"},
			desired:  map[string]string{rolloutAnnotation: newRollout},
			want: map[string]string{
				rolloutAnnotation:                newRollout,
				"argocd.argoproj.io/tracking-id": "x",
			},
		},
		{
			name:     "one the Workload stopped carrying is retracted",
			existing: map[string]string{rolloutAnnotation: oldRollout, revisionAnnotation: "7"},
			desired:  nil,
			want:     map[string]string{revisionAnnotation: "7"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileAnnotations(tc.existing, tc.desired)
			if len(got) != len(tc.want) {
				t.Fatalf("annotations = %v, want %v", got, tc.want)
			}
			if tc.want == nil && got != nil {
				t.Fatalf("annotations = %v, want a nil map rather than an empty one", got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("annotation %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
