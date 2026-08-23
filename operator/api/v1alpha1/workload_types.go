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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EnvVar is a literal environment variable. Anything that should not be read
// from `kubectl get workload -o yaml` belongs in a Secret listed under
// EnvFrom instead.
type EnvVar struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	Value string `json:"value,omitempty"`
}

// Resources is what the container asks for and what it may not exceed. Both are
// required: a pod with no request is scheduled blind and a pod with no memory
// limit can take a node down with it. The admission policy rejects workloads
// that omit either, so there is no point in letting an Workload omit them.
type Resources struct {
	// +kubebuilder:default="100m"
	CPURequest resource.Quantity `json:"cpuRequest,omitempty"`

	// +kubebuilder:default="128Mi"
	MemoryRequest resource.Quantity `json:"memoryRequest,omitempty"`

	// There is deliberately no CPU limit. CPU is compressible — throttling a
	// container is better than the latency cliff a limit produces — while memory
	// is not, so exceeding it has to mean eviction.
	// +kubebuilder:default="512Mi"
	MemoryLimit resource.Quantity `json:"memoryLimit,omitempty"`
}

// Health is how the platform decides whether a replica should receive traffic
// and whether it is still alive. The two questions are answered separately on
// purpose: a readiness failure removes one replica from a Service, a liveness
// failure restarts it. Pointing liveness at a dependency turns one slow database
// into every replica restarting at once.
type Health struct {
	// +kubebuilder:default="/healthz"
	LivenessPath string `json:"livenessPath,omitempty"`

	// +kubebuilder:default="/readyz"
	ReadinessPath string `json:"readinessPath,omitempty"`
}

// Autoscale replaces a fixed replica count with a range. Scale-up is immediate
// and scale-down is deliberately slow: being late to scale up costs users,
// being hasty to scale down causes thrashing.
type Autoscale struct {
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=2
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=60
	TargetCPUPercent int32 `json:"targetCPUPercent,omitempty"`
}

// WorkloadSpec is the whole surface a user writes. Everything absent from it
// is a decision the platform has already made: non-root, read-only root
// filesystem, all capabilities dropped, no service-account token, default-deny
// networking, a disruption budget, and a rollout that never takes a replica out
// before its replacement is ready. Those are not defaults — there is no field
// that turns them off.
// +kubebuilder:validation:XValidation:rule="has(self.replicas) ? !has(self.autoscale) : true",message="set replicas or autoscale, not both"
type WorkloadSpec struct {
	// Image is the container image to run. A digest is preferred; a tag is
	// accepted as long as it is not :latest, which means "whatever is there
	// today" and makes a rollback restore something other than what was rolled
	// back from.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self.contains(':') || self.contains('@')",message="image must carry an explicit tag or digest"
	// +kubebuilder:validation:XValidation:rule="!self.endsWith(':latest')",message="image must not use the :latest tag"
	Image string `json:"image"`

	// Port is the port the container listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	Port int32 `json:"port,omitempty"`

	// Replicas is a fixed replica count. Leave it unset and set autoscale
	// instead to hand the decision to the HorizontalPodAutoscaler.
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Domain publishes the workload at a hostname over HTTPS. Without it the
	// workload is reachable only inside the cluster.
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$')",message="domain must be a lowercase DNS name"
	Domain string `json:"domain,omitempty"`

	// Env holds literal environment variables. These are visible to anyone who
	// can read the Workload, so credentials belong in EnvFrom.
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// EnvFrom names Secrets whose keys are injected as environment variables.
	// The Secret is referenced, never copied, so its value never appears in this
	// object or in whatever produced it.
	// +listType=set
	EnvFrom []string `json:"envFrom,omitempty"`

	Resources Resources `json:"resources,omitempty"`

	Health Health `json:"health,omitempty"`

	Autoscale *Autoscale `json:"autoscale,omitempty"`
}

// WorkloadStatus reports what the platform observed, never what it was asked
// for. A field here that merely echoes the spec would make an Workload look
// healthy before anything had happened.
type WorkloadStatus struct {
	// Conditions follows the standard Kubernetes convention: Ready says whether
	// the workload is serving, Progressing says whether it is still rolling.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// URL is where the workload answers, once an ingress exists for it.
	URL string `json:"url,omitempty"`

	Replicas int32 `json:"replicas,omitempty"`

	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedGeneration is the spec generation this status describes. When it
	// trails metadata.generation the status is about the previous spec, which is
	// the difference between "not ready yet" and "ready, but not to this".
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:resource:shortName=wl
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Replicas",type=string,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Image",type=string,priority=1,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workload is one deployable thing: an image, a port, and optionally a
// hostname. The platform renders it into the dozen Kubernetes objects a
// production workload needs, all of them hardened the same way, so that the
// safe configuration is the only configuration rather than the one a careful
// author remembers to write.
type Workload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadSpec   `json:"spec,omitempty"`
	Status WorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadList contains a list of Workload.
type WorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workload `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Workload{}, &WorkloadList{})
		return nil
	})
}
