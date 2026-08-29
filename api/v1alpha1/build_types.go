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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BuildSpec is one commit turned into one image.
//
// A Build is a fact about the past, not a desired state — which makes it the
// odd one out in this API. Everything else here says what should be true and is
// reconciled towards for ever; a Build says "this commit was asked for" and is
// finished as soon as it succeeds or fails. It is a resource anyway, for two
// reasons: it keeps the write path unchanged (a build is requested by a commit,
// like everything else), and it makes every build an object somebody can look
// at afterwards rather than a log line.
//
// The whole spec is immutable. Rebuilding is a new Build, because a Build that
// can be edited cannot answer "what produced the image that is running".
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a build is a record of one commit; request another by creating a new Build"
type BuildSpec struct {
	// Repo is the git repository to build.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self.startsWith('https://') || self.startsWith('git@')",message="repo must be an https:// or git@ URL"
	Repo string `json:"repo"`

	// Revision is the commit to build.
	//
	// A full SHA and never a branch name. A branch is a moving target, and a
	// record that says "built main" cannot answer which main — which is the
	// only question anybody asks of it later.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Revision string `json:"revision"`

	// Path is the directory inside the repository to build, for a monorepo.
	// Empty is the root.
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/') && !self.contains('..')",message="path must be relative and must not climb out of the repository"
	Path string `json:"path,omitempty"`

	// Image is where the result is pushed, without a tag: the platform appends
	// the revision, so an image reference always names the commit that produced
	// it.
	//
	// The tag is looked for in the last path segment and not in the whole
	// string, because a registry carries a port: this rule first read
	// `!self.contains(':')` and refused `registry.damga-registry.svc:5000/ci/app`
	// — the platform's own registry, on the first build that ever ran. The
	// opposite half of the same trap is already written down in WorkloadSpec's
	// Image, which had to look in the last segment for the tag rather than at
	// the whole string. Same colon, both directions, and the note that existed
	// did not stop the second one.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="!self.split('/')[int(self.split('/').size()) - 1].contains(':')",message="image must not carry a tag; the revision is appended"
	Image string `json:"image"`

	// Builder chooses how the source becomes an image. Empty means detect:
	// a Dockerfile is used when there is one, and the language is detected when
	// there is not — which is what a user who has never written a Dockerfile
	// expects to happen.
	// +kubebuilder:validation:Enum=detect;dockerfile;buildpack
	// +kubebuilder:default=detect
	Builder BuildMethod `json:"builder,omitempty"`

	Resources Resources `json:"resources,omitempty"`
}

// BuildMethod is how source becomes an image.
type BuildMethod string

const (
	// BuildDetect uses the Dockerfile if the repository has one and detects the
	// language if it does not.
	BuildDetect BuildMethod = "detect"
	// BuildDockerfile always uses the Dockerfile, and fails when there is none
	// rather than quietly building something else.
	BuildDockerfile BuildMethod = "dockerfile"
	// BuildBuildpack always detects the language, ignoring any Dockerfile.
	BuildBuildpack BuildMethod = "buildpack"
)

// BuildPhase is where a build got to.
type BuildPhase string

const (
	BuildPending   BuildPhase = "Pending"
	BuildRunning   BuildPhase = "Running"
	BuildSucceeded BuildPhase = "Succeeded"
	BuildFailed    BuildPhase = "Failed"
)

// BuildStatus is what happened, reported and never inferred.
type BuildStatus struct {
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Phase BuildPhase `json:"phase,omitempty"`

	// Digest is the image the build produced, and it is the point of the whole
	// resource: a tag can be moved and a digest cannot, so this is what gets
	// written into the Workload.
	//
	// Reported by the build itself through its termination log, which is the
	// only channel a job with no API token has. Giving a build container a
	// cluster identity would hand an identity to whatever it is compiling.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`

	// Method is what the build actually did, which is not always what was
	// asked: "detect" resolves to one of the others, and knowing which one ran
	// is the difference between a reproducible record and a guess.
	Method BuildMethod `json:"method,omitempty"`

	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Message is why a failed build failed, quoted from the builder rather than
	// summarised. A build fails for reasons that belong to the user's code, and
	// a platform that paraphrases the compiler is a platform nobody can debug
	// against.
	Message string `json:"message,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Method",type=string,JSONPath=`.status.method`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.spec.revision`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Build turns one commit into one image.
type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BuildSpec   `json:"spec,omitempty"`
	Status BuildStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BuildList contains a list of Build.
type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Build `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Build{}, &BuildList{})
		return nil
	})
}
