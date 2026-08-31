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

	// prepareContainer clones and decides. It only exists when the buildpack
	// path is possible, because that path needs a second image and the
	// decision needs the working tree.
	prepareContainer = "prepare"

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

	// builderImage is where the Cloud Native Buildpacks lifecycle lives, and it
	// has to be a second container rather than a second toolchain: the
	// lifecycle does not just call the buildpacks, it runs them *in* this
	// image's filesystem, so the build base and the buildpacks are the
	// container. buildkit's image has no /cnb at all.
	//
	// Measured from the image config on 2026-09-01 rather than assumed, because
	// three of the numbers below are load-bearing and two of them are not what
	// a reader would guess:
	//
	//   lifecycle 0.21.18, platform APIs 0.7 through 0.15, none deprecated
	//   User 1001:1000 (CNB_USER_ID=1001, CNB_GROUP_ID=1000) — jammy, not 1000
	//   linux/amd64 only: the tag resolves to one manifest, not an index
	//   1267 MB of compressed layers, against buildkit's 101 MB
	//
	// The third is a limitation nothing here enforces: on an arm64 node this
	// pod cannot start, and neither the API nor the status says why beforehand.
	// The fourth is why the pod shape below depends on what was asked for.
	//
	// -base rather than -full, whose extra 393 MB buys Ruby and PHP. Base's own
	// description lists Java, Go, .NET Core, Node.js, Python, Apache HTTPD,
	// NGINX and Procfile — so "every language" is already not true here, and
	// the two the brief named that are missing are exactly those two.
	builderImage = "paketobuildpacks/builder-jammy-base:0.4.630"

	// platformAPI is the contract version the phases are asked to speak.
	//
	// Pinned rather than left to default, because a lifecycle with no
	// CNB_PLATFORM_API in its environment picks for itself and the choice
	// changes what the flags below mean. 0.13 sits inside the window the
	// pinned builder supports (0.7-0.15, measured) with room on both sides, so
	// a builder bumped forward or nudged back still speaks it.
	platformAPI = "0.13"

	// preparedPath is where the second container sees the tree the first one
	// cloned. Deliberately not /workspace: in the builder image /workspace is
	// the application directory the lifecycle builds from, it is owned by the
	// buildpack user, and a volume mounted there would cover it — the same
	// trap emptyDir has already sprung once in this repository.
	preparedPath = "/prepared"

	// workspaceVolume carries the checkout from the container that clones to
	// the container that builds.
	workspaceVolume = "workspace"

	// The two inputs both containers need: where the result is pushed, and
	// which directory of the repository is the application.
	envImage      = "IMAGE"
	envPathInRepo = "PATH_IN_REPO"

	// buildpackHome is set because the platform specification lists HOME as a
	// base-image variable that SHOULD be in the lifecycle's environment and is
	// inherited by every buildpack unmodified — and the builder image's config
	// does not set one (measured: its env is PATH, CNB_USER_ID, CNB_GROUP_ID,
	// CNB_STACK_ID). Whether the runtime would supply one anyway is not
	// measured, which is the reason to set it rather than to find out from a
	// buildpack failing somewhere inside itself. /home/cnb exists in the image
	// and belongs to the buildpack user.
	buildpackHome = "/home/cnb"
)

// resultPrelude is the half of both build scripts that talks to the control
// plane, shared so the two cannot drift: the field names here are the field
// names in buildResult, and a shell script is a joint nothing type-checks.
//
// Written to the termination log on both paths. A build that succeeds and says
// nothing is indistinguishable from one that never ran, and the digest is the
// entire product of this job — the tag is a name that can be moved and the
// digest is what goes into the Workload.
//
// RESULT is read from the environment so a test can point it at a file and run
// the script for real. It defaults to the same path the pod spec sets, so the
// container behaves identically whether or not anybody set it.
const resultPrelude = `set -eu
: "${IMAGE:?}"

RESULT=${RESULT:-` + BuildResultPath + `}
RAN=unknown
SAID=

say() {
  SAID=1
  printf '{"method":"%s","message":%s}' "$RAN" \
    "$(printf '%s' "$1" | head -c 900 | sed 's/\\/\\\\/g; s/"/\\"/g' \
       | tr '\n\t' '  ' | tr -d '\000-\037' | sed 's/^/"/; s/$/"/')" \
    > "$RESULT"
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
`

