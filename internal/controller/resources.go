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
		"app.kubernetes.io/instance":   app.Name,
		"app.kubernetes.io/managed-by": "damga-platform",
	}
}

func selectorFor(app *platformv1alpha1.Workload) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     app.Name,
		"app.kubernetes.io/instance": app.Name,
	}
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

	envFrom := make([]corev1.EnvFromSource, 0, len(app.Spec.EnvFrom))
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
		ObjectMeta: objectMeta(app),
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
						Name:  "app",
						Image: app.Spec.Image,
						Ports: []corev1.ContainerPort{{
							Name:          "http",
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
						VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
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
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromString("http"),
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
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
				{
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
