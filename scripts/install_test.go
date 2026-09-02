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

// Package scripts holds the tests for the shell this repository ships.
//
// A Go file for a shell script, and the reason is the gate: `go test ./...` is
// what CI runs, and a rule proved by a script nothing runs is decoration. What
// is under test here is scripts/install.sh's DRY_RUN plan — the real command
// sequence, emitted by the same run() every mutation goes through, rather than
// a second list maintained beside it that would stop matching in silence.
package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The three flags install.sh requires, named once. A test that spells a flag
// wrong asks the script something it refuses, and a refusal is what several of
// these cases expect — so the misspelling would pass.
const (
	flagDomain = "--domain"
	flagEmail  = "--email"
	flagTenant = "--tenant"

	someDomain = "demo.example.test"
	someEmail  = "you@example.test"
	someTenant = "acme"
)

// plan runs the installer's dry run and returns its output as lines.
//
// DRY_RUN needs no cluster, no root and no network, which is what makes it
// runnable in the same gate as everything else.
func plan(t *testing.T) []string {
	t.Helper()
	return planWith(t)
}

// planWith is plan, plus the flags whose effect on the plan is under test.
func planWith(t *testing.T, extra ...string) []string {
	t.Helper()
	args := append([]string{"./install.sh",
		flagDomain, someDomain,
		flagEmail, someEmail,
		flagTenant, someTenant}, extra...)
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(), "DRY_RUN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh --dry-run failed: %v\n%s", err, out)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// firstLineWith is the index of the first line containing want, or -1.
func firstLineWith(lines []string, want string) int {
	for i, line := range lines {
		if strings.Contains(line, want) {
			return i
		}
	}
	return -1
}

// TestTheCRDsGoInBeforeTheQuotaThatCountsThem.
//
// cluster/build-namespace.yaml puts a quota on count/builds.platform.damga.co.
// A ResourceQuota that counts a type the API server has never heard of cannot
// compute a usage for it, and until it can, every create in that namespace is
// refused with "status unknown for quota" — a message that names the quota and
// not the missing type. Measured in this repository: applying the quota before
// the operator's CRDs made the first build of every CI run fail on the
// platform's own guard rail.
//
// Reordering those two lines in install.sh fails here, and the message says
// which way round they have to be. Nothing else in the plan would notice: the
// apply succeeds, and the failure arrives at the first build, on somebody
// else's machine.
func TestTheCRDsGoInBeforeTheQuotaThatCountsThem(t *testing.T) {
	lines := plan(t)

	crds := firstLineWith(lines, "config/crd")
	quota := firstLineWith(lines, "build-namespace.yaml")
	switch {
	case crds < 0:
		t.Fatal("the plan never installs the CRDs")
	case quota < 0:
		t.Fatal("the plan never applies cluster/build-namespace.yaml")
	case crds > quota:
		t.Errorf("the plan applies the build namespace (line %d) before the CRDs (line %d); "+
			"the quota on count/builds.platform.damga.co cannot compute a usage for a type "+
			"the API server has not heard of, and refuses every create until it can",
			quota+1, crds+1)
	}
}

// TestThePlanNamesOnlyFilesThatExist.
//
// Every path in the plan that points into this repository is one an operator's
// run will reach minutes in, on a machine that is already half installed. A
// file renamed here and not there is a failure that costs a rollback rather
// than a compile.
func TestThePlanNamesOnlyFilesThatExist(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}
	checked := 0
	for _, line := range plan(t) {
		if !strings.HasPrefix(line, "RUN ") {
			continue
		}
		for token := range strings.FieldsSeq(line) {
			if !strings.HasPrefix(token, root+"/") {
				continue
			}
			checked++
			if _, err := os.Stat(token); err != nil {
				t.Errorf("the plan names %s, which is not there: %v", token, err)
			}
		}
	}
	if checked == 0 {
		// The assertion above passes trivially if the plan stops naming repo
		// paths, which is exactly what a rewrite that inlines the manifests
		// would look like.
		t.Error("the plan named no files inside this repository; this test proved nothing")
	}
}

