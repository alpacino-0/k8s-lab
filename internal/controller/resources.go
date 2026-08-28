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

	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// Everything in this block is the platform's decision rather than the user's.
// There is no field on Workload that changes any of it, which is the point:
// a security context that can be switched off is a convention, and conventions
// are what the workload written at 2am does not follow.
const (
	runAsUID  int64 = 1000
	runAsGID  int64 = 1000
	graceSecs int64 = 30

	// The single container in a rendered pod. Named here rather than
	// inline because CI's diagnostics and the hardening assertions both
	// reach for containers[0] by this name.
	containerName = "app"

	// The port the Service and the Ingress agree to call the application
	// port, and the label whose second writer is Argo CD. Both are named
	// because more than one place has to spell them the same way.
	portName       = "http"
	instanceLabel  = "app.kubernetes.io/instance"
	nameLabel      = "app.kubernetes.io/name"
	managedByLabel = "app.kubernetes.io/managed-by"
	componentLabel = "app.kubernetes.io/component"
	managedByDamga = "damga-platform"

	// The default database and role name a Database gets, and the value
	// the CRD defaults them to. Spelled once so the two cannot drift.
	defaultDatabaseName = "app"

	// Dropped from every container this platform renders.
	capabilityAll = "ALL"

	// Where the tmp volume is mounted. A read-only root filesystem leaves
	// nowhere else to write, and most runtimes write something.
	tmpPath = "/tmp"

	// preStop buys the endpoint removal a head start. Without it, a pod is
	// removed from the Service and its process is killed at the same moment,
	// and whichever loses that race drops requests that were already in flight.
	preStopSleepSecs int64 = 5

	// How long a container may take to answer its first probe before the kubelet
	// gives up. Liveness does not run until the startup probe has succeeded, so
	// this is the budget for a slow start — a JVM, a runtime that compiles on
	// boot, a process that runs a migration first.
	startupPeriodSecs    int32 = 5
	startupFailureBudget int32 = 60

	// The ingress controller lives here. Namespaces carry
	// kubernetes.io/metadata.name automatically, so this needs no cooperation
	// from whoever installed it.
	ingressNamespace = "ingress-nginx"

	// Link-local. The cloud metadata service sits at 169.254.169.254, and
	// reaching it from an workload container turns any request-forgery bug
	// into cloud credentials. Egress is otherwise open, because an workload
	// that cannot make outbound calls is not a platform, it is a sandbox.
	metadataCIDR = "169.254.0.0/16"

	// ingress-nginx reads its annotations as strings, so "true" is a value here
	// rather than a boolean.
	annotationTrue = "true"
)

func labelsFor(app *platformv1alpha1.Workload) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app.Name,
		instanceLabel:                  app.Name,
		"app.kubernetes.io/managed-by": "damga-platform",
	}
}

func selectorFor(app *platformv1alpha1.Workload) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": app.Name,
		instanceLabel:            app.Name,
	}
}

// annotationPrefix is what marks an annotation as this platform's own, and
// therefore as something worth carrying from a Workload onto what it renders.
//
// A prefix rather than a list because the set grows, and a filter rather than
// copying everything because a Workload arrives through git and through Argo
// CD and picks up their annotations on the way. last-applied-configuration on
// a Deployment it does not describe is noise at best; a sync-wave copied onto
// a child object is an instruction meant for the parent.
const annotationPrefix = "damga.co/"

