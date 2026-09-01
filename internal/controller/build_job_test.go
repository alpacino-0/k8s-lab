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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	testRevision = "0123456789abcdef0123456789abcdef01234567"
	testDigest   = "sha256:1f5c5a3f21b0e8a9c6d4b2e0f7a1c3d5e7f9b1d3a5c7e9f1b3d5a7c9e1f3b5d7"
)

func testBuild(method platformv1alpha1.BuildMethod) *platformv1alpha1.Build {
	return &platformv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "damga-build"},
		Spec: platformv1alpha1.BuildSpec{
			Repo:     testRepo,
			Revision: testRevision,
			Image:    "registry.damga-registry.svc:5000/tenant-a/app",
			Builder:  method,
		},
	}
}

// The shape that CI exercises against a real cluster, and the one that has to
// stay cheap: a pod pulls every image named in its spec whether the path that
// needs it runs or not, and the buildpack builder is the largest thing this
// platform pulls.
func TestAnExplicitDockerfileBuildPullsNoBuilderImage(t *testing.T) {
	pod := desiredBuildJob(testBuild(platformv1alpha1.BuildDockerfile)).Spec.Template.Spec

	if len(pod.InitContainers) != 0 {
		t.Fatalf("a Dockerfile build grew %d init containers; it was told which builder to use, "+
			"so it must not carry the images of the one it will not run",
			len(pod.InitContainers))
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Image != buildkitImage {
		t.Fatalf("a Dockerfile build runs %d container(s) with image %q; it should be exactly "+
			"buildkit, because every other image in the spec is pulled before the pod starts",
			len(pod.Containers), pod.Containers[0].Image)
	}
}

// Detection needs the working tree, so the pod has to carry both toolchains and
// decide inside itself. The lifecycle cannot run in buildkit's image: it runs
// the buildpacks *in* the builder image's filesystem.
func TestDetectCarriesBothToolchainsAndReportsFromTheLifecycleContainer(t *testing.T) {
	for _, method := range []platformv1alpha1.BuildMethod{"", platformv1alpha1.BuildBuildpack} {
		pod := desiredBuildJob(testBuild(method)).Spec.Template.Spec

		if len(pod.InitContainers) != 1 || pod.InitContainers[0].Image != buildkitImage {
			t.Fatalf("builder %q: the clone and the decision must run first and in buildkit's "+
				"image; got %d init container(s)", method, len(pod.InitContainers))
		}
		if len(pod.Containers) != 1 || pod.Containers[0].Image != builderImage {
			t.Fatalf("builder %q: the lifecycle has no filesystem of its own and must run in "+
				"%s; got image %q", method, builderImage, pod.Containers[0].Image)
		}

		// The control plane reads a terminated container's message. The
		// container that finishes has to be able to speak.
		last := pod.Containers[0]
		if last.TerminationMessagePath != BuildResultPath ||
			last.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
			t.Fatalf("builder %q: the lifecycle container reports through %q/%v; a job with no "+
				"token has no other channel", method, last.TerminationMessagePath,
				last.TerminationMessagePolicy)
		}

		if len(last.VolumeMounts) != 1 || last.VolumeMounts[0].MountPath != preparedPath ||
			!last.VolumeMounts[0].ReadOnly {
			t.Fatalf("builder %q: the checkout must arrive at %s read-only, so the copy into "+
				"the application directory is the only way it is used; got %+v",
				method, preparedPath, last.VolumeMounts)
		}
	}
}

