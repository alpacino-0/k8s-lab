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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	// BuildResultPath is where the build writes its answer. The same channel
	// the restore rehearsal uses, for the same reason: a job with no service
	// account token has one way to speak to the control plane, and giving a
	// build container an API identity would hand that identity to whatever it
	// is compiling.
	BuildResultPath = "/dev/termination-log"

	buildContainer = "build"

	buildComponent = "build"

	// buildkitImage builds from a Dockerfile without a Docker daemon.
	//
	// Not the rootless variant, which was tried first and does not work on the
	// hosts this targets: Ubuntu 23.10 and later restrict unprivileged user
	// namespaces through AppArmor, so rootlesskit cannot start its child and
	// buildkitd never comes up. That is a host kernel policy and no pod setting
	// reaches it. cluster/build-namespace.yaml carries the full reasoning and
	// the boundary that replaces the pod-level one.
	buildkitImage = "moby/buildkit:v0.27.0"
)

// buildScript decides how to build and then builds.
//
// One script rather than two containers, because the decision needs the working
// tree: "is there a Dockerfile" is answerable only after the clone, and a
// controller that guessed beforehand would be guessing about somebody else's
// repository.
//
// Written to the termination log on both paths. A build that succeeds and says
// nothing is indistinguishable from one that never ran, and the digest is the
// entire product of this job — the tag is a name that can be moved and the
// digest is what goes into the Workload.
const buildScript = `set -eu
: "${REPO:?}" "${REVISION:?}" "${IMAGE:?}" "${METHOD:?}"

RAN=unknown
SAID=

say() {
  SAID=1
  printf '{"method":"%s","message":%s}' "$RAN" \
    "$(printf '%s' "$1" | head -c 900 | sed 's/\\/\\\\/g; s/"/\\"/g' | tr -d '\n' | sed 's/^/"/; s/$/"/')" \
    > ` + BuildResultPath + `
}

fail() { say "$1"; exit 1; }

# Anything that exits without having written a result writes one here.
#
# Added because the first real build died on a git command that had no || fail
# after it, so set -e killed the script before anything reached the termination
# log. The control plane then had only the Job's own condition — "backoff limit
# exceeded" — while the actual reason sat in a pod log nobody was reading. A
# handler that only covers the failures it anticipated is a handler for the
# failures that do not happen.
# Guarded on SAID, because fail() writes and then exits — which fires this trap,
# which would otherwise overwrite the specific message with the generic one. The
# first version did exactly that, and the only reason it was not shipped is that
# reading it back was cheaper than watching it run.
trap 'rc=$?; [ "$rc" = 0 ] || [ -n "$SAID" ] || say "the build exited with status $rc; see the job log"' EXIT

# A subdirectory the container creates itself, not the mount point.
#
# /workspace is an emptyDir and belongs to root; git refuses to operate on a
# repository whose directory somebody else owns ("detected dubious ownership")
# and this container is uid 1000. Creating the directory here makes the
# container its owner, which is the answer git is actually asking for —
# safe.directory would tell git to stop checking instead.
mkdir -p /workspace/src
cd /workspace/src


git init -q . || fail "could not initialise a repository"
git remote add origin "$REPO" || fail "could not set the remote to $REPO"
# One commit, not a history. A shallow fetch of the exact revision is the
# difference between seconds and minutes on a repository with any age, and
# nothing here reads the log.
git fetch -q --depth 1 origin "$REVISION" || fail "could not fetch $REVISION from $REPO"
git checkout -q FETCH_HEAD || fail "could not check out $REVISION"

SRC="/workspace/src${PATH_IN_REPO:+/$PATH_IN_REPO}"
[ -d "$SRC" ] || fail "the path $PATH_IN_REPO does not exist in this repository"

RAN="$METHOD"
if [ "$METHOD" = detect ]; then
  if [ -f "$SRC/Dockerfile" ]; then RAN=dockerfile; else RAN=buildpack; fi
fi

case "$RAN" in
  dockerfile)
    [ -f "$SRC/Dockerfile" ] || fail "builder is dockerfile but this repository has none"
    # stderr kept rather than discarded. The first run of this reported "the
    # Dockerfile build failed", which is true of every failure and says nothing;
    # the actual reason — a directory that was still read-only — was in a pod
    # log the control plane never reads. A build fails for reasons that belong
    # to the user's code, so the user's own tool has to be the one talking.
    buildctl-daemonless.sh build \
      --frontend dockerfile.v0 \
      --local context="$SRC" --local dockerfile="$SRC" \
      --output "type=image,name=${IMAGE},push=true,registry.insecure=true" \
      --metadata-file /tmp/meta.json 2>/tmp/build.err \
      || fail "$(tail -c 700 /tmp/build.err)"
    DIGEST=$(sed -n 's/.*"containerimage.digest":"\([^"]*\)".*/\1/p' /tmp/meta.json)
    ;;
  buildpack)
    # The half that is not written. Cloud Native Buildpacks' lifecycle runs
    # daemonless — which is why it and not the pack CLI, since pack needs a
    # Docker daemon — but it is five phases with their own cache layout, and a
    # half-wired one would report a digest for an image nobody can run.
    fail "this repository has no Dockerfile, and language detection is not built yet"
    ;;
esac

[ -n "${DIGEST:-}" ] || fail "the build reported no digest"
printf '{"digest":"%s","method":"%s"}' "$DIGEST" "$RAN" > ` + BuildResultPath + `
`