// platformAnnotations returns the Workload's own annotations, and nil rather
// than an empty map when there are none — an empty map is a diff Argo CD
// reports for ever.
func platformAnnotations(app *platformv1alpha1.Workload) map[string]string {
	var out map[string]string
	for k, v := range app.Annotations {
		if !strings.HasPrefix(k, annotationPrefix) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// reconcileAnnotations brings the platform's annotations on a live object up to
// date without claiming the ones it does not own.
//
// Nothing owns a Deployment's annotation map outright, and this operator least
// of all. The deployment controller owns deployment.kubernetes.io/revision and
// writes it on every sync; kubectl owns last-applied-configuration; Argo CD
// owns its tracking id. Assigning the whole map — which is what the mutate used
// to do — claims all of them, so every reconcile deletes the revision
// annotation, the deployment controller puts it back, that write wakes this
// controller through Owns(&appsv1.Deployment{}), and the two rewrite one object
// without pause. The write the deployment controller loses in that fight is its
// own status write, which is how a Deployment reports READY 0/2 while every pod
// is Ready and its own ReplicaSet reports 2.
//
// So the operator claims exactly one namespace of keys, annotationPrefix, and
// reconciles that namespace and nothing else: desired keys are written, keys
// under the prefix that the Workload has stopped carrying are deleted so a
// rollout id can be retracted, and every other key is left as it was found.
//
// It changes nothing when there is nothing to change — no key is deleted that
// was not there, no value is written that is not different, and an object with
// no annotations keeps a nil map rather than growing an empty one. That is what
// makes the reconcile idempotent, and idempotence here is not tidiness: a write
// per pass is what turns a shared object into a war.
func reconcileAnnotations(existing, desired map[string]string) map[string]string {
	for k := range existing {
		if _, wanted := desired[k]; wanted || !strings.HasPrefix(k, annotationPrefix) {
			continue
		}
		delete(existing, k)
	}
	for k, v := range desired {
		if existing == nil {
			existing = make(map[string]string, len(desired))
		}
		existing[k] = v
	}
	return existing
}

// reconcileLabels writes the labels this operator sets and leaves every other
// label on the object alone.
//
// A merge and not an assignment for the same reason as the annotations, with a
// sharper edge: app.kubernetes.io/instance is one of the keys written here, and
// it is also Argo CD's default tracking label. The chart renders the same kinds
// into the same namespace as the operator, so a Workload that shares a name with
// a release puts two writers on one object, each rewriting that label with its
// own idea of what the instance is — and Argo CD reads its own tracking label to
// decide what it owns.
//
// No delete pass, unlike the annotations. labelsFor returns a fixed set, so
// there is never a key this operator wrote and has since stopped wanting;
// adding one that varies would mean this needs the same retraction the
// annotations have. It writes nothing when nothing differs, which is what keeps
// the reconcile from producing an Update on every pass.
func reconcileLabels(existing, desired map[string]string) map[string]string {
	for k, v := range desired {
		if existing == nil {
			existing = make(map[string]string, len(desired))
		}
		existing[k] = v
	}
	return existing
}

// reconcileServicePorts writes the ports this operator renders while keeping the
// node port the API server allocated for each one.
//
// nodePort is not a field this platform has an opinion about — it is not
// reachable from the Workload spec and the rendered value is always zero, which
// means "allocate me one". Writing that zero back over a Service that has one
// releases it and the next allocation is a different number, so a Service an
// administrator moved to NodePort changes its port on every reconcile while
// keeping the type that made it matter.
//
// Matched by port number, which is what a nodePort is attached to. A port this
// operator no longer renders is dropped, because the rendered list is the whole
// truth about which ports exist.
func reconcileServicePorts(existing, desired []corev1.ServicePort) []corev1.ServicePort {
	allocated := make(map[int32]int32, len(existing))
	for _, p := range existing {
		if p.NodePort != 0 {
			allocated[p.Port] = p.NodePort
		}
	}
	out := make([]corev1.ServicePort, 0, len(desired))
	for _, p := range desired {
		if p.NodePort == 0 {
			p.NodePort = allocated[p.Port]
		}
		out = append(out, p)
	}
	return out
}

// reconcileHPABehavior writes the stabilization windows this operator sets and
// leaves the scaling policies beside them alone.
//
// Behavior is a struct this platform only half fills in: it renders a
// StabilizationWindowSeconds for each direction and nothing else, so the API
// server defaults the policies and selectPolicy. Assigning the whole Behavior
// deleted those defaults on every pass, and the server put them straight back —
// on an object the autoscaler controller is also writing to. Nothing had been
// observed to break yet, which is the only reason this reads as tidiness rather
// than as the outage it is one busy cluster away from being.
func reconcileHPABehavior(
	existing, desired *autoscalingv2.HorizontalPodAutoscalerBehavior,
) *autoscalingv2.HorizontalPodAutoscalerBehavior {
	if desired == nil {
		return existing
	}
	if existing == nil {
		return desired
	}
	setWindow := func(e, d *autoscalingv2.HPAScalingRules) *autoscalingv2.HPAScalingRules {
		if d == nil {
			return e
		}
		if e == nil {
			return d
		}
		e.StabilizationWindowSeconds = d.StabilizationWindowSeconds
		return e
	}
	existing.ScaleUp = setWindow(existing.ScaleUp, desired.ScaleUp)
	existing.ScaleDown = setWindow(existing.ScaleDown, desired.ScaleDown)
	return existing
}

// withAnnotations attaches annotations to metadata built by objectMeta,
// leaving the field nil when there are none.
func withAnnotations(meta metav1.ObjectMeta, annotations map[string]string) metav1.ObjectMeta {
	meta.Annotations = annotations
	return meta
}

func objectMeta(app *platformv1alpha1.Workload) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      app.Name,
		Namespace: app.Namespace,
		Labels:    labelsFor(app),
	}
}

// desiredServiceAccount exists so the workload has an identity that is not
// `default`, and so that identity can be denied a token. An workload that
// does not call the Kubernetes API has no reason to carry a credential for it.
func desiredServiceAccount(app *platformv1alpha1.Workload) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta:                   objectMeta(app),
		AutomountServiceAccountToken: ptr.To(false),
	}
}

