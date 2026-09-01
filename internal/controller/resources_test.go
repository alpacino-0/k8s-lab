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
	"strings"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	testDomain = "blog.example.com"

	// A pinned PostgreSQL, because the API refuses one that can move under a
	// running data directory.
	testPostgresImage = "postgres:17.2-alpine3.21"

	// The Database every case here names, shared so the specs that render it
	// and the specs that read it back cannot drift apart.
	dbName = "shop-db"

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

	var dnsAllowed, dnsScoped, metadataBlocked bool
	for _, rule := range np.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntValue() == 53 {
				dnsAllowed = true
				// Allowed is not the same as safe. This rule once had no
				// destination at all, which meant port 53 to any host on the
				// internet — a workload that gets compromised encodes what it
				// took into hostnames and reads the replies back out.
				for _, to := range rule.To {
					if to.PodSelector != nil && to.PodSelector.MatchLabels["k8s-app"] == "kube-dns" {
						dnsScoped = true
					}
				}
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
	if !dnsScoped {
		t.Error("DNS egress has no destination, so port 53 is open to every host on the internet " +
			"and a compromised workload can tunnel data out over it")
	}
	if !metadataBlocked {
		t.Errorf("%s is reachable, so a request-forgery bug reaches cloud credentials", metadataCIDR)
	}
}

// The metadata block is an IPv4 ipBlock, so it says nothing about IPv6 — and it
// does not have to, because no rule permits IPv6 egress and an ipBlock matches
// one family. The IPv6 metadata endpoint is unreachable today by omission.
//
// Omission is a bad way to hold a security property, because nothing announces
// when it stops holding. The day somebody makes this platform dual-stack they
// will add a ::/0 egress rule, it will look like the obvious counterpart of the
// rule already there, and it will reopen SSRF-to-cloud-credentials with nothing
// in the diff to say so. This test is the thing that says so.
func TestEgressHasNoIPv6Hole(t *testing.T) {
	// fd00:ec2::254 is where AWS answers; fe80::/10 is link-local generally.
	// An Except entry has to cover the address for the rule to be safe.
	const awsV6Metadata = "fd00:ec2::254"

	for _, rule := range desiredNetworkPolicy(app()).Spec.Egress {
		for _, to := range rule.To {
			if to.IPBlock == nil || !strings.Contains(to.IPBlock.CIDR, ":") {
				continue
			}
			var covered bool
			for _, ex := range to.IPBlock.Except {
				if strings.Contains(ex, ":") {
					covered = true
				}
			}
			if !covered {
				t.Errorf("egress rule %q permits IPv6 with no metadata address excepted, "+
					"so a request-forgery bug now reaches %s and returns cloud credentials — "+
					"add the IPv6 metadata ranges to Except",
					to.IPBlock.CIDR, awsV6Metadata)
			}
		}
	}
}

// Three fields, one rule: write what this operator renders, leave what it does
// not. Each case below is a thing that was observed to be destroyed, or that
// would have been on the first cluster where it mattered.
func TestReconcileWritesOnlyWhatThisOperatorOwns(t *testing.T) {
	t.Run("labels keep what another writer put there", func(t *testing.T) {
		// app.kubernetes.io/instance is also Argo CD's tracking label, and the
		// chart renders these same kinds into this same namespace.
		existing := map[string]string{
			instanceLabel:                     "argo-release",
			"argocd.argoproj.io/instance":     "shop",
			"app.kubernetes.io/part-of":       "storefront",
			"app.kubernetes.io/managed-by":    "Helm",
			"pod-template-hash":               "d45697f5c",
			"kubernetes.io/metadata.name-ish": "noise",
			"custom.example.com/cost-centre":  "eng-42",
			"app.kubernetes.io/version-ish":   "1.2.3",
		}
		got := reconcileLabels(existing, labelsFor(app()))

		if got["app.kubernetes.io/managed-by"] != "damga-platform" {
			t.Errorf("managed-by = %q, want the operator's own value", got["app.kubernetes.io/managed-by"])
		}
		if got["app.kubernetes.io/instance"] != "blog" {
			t.Errorf("instance = %q, want the workload name", got["app.kubernetes.io/instance"])
		}
		for _, k := range []string{
			"argocd.argoproj.io/instance", "app.kubernetes.io/part-of",
			"pod-template-hash", "custom.example.com/cost-centre",
		} {
			if _, ok := got[k]; !ok {
				t.Errorf("label %q was deleted; it belongs to somebody else and nothing here "+
					"can put it back", k)
			}
		}
	})

	t.Run("a nil label map is not grown into an empty one", func(t *testing.T) {
		if got := reconcileLabels(nil, nil); got != nil {
			t.Errorf("labels = %v, want nil — an empty map is a diff on every pass", got)
		}
	})

	t.Run("an allocated node port survives", func(t *testing.T) {
		// What an administrator gets after flipping the Service to NodePort.
		existing := []corev1.ServicePort{{Name: portName, Port: 80, NodePort: 31514}}
		desired := []corev1.ServicePort{{Name: portName, Port: 80, TargetPort: intstr.FromInt32(3000)}}

		got := reconcileServicePorts(existing, desired)
		if len(got) != 1 {
			t.Fatalf("ports = %v, want one", got)
		}
		if got[0].NodePort != 31514 {
			t.Errorf("nodePort = %d, want 31514 kept — writing 0 back releases it and the "+
				"next allocation is a different number, on every reconcile", got[0].NodePort)
		}
		if got[0].TargetPort.IntValue() != 3000 {
			t.Errorf("targetPort = %v, want the rendered value", got[0].TargetPort)
		}
	})

	t.Run("a port that is no longer rendered goes away", func(t *testing.T) {
		existing := []corev1.ServicePort{{Port: 80, NodePort: 31514}, {Port: 9090, NodePort: 32000}}
		got := reconcileServicePorts(existing, []corev1.ServicePort{{Port: 80}})
		if len(got) != 1 || got[0].Port != 80 {
			t.Errorf("ports = %v, want only the rendered one — the rendered list is the whole "+
				"truth about which ports exist", got)
		}
	})

	t.Run("autoscaler policies the API server defaulted are left in place", func(t *testing.T) {
		pods := autoscalingv2.PodsScalingPolicy
		existing := &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(int32(9999)),
				SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
				Policies: []autoscalingv2.HPAScalingPolicy{
					{Type: pods, Value: 4, PeriodSeconds: 60},
				},
			},
		}
		desired := desiredHPA(app(func(a *platformv1alpha1.Workload) {
			a.Spec.Autoscale = &platformv1alpha1.Autoscale{MinReplicas: 2, MaxReplicas: 8, TargetCPUPercent: 60}
		})).Spec.Behavior

		got := reconcileHPABehavior(existing, desired)
		if got.ScaleDown.StabilizationWindowSeconds == nil || *got.ScaleDown.StabilizationWindowSeconds == 9999 {
			t.Error("the stabilization window is this operator's to set and it was not written")
		}
		if len(got.ScaleDown.Policies) != 1 || got.ScaleDown.SelectPolicy == nil {
			t.Error("the scaling policies were deleted; the API server defaults them and puts " +
				"them straight back, so every pass writes to an object the autoscaler " +
				"controller is writing to as well")
		}
	})
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

