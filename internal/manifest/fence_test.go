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

package manifest_test

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/damgahq/damga/internal/manifest"
)

const (
	policyNamespace = "../../policies/namespace.yaml"
	policyQuota     = "../../policies/tenant-quota.yaml"

	// The namespace every case here renders a fence for.
	tenantNamespace = "acme-prod"

	// The reference tenant's own fence, committed as two files for the reason
	// Fence exists: Argo CD applies them and selfHeal puts them back.
	gitopsFence          = "../../gitops/fence/"
	gitopsNamespace      = "damga-gitops"
	referenceApplication = "../../gitops/application.yaml"

	// The manifest that declares the control plane's identity and what it may
	// do outside a tenant's namespace.
	controlPlane = "../../cluster/control-plane.yaml"
)

func fenceFiles(t *testing.T) (corev1.Namespace, corev1.ResourceQuota) {
	t.Helper()
	files, err := manifest.Fence(tenantNamespace)
	if err != nil {
		t.Fatal(err)
	}
	var ns corev1.Namespace
	if err := yaml.Unmarshal(files[manifest.NamespaceFile], &ns); err != nil {
		t.Fatalf("the namespace this platform writes is not readable: %v", err)
	}
	var quota corev1.ResourceQuota
	if err := yaml.Unmarshal(files[manifest.QuotaFile], &quota); err != nil {
		t.Fatalf("the quota this platform writes is not readable: %v", err)
	}
	return ns, quota
}

// The fence a tenant gets is the fence the reference tenant has, and there are
// two copies of it: this package's, written per tenant into their repository,
// and policies/namespace.yaml, applied once by the installer for the namespace
// this project ships with.
//
// Two copies of a fence is one too many, and the one that drifts is the one
// nobody applies by hand. Pod Security Admission is compared key for key,
// because a missing label is not a smaller fence — enforce alone with no
// enforce-version pins the rule at whatever the cluster shipped with, and audit
// and warn are what make a violation visible on the paths enforcement does not
// cover.
func TestATenantsFenceIsTheFenceThisProjectShips(t *testing.T) {
	body, err := os.ReadFile(policyNamespace)
	if err != nil {
		t.Fatalf("the reference namespace is unreadable: %v", err)
	}
	var reference corev1.Namespace
	if err := yaml.Unmarshal(body, &reference); err != nil {
		t.Fatalf("%s is not a Namespace: %v", policyNamespace, err)
	}

	ns, _ := fenceFiles(t)
	for key, want := range reference.Labels {
		if !strings.HasPrefix(key, "pod-security.kubernetes.io/") {
			// Only the fence is compared. The reference namespace carried two
			// damga.co labels until 2026-09-02 — an opt-in to admission
			// policies removed four days before them — and nothing stops a
			// future label that is about that namespace and not about the
			// fence.
			continue
		}
		if got := ns.Labels[key]; got != want {
			t.Errorf("a tenant namespace has %s=%q and %s has %q; the tenant is behind the "+
				"fence this project ships with", key, got, policyNamespace, want)
		}
	}
	for key := range ns.Labels {
		if _, inReference := reference.Labels[key]; !inReference {
			t.Errorf("a tenant namespace carries %s, which %s does not — one of the two is "+
				"wrong and nothing else would say which", key, policyNamespace)
		}
	}
}