// desiredDeployment renders the Deployment.
//
// It is the only resource that carries the platform's annotations, and the
// only one that needs to: the observer watches Deployments, reads
// damga.co/rollout off one, and attaches the running state to the evidence
// record that annotation names. Putting them on the Service or the autoscaler
// as well would be annotations nothing reads.
func desiredDeployment(app *platformv1alpha1.Workload) *appsv1.Deployment {
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: path,
					Port: intstr.FromInt32(app.Spec.Port),
				},
			},
			PeriodSeconds:    5,
			TimeoutSeconds:   3,
			FailureThreshold: 3,
		}
	}

	env := make([]corev1.EnvVar, 0, len(app.Spec.Env))
	for _, e := range app.Spec.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}

	// The database's Secret first, so a variable the tenant sets explicitly in
	// EnvFrom below wins. Someone naming a database and then overriding
	// DB_HOST is pointing the app somewhere else on purpose, and a platform
	// that silently put its own value last would be overruling them.
	envFrom := make([]corev1.EnvFromSource, 0, len(app.Spec.EnvFrom)+1)
	if app.Spec.Database != "" {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: app.Spec.Database},
			},
		})
	}
	for _, name := range app.Spec.EnvFrom {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		})
	}

	replicas := app.Spec.Replicas
	if app.Spec.Autoscale != nil {
		// Owned by the HorizontalPodAutoscaler from here on. Writing a replica
		// count into the Deployment as well would make the two fight, with the
		// operator restoring a number the autoscaler had just corrected.
		replicas = nil
	} else if replicas == nil {
		replicas = ptr.To(int32(2))
	}

	return &appsv1.Deployment{
		ObjectMeta: withAnnotations(objectMeta(app), platformAnnotations(app)),
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorFor(app)},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					// Never fewer than the current count while rolling. Combined
					// with readiness gating this is what makes an upgrade cost
					// zero requests rather than a few.
					MaxUnavailable: ptr.To(intstr.FromInt32(0)),
					MaxSurge:       ptr.To(intstr.FromInt32(1)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(app)},
				Spec: corev1.PodSpec{
					ServiceAccountName:            app.Name,
					AutomountServiceAccountToken:  ptr.To(false),
					TerminationGracePeriodSeconds: ptr.To(graceSecs),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(runAsUID),
						RunAsGroup:     ptr.To(runAsGID),
						FSGroup:        ptr.To(runAsGID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					// Spread across nodes where possible, but ScheduleAnyway: a
					// single-node cluster should still run the workload, just
					// without the guarantee that draining a node is free.
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew:           1,
						TopologyKey:       corev1.LabelHostname,
						WhenUnsatisfiable: corev1.ScheduleAnyway,
						LabelSelector:     &metav1.LabelSelector{MatchLabels: selectorFor(app)},
					}},
					Containers: []corev1.Container{{
						Name:  containerName,
						Image: app.Spec.Image,
						Ports: []corev1.ContainerPort{{
							Name:          portName,
							ContainerPort: app.Spec.Port,
						}},
						Env:     env,
						EnvFrom: envFrom,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    app.Spec.Resources.CPURequest,
								corev1.ResourceMemory: app.Spec.Resources.MemoryRequest,
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: app.Spec.Resources.MemoryLimit,
							},
						},
						// Liveness is held back until startup succeeds. Without
						// a startup probe the kubelet begins liveness checks
						// immediately, and a container that needs 30 seconds to
						// answer is killed at 15 — every time, forever, with no
						// field on the spec that could rescue it.
						StartupProbe:   startupProbe(app),
						LivenessProbe:  probe(app.Spec.Health.LivenessPath),
						ReadinessProbe: probe(app.Spec.Health.ReadinessPath),
						Lifecycle: &corev1.Lifecycle{
							// The kubelet's own sleep, not /bin/sleep. An exec
							// hook needs that binary to exist in the image, and
							// the images worth running here are distroless ones
							// that have no shell and no coreutils — so the exec
							// form fails silently and the grace period this is
							// supposed to buy never happens.
							PreStop: &corev1.LifecycleHandler{
								Sleep: &corev1.SleepAction{Seconds: preStopSleepSecs},
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						// A read-only root filesystem breaks anything that writes
						// a temp file, which is most runtimes. This gives them
						// somewhere to write that does not survive the pod.
						VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: tmpPath}},
					}},
					Volumes: []corev1.Volume{{
						Name:         "tmp",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
				},
			},
		},
	}
}

