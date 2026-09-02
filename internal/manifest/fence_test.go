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
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/damgahq/damga/internal/manifest"
)

const (
	policyNamespace = "../../policies/namespace.yaml"
	policyQuota     = "../../policies/tenant-quota.yaml"
)

func fenceFiles(t *testing.T) (corev1.Namespace, corev1.ResourceQuota) {
	t.Helper()
	files, err := manifest.Fence("acme-prod")
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
			// The two damga.co labels on the reference namespace belong to the
			// admission policies that were removed on 2026-08-29 and are not
			// carried to a tenant. Only the fence is compared.
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