// TestThePlanNeverPrintsACredential.
//
// The database password is generated inside a function, so the plan can print
// the function's name and not its arguments. Hoisting that `kubectl create
// secret` up into a run line — the obvious tidy-up — would put a live password
// in the terminal of anybody who asked what the installer was going to do, and
// in whatever captured that output.
func TestThePlanNeverPrintsACredential(t *testing.T) {
	for i, line := range plan(t) {
		for _, forbidden := range []string{"--from-literal", "POSTGRES_PASSWORD", "openssl rand"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("line %d of the plan carries %q: %s", i+1, forbidden, line)
			}
		}
	}
}

// TestTheRegistryHostIsWhateverTheClusterWasToldItIs.
//
// This string lives in cluster/registry.yaml, in scripts/bootstrap.sh, in CI
// and in the control plane's -registry flag. install.sh reads it out of
// cluster/control-plane.yaml rather than adding a fifth copy, and this asserts
// the value it reads is the one the rest of the repository uses — a parse that
// starts matching the wrong line would otherwise produce an installer that
// redirects containerd to a registry nothing pushes to.
func TestTheRegistryHostIsWhateverTheClusterWasToldItIs(t *testing.T) {
	body, err := os.ReadFile("bootstrap.sh")
	if err != nil {
		t.Fatalf("reading bootstrap.sh: %v", err)
	}
	found := regexp.MustCompile(`REGISTRY_HOST="([^"]+)"`).FindSubmatch(body)
	if found == nil {
		t.Fatal("bootstrap.sh no longer names REGISTRY_HOST; this test cannot compare")
	}
	want := "PLAN registry=" + string(found[1]) + " "

	lines := plan(t)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], want) {
		t.Errorf("install.sh resolved a different registry from bootstrap.sh:\n got %q\nwant prefix %q",
			lines[0], want)
	}
}

// TestTheInstallerAndTheRunbookPinTheSameVersions.
//
// docs/DEPLOY.md is the same sequence written for a person, and the two carry
// the ingress controller's and cert-manager's versions separately because one
// is prose and the other is a variable. Two copies of a version is the shape
// this repository has already been bitten by twice, and the way it bites is
// quiet: the page says one thing, the installer does another, and the person
// following the page by hand gets a different cluster from the person who ran
// the script.
func TestTheInstallerAndTheRunbookPinTheSameVersions(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("reading install.sh: %v", err)
	}
	runbook, err := os.ReadFile("../docs/DEPLOY.md")
	if err != nil {
		t.Fatalf("reading docs/DEPLOY.md: %v", err)
	}

	for _, pin := range []struct{ name, pattern string }{
		{"the ingress controller", `INGRESS_VERSION="([^"]+)"`},
		{"cert-manager", `CERT_MANAGER_VERSION="([^"]+)"`},
	} {
		found := regexp.MustCompile(pin.pattern).FindSubmatch(script)
		if found == nil {
			t.Errorf("install.sh no longer pins %s; this test cannot compare", pin.name)
			continue
		}
		version := string(found[1])
		if !strings.Contains(string(runbook), version) {
			t.Errorf("install.sh installs %s %s and docs/DEPLOY.md never mentions that version",
				pin.name, version)
		}
	}
}

// TestTheInstallerRefusesAnIncompleteOrWrongCommandLine.
//
// Exit 2 and not 1, so that a wrapper can tell "you typed this wrong" from
// "the install failed halfway".
func TestTheInstallerRefusesAnIncompleteOrWrongCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no domain", []string{flagEmail, someEmail, flagTenant, someTenant}},
		{"no email", []string{flagDomain, someDomain, flagTenant, someTenant}},
		{"no tenant", []string{flagDomain, someDomain, flagEmail, someEmail}},
		{"an issuer that is neither", []string{
			flagDomain, someDomain, flagEmail, someEmail,
			flagTenant, someTenant, "--issuer", "self-signed",
		}},
		{"an option it does not have", []string{
			flagDomain, someDomain, flagEmail, someEmail,
			flagTenant, someTenant, "--force",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", append([]string{"./install.sh"}, tc.args...)...)
			cmd.Env = append(os.Environ(), "DRY_RUN=1")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("install.sh accepted it:\n%s", out)
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Errorf("exit %v, want 2:\n%s", err, out)
			}
		})
	}
}

