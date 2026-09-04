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

package manifest

import (
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// The files that make a tenant's directory a place a workload may land.
const (
	NamespaceFile   = "namespace.yaml"
	QuotaFile       = "quota.yaml"
	RoleFile        = "platform-role.yaml"
	RoleBindingFile = "platform-rolebinding.yaml"

	// tenantQuotaName is what the quota is called inside the namespace. Fixed,
	// because there is one per namespace and the namespace already names the
	// tenant.
	tenantQuotaName = "tenant-quota"

	// platformRoleName is what the platform may do inside this namespace, and
	// it is named for the feature rather than for the platform: somebody
	// reading `kubectl get role` in their own namespace should be able to tell
	// what it is for without reading its rules.
	platformRoleName = "damga-settings"

	// Who the Role is bound to. The control plane's identity, which
	// cluster/control-plane.yaml declares and a test in this package reads back
	// out of that file — a name agreed in two places is a name that drifts, and
	// this one drifts into a RoleBinding that authorizes nobody while looking
	// exactly right.
	controlPlaneServiceAccount = "damga"
	controlPlaneNamespace      = "damga-system"
)

// fenceLabels is Pod Security Admission, enforced at the namespace.
//
// The same four labels policies/namespace.yaml carries for the reference
// tenant, and a test reads that file and fails when the two sets differ. Two
// copies of a fence is one copy too many, and the copy that drifts is the one
// nobody applied by hand.
//
// enforce rejects the pod; audit and warn make a violation visible for the
// paths enforcement does not cover — an existing pod, a subresource. All three
// at restricted, and the version pinned to latest so a cluster upgrade tightens
// the rule rather than leaving it at the level it was written against.
var fenceLabels = map[string]string{
	"pod-security.kubernetes.io/enforce":         restricted,
	"pod-security.kubernetes.io/enforce-version": "latest",
	"pod-security.kubernetes.io/audit":           restricted,
	"pod-security.kubernetes.io/warn":            restricted,
}

// restricted is the Pod Security Standard all three levels are set to.
const restricted = "restricted"

// fenceQuota is the ceiling one tenant namespace may take in total.
//
// The same numbers policies/tenant-quota.yaml carries, bound by the same test,
// and measured there rather than here: 410m CPU and 536Mi of memory requested
// with everything running, with headroom for the autoscaler's maximum, a
// rolling upgrade's surge and the backup jobs.
//
// limits.cpu is deliberately absent and it is not an oversight: with it, the
// API server refuses every pod that does not set a CPU limit, and the operator
// sets none on purpose — throttling a container is worse than the latency cliff
// a limit produces. One line here would make every Workload unschedulable.
var fenceQuota = map[corev1.ResourceName]string{
	corev1.ResourceRequestsCPU:     "2",
	corev1.ResourceRequestsMemory:  "3Gi",
	corev1.ResourceLimitsMemory:    "10Gi",
	corev1.ResourcePods:            "30",
	"count/persistentvolumeclaims": "4",
}

// IsFence says whether a committed file is one Fence renders.
//
// A function rather than four comparisons at each call site, because the fence
// has grown twice and both times the places that enumerate it by hand were
// found by a failing test rather than by whoever added the file. What those
// call sites actually mean is "this is the container, not an object of the
// app", and that sentence should not have to be edited when the container gains
// a part.
func IsFence(name string) bool {
	switch name {
	case NamespaceFile, QuotaFile, RoleFile, RoleBindingFile:
		return true
	}
	return false
}

// Fence renders the namespace a tenant's objects live in and the quota that
// bounds it, as two files for the tenant's own repository.
//
// # Why these are manifests rather than something the platform applies
//
// Measured on a real cluster against the Argo CD this project installs (chart
// 8.5.10, Argo CD v3.1.8): a namespace created through CreateNamespace=true and
// labelled through managedNamespaceMetadata keeps its labels only until
// somebody removes one. A deleted pod-security label was not restored in four
// minutes, nor by a forced sync, nor after a hard refresh — and the Application
// reported Synced and Healthy the whole time. The fence could be taken down and
// GitOps would not call it drift.
//
// The same two objects committed as ordinary manifests are tracked: the same
// deleted label came back in about ten seconds, and so did a quota deleted
// outright. That is the whole reason they are files.
//
// # Why they travel with the application rather than beside it
//
// A namespace with no quota is the sibling of a trap this repository has
// already paid for — a quota that counts a type the API server does not know
// blocks every create in its namespace. An application that lands in a
// namespace with no fence is the same failure from the other end: it runs, it
// is unbounded, and nothing says so. Both files are written into the same
// directory as the workload, so one sync brings the container and the thing it
// contains, and Argo CD applies the namespace before what goes in it.
func Fence(namespace string) (map[string][]byte, error) {
	if namespace == "" {
		return nil, fmt.Errorf("manifest: a fence needs a namespace")
	}

	ns := corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: maps.Clone(fenceLabels)},
	}
	nsBody, err := yaml.Marshal(ns)
	if err != nil {
		return nil, fmt.Errorf("manifest: rendering the namespace: %w", err)
	}

	hard := corev1.ResourceList{}
	for _, name := range slices.Sorted(maps.Keys(fenceQuota)) {
		q, err := resource.ParseQuantity(fenceQuota[name])
		if err != nil {
			return nil, fmt.Errorf("manifest: the quota's %s is not a quantity: %w", name, err)
		}
		hard[name] = q
	}
	quota := corev1.ResourceQuota{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
		ObjectMeta: metav1.ObjectMeta{
			Name: tenantQuotaName, Namespace: namespace, Labels: map[string]string{},
		},
		Spec: corev1.ResourceQuotaSpec{Hard: hard},
	}
	quotaBody, err := yaml.Marshal(quota)
	if err != nil {
		return nil, fmt.Errorf("manifest: rendering the quota: %w", err)
	}

	roleBody, bindingBody, err := platformAccess(namespace)
	if err != nil {
		return nil, err
	}

	return map[string][]byte{
		NamespaceFile: nsBody, QuotaFile: quotaBody,
		RoleFile: roleBody, RoleBindingFile: bindingBody,
	}, nil
}

