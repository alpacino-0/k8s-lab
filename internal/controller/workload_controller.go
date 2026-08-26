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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// WorkloadReconciler renders one Workload into the objects a production
// workload needs, and keeps them that way.
type WorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.damga.co,resources=workloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.damga.co,resources=workloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.damga.co,resources=workloads/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var app platformv1alpha1.Workload
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		// Gone. Every object this created carries an owner reference, so the
		// garbage collector has already dealt with them.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	normalise(&app)

	if err := r.reconcileOwned(ctx, &app); err != nil {
		logger.Error(err, "rendering the workload failed")
		if statusErr := r.updateStatus(ctx, &app, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.updateStatus(ctx, &app, nil)
}

// normalise fills in what the CRD's defaults would have supplied. An Workload
// built in Go and handed straight to the reconciler never passes through the API
// server's defaulting, and a zero Quantity means "no CPU at all" rather than
// "unset" — which the admission policies would rightly reject.
func normalise(app *platformv1alpha1.Workload) {
	app.Spec.Resources.CPURequest = quantityOrDefault(app.Spec.Resources.CPURequest, "100m")
	app.Spec.Resources.MemoryRequest = quantityOrDefault(app.Spec.Resources.MemoryRequest, "128Mi")
	app.Spec.Resources.MemoryLimit = quantityOrDefault(app.Spec.Resources.MemoryLimit, "512Mi")

	if app.Spec.Port == 0 {
		app.Spec.Port = 8080
	}
	if app.Spec.Health.LivenessPath == "" {
		app.Spec.Health.LivenessPath = "/healthz"
	}
	if app.Spec.Health.ReadinessPath == "" {
		app.Spec.Health.ReadinessPath = "/readyz"
	}
}

func (r *WorkloadReconciler) reconcileOwned(ctx context.Context, app *platformv1alpha1.Workload) error {
	// Order matters only for the ServiceAccount, which the Deployment names.
	if err := r.apply(ctx, app, desiredServiceAccount(app), func(existing, desired client.Object) {
		e := existing.(*corev1.ServiceAccount)
		d := desired.(*corev1.ServiceAccount)
		e.AutomountServiceAccountToken = d.AutomountServiceAccountToken
		e.Labels = d.Labels
	}); err != nil {
		return fmt.Errorf("service account: %w", err)
	}

	if err := r.apply(ctx, app, desiredDeployment(app), func(existing, desired client.Object) {
		e := existing.(*appsv1.Deployment)
		d := desired.(*appsv1.Deployment)
		// Replicas is left alone when autoscaling owns it, so that reconciling
		// does not undo the autoscaler's most recent decision.
		if d.Spec.Replicas != nil {
			e.Spec.Replicas = d.Spec.Replicas
		}
		e.Spec.Selector = d.Spec.Selector
		e.Spec.Strategy = d.Spec.Strategy
		e.Spec.Template = d.Spec.Template
		e.Labels = d.Labels
	}); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}

	if err := r.apply(ctx, app, desiredService(app), func(existing, desired client.Object) {
		e := existing.(*corev1.Service)
		d := desired.(*corev1.Service)
		e.Spec.Selector = d.Spec.Selector
		e.Spec.Ports = d.Spec.Ports
		e.Labels = d.Labels
	}); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if err := r.apply(ctx, app, desiredNetworkPolicy(app), func(existing, desired client.Object) {
		e := existing.(*networkingv1.NetworkPolicy)
		d := desired.(*networkingv1.NetworkPolicy)
		e.Spec = d.Spec
		e.Labels = d.Labels
	}); err != nil {
		return fmt.Errorf("network policy: %w", err)
	}

	if err := r.apply(ctx, app, desiredPodDisruptionBudget(app), func(existing, desired client.Object) {
		e := existing.(*policyv1.PodDisruptionBudget)
		d := desired.(*policyv1.PodDisruptionBudget)
		e.Spec.MinAvailable = d.Spec.MinAvailable
		e.Spec.Selector = d.Spec.Selector
		e.Labels = d.Labels
	}); err != nil {
		return fmt.Errorf("pod disruption budget: %w", err)
	}

	// The last two are conditional, so each is either applied or removed. Only
	// creating them would leave an Ingress serving a domain the user deleted.
	if hpa := desiredHPA(app); hpa != nil {
		if err := r.apply(ctx, app, hpa, func(existing, desired client.Object) {
			e := existing.(*autoscalingv2.HorizontalPodAutoscaler)
			d := desired.(*autoscalingv2.HorizontalPodAutoscaler)
			e.Spec = d.Spec
			e.Labels = d.Labels
		}); err != nil {
			return fmt.Errorf("autoscaler: %w", err)
		}
	} else if err := r.deleteIfPresent(ctx, app, &autoscalingv2.HorizontalPodAutoscaler{}); err != nil {
		return fmt.Errorf("removing autoscaler: %w", err)
	}

	if ing := desiredIngress(app); ing != nil {
		if err := r.apply(ctx, app, ing, func(existing, desired client.Object) {
			e := existing.(*networkingv1.Ingress)
			d := desired.(*networkingv1.Ingress)
			e.Spec = d.Spec
			e.Labels = d.Labels
			e.Annotations = d.Annotations
		}); err != nil {
			return fmt.Errorf("ingress: %w", err)
		}
	} else if err := r.deleteIfPresent(ctx, app, &networkingv1.Ingress{}); err != nil {
		return fmt.Errorf("removing ingress: %w", err)
	}

	return nil
}