// buildScript clones, decides how to build, and builds from a Dockerfile.
//
// One script rather than two containers for the decision, because the decision
// needs the working tree: "is there a Dockerfile" is answerable only after the
// clone, and a controller that guessed beforehand would be guessing about
// somebody else's repository.
//
// When the answer is a buildpack it stops here and says nothing at all. The
// lifecycle cannot run in this image, so the container after this one finishes
// the job — and a termination message written here would be the one the control
// plane reads, because it reads the first container that left a parseable one.
const buildScript = resultPrelude + `
: "${REPO:?}" "${REVISION:?}" "${METHOD:?}"

WORKSPACE=${WORKSPACE:-/workspace}

# A subdirectory the container creates itself, not the mount point.
#
# /workspace is an emptyDir and belongs to root; git refuses to operate on a
# repository whose directory somebody else owns ("detected dubious ownership")
# and this container is uid 1000. Creating the directory here makes the
# container its owner, which is the answer git is actually asking for —
# safe.directory would tell git to stop checking instead.
mkdir -p "$WORKSPACE/src"
cd "$WORKSPACE/src"


git init -q . || fail "could not initialise a repository"
git remote add origin "$REPO" || fail "could not set the remote to $REPO"
# One commit, not a history. A shallow fetch of the exact revision is the
# difference between seconds and minutes on a repository with any age, and
# nothing here reads the log.
git fetch -q --depth 1 origin "$REVISION" || fail "could not fetch $REVISION from $REPO"
git checkout -q FETCH_HEAD || fail "could not check out $REVISION"

SRC="$WORKSPACE/src${PATH_IN_REPO:+/$PATH_IN_REPO}"
[ -d "$SRC" ] || fail "the path $PATH_IN_REPO does not exist in this repository"

RAN="$METHOD"
if [ "$METHOD" = detect ]; then
  if [ -f "$SRC/Dockerfile" ]; then RAN=dockerfile; else RAN=buildpack; fi
fi
# The decision, written where the next container can read it. It cannot repeat
# the detection itself: it sees the tree read-only, and two answers to one
# question is one answer too many.
printf '%s' "$RAN" > "$WORKSPACE/method"

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
    # Whitespace-tolerant, because buildctl writes the metadata file
    # indented: "containerimage.digest": "sha256:..." has a space after the
    # colon and the first version of this pattern did not allow one. The build
    # had already run and pushed; only the reading failed, and the platform
    # reported "no digest" — which reads as a broken build rather than as a
    # broken sed.
    DIGEST=$(tr -d '\n' < /tmp/meta.json \
      | sed -n 's/.*"containerimage\.digest"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    # And if it is still empty, hand over the file rather than a verdict about
    # it. A parser that cannot find what it wants knows less about why than the
    # bytes it was given.
    [ -n "$DIGEST" ] || fail "the build finished but no digest could be read from: $(head -c 400 /tmp/meta.json)"
    printf '{"digest":"%s","method":"%s"}' "$DIGEST" "$RAN" > "$RESULT"
    ;;
  buildpack)
    # Silence on purpose. The lifecycle container answers, and this one exiting
    # zero with an empty termination message is what lets it.
    ;;
esac
`