// platformAccess is the only thing the control plane may do inside a tenant's
// namespace, said in the tenant's own repository.
//
// # What this replaces
//
// A ClusterRole granting create and patch on Secrets in every namespace in the
// cluster. That was the smallest grant RBAC could express for the shape the
// settings endpoint needed — tenant namespaces are made as tenants arrive, and
// a rule cannot say "the ones this platform owns". It was also a write into
// every namespace on the machine, including the ones holding Argo CD's admin
// secret and every database password.
//
// This says the same thing in the only place that knows the namespace's name:
// the manifest that creates it. Measured on a cluster (2026-09-05): with this
// Role and RoleBinding present, the control plane's ServiceAccount answers yes
// to create and patch in that namespace and no to get, list, watch and delete;
// in a namespace without them it answers no to everything. The cluster-wide
// rule is gone.
//
// # Why it is a committed manifest and not something the control plane applies
//
// The same reason the namespace and the quota are, and the measurement is the
// one from 2026-09-02: a manifest Argo CD applies comes back about ten seconds
// after somebody deletes it, and an object written directly does not come back
// at all. A platform whose access to a namespace can be silently removed is a
// platform whose settings page silently stops working.
//
// It also means the tenant's repository shows what the platform may do in their
// namespace, in the same commit as the namespace itself, rather than in a
// ClusterRole they cannot see.
//
// # What it does not grant
//
// No get, no list, no watch. That is the same asymmetry the endpoint is built
// on: the control plane can write a value it was handed and can never read one
// back. A raw merge patch needs only the patch verb — measured against the API
// server rather than assumed, because kubectl's own `patch` does a get first
// and its refusal makes it look as though the API requires one.
//
// Create cannot be narrowed to one name: RBAC matches resourceNames on objects
// that already exist, and a create has no name to match yet. The scope is
// therefore the namespace, which is the tenant's own.
func platformAccess(namespace string) (role, binding []byte, err error) {
	r := rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name: platformRoleName, Namespace: namespace, Labels: map[string]string{},
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"create", "patch"},
		}},
	}
	roleBody, err := yaml.Marshal(r)
	if err != nil {
		return nil, nil, fmt.Errorf("manifest: rendering the platform role: %w", err)
	}

	b := rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name: platformRoleName, Namespace: namespace, Labels: map[string]string{},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: platformRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      controlPlaneServiceAccount,
			Namespace: controlPlaneNamespace,
		}},
	}
	bindingBody, err := yaml.Marshal(b)
	if err != nil {
		return nil, nil, fmt.Errorf("manifest: rendering the platform rolebinding: %w", err)
	}
	return roleBody, bindingBody, nil
}
