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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// DatabaseReconciler renders one Database into the server a small team gets
// without operating one.
type DatabaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.damga.co,resources=databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.damga.co,resources=databases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var db platformv1alpha1.Database
	if err := r.Get(ctx, req.NamespacedName, &db); err != nil {
		// Gone. The StatefulSet and Service carry owner references and the
		// garbage collector removes them — but not the PersistentVolumeClaims a
		// StatefulSet created, which Kubernetes deliberately leaves behind. That
		// is the behaviour this platform wants: deleting the object that
		// describes a database should not be the way its data is destroyed.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	normaliseDatabase(&db)

	if err := r.reconcileDatabase(ctx, &db); err != nil {
		if statusErr := r.updateDatabaseStatus(ctx, &db, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.updateDatabaseStatus(ctx, &db, nil)
}

// normaliseDatabase fills in what the CRD's defaults would have supplied, for
// an object built in Go that never passed through the API server.
func normaliseDatabase(db *platformv1alpha1.Database) {
	if db.Spec.Database == "" {
		db.Spec.Database = defaultDatabaseName
	}
	if db.Spec.Username == "" {
		db.Spec.Username = defaultDatabaseName
	}
	db.Spec.Resources.CPURequest = quantityOrDefault(db.Spec.Resources.CPURequest, "100m")
	db.Spec.Resources.MemoryRequest = quantityOrDefault(db.Spec.Resources.MemoryRequest, "256Mi")
	db.Spec.Resources.MemoryLimit = quantityOrDefault(db.Spec.Resources.MemoryLimit, "512Mi")
}

func (r *DatabaseReconciler) reconcileDatabase(ctx context.Context, db *platformv1alpha1.Database) error {
	// The Secret first, and read before it is written. A password minted on
	// every pass would lock the application out of a database whose data
	// directory still holds the first one — see desiredDatabaseSecret.
	var live corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Name: db.Name, Namespace: db.Namespace}, &live)
	switch {
	case apierrors.IsNotFound(err):
		// Nothing there. desiredDatabaseSecret generates one.
	case err != nil:
		// Deliberately fatal rather than proceeding with nil. Not being able to
		// read the Secret and generating a fresh password are indistinguishable
		// to everything downstream, and one of them destroys access to a
		// running database.
		return fmt.Errorf("reading the existing credentials: %w", err)
	}
	existing := &live
	if apierrors.IsNotFound(err) {
		existing = nil
	}

	secret, err := desiredDatabaseSecret(db, existing)
	if err != nil {
		return err
	}
	if err := r.applyDatabase(ctx, db, secret, func(e, d client.Object) {
		es, ds := e.(*corev1.Secret), d.(*corev1.Secret)
		es.StringData = ds.StringData
		es.Labels = reconcileLabels(es.Labels, ds.Labels)
	}); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}

	if err := r.applyDatabase(ctx, db, desiredDatabaseService(db), func(e, d client.Object) {
		es, ds := e.(*corev1.Service), d.(*corev1.Service)
		es.Spec.Selector = ds.Spec.Selector
		es.Spec.Ports = reconcileServicePorts(es.Spec.Ports, ds.Spec.Ports)
		es.Labels = reconcileLabels(es.Labels, ds.Labels)
	}); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if err := r.applyDatabase(ctx, db, desiredDatabaseStatefulSet(db), func(e, d client.Object) {
		es, ds := e.(*appsv1.StatefulSet), d.(*appsv1.StatefulSet)
		es.Spec.Replicas = ds.Spec.Replicas
		es.Spec.Template = ds.Spec.Template
		es.Labels = reconcileLabels(es.Labels, ds.Labels)
		// Selector, ServiceName and VolumeClaimTemplates are deliberately not
		// written on an existing object. All three are immutable on a
		// StatefulSet, so assigning them turns a harmless no-op into a rejected
		// update on every pass — and the one that would matter, a volume grown
		// in the spec, has to be resized on the PersistentVolumeClaim itself.
	}); err != nil {
		return fmt.Errorf("statefulset: %w", err)
	}
	return nil
}

// applyDatabase is the Workload reconciler's apply, with the same ownership
// question asked first.
//
// The check is not optional here for the reason it was not there: an object
// that already exists and belongs to somebody else is left alone. A Database
// named after an existing StatefulSet would otherwise adopt it, rewrite its
// pod template, and take it down when the Database was deleted.
func (r *DatabaseReconciler) applyDatabase(
	ctx context.Context,
	db *platformv1alpha1.Database,
	desired client.Object,
	mutate func(existing, desired client.Object),
) error {
	existing, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object %T is not a client.Object", desired)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		if existing.GetUID() != "" && !metav1.IsControlledBy(existing, db) {
			return &conflictError{kind: r.kindOf(existing), name: existing.GetName()}
		}
		mutate(existing, desired)
		return controllerutil.SetControllerReference(db, existing, r.Scheme)
	})
	return err
}

func (r *DatabaseReconciler) kindOf(obj client.Object) string {
	return (&WorkloadReconciler{Scheme: r.Scheme}).kindOf(obj)
}

func (r *DatabaseReconciler) updateDatabaseStatus(
	ctx context.Context, db *platformv1alpha1.Database, reconcileErr error,
) error {
	var set appsv1.StatefulSet
	setErr := r.Get(ctx, client.ObjectKey{Name: db.Name, Namespace: db.Namespace}, &set)
	if setErr != nil && !apierrors.IsNotFound(setErr) {
		return setErr
	}

	db.Status.ObservedGeneration = db.Generation
	db.Status.SecretName = db.Name
	db.Status.Host = databaseHost(db)

	ready := metav1.Condition{
		Type: "Ready", ObservedGeneration: db.Generation, LastTransitionTime: metav1.Now(),
	}
	switch {
	case reconcileErr != nil:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "RenderFailed"
		ready.Message = reconcileErr.Error()
	case apierrors.IsNotFound(setErr):
		ready.Status = metav1.ConditionFalse
		ready.Reason = "AwaitingStatefulSet"
		ready.Message = "the server has not been observed yet"
	case set.Status.ReadyReplicas == 0:
		// Read from the StatefulSet rather than derived from anything here.
		// Ready means PostgreSQL answered pg_isready, which is the only thing
		// that makes the connection string in the Secret worth handing out.
		ready.Status = metav1.ConditionFalse
		ready.Reason = "NotAcceptingConnections"
		ready.Message = "the server is not answering yet"
	default:
		ready.Status = metav1.ConditionTrue
		ready.Reason = "Serving"
		ready.Message = "the server is accepting connections"
	}
	setCondition(&db.Status.Conditions, ready)

	return r.Status().Update(ctx, db)
}

// SetupWithManager registers the reconciler.
func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Database{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Named("database").
		Complete(r)
}