// buildpackScript turns a repository with no Dockerfile into an image.
//
// Cloud Native Buildpacks' lifecycle rather than the pack CLI: pack builds by
// driving a Docker daemon, and there is no daemon here and will not be one —
// mounting docker.sock into a pod is root on the node. The lifecycle binaries
// talk to a registry over HTTP instead, which is the only reason any of this
// fits in a cluster.
//
// The five phases are run one at a time rather than through the lifecycle's own
// `creator`, which bundles exactly these five into one process. creator is less
// code here and strictly worse to fail with: one exit status for five different
// jobs, so "the build failed" could mean the language was not recognised, or a
// dependency did not download, or the registry refused the push, and the
// control plane could not tell which. Run separately, the phase that failed is
// named by the thing that failed.
//
// The order is the specification's, not the obvious one. detect looks like it
// should come first and does not: the platform specification (Platform API 0.12
// through 0.15, all checked) mandates analyzer, detector, restorer, builder,
// exporter, because the analyzer resolves the run image and writes the
// analyzed.toml that the detector then updates. Running detect first appears to
// work and produces an image built against the wrong base.
//
// Every path below is read from the environment with the real default, so the
// script can be run for real against fake phases in a test. That is the whole
// reason the indirection exists: the rule this file is under is that a failure
// has to arrive carrying the builder's own words, and a rule nothing executes
// is decoration.
const buildpackScript = resultPrelude + `
PREPARED=${PREPARED:-` + preparedPath + `}
APP=${APP:-/workspace}
LAYERS=${LAYERS:-/layers}
LIFECYCLE=${LIFECYCLE:-/cnb/lifecycle}
ERR=${ERR:-/tmp/phase.err}

[ -f "$PREPARED/method" ] || fail "the prepare step ended without saying how this repository should be built"
# Already built and already reported, by the container that made the decision.
# Exiting quietly leaves its answer as the only one there is.
[ "$(cat "$PREPARED/method")" = buildpack ] || exit 0

RAN=buildpack

SRC="$PREPARED/src${PATH_IN_REPO:+/$PATH_IN_REPO}"
[ -d "$SRC" ] || fail "the path $PATH_IN_REPO does not exist in this repository"

# Copied, not built where it landed, and the mount is read-only to keep it that
# way. The clone belongs to the uid that cloned it, in a container that is not
# this one; the application directory in this image belongs to the buildpack
# user (1001:1000, measured from the image). Buildpacks write into the
# application directory, so handing them a tree they do not own fails somewhere
# inside somebody else's buildpack with a message about permissions.
mkdir -p "$APP"
cp -R "$SRC/." "$APP/" || fail "could not copy the checkout into $APP"

# The lifecycle's own stderr, forwarded rather than summarised.
#
# This is the rule this whole file is under: a build fails for reasons that
# belong to the user's code, and a platform that replaces the compiler's message
# with its own is a platform nobody can debug against. The phase name is put in
# front of the quote because it says which of the five spoke, and that is the
# one thing the quoted text never says.
#
# It also goes to the pod log. The termination message is capped and a real
# stack of buildpack output is not, so the short answer reaches the control
# plane and the long one stays somewhere it can still be read.
phase() {
  name=${1##*/}
  rc=0
  "$@" 2>"$ERR" || rc=$?
  if [ "$rc" != 0 ]; then
    cat "$ERR" >&2
    said=$(tail -c 700 "$ERR" 2>/dev/null || true)
    [ -n "$said" ] || said="the $name phase exited with status $rc and said nothing; see the job log"
    fail "$name: $said"
  fi
}

# Registry access to the previous image and the run image, and the analyzed.toml
# the rest of the phases build on.
phase "$LIFECYCLE/analyzer" -layers="$LAYERS" "$IMAGE"
# Which buildpacks want this repository. Exits 20 or 21 when none do, which is
# the honest answer to "we could not work out what this is".
phase "$LIFECYCLE/detector" -app="$APP" -layers="$LAYERS"
# Layers this build can reuse. Restores from the previous image only: there is
# no cache directory, because the only disk this pod has dies with it. A cache
# that survives a build needs a volume or a cache image, and neither is wired.
phase "$LIFECYCLE/restorer" -layers="$LAYERS"
# The user's code, compiled by somebody else's buildpack.
phase "$LIFECYCLE/builder" -app="$APP" -layers="$LAYERS"
# Assembles the layers onto the run image and pushes. No -process-type: the
# buildpacks name their own default, and naming one here that no buildpack
# provides fails the export.
phase "$LIFECYCLE/exporter" -app="$APP" -layers="$LAYERS" -report="$LAYERS/report.toml" "$IMAGE"

# Whitespace-tolerant for the reason the Dockerfile path already learned: the
# file is indented TOML, digest = "sha256:..." carries spaces around the equals
# sign, and a pattern that had already pushed an image reported "no digest".
DIGEST=$(sed -n 's/^[[:space:]]*digest[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
  "$LAYERS/report.toml" 2>/dev/null | head -1)
[ -n "$DIGEST" ] || fail "the export finished but no digest could be read from: $(head -c 400 "$LAYERS/report.toml" 2>/dev/null || echo 'no report at all')"
printf '{"digest":"%s","method":"%s"}' "$DIGEST" "$RAN" > "$RESULT"
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
	limits := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    res.CPURequest,
			corev1.ResourceMemory: res.MemoryRequest,
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: res.MemoryLimit},
	}

	pod := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		},
		// Still no token. The concession below is about the node, not about
		// the API: a build has no business talking to the control plane, and it
		// reports its result through the termination log precisely so it never
		// needs to.
		AutomountServiceAccountToken: ptr.To(false),
		Volumes: []corev1.Volume{
			{Name: workspaceVolume, VolumeSource: corev1.VolumeSource{
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
	}

	// One container when the answer is already known, two when it is not.
	//
	// The pod's images are fixed when it is created and the kubelet pulls every
	// one of them whether the path that needs it runs or not. The builder image
	// is 1267 MB of compressed layers against buildkit's 101 MB, so a build
	// that was told to use a Dockerfile keeps exactly the shape it had before
	// buildpacks existed here — which is also the shape CI exercises against a
	// real cluster, and it is not made slower by a path it will not take.
	//
	// "detect" pays for the builder image even when it finds a Dockerfile. That
	// is the cost of the decision needing the working tree, and the alternative
	// — deciding in the controller, before the clone — is guessing about
	// somebody else's repository.
	if method == platformv1alpha1.BuildDockerfile {
		pod.Containers = []corev1.Container{buildkitSide(b, method, buildContainer, limits)}
	} else {
		pod.InitContainers = []corev1.Container{buildkitSide(b, method, prepareContainer, limits)}
		pod.Containers = []corev1.Container{lifecycleSide(b, limits)}
		// And only then, because only then is there an image that cannot run
		// everywhere. The builder tag resolves to a single linux/amd64 manifest
		// rather than an index (measured), so on an arm64 node — Apple silicon,
		// Graviton — the kubelet takes the pod, fails to run that image, and
		// with one attempt allowed the Job sits in the state this file already
		// has a comment about: a build that cannot start looks exactly like a
		// build that is slow.
		//
		// Asking the scheduler instead answers it twice. On a cluster that has
		// an amd64 node the build lands there and works; on one that has none
		// the pod stays Pending against an event that names the constraint.
		//
		// Temporary, and tied to the image rather than to the platform: it goes
		// when the builder is published as an index. buildkit's image already
		// is one, which is why a Dockerfile build is not pinned to anything.
		pod.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
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
				Spec:       pod,
			},
		},
	}
}

// buildkitSide clones, decides, and builds a Dockerfile.
func buildkitSide(
	b *platformv1alpha1.Build, method platformv1alpha1.BuildMethod,
	name string, limits corev1.ResourceRequirements,
) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   buildkitImage,
		Command: []string{"sh", "-c", buildScript},
		Env: []corev1.EnvVar{
			{Name: "REPO", Value: b.Spec.Repo},
			{Name: "REVISION", Value: b.Spec.Revision},
			{Name: envPathInRepo, Value: b.Spec.Path},
			{Name: envImage, Value: buildImageRef(b)},
			{Name: "METHOD", Value: string(method)},
		},
		SecurityContext: &corev1.SecurityContext{
			// Privileged, and this is the concession the whole design turns on.
			// buildkitd creates mount and network namespaces for each build
			// step; rootless avoids needing that and is unavailable here.
			//
			// Written as one field rather than a list of capabilities on
			// purpose: CAP_SYS_ADMIN plus mount is most of privileged with a
			// longer spelling, and a spec that looks narrow while granting the
			// same thing is worse than one that says what it does. Narrowing it
			// is real work and is not done.
			//
			// Except when the build was told to use a buildpack, where this
			// container only clones. buildkitd never starts, so the privilege
			// is not a concession being made carefully — it is one not being
			// made at all.
			Privileged: ptr.To(method != platformv1alpha1.BuildBuildpack),
		},
		TerminationMessagePath:   BuildResultPath,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Resources:                limits,
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolume, MountPath: "/workspace"},
			{Name: tmpVolume, MountPath: tmpPath},
			{Name: "buildkit", MountPath: "/var/lib/buildkit"},
			{Name: runVolume, MountPath: "/run"},
		},
	}
}

// lifecycleSide runs the buildpack phases, or reports what the container before
// it already did.
func lifecycleSide(b *platformv1alpha1.Build, limits corev1.ResourceRequirements) corev1.Container {
	env := []corev1.EnvVar{
		{Name: envImage, Value: buildImageRef(b)},
		{Name: envPathInRepo, Value: b.Spec.Path},
		{Name: "CNB_PLATFORM_API", Value: platformAPI},
		{Name: "HOME", Value: buildpackHome},
	}
	if host := insecureRegistry(b.Spec.Image); host != "" {
		// The same concession the Dockerfile path already makes one line at a
		// time with registry.insecure=true, in the lifecycle's spelling. The
		// registry this platform installs speaks plain HTTP inside the cluster
		// and there is nowhere yet to put a credential for one that does not,
		// so ours is the only registry a build can currently push to. When that
		// changes both paths need the same answer, and neither should be the
		// one that quietly downgrades somebody's TLS.
		env = append(env, corev1.EnvVar{Name: "CNB_INSECURE_REGISTRIES", Value: host})
	}

	return corev1.Container{
		Name:    buildContainer,
		Image:   builderImage,
		Command: []string{"sh", "-c", buildpackScript},
		Env:     env,
		SecurityContext: &corev1.SecurityContext{
			// Neither privileged nor unconfined, and this is the difference
			// between the two containers rather than an oversight in one of
			// them. buildkitd needs the node; the lifecycle needs a filesystem
			// and a socket to the registry. This is also the container that
			// executes somebody else's buildpack against somebody else's code,
			// which is the last place to spend a privilege that is not needed.
			//
			// The seccomp profile is set here rather than on the pod, because
			// the pod-level one is Unconfined for buildkit's sake and a
			// container-level profile is what overrides it.
			Privileged:     ptr.To(false),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		TerminationMessagePath:   BuildResultPath,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Resources:                limits,
		VolumeMounts: []corev1.VolumeMount{
			// Read-only, so the copy into the application directory is the only
			// way the tree gets used and cannot be half-done in place.
			{Name: workspaceVolume, MountPath: preparedPath, ReadOnly: true},
		},
	}
}

// buildImageRef is where the result is pushed. The tag names the commit: a
// build whose output is called :latest cannot answer which commit is running.
func buildImageRef(b *platformv1alpha1.Build) string {
	return b.Spec.Image + ":" + b.Spec.Revision
}

// insecureRegistry is the registry host the lifecycle is told not to use TLS
// for, or empty when the reference names no host at all.
//
// Docker's own rule for telling a registry from the first path segment, because
// there is no other: a host has a dot or a port, or it is localhost. Without it
// "library/nginx" would name "library" as a registry and the push would go to
// Docker Hub over plain HTTP.
func insecureRegistry(image string) string {
	host, _, found := strings.Cut(image, "/")
	if !found {
		return ""
	}
	if host == "localhost" || strings.ContainsAny(host, ".:") {
		return host
	}
	return ""
}
