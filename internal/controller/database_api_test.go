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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// The Database API, against a real API server.
//
// Every rule below is CEL compiled by the apiserver and evaluated by it, which
// is the only place that can say whether an expression this repository wrote is
// valid — a rule with a typo compiles to nothing and admits everything, and a
// unit test on the Go struct would never see it. The same shape of rule on
// Workload was worth having for the same reason.
var _ = Describe("Database API", func() {
	ctx := context.Background()
	const namespace = "default"

	db := func(name string, mutate ...func(*platformv1alpha1.Database)) *platformv1alpha1.Database {
		d := &platformv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: platformv1alpha1.DatabaseSpec{
				Image:   testPostgresImage,
				Storage: resource.MustParse("1Gi"),
			},
		}
		for _, m := range mutate {
			m(d)
		}
		return d
	}

	AfterEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &platformv1alpha1.Database{},
			client.InNamespace(namespace))).To(Succeed())
	})

	It("accepts a database with a pinned image and a size", func() {
		Expect(k8sClient.Create(ctx, db("accepted"))).To(Succeed())

		got := &platformv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "accepted", Namespace: namespace}, got)).To(Succeed())

		// Defaulted by the server rather than by whatever renders this later.
		// A default applied in Go is a default the tenant cannot see in the
		// object they submitted, and the object is what git carries.
		Expect(got.Spec.Database).To(Equal("app"))
		Expect(got.Spec.Username).To(Equal("app"))
	})

	// PostgreSQL will not start on a data directory written by a newer major
	// version. An image that moves is therefore an outage on the next pod
	// restart — at a moment with nothing to connect it to the change that
	// caused it, because the change was somebody else's push to a tag.
	It("refuses an image that can move under a running database", func() {
		Expect(k8sClient.Create(ctx, db("latest", func(d *platformv1alpha1.Database) {
			d.Spec.Image = "postgres:latest"
		}))).NotTo(Succeed(), "an image whose meaning changes was accepted")

		Expect(k8sClient.Create(ctx, db("untagged", func(d *platformv1alpha1.Database) {
			d.Spec.Image = "postgres"
		}))).NotTo(Succeed(), "an untagged image was accepted; the kubelet resolves it to :latest")
	})

	// A registry port is a colon that is not a tag, and a rule that looks at the
	// whole string lets it through. The Workload API had exactly this bug once.
	It("does not mistake a registry port for a tag", func() {
		Expect(k8sClient.Create(ctx, db("port-not-tag", func(d *platformv1alpha1.Database) {
			d.Spec.Image = "registry.local:5000/team-a/postgres"
		}))).NotTo(Succeed(), "a registry port was read as a tag, so an untagged image was admitted")
	})

	// Storage has no default, and that is the point: a size the platform guesses
	// is a size somebody discovers when the volume is full, and a PVC cannot be
	// shrunk afterwards.
	It("requires a size rather than choosing one", func() {
		d := db("sizeless")
		d.Spec.Storage = resource.Quantity{}
		Expect(k8sClient.Create(ctx, d)).NotTo(Succeed(),
			"a database was accepted with no storage size, so the platform picked one")
	})

	It("refuses names PostgreSQL would need quoting for", func() {
		for _, bad := range []string{"App", "1app", "app-name", "app;drop"} {
			Expect(k8sClient.Create(ctx, db("badname", func(d *platformv1alpha1.Database) {
				d.Spec.Database = bad
			}))).NotTo(Succeed(), "accepted database name %q", bad)
		}
	})

	// The status a page reads, and the reason this type has one beyond Ready.
	// "The backup was last restored three hours ago" is a claim about the past,
	// and nothing live can be asked it — somebody has to have written it down.
	It("carries a restore rehearsal in its status", func() {
		Expect(k8sClient.Create(ctx, db("with-status"))).To(Succeed())

		key := types.NamespacedName{Name: "with-status", Namespace: namespace}
		got := &platformv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())

		got.Status.LastRestore = &platformv1alpha1.RestoreRehearsal{
			FinishedAt: metav1.Now(), Archive: "/backup/app-20260828.sql.gz",
			Rows: 1284, Tables: 7, SourceRows: 1284,
		}
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.LastRestore).NotTo(BeNil())
		Expect(got.Status.LastRestore.Rows).To(Equal(int64(1284)))
		// Both numbers, because "1,284 rows came back" and "1,284 rows came
		// back out of 1,284" are different claims and only the second one is
		// the one the product makes.
		Expect(got.Status.LastRestore.SourceRows).To(Equal(int64(1284)))
	})
})
