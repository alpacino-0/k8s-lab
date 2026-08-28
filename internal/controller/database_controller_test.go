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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

var _ = Describe("Database Controller", func() {
	const (
		dbName    = "shop-db"
		namespace = "default"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: dbName, Namespace: namespace}

	reconciler := func() *DatabaseReconciler {
		return &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}
	reconcileNow := func() {
		GinkgoHelper()
		_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}
	create := func() {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &platformv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: dbName, Namespace: namespace},
			Spec: platformv1alpha1.DatabaseSpec{
				Image:   testPostgresImage,
				Storage: resource.MustParse("1Gi"),
			},
		})).To(Succeed())
	}

	AfterEach(func() {
		db := &platformv1alpha1.Database{}
		if err := k8sClient.Get(ctx, key, db); err == nil {
			Expect(k8sClient.Delete(ctx, db)).To(Succeed())
		}
		// envtest runs no garbage collector, so owner references do not cascade.
		for _, obj := range []client.Object{
			&appsv1.StatefulSet{}, &corev1.Service{}, &corev1.Secret{},
		} {
			obj.SetName(dbName)
			obj.SetNamespace(namespace)
			_ = k8sClient.Delete(ctx, obj)
		}
	})

	It("renders a server, a headless service and credentials", func() {
		create()
		reconcileNow()

		set := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, set)).To(Succeed())
		Expect(*set.Spec.Replicas).To(Equal(int32(1)))
		Expect(set.Spec.VolumeClaimTemplates).To(HaveLen(1))
		Expect(set.Spec.Template.Spec.Containers[0].Image).To(Equal(testPostgresImage))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone),
			"a service with an address of its own gives clients whatever DNS returns; "+
				"the pod's stable name is the thing that stays true")

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
		Expect(secret.Data).To(HaveKey("POSTGRES_PASSWORD"))
		Expect(secret.Data["POSTGRES_PASSWORD"]).NotTo(BeEmpty())
		// The workload needs the address as much as the credentials, and a
		// Secret it already mounts is one fewer thing to be told separately.
		Expect(string(secret.Data["DB_HOST"])).To(ContainSubstring(dbName + "-0."))

		// Everything carries an owner reference, so deleting the Database
		// removes the server — and deliberately not its volume, which
		// Kubernetes leaves behind for a StatefulSet.
		for _, obj := range []client.Object{set, svc, secret} {
			Expect(obj.GetOwnerReferences()).To(HaveLen(1))
			Expect(obj.GetOwnerReferences()[0].Kind).To(Equal("Database"))
		}
	})

	// The one that would destroy data if it were wrong.
	//
	// PostgreSQL bakes the password into the data directory when it first
	// initialises. A reconcile that mints a new one leaves the server accepting
	// the old password and the application holding the new one — which looks
	// like a credentials bug in the application and is not.
	It("keeps the password it generated", func() {
		create()
		reconcileNow()

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
		first := string(secret.Data["POSTGRES_PASSWORD"])
		Expect(first).NotTo(BeEmpty())

		reconcileNow()
		reconcileNow()

		Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
		Expect(string(secret.Data["POSTGRES_PASSWORD"])).To(Equal(first),
			"the password changed under a running database; the server still holds "+
				"the first one and nothing can connect with the second")
	})

	// A Database is not a Workload's child, and the whole reason it is its own
	// kind is that deleting an app must not delete its data. The owner
	// references above cover the objects; this covers who may adopt what.
	It("leaves a statefulset it does not own alone", func() {
		// Somebody else's server, named the same. "other" everywhere below is
		// deliberately not any value this controller renders — the assertions
		// are that it survives untouched.
		const theirs = "other"
		foreign := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: dbName, Namespace: namespace,
				Labels: map[string]string{"owner": "somebody-else"},
			},
			Spec: appsv1.StatefulSetSpec{
				ServiceName: theirs,
				Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{containerName: theirs}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{containerName: theirs}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "c", Image: testPostgresImage,
					}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, foreign) }()

		create()
		_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})

		// Asserting that *an* error happened is not enough, and this test found
		// that out about itself: with the ownership check removed it still
		// passed, because the API server rejects a change to a StatefulSet's
		// immutable selector and the reconcile failed for that instead. A test
		// satisfied by the right outcome for the wrong reason is one that will
		// keep passing after the thing it guards is gone.
		var conflict *conflictError
		Expect(errors.As(err, &conflict)).To(BeTrue(),
			"a statefulset belonging to somebody else was adopted; the error was %v", err)
		Expect(conflict.Error()).To(ContainSubstring("not owned by this resource"))

		got := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Spec.ServiceName).To(Equal(theirs),
			"the foreign statefulset was rewritten")
		Expect(got.OwnerReferences).To(BeEmpty(),
			"an owner reference was stamped on it, so deleting the Database would "+
				"now delete somebody else's server")
	})

	// Immutable fields are not written on an existing object. Assigning them
	// turns every pass into a rejected update, which reads as the controller
	// being broken rather than as the field being fixed at creation.
	It("reconciles an existing server without fighting its immutable fields", func() {
		create()
		reconcileNow()
		reconcileNow()

		db := &platformv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, key, db)).To(Succeed())
		Expect(meta.FindStatusCondition(db.Status.Conditions, "Ready")).NotTo(BeNil())
	})

	// Ready means PostgreSQL answered, not that objects exist. A connection
	// string handed out before the server accepts connections is a string that
	// does not work.
	It("is not ready until the server is answering", func() {
		create()
		reconcileNow()

		db := &platformv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, key, db)).To(Succeed())
		cond := meta.FindStatusCondition(db.Status.Conditions, "Ready")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NotAcceptingConnections"))

		By("standing in for the statefulset controller")
		set := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, set)).To(Succeed())
		set.Status.Replicas = 1
		set.Status.ReadyReplicas = 1
		Expect(k8sClient.Status().Update(ctx, set)).To(Succeed())

		reconcileNow()
		Expect(k8sClient.Get(ctx, key, db)).To(Succeed())
		cond = meta.FindStatusCondition(db.Status.Conditions, "Ready")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(db.Status.SecretName).To(Equal(dbName))
		Expect(db.Status.Host).NotTo(BeEmpty())
	})
})