// apply creates the object or brings the existing one back to what it should be.
// mutate copies only the fields this operator owns, so that defaults the API
// server filled in — a Service's clusterIP, a Deployment's revision history —
// are not clobbered on every pass.
func (r *WorkloadReconciler) apply(
	ctx context.Context,
	app *platformv1alpha1.Workload,
	desired client.Object,
	mutate func(existing, desired client.Object),
) error {
	existing, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object %T is not a client.Object", desired)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		mutate(existing, desired)
		return controllerutil.SetControllerReference(app, existing, r.Scheme)
	})
	return err
}

// deleteIfPresent removes an object this Workload no longer wants — but only if
// this Workload is the thing that created it.
//
// Deleting by name and namespace alone is enough to destroy something that
// merely shares a name. A Workload called `blog` with no domain would delete an
// unrelated Ingress called `blog` in the same namespace, one that could be
// carrying live traffic and that this operator never created. The owner
// reference is the only thing that distinguishes the two.
func (r *WorkloadReconciler) deleteIfPresent(
	ctx context.Context,
	app *platformv1alpha1.Workload,
	obj client.Object,
) error {
	key := client.ObjectKey{Name: app.Name, Namespace: app.Namespace}
	if err := r.Get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, app) {
		log.FromContext(ctx).Info(
			"leaving an object alone because this workload does not own it",
			"kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", key.Name)
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

func (r *WorkloadReconciler) updateStatus(
	ctx context.Context,
	app *platformv1alpha1.Workload,
	reconcileErr error,
) error {
	var dep appsv1.Deployment
	depErr := r.Get(ctx, client.ObjectKey{Name: app.Name, Namespace: app.Namespace}, &dep)
	if depErr != nil && !apierrors.IsNotFound(depErr) {
		return depErr
	}

	app.Status.ObservedGeneration = app.Generation
	app.Status.Replicas = dep.Status.Replicas
	app.Status.ReadyReplicas = dep.Status.ReadyReplicas

	if app.Spec.Domain != "" {
		app.Status.URL = "https://" + app.Spec.Domain
	} else {
		app.Status.URL = ""
	}

	ready := metav1.Condition{
		Type:               "Ready",
		ObservedGeneration: app.Generation,
		LastTransitionTime: metav1.Now(),
	}
	switch {
	case reconcileErr != nil:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "RenderFailed"
		ready.Message = reconcileErr.Error()
	case apierrors.IsNotFound(depErr):
		ready.Status = metav1.ConditionFalse
		ready.Reason = "AwaitingDeployment"
		ready.Message = "the deployment has not been observed yet"
	case dep.Status.ReadyReplicas == 0:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "NoReadyReplicas"
		ready.Message = "no replica has passed its readiness probe"
	default:
		ready.Status = metav1.ConditionTrue
		ready.Reason = "Serving"
		ready.Message = fmt.Sprintf("%d of %d replicas ready", dep.Status.ReadyReplicas, dep.Status.Replicas)
	}
	meta := &app.Status.Conditions
	setCondition(meta, ready)

	return r.Status().Update(ctx, app)
}

// setCondition keeps LastTransitionTime honest: it only moves when the status
// actually changes, so "how long has this been broken" stays answerable.
func setCondition(conditions *[]metav1.Condition, next metav1.Condition) {
	for i, existing := range *conditions {
		if existing.Type != next.Type {
			continue
		}
		if existing.Status == next.Status {
			next.LastTransitionTime = existing.LastTransitionTime
		}
		(*conditions)[i] = next
		return
	}
	*conditions = append(*conditions, next)
}

func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Workload{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Named("workload").
		Complete(r)
}