// The other half of the same rule, and the reason restart needed its own field.
//
// A restart has to change the pod template, because that is the only thing the
// deployment controller hashes — and the first attempt reached for the platform
// annotations already on the object, which the test above refuses. This asserts
// the narrow version: exactly one annotation, present only when asked for.
func TestOnlyARestartPutsAnAnnotationOnThePodTemplate(t *testing.T) {
	quiet := desiredDeployment(app(func(a *platformv1alpha1.Workload) {
		a.Annotations = map[string]string{rolloutAnnotation: "t_alpha-api-prod-41"}
	}))
	if quiet.Spec.Template.Annotations != nil {
		t.Errorf("an untouched workload's pod template carries %v — an empty map is a "+
			"diff Argo CD reports for ever", quiet.Spec.Template.Annotations)
	}

	asked := desiredDeployment(app(func(a *platformv1alpha1.Workload) {
		a.Annotations = map[string]string{rolloutAnnotation: "t_alpha-api-prod-41"}
		a.Spec.RestartedAt = "2026-09-01T03:40:00Z"
	}))
	got := asked.Spec.Template.Annotations
	if got[restartedAtAnnotation] != "2026-09-01T03:40:00Z" {
		t.Errorf("restartedAt did not reach the pod template (%v); without it the "+
			"deployment controller sees no change and no pod is replaced", got)
	}
	if len(got) != 1 {
		t.Errorf("the pod template carries %d annotations, want exactly the restart one — "+
			"anything else here rolls pods on a value that was not a restart", len(got))
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

// A name and a Secret is the whole coupling between an app and its database.
//
// Deliberately no owner reference in either direction: an app is redeployed many
// times a day and its data outlives every one of those deploys, so deleting the
// app must not take the database with it. What crosses between them is the
// Secret the Database publishes, injected the way any other is.
func TestNamingADatabaseInjectsItsCredentials(t *testing.T) {
	withDB := app(func(a *platformv1alpha1.Workload) {
		a.Spec.Database = dbName
		a.Spec.EnvFrom = []string{"extra-secrets"}
	})
	c := desiredDeployment(withDB).Spec.Template.Spec.Containers[0]

	names := make([]string, 0, len(c.EnvFrom))
	for _, e := range c.EnvFrom {
		names = append(names, e.SecretRef.Name)
	}
	if len(names) != 2 || names[0] != dbName {
		t.Fatalf("envFrom = %v, want the database first", names)
	}
	// Ordering matters and is not alphabetical. Later entries win in
	// Kubernetes, so a variable the tenant sets explicitly overrides the
	// platform's — somebody naming a database and then overriding DB_HOST is
	// pointing the app elsewhere on purpose.
	if names[1] != "extra-secrets" {
		t.Errorf("envFrom = %v; the tenant's own secrets must come last or the "+
			"platform silently overrules them", names)
	}

	// And no reference in the other direction. Nothing rendered here names the
	// database as an owner or a dependency the garbage collector can follow.
	for _, v := range desiredDeployment(withDB).Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("the workload mounts %s; a database attached by volume is one "+
				"that cannot be redeployed independently", v.Name)
		}
	}
}

func TestNoDatabaseMeansNoInjectedSecret(t *testing.T) {
	c := desiredDeployment(app()).Spec.Template.Spec.Containers[0]
	if len(c.EnvFrom) != 0 {
		t.Errorf("envFrom = %v for a workload that named no database", c.EnvFrom)
	}
}