var _ = Describe("Database backups", func() {
	const (
		dbName    = "shop-db"
		namespace = "default"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: dbName, Namespace: namespace}
	backupKey := types.NamespacedName{Name: dbName + "-backup", Namespace: namespace}

	reconcile1 := func() {
		GinkgoHelper()
		_, err := (&DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).
			Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}
	withBackup := func(backup *platformv1alpha1.DatabaseBackup) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &platformv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: dbName, Namespace: namespace},
			Spec: platformv1alpha1.DatabaseSpec{
				Image: testPostgresImage, Storage: resource.MustParse("1Gi"), Backup: backup,
			},
		})).To(Succeed())
	}

	AfterEach(func() {
		db := &platformv1alpha1.Database{}
		if err := k8sClient.Get(ctx, key, db); err == nil {
			Expect(k8sClient.Delete(ctx, db)).To(Succeed())
		}
		for _, obj := range []client.Object{
			&appsv1.StatefulSet{}, &corev1.Service{}, &corev1.Secret{},
		} {
			obj.SetName(dbName)
			obj.SetNamespace(namespace)
			_ = k8sClient.Delete(ctx, obj)
		}
		for _, obj := range []client.Object{&batchv1.CronJob{}, &corev1.PersistentVolumeClaim{}} {
			obj.SetName(dbName + "-backup")
			obj.SetNamespace(namespace)
			_ = k8sClient.Delete(ctx, obj)
		}
	})

	It("renders a schedule and a volume of its own", func() {
		withBackup(&platformv1alpha1.DatabaseBackup{Storage: resource.MustParse("2Gi")})
		reconcile1()

		cron := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, backupKey, cron)).To(Succeed())
		Expect(cron.Spec.Schedule).To(Equal("0 2 * * *"))
		Expect(cron.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent),
			"two dumps at once is how a backup window becomes a half-written archive")

		// The channel between this job and the operator. The job holds no
		// service-account token by design, so the termination log is the only
		// way its answer gets out.
		container := cron.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
		Expect(container.TerminationMessagePath).To(Equal(RestoreResultPath))
		Expect(container.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageReadFile),
			"the message has to be the file the container wrote, not the tail of its log")
		Expect(cron.Spec.JobTemplate.Spec.Template.Spec.AutomountServiceAccountToken).
			To(Equal(ptrBool(false)))

		claim := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, backupKey, claim)).To(Succeed())
		Expect(claim.Spec.Resources.Requests.Storage().String()).To(Equal("2Gi"),
			"the archives share the data volume, so filling it with backups stops "+
				"the database accepting writes")
		// Unowned on purpose. An owner reference would have the garbage
		// collector delete the archives with the Database — the same cascade
		// that made this a separate kind, one level down.
		Expect(claim.OwnerReferences).To(BeEmpty(),
			"deleting the Database would destroy every backup it ever took")
	})

	// Turning backups off has to stop them. Only ever creating conditional
	// objects leaves a schedule running against a database whose owner asked
	// for it to stop, still writing the archives they stopped paying for.
	It("stops the schedule when backups are turned off", func() {
		withBackup(&platformv1alpha1.DatabaseBackup{Storage: resource.MustParse("1Gi")})
		reconcile1()
		Expect(k8sClient.Get(ctx, backupKey, &batchv1.CronJob{})).To(Succeed())

		db := &platformv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, key, db)).To(Succeed())
		db.Spec.Backup = nil
		Expect(k8sClient.Update(ctx, db)).To(Succeed())
		reconcile1()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, backupKey, &batchv1.CronJob{}))).
			To(BeTrue(), "the schedule kept running after backups were turned off")

		// And the archives are still there. Turning off a schedule is not
		// asking for what it already wrote to be destroyed, and this platform
		// does not delete data as a side effect of a spec change.
		Expect(k8sClient.Get(ctx, backupKey, &corev1.PersistentVolumeClaim{})).To(Succeed(),
			"the archives were deleted along with the schedule")
	})

	It("can back up without rehearsing, and says which it is doing", func() {
		no := false
		withBackup(&platformv1alpha1.DatabaseBackup{
			Storage: resource.MustParse("1Gi"), Rehearse: &no,
		})
		reconcile1()

		cron := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, backupKey, cron)).To(Succeed())
		var rehearse string
		for _, e := range cron.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "REHEARSE" {
				rehearse = e.Value
			}
		}
		Expect(rehearse).To(Equal("false"),
			"a tenant who turned the rehearsal off would still pay for it")
	})
})

func ptrBool(b bool) *bool { return &b }