// The builder image is one linux/amd64 manifest, not an index. Without the
// scheduler being told, an arm64 node accepts the pod and then cannot run it —
// and a Job with one attempt allowed reports that the same way it reports a
// slow build. buildkit's image is multi-architecture, so a Dockerfile build
// that carries no builder image must not be narrowed to anything.
func TestOnlyTheBuilderImagePinsAnArchitecture(t *testing.T) {
	if sel := desiredBuildJob(testBuild(platformv1alpha1.BuildDockerfile)).
		Spec.Template.Spec.NodeSelector; len(sel) != 0 {
		t.Fatalf("a Dockerfile build was narrowed to %v; buildkit runs on every architecture "+
			"this platform supports and pinning it takes nodes away for nothing", sel)
	}
	for _, method := range []platformv1alpha1.BuildMethod{"", platformv1alpha1.BuildBuildpack} {
		sel := desiredBuildJob(testBuild(method)).Spec.Template.Spec.NodeSelector
		if sel["kubernetes.io/arch"] != "amd64" {
			t.Fatalf("builder %q carries the buildpack builder image and asks the scheduler for "+
				"%v; that image has no arm64 manifest, so without this the kubelet accepts a "+
				"pod it cannot run", method, sel)
		}
	}
}

// The privilege buildkitd needs is a concession to the node, and it is not one
// the container that runs somebody else's buildpacks has any reason to inherit.
func TestNothingThatRunsBuildpacksIsPrivilegedOrUnconfined(t *testing.T) {
	pod := desiredBuildJob(testBuild(platformv1alpha1.BuildBuildpack)).Spec.Template.Spec

	all := append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
	for _, c := range all {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			t.Fatalf("container %q is privileged in a build that was told to use a buildpack; "+
				"buildkitd never starts on that path, so nothing here needs the node", c.Name)
		}
	}

	sc := pod.Containers[0].SecurityContext
	if sc == nil || sc.SeccompProfile == nil ||
		sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("the lifecycle container inherited the pod's Unconfined seccomp profile; that " +
			"profile exists for buildkitd, and this is the container that executes somebody " +
			"else's buildpack against somebody else's code")
	}
}

// Both containers get the same bound. The pod's request is the larger of its
// init containers and the sum of its regular ones, so this does not double what
// the namespace quota counts — but leaving one unset would let it run unbounded.
func TestBothContainersCarryTheSameBound(t *testing.T) {
	pod := desiredBuildJob(testBuild(platformv1alpha1.BuildDetect)).Spec.Template.Spec

	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		if c.Resources.Limits.Memory().IsZero() || c.Resources.Requests.Cpu().IsZero() {
			t.Fatalf("container %q declares no bound; the build namespace requires requests and "+
				"limits and refuses the pod without them", c.Name)
		}
	}
}

// The trap this repository has already stepped in twice, from the other side: a
// registry carries a port and a repository does not carry a registry. Getting
// this wrong does not fail loudly — it names "library" as a registry and pushes
// somebody's image to Docker Hub over plain HTTP.
func TestInsecureRegistryNamesOnlyARegistry(t *testing.T) {
	for image, want := range map[string]string{
		"registry.damga-registry.svc:5000/ci/app": "registry.damga-registry.svc:5000",
		"ghcr.io/damgahq/damga":                   "ghcr.io",
		"localhost:30500/ci/app":                  "localhost:30500",
		"localhost/ci/app":                        "localhost",
		"library/nginx":                           "",
		"nginx":                                   "",
	} {
		if got := insecureRegistry(image); got != want {
			t.Errorf("insecureRegistry(%q) = %q, want %q", image, got, want)
		}
	}
}

// --- the scripts, run the way the pod runs them -----------------------------

// fakeLifecycle writes stand-ins for the five phase binaries. Each records that
// it ran; the one named in failing prints to stderr and exits non-zero.
// The five, in the order the platform specification mandates. Named once so the
// stand-ins and the assertions cannot disagree about which exist.
var lifecyclePhases = []string{"analyzer", "detector", phaseRestorer, "builder", phaseExporter}

// The two that read and write the cache, named because three places assert
// about them by name.
const (
	phaseRestorer = "restorer"
	phaseExporter = "exporter"
)