// startupProbe gives a slow container startupFailureBudget * startupPeriodSecs
// seconds to answer once, and asks nothing of it after that.
func startupProbe(app *platformv1alpha1.Workload) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: app.Spec.Health.LivenessPath,
				Port: intstr.FromInt32(app.Spec.Port),
			},
		},
		PeriodSeconds:    startupPeriodSecs,
		TimeoutSeconds:   3,
		FailureThreshold: startupFailureBudget,
	}
}

func desiredService(app *platformv1alpha1.Workload) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: objectMeta(app),
		Spec: corev1.ServiceSpec{
			Selector: selectorFor(app),
			Ports: []corev1.ServicePort{{
				Name:       portName,
				Port:       80,
				TargetPort: intstr.FromString(portName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// desiredNetworkPolicy denies everything this workload is not explicitly
// allowed. One policy is enough: selecting a pod with both policy types set
// means only the listed traffic survives.
func desiredNetworkPolicy(app *platformv1alpha1.Workload) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	appPort := intstr.FromInt32(app.Spec.Port)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: objectMeta(app),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selectorFor(app)},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": ingressNamespace},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &appPort}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Forgetting this rule is the most common way a default-deny
					// policy silently breaks an workload: name resolution
					// fails and every outbound call times out with no obvious
					// cause.
					//
					// Addressed to the cluster's resolver and nowhere else. With
					// no "to" this said port 53 to any host on the internet,
					// which is a data exfiltration channel wearing the one
					// protocol nobody blocks: a compromised workload encodes
					// what it stole into names and reads the answers back. The
					// chart's own policy has always scoped it this way; the
					// operator's copy of the rule lost the destination.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k8s-app": "kube-dns"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
				{
					// Everything else, minus the cloud metadata service.
					//
					// This rule is IPv4 only, and that is load-bearing rather
					// than an oversight: an ipBlock matches one family, so with
					// no ::/0 rule beside it egress over IPv6 is denied outright
					// — including to the IPv6 metadata endpoints that this
					// except list does not name. Deny-by-omission is doing real
					// work here, and it is one line away from being undone.
					//
					// So whoever makes this platform dual-stack: adding a ::/0
					// egress rule without an Except carrying the IPv6 metadata
					// ranges (fd00:ec2::254/128 on AWS, and fe80::/10 for
					// link-local generally) reopens SSRF-to-cloud-credentials on
					// the day it lands, with nothing in the diff to say so.
					// TestEgressHasNoIPv6Hole fails if that rule appears without
					// them.
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{
							CIDR:   "0.0.0.0/0",
							Except: []string{metadataCIDR},
						},
					}},
				},
			},
		},
	}
}

func desiredPodDisruptionBudget(app *platformv1alpha1.Workload) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: objectMeta(app),
		Spec: policyv1.PodDisruptionBudgetSpec{
			// Expressed as a percentage rather than a count so it stays correct
			// while the autoscaler moves the replica count around. A fixed
			// minAvailable can silently block every eviction once the app scales
			// down to it.
			MinAvailable: ptr.To(intstr.FromString("50%")),
			Selector:     &metav1.LabelSelector{MatchLabels: selectorFor(app)},
		},
	}
}

func desiredHPA(app *platformv1alpha1.Workload) *autoscalingv2.HorizontalPodAutoscaler {
	if app.Spec.Autoscale == nil {
		return nil
	}
	a := app.Spec.Autoscale

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: objectMeta(app),
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       app.Name,
			},
			MinReplicas: ptr.To(a.MinReplicas),
			MaxReplicas: a.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptr.To(a.TargetCPUPercent),
					},
				},
			}},
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(0)),
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(300)),
				},
			},
		},
	}
}

func desiredIngress(app *platformv1alpha1.Workload) *networkingv1.Ingress {
	if app.Spec.Domain == "" {
		return nil
	}
	pathType := networkingv1.PathTypePrefix
	meta := objectMeta(app)
	meta.Annotations = map[string]string{
		"cert-manager.io/cluster-issuer":                 "letsencrypt-prod",
		"nginx.ingress.kubernetes.io/ssl-redirect":       annotationTrue,
		"nginx.ingress.kubernetes.io/force-ssl-redirect": annotationTrue,
	}

	return &networkingv1.Ingress{
		ObjectMeta: meta,
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("nginx"),
			TLS: []networkingv1.IngressTLS{{
				Hosts:      []string{app.Spec.Domain},
				SecretName: fmt.Sprintf("%s-tls", app.Name),
			}},
			Rules: []networkingv1.IngressRule{{
				Host: app.Spec.Domain,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: app.Name,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

// quantityOrDefault keeps a zero Quantity from reaching the API server as "0",
// which would be a request for no CPU rather than the default the CRD declares.
func quantityOrDefault(q resource.Quantity, fallback string) resource.Quantity {
	if q.IsZero() {
		return resource.MustParse(fallback)
	}
	return q
}