// TestTheCatalogueIsDeployedAndTheInstallerRefusesItsAbsence.
//
// The one-click catalogue is the product's headline feature and it is off by
// default: with -catalog-dir unset the server answers 503 and names the flag,
// which is right for a library and wrong for an install. So the deployed
// manifest has to carry it, the image has to put the templates where it points,
// and the installer has to refuse rather than produce a cluster whose catalogue
// page is empty for a reason nobody can see from the page.
//
// Three files have to agree and each is read here rather than assumed. The
// script is read as well as executed, because go test invalidates its cache on
// files a test opened and not on ones it only ran — editing install.sh and
// rerunning the gate would otherwise replay the previous verdict.
func TestTheCatalogueIsDeployedAndTheInstallerRefusesItsAbsence(t *testing.T) {
	manifest, err := os.ReadFile("../cluster/control-plane.yaml")
	if err != nil {
		t.Fatalf("reading cluster/control-plane.yaml: %v", err)
	}
	found := regexp.MustCompile(`-catalog-dir=(\S+)`).FindSubmatch(manifest)
	if found == nil {
		t.Fatal("cluster/control-plane.yaml has no -catalog-dir argument: this install would " +
			"come up with no catalogue and every /catalog request would answer 503")
	}
	dir := string(found[1])

	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("reading install.sh: %v", err)
	}
	if !strings.Contains(string(script), "-catalog-dir=") {
		t.Error("install.sh does not read -catalog-dir out of the manifest, so removing that " +
			"flag would install a cluster with no catalogue and no complaint")
	}

	// The image is the other half: the flag points inside the container, so a
	// Dockerfile that does not put the templates there leaves the flag correct
	// and the directory empty.
	dockerfile, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("reading Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfile), " "+dir+"\n") {
		t.Errorf("cluster/control-plane.yaml points -catalog-dir at %s and the Dockerfile does "+
			"not copy the templates there", dir)
	}

	// And the build context has to include them at all. This file ignores
	// everything by default, so the templates are only present by name.
	ignore, err := os.ReadFile("../.dockerignore")
	if err != nil {
		t.Fatalf("reading .dockerignore: %v", err)
	}
	if !strings.Contains(string(ignore), "!catalog/templates/**") {
		t.Error(".dockerignore does not re-include catalog/templates, so the image would be " +
			"built without the catalogue while every manifest still says it has one")
	}
}

// What the installer runs inside the control plane's container is the path the
// Dockerfile installs, in every place it says it.
//
// The image is gcr.io/distroless/static: no shell, and no PATH lookup for a
// bare name. `kubectl exec -- damga bootstrap` fails with "executable file not
// found in $PATH", which ends an install at its last step with nobody able to
// log in. Measured on k3s on 2026-09-02, and then measured again with the
// absolute path, which created the owner.
//
// The guard is here rather than the fix alone because of the shape it came in.
// The same call was written three times: ci.yml had /damga and was right,
// install.sh had a bare damga twice — once in the command it runs and once in
// the advice it prints when you skip it — and both were wrong. Only the right
// copy ever executed, so CI was green for two days over a script no CI job
// runs. That is the fourth time this repository has paid for one value living
// in several places, and the first three are already written down.
func TestTheInstallerExecsThePathTheImageInstalls(t *testing.T) {
	dockerfile, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("reading the Dockerfile: %v", err)
	}
	entrypoint := regexp.MustCompile(`ENTRYPOINT \["([^"]+)"\]`).FindSubmatch(dockerfile)
	if entrypoint == nil {
		t.Fatal("the Dockerfile declares no ENTRYPOINT, so this case has nothing to hold " +
			"the installer against")
	}
	want := string(entrypoint[1])

	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("reading install.sh: %v", err)
	}
	// One spelling in the file, and it is the one the image installs.
	declared := regexp.MustCompile(`(?m)^CONTROL_PLANE_BIN="([^"]+)"`).FindSubmatch(script)
	if declared == nil {
		t.Fatal("install.sh no longer names the control plane's binary in one place; " +
			"it was written out twice before, and both copies were wrong")
	}
	if got := string(declared[1]); got != want {
		t.Errorf("install.sh runs %q in the container and the Dockerfile installs %q; "+
			"a bare name has no PATH to resolve in a distroless image", got, want)
	}

	// And nowhere spells it the other way. This is the half that catches a
	// third copy being added rather than the two that existed being changed —
	// which is how this arrived: the wrong spelling was added beside a right
	// one and nothing compared them.
	//
	// Over the commands and not over the comments. The first version of this
	// read the whole file and failed on the paragraph in install.sh explaining
	// why a bare name does not work — prose quoting the mistake is how the
	// mistake gets explained, and a guard that cannot tell the two apart makes
	// the explanation unwritable.
	var commands strings.Builder
	for line := range strings.SplitSeq(string(script), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		commands.WriteString(line)
		commands.WriteByte('\n')
	}
	base := want[strings.LastIndex(want, "/")+1:]
	for _, bare := range []string{
		"-- " + base + " ",
		" " + base + " bootstrap",
	} {
		if strings.Contains(commands.String(), bare) {
			t.Errorf("install.sh runs %q, which resolves through a PATH the image "+
				"does not have; it has to be %s", bare, want)
		}
	}
}