// The same, for what bounds the namespace. The numbers were measured for
// policies/tenant-quota.yaml and are not measured again here: what this asserts
// is that the copy a tenant gets is that measurement rather than a second one.
func TestATenantsQuotaIsTheQuotaThisProjectShips(t *testing.T) {
	body, err := os.ReadFile(policyQuota)
	if err != nil {
		t.Fatalf("the reference quota is unreadable: %v", err)
	}
	var reference corev1.ResourceQuota
	if err := yaml.Unmarshal(body, &reference); err != nil {
		t.Fatalf("%s is not a ResourceQuota: %v", policyQuota, err)
	}

	_, quota := fenceFiles(t)
	if len(quota.Spec.Hard) != len(reference.Spec.Hard) {
		t.Fatalf("a tenant's quota bounds %d resources and %s bounds %d",
			len(quota.Spec.Hard), policyQuota, len(reference.Spec.Hard))
	}
	for name, want := range reference.Spec.Hard {
		got, ok := quota.Spec.Hard[name]
		if !ok {
			t.Errorf("a tenant's quota does not bound %s at all", name)
			continue
		}
		if got.Cmp(want) != 0 {
			t.Errorf("a tenant's %s is %s and %s says %s", name, got.String(), policyQuota, want.String())
		}
	}
	// limits.cpu is absent on purpose in both: with it the API server refuses
	// every pod that sets no CPU limit, and the operator sets none.
	if _, present := quota.Spec.Hard[corev1.ResourceLimitsCPU]; present {
		t.Error("a tenant's quota bounds limits.cpu, which makes every Workload this platform " +
			"renders unschedulable — the operator sets no CPU limit on purpose")
	}
}

// The namespace has to be a namespace and the quota has to be inside it. Both
// are committed and applied by Argo CD, so a missing kind or a quota with no
// namespace is a sync that half succeeds.
func TestTheFenceIsTwoApplyableObjects(t *testing.T) {
	ns, quota := fenceFiles(t)

	if ns.APIVersion != "v1" || ns.Kind != "Namespace" {
		t.Errorf("the namespace file is %s/%s", ns.APIVersion, ns.Kind)
	}
	if ns.Name != "acme-prod" {
		t.Errorf("the namespace is called %q", ns.Name)
	}
	if quota.APIVersion != "v1" || quota.Kind != "ResourceQuota" {
		t.Errorf("the quota file is %s/%s", quota.APIVersion, quota.Kind)
	}
	if quota.Namespace != "acme-prod" {
		t.Errorf("the quota lands in namespace %q, not the one it bounds", quota.Namespace)
	}
	if _, err := manifest.Fence(""); err == nil {
		t.Error("a fence with no namespace was rendered; it would apply to whatever the " +
			"caller's context happened to be")
	}
}

// The reference tenant is under the fence tenants get, and is under it the same
// way — as files Argo CD applies rather than as metadata Argo CD writes once.
//
// It was the last exception. gitops/application.yaml labelled its namespace
// through managedNamespaceMetadata, and that is the mechanism this package was
// written to replace: measured against Argo CD v3.1.8, a pod-security label
// removed from a namespace managed that way was never restored — not in four
// minutes, not after a forced sync, not after a hard refresh — while the
// Application reported Synced and Healthy the whole time. The demo of drift
// correction could not correct drift in its own fence.
//
// Byte for byte, and that is the point rather than strictness for its own sake.
// A comparison of parsed objects would let the two agree on every field this
// test happens to name while differing on the next one somebody adds to Fence.
// These files have no comments in them for the same reason; gitops/fence/README
// says why, and Argo CD ignores it.
func TestTheReferenceTenantIsUnderTheFenceTenantsGet(t *testing.T) {
	want, err := manifest.Fence(gitopsNamespace)
	if err != nil {
		t.Fatal(err)
	}
	// Every file Fence renders, and nothing else in the directory. Named files
	// were what this iterated over first, so when the fence gained a Role and a
	// RoleBinding the reference tenant silently did not get them: the two files
	// it did have still matched, and the test still passed.
	for name := range want {
		got, err := os.ReadFile(gitopsFence + name)
		if err != nil {
			t.Fatalf("the reference tenant has no %s: %v\n"+
				"gitops/application.yaml points a source at gitops/fence, so a missing file "+
				"there is a namespace that arrives with no fence at all", name, err)
		}
		if !bytes.Equal(got, want[name]) {
			t.Errorf("gitops/fence/%s is not what this platform writes for a tenant.\n"+
				"Regenerate it from manifest.Fence(%q) rather than editing it: the reference "+
				"tenant and a real one have to be the same fence, and the copy that drifts is "+
				"the one nobody applies by hand.\n--- committed ---\n%s\n--- Fence renders ---\n%s",
				name, gitopsNamespace, got, want[name])
		}
	}

	// And nothing beside them. A file left behind by a fence that stopped
	// rendering it is a manifest Argo CD goes on applying — the reference
	// tenant's own version of a stale object nobody removed.
	entries, err := os.ReadDir(gitopsFence)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			// The README that explains the directory. Argo CD reads only YAML.
			continue
		}
		if _, rendered := want[e.Name()]; !rendered {
			t.Errorf("gitops/fence/%s is not something Fence renders any more; Argo CD is "+
				"still applying it", e.Name())
		}
	}
}

