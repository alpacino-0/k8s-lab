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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// The two files that make a tenant's directory a place a workload may land.
const (
	NamespaceFile = "namespace.yaml"
	QuotaFile     = "quota.yaml"

	// tenantQuotaName is what the quota is called inside the namespace. Fixed,
	// because there is one per namespace and the namespace already names the
	// tenant.
	tenantQuotaName = "tenant-quota"
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

	return map[string][]byte{NamespaceFile: nsBody, QuotaFile: quotaBody}, nil
}