// A condition-wait on a label selector has to be preceded, in the same
// function, by a wait for the object to exist.
//
// `kubectl wait --for=condition=Ready -l ...` against a selector that matches
// nothing does not wait: it answers "no matching resources found" immediately
// and returns non-zero, spending none of its --timeout. Placed straight after
// the apply that creates the object, it races the API server and loses.
// Measured on k3s on 2026-09-02: the ingress-nginx Deployment and its Pod both
// carry creationTimestamp 00:20:52 and the wait failed inside that same second,
// twenty seconds into a fresh install. The fixed run printed "existed after 2s"
// — the whole margin the old command needed and did not take.
//
// Structural rather than behavioural, because the failure is a race and a test
// that tried to reproduce it would be the same guess about somebody else's
// scheduler that the fix refuses to make. What is checkable is the shape: the
// existence poll and the condition-wait live together, so moving the wait back
// out to the top level — which is exactly what reverting the fix does — has
// nowhere to find one.
func TestNoConditionWaitRunsBeforeTheObjectIsKnownToExist(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("reading install.sh: %v", err)
	}

	// Comments dropped and continuations folded, so a command spelled over
	// three lines is one string to search and the paragraph above explaining
	// the race is not mistaken for the race.
	var folded []string
	var pending string
	for line := range strings.SplitSeq(string(script), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if cut, ok := strings.CutSuffix(line, `\`); ok {
			pending += cut + " "
			continue
		}
		folded = append(folded, pending+line)
		pending = ""
	}

	// The enclosing function, or "" for the top level of the script.
	opens := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\)\s*\{`)
	selector := regexp.MustCompile(`-l\s+(\S+)`)
	blocks := map[string][]string{}
	scope := ""
	for _, line := range folded {
		if m := opens.FindStringSubmatch(line); m != nil {
			scope = m[1]
			continue
		}
		if scope != "" && line == "}" {
			scope = ""
			continue
		}
		blocks[scope] = append(blocks[scope], line)
	}

	checked := 0
	for scope, lines := range blocks {
		body := strings.Join(lines, "\n")
		for _, line := range lines {
			if !strings.Contains(line, "kubectl wait") || !strings.Contains(line, "--for=condition=") {
				continue
			}
			m := selector.FindStringSubmatch(line)
			if m == nil {
				continue // waiting on a named object, which exists or does not
			}
			checked++
			// The poll that proves the object turned up: the same selector,
			// asked with -o name so an empty answer is an empty string.
			if !strings.Contains(body, "-o name") || strings.Count(body, m[1]) < 2 {
				where := "the top level of the script"
				if scope != "" {
					where = scope + "()"
				}
				t.Errorf("in %s, `kubectl wait --for=condition` runs against %s with nothing "+
					"first waiting for a pod to match it; a selector that matches nothing is "+
					"answered \"no matching resources found\" in zero seconds, so this races "+
					"the apply above it and does not retry", where, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("install.sh has no condition-wait on a label selector at all; this case " +
			"passed by finding nothing, which is how a guard stops guarding")
	}
	t.Logf("condition-waits on a label selector checked: %d", checked)
}

// commands returns only the lines that are commands the installer would run.
//
// The plan interleaves commands with step headings and notes, and a rule about
// what the installer DOES has to be asked of the commands alone. A guard that
// searches the whole output can be satisfied by a sentence describing the thing
// it is meant to forbid -- which makes the sentence unwritable, and it is
// usually the sentence that explains why the rule exists.
func commands(lines []string) []string {
	var out []string
	for _, line := range lines {
		if after, ok := strings.CutPrefix(line, "RUN "); ok {
			out = append(out, after)
		}
	}
	return out
}

// The --skip-k3s path asks the cluster what is already holding the ingress
// ports; the path that installs k3s does not, because it disabled Traefik.
//
// --skip-k3s is documented in docs/DEPLOY.md as "k3s is already there, or the
// kubeconfig points elsewhere" -- the door for everyone who already has a
// cluster. On a stock k3s that cluster has Traefik, whose klipper-lb DaemonSet
// holds hostPort 80 and 443, and ingress-nginx never gets an address.
//
// The half that matters is the second assertion. A check that only proved the
// probe is present would also pass for a script that probes unconditionally,
// and probing after we disabled Traefik ourselves would refuse the install on
// the strength of the Service we just created.
func TestTheSkipK3sPathAsksTheClusterWhatIsAlreadyThere(t *testing.T) {
	const probe = "refuse_if_the_ingress_ports_are_taken"

	skipped := commands(planWith(t, "--skip-k3s"))
	at := firstLineWith(skipped, probe)
	if at < 0 {
		t.Fatalf("--skip-k3s runs no %s; it inherits a cluster this script did not "+
			"build, and nothing asks that cluster whether the ingress ports are free", probe)
	}

	// Before the ingress goes in, or it is a post-mortem rather than a check.
	apply := firstLineWith(skipped, "deploy/static/provider/baremetal/deploy.yaml")
	if apply < 0 {
		t.Fatal("the plan no longer applies the ingress-nginx manifest; this case " +
			"orders the probe against it and has lost its second half")
	}
	if at > apply {
		t.Errorf("%s runs at command %d, after the ingress-nginx apply at %d; the "+
			"point is to refuse before installing a controller that cannot get an "+
			"address", probe, at, apply)
	}

	own := commands(plan(t))
	if i := firstLineWith(own, probe); i >= 0 {
		t.Errorf("the plan that installs k3s itself also runs %s (command %d); that "+
			"path passes --disable traefik, so the only LoadBalancer holding those "+
			"ports is the one this script is about to create", probe, i)
	}
}

// The refusal tells you to delete the Service, and warns off the Deployment.
//
// Behavioural rather than a grep of the script, because what is under test is
// the text a person actually gets. kubectl and helm are stubbed, so this needs
// no cluster and the installer exits at step 2 having changed nothing.
//
// The advice is the measurement. On 2026-09-02, deleting only the Traefik
// Deployment left its klipper-lb pods holding 80 and 443 -- they follow the
// Service -- so ingress-nginx stayed <pending> and the address went from
// Traefik's 404 to a refused connection. The obvious advice makes it worse, and
// that is exactly the kind of thing a later edit "tidies" back in.
func TestTheRefusalSaysToDeleteTheServiceAndNotTheDeployment(t *testing.T) {
	bin := t.TempDir()
	// Answers the one query the probe makes, in the shape kubectl would.
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("writing the %s stub: %v", name, err)
		}
	}
	write("kubectl", "#!/bin/sh\nprintf 'kube-system/traefik 80 443\\n'\n")
	write("helm", "#!/bin/sh\nexit 0\n")

	cmd := exec.Command("bash", "./install.sh",
		flagDomain, someDomain, flagEmail, someEmail, flagTenant, someTenant,
		"--skip-k3s", "--skip-node-config")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	said := string(out)

	if err == nil {
		t.Fatalf("install.sh carried on against a cluster where kube-system/traefik "+
			"already holds 80 and 443; it has to refuse, because it would otherwise "+
			"finish, say it is ready, and serve somebody else's 404\n%s", said)
	}
	if !strings.Contains(said, "kube-system/traefik") {
		t.Errorf("the refusal does not name what it found; it has to say which "+
			"Service is holding the ports\n%s", said)
	}
	if !strings.Contains(said, "delete svc traefik") {
		t.Errorf("the refusal does not tell the operator to delete the Service. "+
			"That is the removal that was measured to work\n%s", said)
	}
	if !strings.Contains(said, "not the Deployment") {
		t.Errorf("the refusal no longer warns against deleting the Deployment. "+
			"Measured: the klipper-lb pods follow the Service, so deleting the "+
			"Deployment keeps the ports held and turns the 404 into a refused "+
			"connection -- the obvious advice is the wrong advice\n%s", said)
	}
	if !strings.Contains(said, "disable traefik") {
		t.Errorf("the refusal does not say how to make the removal durable; k3s "+
			"re-applies Traefik from its manifests directory\n%s", said)
	}
}