// And that the Application actually points at them.
//
// The files can be right and unreferenced, which is the same namespace with no
// fence and a directory that looks like the fix.
//
// Parsed rather than searched as text, and the first version of this test is
// why: it looked for the strings, and the strings are also in the comment that
// explains why they are gone. It failed on a file that was already correct,
// which is the failure mode that teaches people to delete tests.
func TestTheReferenceApplicationAppliesThatFence(t *testing.T) {
	body, err := os.ReadFile(referenceApplication)
	if err != nil {
		t.Fatal(err)
	}
	var app struct {
		Spec struct {
			Sources []struct {
				Path string `json:"path"`
			} `json:"sources"`
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
			SyncPolicy struct {
				SyncOptions              []string       `json:"syncOptions"`
				ManagedNamespaceMetadata map[string]any `json:"managedNamespaceMetadata"`
			} `json:"syncPolicy"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(body, &app); err != nil {
		t.Fatalf("%s is not an Application: %v", referenceApplication, err)
	}

	fenced := false
	for _, src := range app.Spec.Sources {
		if src.Path == "gitops/fence" {
			fenced = true
		}
	}
	if !fenced {
		t.Errorf("%s applies %q and not gitops/fence, so the reference tenant's namespace "+
			"is whatever Argo CD conjured for it", referenceApplication, app.Spec.Source.Path)
	}
	if len(app.Spec.SyncPolicy.ManagedNamespaceMetadata) > 0 {
		t.Error("the reference Application labels its namespace through " +
			"managedNamespaceMetadata again. Measured: a label removed from a namespace " +
			"managed that way is never put back, and the Application says Synced throughout")
	}
	if slices.Contains(app.Spec.SyncPolicy.SyncOptions, "CreateNamespace=true") {
		t.Error("the reference Application creates its own namespace again, which arrives " +
			"with none of the labels in gitops/fence/namespace.yaml")
	}
}

// What the platform may do inside a tenant's namespace, and what it may not.
//
// The verbs that are absent are the assertion. The settings endpoint's promise
// is that a secret's value is never in an API response, and what makes that a
// property of the deployment rather than a sentence in a handler is that the
// control plane cannot read a Secret at all: no get, no list, no watch. A raw
// merge patch needs neither — measured against a real API server on 2026-09-05,
// because kubectl's own `patch` does a get first and its refusal makes it look
// as though the API requires one.
//
// delete is absent for a different reason: this platform never removes a
// tenant's Secret object, only keys inside it. A grant that could would be a
// grant that can destroy every value a tenant ever typed.
func TestThePlatformsAccessToATenantNamespaceIsWriteOnly(t *testing.T) {
	files, err := manifest.Fence(tenantNamespace)
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	if err := yaml.Unmarshal(files[manifest.RoleFile], &role); err != nil {
		t.Fatalf("the platform role is not a Role: %v", err)
	}
	if role.Namespace != tenantNamespace {
		t.Errorf("the role lands in namespace %q, not the one it is about", role.Namespace)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("the role carries %d rules; it grants one thing", len(role.Rules))
	}
	rule := role.Rules[0]
	if !slices.Equal(rule.Resources, []string{"secrets"}) ||
		!slices.Equal(rule.APIGroups, []string{""}) {
		t.Fatalf("the role is about %v in %v", rule.Resources, rule.APIGroups)
	}
	if !slices.Equal(slices.Sorted(slices.Values(rule.Verbs)), []string{"create", "patch"}) {
		t.Fatalf("the platform may %v in a tenant's namespace, and it needs create and patch.\n"+
			"Every other verb is a decision: get, list and watch would let the control plane "+
			"read a value it promises never to return, and delete would let it destroy every "+
			"value a tenant has typed", rule.Verbs)
	}
	if len(rule.ResourceNames) != 0 {
		t.Errorf("the rule names %v: RBAC matches resourceNames on objects that already "+
			"exist, so a create would be refused by a rule that looks tighter and is broken",
			rule.ResourceNames)
	}
}

// The binding names the control plane, and the control plane is what
// cluster/control-plane.yaml says it is.
//
// Read out of that file rather than written twice. A RoleBinding whose subject
// does not match the running ServiceAccount authorizes nobody and looks exactly
// right, and the failure arrives as a 403 at the moment somebody saves a
// password.
func TestTheBindingNamesTheServiceAccountTheControlPlaneRunsAs(t *testing.T) {
	files, err := manifest.Fence(tenantNamespace)
	if err != nil {
		t.Fatal(err)
	}
	var binding rbacv1.RoleBinding
	if err := yaml.Unmarshal(files[manifest.RoleBindingFile], &binding); err != nil {
		t.Fatalf("the platform rolebinding is not a RoleBinding: %v", err)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != "damga-settings" {
		t.Errorf("the binding points at %s/%s", binding.RoleRef.Kind, binding.RoleRef.Name)
	}
	if len(binding.Subjects) != 1 {
		t.Fatalf("the binding has %d subjects; it authorizes one identity", len(binding.Subjects))
	}

	sa, ns := controlPlaneIdentity(t)
	got := binding.Subjects[0]
	if got.Kind != "ServiceAccount" || got.Name != sa || got.Namespace != ns {
		t.Errorf("the binding authorizes %s %s/%s and the control plane runs as "+
			"ServiceAccount %s/%s.\nA binding whose subject does not exist authorizes nobody "+
			"and looks correct; it fails as a 403 when somebody saves a secret",
			got.Kind, got.Namespace, got.Name, ns, sa)
	}
}

// And the grant this replaced does not come back.
//
// It was create and patch on Secrets in every namespace in the cluster, because
// RBAC cannot say "the namespaces this platform owns" from a cluster-scoped
// rule. The namespaces are known one at a time, in the manifest that creates
// them, which is where the grant lives now. If a cluster-wide rule ever
// reappears, the tenant-scoped one has become decoration.
func TestTheControlPlaneHoldsNoClusterWideAccessToSecrets(t *testing.T) {
	body, err := os.ReadFile(controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	for doc := range strings.SplitSeq(string(body), "\n---\n") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil || role.Kind != "ClusterRole" {
			continue
		}
		for _, rule := range role.Rules {
			if !slices.Contains(rule.Resources, "secrets") {
				continue
			}
			t.Errorf("ClusterRole %s may %v secrets in every namespace in the cluster, "+
				"including the ones holding Argo CD's admin password and every tenant's "+
				"database credentials.\nThe tenant-scoped Role in internal/manifest.Fence is "+
				"what replaced this", role.Name, rule.Verbs)
		}
	}
}

// controlPlaneIdentity reads the ServiceAccount the control plane runs as.
func controlPlaneIdentity(t *testing.T) (name, namespace string) {
	t.Helper()
	body, err := os.ReadFile(controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	for doc := range strings.SplitSeq(string(body), "\n---\n") {
		var sa corev1.ServiceAccount
		if err := yaml.Unmarshal([]byte(doc), &sa); err != nil || sa.Kind != "ServiceAccount" {
			continue
		}
		return sa.Name, sa.Namespace
	}
	t.Fatalf("%s declares no ServiceAccount", controlPlane)
	return "", ""
}