// desiredBuildJob renders the job that turns a commit into an image.
func desiredBuildJob(b *platformv1alpha1.Build) *batchv1.Job {
	labels := map[string]string{
		nameLabel:      b.Name,
		instanceLabel:  b.Name,
		componentLabel: buildComponent,
		managedByLabel: managedByDamga,
	}
	method := b.Spec.Builder
	if method == "" {
		method = platformv1alpha1.BuildDetect
	}

	res := b.Spec.Resources
	if res.MemoryLimit.IsZero() {
		// A build is the burstiest thing on the cluster and the only workload
		// here that is expected to end. Bounded so one repository's compile
		// cannot take the node away from every application running on it.
		res.MemoryLimit = resource.MustParse("2Gi")
	}
	if res.MemoryRequest.IsZero() {
		res.MemoryRequest = resource.MustParse("512Mi")
	}
	if res.CPURequest.IsZero() {
		res.CPURequest = resource.MustParse("250m")
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: b.Name, Namespace: b.Namespace, Labels: labels,
		},
		Spec: batchv1.JobSpec{
			// One attempt. A build fails because the code does not compile far
			// more often than because the cluster hiccuped, and retrying a
			// compile error three times turns a ten-second answer into a
			// thirty-second one while producing the same message.
			BackoffLimit: ptr.To(int32(0)),

			// And a wall clock, which BackoffLimit does not provide.
			//
			// Measured the hard way: a pod template that admission refuses is
			// not a pod failure, so it does not count against BackoffLimit at
			// all. The job controller retried the create twenty times in
			// fifteen minutes, the Job never reached a terminal condition, and
			// the Build sat in Running for ever — a build that cannot start
			// looked exactly like a build that is slow.
			//
			// activeDeadlineSeconds is measured from the Job's start rather
			// than from a pod's, so it bounds that case too.
			ActiveDeadlineSeconds: ptr.To(int64(30 * 60)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
					},
					// Still no token. The concession below is about the node,
					// not about the API: a build has no business talking to the
					// control plane, and it reports its result through the
					// termination log precisely so it never needs to.
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:    buildContainer,
						Image:   buildkitImage,
						Command: []string{"sh", "-c", buildScript},
						Env: []corev1.EnvVar{
							{Name: "REPO", Value: b.Spec.Repo},
							{Name: "REVISION", Value: b.Spec.Revision},
							{Name: "PATH_IN_REPO", Value: b.Spec.Path},
							{Name: "IMAGE", Value: b.Spec.Image + ":" + b.Spec.Revision},
							{Name: "METHOD", Value: string(method)},
						},
						SecurityContext: &corev1.SecurityContext{
							// Privileged, and this is the concession the whole
							// design turns on. buildkitd creates mount and
							// network namespaces for each build step; rootless
							// avoids needing that and is unavailable here.
							//
							// Written as one field rather than a list of
							// capabilities on purpose: CAP_SYS_ADMIN plus mount
							// is most of privileged with a longer spelling, and
							// a spec that looks narrow while granting the same
							// thing is worse than one that says what it does.
							// Narrowing it is real work and is not done.
							Privileged: ptr.To(true),
						},
						TerminationMessagePath:   BuildResultPath,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    res.CPURequest,
								corev1.ResourceMemory: res.MemoryRequest,
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: res.MemoryLimit},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "workspace", MountPath: "/workspace"},
							{Name: tmpVolume, MountPath: tmpPath},
							{Name: "buildkit", MountPath: "/var/lib/buildkit"},
							{Name: runVolume, MountPath: "/run"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "workspace", VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						}},
						{Name: tmpVolume, VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						}},
						{Name: "buildkit", VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						}},
						{Name: runVolume, VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						}},
					},
				},
			},
		},
	}
}