func fakeLifecycle(t *testing.T, dir, failing string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, phase := range lifecyclePhases {
		body := "#!/bin/sh\nprintf '%s\\n' " + phase + " >> \"$TRACE\"\n"
		switch phase {
		case failing:
			body += "printf '%s' \"$FAILURE\" >&2\nexit 51\n"
		case phaseExporter:
			body += "printf '[image]\\n  tags = [\"x\"]\\n  digest = \"" + testDigest + "\"\\n' > \"$LAYERS/report.toml\"\n"
		}
		if err := os.WriteFile(filepath.Join(dir, phase), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

type scriptRun struct {
	code   int
	stderr string
	result string
}

func runScript(t *testing.T, script string, env map[string]string) scriptRun {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	err := cmd.Run()
	run := scriptRun{stderr: stderr.String()}
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running the script: %v", err)
		}
		run.code = exit.ExitCode()
	}
	if b, err := os.ReadFile(env[envResult]); err == nil {
		run.result = string(b)
	}
	return run
}

// buildpackEnv lays out a tree that looks like the pod's and returns the
// environment the lifecycle container would carry.
func buildpackEnv(t *testing.T, method string) map[string]string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"prepared/src", "app", "layers", "lifecycle"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "prepared/src/main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prepared/method"), []byte(method), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		envImage:      "registry.damga-registry.svc:5000/tenant-a/app:" + testRevision,
		envCacheImage: testCacheRef,
		envPathInRepo: "",
		"PREPARED":    filepath.Join(root, "prepared"),
		"APP":         filepath.Join(root, "app"),
		"LAYERS":      filepath.Join(root, "layers"),
		"LIFECYCLE":   filepath.Join(root, "lifecycle"),
		"ERR":         filepath.Join(root, "phase.err"),
		envResult:     filepath.Join(root, "result"),
		"TRACE":       filepath.Join(root, "trace"),
	}
}

// The order is the specification's and it is not the obvious one. Platform API
// 0.12 through 0.15 all mandate analyzer first: it resolves the run image and
// writes the analyzed.toml the detector then updates. Detect-first appears to
// work and produces an image built against the wrong base.
func TestTheFivePhasesRunInTheOrderTheSpecificationMandates(t *testing.T) {
	env := buildpackEnv(t, "buildpack")
	fakeLifecycle(t, env["LIFECYCLE"], "")

	run := runScript(t, buildpackScript, env)
	if run.code != 0 {
		t.Fatalf("the buildpack path failed with %d: %s\n%s", run.code, run.result, run.stderr)
	}

	trace, err := os.ReadFile(env["TRACE"])
	if err != nil {
		t.Fatal(err)
	}
	const want = "analyzer\ndetector\nrestorer\nbuilder\nexporter\n"
	if string(trace) != want {
		t.Fatalf("the phases ran\n%s\nand the specification mandates\n%s\n"+
			"the analyzer resolves the run image the detector then records, so any other "+
			"order builds against a base nobody chose", trace, want)
	}

	var res buildResult
	if err := json.Unmarshal([]byte(run.result), &res); err != nil {
		t.Fatalf("the control plane cannot parse what the build wrote (%v): %s", err, run.result)
	}
	if res.Digest != testDigest {
		t.Fatalf("the digest reported was %q, and the exporter's report said %q; the digest is "+
			"the entire product of this job", res.Digest, testDigest)
	}
	if res.Method != platformv1alpha1.BuildBuildpack {
		t.Fatalf("the build recorded method %q; which of the two paths ran is the difference "+
			"between a reproducible record and a guess", res.Method)
	}
}

