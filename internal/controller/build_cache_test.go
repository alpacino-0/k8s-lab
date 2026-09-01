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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// testCacheRef is where every build of testBuild's repository keeps its layers.
const testCacheRef = "registry.damga-registry.svc:5000/tenant-a/app:buildcache"

// The measurement that chose a registry reference over a shared volume, written
// as the assertion it produced.
//
// Ten Jobs mounting one ReadWriteOnce claim were all scheduled, all ran, all
// landed on one node and all mounted it read-write; each appended a line and
// then counted the file, and all ten read 10. ReadWriteOnce is one node, not
// one pod, so the claim serialises nothing — it hands ten concurrent builds a
// single cache directory without a word, and pins the queue to one node while
// it does it.
func TestTheCacheIsARegistryReferenceAndNotAVolume(t *testing.T) {
	for _, method := range []platformv1alpha1.BuildMethod{
		platformv1alpha1.BuildDockerfile, platformv1alpha1.BuildBuildpack, platformv1alpha1.BuildDetect,
	} {
		pod := desiredBuildJob(testBuild(method)).Spec.Template.Spec
		for _, v := range pod.Volumes {
			if v.PersistentVolumeClaim != nil {
				t.Errorf("builder %q mounts claim %q: ten builds are admitted at once and a "+
					"ReadWriteOnce claim does not stop them sharing it — measured, all ten "+
					"mounted one claim on one node and wrote to it together",
					method, v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
}

// Per repository and not per revision, which is the whole of it: a cache tagged
// with the commit could only be read by a build of that same commit, and a
// build of the same commit does not need one.
func TestTheCacheIsSharedByEveryRevisionAndCollidesWithNone(t *testing.T) {
	first := testBuild(platformv1alpha1.BuildDetect)
	second := testBuild(platformv1alpha1.BuildDetect)
	second.Spec.Revision = strings.Repeat("b", 40)

	if buildCacheRef(first) != buildCacheRef(second) {
		t.Errorf("two revisions of one repository cache to %q and %q; a cache only its own "+
			"commit can read is a cache nothing ever reads",
			buildCacheRef(first), buildCacheRef(second))
	}
	if buildCacheRef(first) != testCacheRef {
		t.Errorf("the cache reference is %q, want %q", buildCacheRef(first), testCacheRef)
	}
	for _, b := range []*platformv1alpha1.Build{first, second} {
		if buildCacheRef(b) == buildImageRef(b) {
			t.Fatalf("the cache and the image are both %q, so a build overwrites the image it "+
				"just pushed with a cache manifest", buildCacheRef(b))
		}
	}
}

// Whichever container ends up doing the build has to know where the cache is,
// and which one that is depends on what the working tree turns out to hold.
func TestEveryContainerThatCouldBuildIsToldWhereTheCacheIs(t *testing.T) {
	for _, method := range []platformv1alpha1.BuildMethod{
		platformv1alpha1.BuildDockerfile, platformv1alpha1.BuildBuildpack, platformv1alpha1.BuildDetect,
	} {
		pod := desiredBuildJob(testBuild(method)).Spec.Template.Spec
		for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
			var got string
			for _, e := range c.Env {
				if e.Name == envCacheImage {
					got = e.Value
				}
			}
			if got != testCacheRef {
				t.Errorf("builder %q, container %q: %s = %q, want %q; both scripts refuse to "+
					"start without it rather than building without a cache and saying nothing",
					method, c.Name, envCacheImage, got, testCacheRef)
			}
		}
	}
}

// --- the scripts, run the way the pod runs them -----------------------------

// recordingBuildctl puts a buildctl-daemonless.sh on PATH that writes down what
// it was asked to do and produces the metadata the script then parses.
//
// A stand-in and not the real thing, which bounds what this can claim: it
// proves the flags reach the builder carrying the values the operator computed.
// That the builder then reuses anything is a different question and was
// answered by measurement rather than by a test — see the comment on cacheTag
// and the one beside the flags.
func recordingBuildctl(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$ARGV\"; done\n" +
		"printf '{\\n  \"containerimage.digest\": \"" + testDigest + "\"\\n}' > \"$META\"\n"
	if err := os.WriteFile(filepath.Join(dir, "buildctl-daemonless.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// dockerfileRepo is a repository with a Dockerfile in it, cloned the way the
// script clones: a shallow fetch of a bare revision, which a server refuses
// unless it is told to allow one.
func dockerfileRepo(t *testing.T) (dir, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir = filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", ".")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "one")
	return dir, git("rev-parse", "HEAD")
}

// The Dockerfile path, run for real against a recording builder.
//
// mode=max and not the default, because min keeps only what ends up in the
// pushed image and the dependency install this exists for is a stage that does
// not: measured on this repository's own app/, where npm ci runs in stage one
// and stage two copies node_modules out of it.
func TestTheDockerfileBuildAsksForAndFillsTheCache(t *testing.T) {
	origin, head := dockerfileRepo(t)
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	recordingBuildctl(t, bin)

	env := map[string]string{
		"PATH":        bin + ":" + os.Getenv("PATH"),
		envRepo:       origin,
		envRevision:   head,
		envMethod:     "detect",
		envPathInRepo: "",
		envImage:      "registry.damga-registry.svc:5000/tenant-a/app:" + head,
		envCacheImage: testCacheRef,
		envWorkspace:  filepath.Join(root, "workspace"),
		"META":        filepath.Join(root, "meta.json"),
		"BUILD_ERR":   filepath.Join(root, "build.err"),
		envResult:     filepath.Join(root, "result"),
		"ARGV":        filepath.Join(root, "argv"),
		envHome:       root,
	}
	run := runScript(t, buildScript, env)
	if run.code != 0 {
		t.Fatalf("the Dockerfile path failed with %d: %s\n%s", run.code, run.result, run.stderr)
	}

	argv, err := os.ReadFile(env["ARGV"])
	if err != nil {
		t.Fatalf("the builder was never invoked: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argv)), "\n")
	find := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}

	imp := find("--import-cache")
	if !strings.Contains(imp, "ref="+testCacheRef) {
		t.Errorf("the build imported %q; without the cache reference every rebuild downloads "+
			"its dependencies again — measured 19s against 6s on this repository's own app/", imp)
	}
	exp := find("--export-cache")
	if !strings.Contains(exp, "ref="+testCacheRef) {
		t.Errorf("the build exported %q; a cache nothing writes is a cache nothing reads", exp)
	}
	if !strings.Contains(exp, "mode=max") {
		t.Errorf("the export is %q: the default keeps only the layers that ship in the image, "+
			"and the dependency stage of a multi-stage build is not one of them", exp)
	}
	if !strings.Contains(exp, "ignore-error=true") {
		t.Errorf("the export is %q: the cache is an optimisation and the image is the product, "+
			"so a registry that takes the image and refuses the cache must not fail the build", exp)
	}
}

// A build started without one of its inputs has to say so, and this is the
// assertion that found the guard did not.
//
// The scripts used to check their inputs with ${VAR:?}, and measured under the
// EXIT trap they carry, that shape exits 0 and writes nothing: the shell aborts
// with $? still 0, the trap concludes nothing went wrong, the container
// succeeds with an empty termination message, and a build that never started
// reads exactly like one that did. fail() and set -e were measured in the same
// run and both exit 1 carrying a message, which is why need() routes through
// fail().
func TestABuildStartedWithoutAnInputSaysSoRatherThanSucceeding(t *testing.T) {
	full := func() map[string]string {
		root := t.TempDir()
		return map[string]string{
			envRepo: "https://example.test/x.git", envRevision: testRevision,
			envMethod: "dockerfile", envImage: "registry.damga-registry.svc:5000/tenant-a/app:x",
			envCacheImage: testCacheRef,
			envResult:     filepath.Join(root, "result"), envWorkspace: filepath.Join(root, "w"),
		}
	}
	for _, missing := range []string{envImage, envCacheImage, "REPO", "REVISION", "METHOD"} {
		env := full()
		delete(env, missing)
		run := runScript(t, buildScript, env)
		if run.code == 0 {
			t.Errorf("with no %s the script exited 0; the container succeeds, the control "+
				"plane reads an empty termination message, and a build that never started "+
				"is indistinguishable from one that did", missing)
			continue
		}
		if !strings.Contains(run.result, missing) {
			t.Errorf("with no %s the build recorded %q, which does not name what is missing; "+
				"the pod log is the place the control plane does not read", missing, run.result)
		}
	}

	// And the lifecycle container, which has one input of its own.
	bp := buildpackEnv(t, "buildpack")
	delete(bp, envCacheImage)
	run := runScript(t, buildpackScript, bp)
	if run.code == 0 || !strings.Contains(run.result, envCacheImage) {
		t.Errorf("the lifecycle script exited %d saying %q with no %s; it would run the five "+
			"phases with an empty -cache-image", run.code, run.result, envCacheImage)
	}
}

// The buildpack path, run for real against phases that record their arguments.
//
// -cache-image on both, and it has to be both: the restorer is what reads the
// cache back and the exporter is what writes it, so either one alone is a cache
// that is never filled or one that is never used.
func TestTheBuildpackPhasesCarryTheCacheImage(t *testing.T) {
	env := buildpackEnv(t, "buildpack")
	dir := env["LIFECYCLE"]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, phase := range lifecyclePhases {
		body := "#!/bin/sh\nprintf '%s' " + phase + " >> \"$TRACE\"\n" +
			"for a in \"$@\"; do printf ' %s' \"$a\" >> \"$TRACE\"; done\n" +
			"printf '\\n' >> \"$TRACE\"\n"
		if phase == phaseExporter {
			body += "printf '[image]\\n  digest = \"" + testDigest + "\"\\n' > \"$LAYERS/report.toml\"\n"
		}
		if err := os.WriteFile(filepath.Join(dir, phase), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	run := runScript(t, buildpackScript, env)
	if run.code != 0 {
		t.Fatalf("the buildpack path failed with %d: %s\n%s", run.code, run.result, run.stderr)
	}
	trace, err := os.ReadFile(env["TRACE"])
	if err != nil {
		t.Fatal(err)
	}

	want := "-cache-image=" + testCacheRef
	for _, phase := range []string{phaseRestorer, phaseExporter} {
		var line string
		for l := range strings.SplitSeq(string(trace), "\n") {
			if strings.HasPrefix(l, phase+" ") {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("the %s never ran:\n%s", phase, trace)
		}
		if !strings.Contains(line, want) {
			t.Errorf("the %s ran as %q and was not given %s; the restorer is what reads the "+
				"cache back and the exporter is what writes it, so one without the other is a "+
				"cache that is never filled or one that is never used", phase, line, want)
		}
	}
}