// The rule this file is under. A build fails for reasons that belong to the
// user's code, and four rounds have been lost in this repository to a defect
// being reported as a summary of itself.
func TestAFailingPhaseArrivesInTheBuildersOwnWords(t *testing.T) {
	// A tab and two lines, because that is what a compiler writes and because
	// both are control characters JSON forbids raw: an escaping that lets one
	// through produces a message the control plane silently drops, and a
	// dropped message reads as "the build reported no digest".
	const compiler = "main.go:7:2:\tcannot use x (variable of type int) as string\nnote: module requires Go 1.24"

	env := buildpackEnv(t, "buildpack")
	env["FAILURE"] = compiler
	fakeLifecycle(t, env["LIFECYCLE"], "builder")

	run := runScript(t, buildpackScript, env)
	if run.code == 0 {
		t.Fatal("a phase exited 51 and the script reported success")
	}

	var res buildResult
	if err := json.Unmarshal([]byte(run.result), &res); err != nil {
		t.Fatalf("the message a failing phase produced is not parseable JSON (%v), so the "+
			"control plane drops it and the build reports no reason at all: %s", err, run.result)
	}
	for _, quote := range []string{"cannot use x (variable of type int) as string", "module requires Go 1.24"} {
		if !strings.Contains(res.Message, quote) {
			t.Fatalf("the builder said %q and the Build records %q; the failure has to arrive "+
				"in the builder's own words, not in a summary of them", quote, res.Message)
		}
	}
	if !strings.HasPrefix(res.Message, "builder:") {
		t.Fatalf("the message does not name which of the five phases failed: %q", res.Message)
	}
	if res.Method != platformv1alpha1.BuildBuildpack {
		t.Fatalf("a failed buildpack build recorded method %q", res.Method)
	}
	// And the long form stays where it can still be read. The termination
	// message is capped; a real stack of buildpack output is not.
	if !strings.Contains(run.stderr, compiler) {
		t.Fatal("the phase's stderr never reached the pod log, so everything past the cap is gone")
	}
}

// The handoff, from the other side. When the first container built the image,
// this one must leave the answer alone: the control plane reads the first
// container that left a parseable message, and two answers is one too many.
func TestTheLifecycleContainerSaysNothingWhenTheDockerfilePathAnswered(t *testing.T) {
	env := buildpackEnv(t, "dockerfile")
	fakeLifecycle(t, env["LIFECYCLE"], "")

	run := runScript(t, buildpackScript, env)
	if run.code != 0 {
		t.Fatalf("the lifecycle container failed a build it had nothing to do with: %d %s",
			run.code, run.stderr)
	}
	if run.result != "" {
		t.Fatalf("it wrote %q over an answer the Dockerfile build had already given", run.result)
	}
	if _, err := os.Stat(env["TRACE"]); err == nil {
		t.Fatal("it ran the lifecycle for a repository that was built from a Dockerfile")
	}
}

// The other half of the handoff: a repository with no Dockerfile has to leave
// the buildkit container quiet, or its empty answer becomes the one the control
// plane reads.
func TestNoDockerfileHandsOffWithoutAnswering(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", ".")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	// A shallow fetch of a bare revision is what the script does, and a server
	// refuses one by default.
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(origin, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "one")
	head := git("rev-parse", "HEAD")

	env := map[string]string{
		envRepo:       origin,
		envRevision:   head,
		envMethod:     "detect",
		envPathInRepo: "",
		envImage:      "registry.damga-registry.svc:5000/tenant-a/app:" + head,
		envCacheImage: testCacheRef,
		envWorkspace:  filepath.Join(root, "workspace"),
		envResult:     filepath.Join(root, "result"),
		envHome:       root,
	}
	run := runScript(t, buildScript, env)
	if run.code != 0 {
		t.Fatalf("the clone-and-decide step failed with %d: %s\n%s", run.code, run.result, run.stderr)
	}
	if run.result != "" {
		t.Fatalf("it answered %q for a build it did not perform; the container after it has "+
			"the answer, and the control plane reads the first one it finds", run.result)
	}
	method, err := os.ReadFile(filepath.Join(env[envWorkspace], "method"))
	if err != nil {
		t.Fatalf("the decision was never written down, so the next container cannot know it: %v", err)
	}
	if string(method) != "buildpack" {
		t.Fatalf("a repository with no Dockerfile was routed to %q", method)
	}
}
