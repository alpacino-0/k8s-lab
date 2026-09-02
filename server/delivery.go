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

package server

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/damgahq/damga/placement"
)

// keyDelivery is what the answer calls the line that says whether anything will
// apply what was just committed. Named because three responses carry it and a
// panel reads it by name.
const keyDelivery = "delivery"

// argoNamespace is where Argo CD watches for Applications.
//
// The chart's own default and the namespace terraform and the installer create.
// Not configurable here yet: an install that put Argo CD somewhere else has one
// more thing to tell this server, and nothing has asked.
const argoNamespace = "argocd"

// Deliverer makes something apply what a deploy commits.
//
// This is the seam the whole product was missing. Every other piece existed:
// the panel picks a catalogue entry, the endpoint plans it, gitwrite commits
// manifests into the tenant's repository — and nothing anywhere turned that
// commit into a running pod. The installer said so in its own words, in the
// list of what it does not install: "Argo CD. Nothing applies what a deploy
// commits."
//
// A seam rather than a hard dependency, for the reason BuildCreator is one: an
// install with no cluster to write to still has to start, serve the panel and
// say what it cannot do. nil means nothing is delivered, and the endpoints that
// would have used it say so rather than reporting a commit as a deployment.
type Deliverer interface {
	// Deliver makes an Argo CD Application for one placement, or updates the
	// one that is already there. It is called after the placement is written
	// and must be safe to call again with the same arguments.
	Deliver(ctx context.Context, place placement.Placement) error
}

// deliver hands one placement to whatever applies commits, and says plainly
// when there is nothing to hand it to.
//
// An error here never fails the request that produced it. The placement is
// written and the manifests are pushed; what is missing is delivery, and a 500
// would tell the caller to undo work that is fine.
func deliverPlacement(ctx context.Context, st stores, place placement.Placement) error {
	if st.delivery == nil {
		return fmt.Errorf("this installation has no delivery: it was started without a " +
			"cluster to write Argo CD Applications into, so commits accumulate and nothing " +
			"applies them")
	}
	return st.delivery.Deliver(ctx, place)
}

// applicationName is what the Application for one placement is called.
//
// Derived rather than generated, because this has to be the same name the next
// call produces: delivery is idempotent only if the second call addresses the
// object the first one made.
//
// Namespace and app, and that pair is unique by a constraint rather than by
// convention — the placement store carries UNIQUE (namespace, app). Both are
// DNS labels because the API refuses a placement whose namespace or app is not,
// so the two joined by a dash are a legal object name. The tenant is not in it
// and does not need to be: a namespace belongs to one tenant, which the store
// also enforces.
func applicationName(place placement.Placement) string {
	return place.Namespace + "-" + place.App
}

// clusterDelivery writes Applications into the cluster this server runs in.
type clusterDelivery struct{ client client.Client }

// Deliver creates or updates the Application, as unstructured rather than
// through Argo CD's Go types.
//
// The types would mean depending on the Argo CD module for four fields, and on
// its transitive graph for the rest of this binary's build. What is written
// here is a document with a known shape; the API server validates it against
// the CRD Argo CD installed, which is the check that matters.
func (d clusterDelivery) Deliver(ctx context.Context, place placement.Placement) error {
	app := &unstructured.Unstructured{}
	app.SetAPIVersion("argoproj.io/v1alpha1")
	app.SetKind("Application")
	app.SetName(applicationName(place))
	app.SetNamespace(argoNamespace)

	spec := map[string]any{
		"project": "default",
		"source": map[string]any{
			"repoURL": place.RepoURL,
			"path":    place.Path,
			// The branch the platform commits to, or the remote's default when
			// the placement named none — the same rule gitwrite pushes by, so
			// the ref that is written is the ref that is read.
			"targetRevision": revisionOf(place),
		},
		"destination": map[string]any{
			"server":    "https://kubernetes.default.svc",
			"namespace": place.Namespace,
		},
		"syncPolicy": map[string]any{
			// selfHeal is not a convenience here; it is why the fence is a
			// manifest at all. Measured against this project's Argo CD (chart
			// 8.5.10, v3.1.8): a pod-security label removed from a namespace
			// created by CreateNamespace was never restored — not in four
			// minutes, not by a forced sync, not after a hard refresh — and the
			// Application called itself Synced and Healthy throughout. The same
			// label removed from a namespace committed as a manifest came back
			// in about ten seconds. What makes that true is this field.
			//
			// prune, so that a file a render stops producing is an object that
			// stops existing. Without it the write path can add and change but
			// never withdraw, which is the state gitwrite was in until it
			// learned to delete.
			"automated":   map[string]any{"prune": true, "selfHeal": true},
			"syncOptions": []any{"CreateNamespace=false"},
		},
	}
	if err := unstructured.SetNestedMap(app.Object, spec, "spec"); err != nil {
		return fmt.Errorf("building the Application: %w", err)
	}

	// Server-side apply, so this owns the fields it sets and leaves anything a
	// human added beside them. Create-then-update would race a second control
	// plane replica through the same endpoint.
	err := d.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(app),
		client.ForceOwnership, client.FieldOwner("damga-control-plane"))
	if err != nil {
		return fmt.Errorf("delivering %s: %w", applicationName(place), err)
	}
	return nil
}

// clusterDelivery builds the writer this server delivers through.
//
// Its own client rather than the manager's, for the reason clusterReader gives:
// the handler is built before the manager exists, and the manager's client
// reads a cache that is empty until it starts.
func (o Options) clusterDelivery() (Deliverer, error) {
	restCfg := o.RestConfig
	if restCfg == nil {
		var err error
		if restCfg, err = ctrl.GetConfig(); err != nil {
			return nil, fmt.Errorf("delivering a deploy needs a cluster: %w", err)
		}
	}
	// The scheme carries nothing: an Application is written as unstructured,
	// which controller-runtime routes by the GVK on the object itself.
	c, err := client.New(restCfg, client.Options{Scheme: runtime.NewScheme()})
	if err != nil {
		return nil, fmt.Errorf("building the delivery client: %w", err)
	}
	return clusterDelivery{client: c}, nil
}

// revisionOf is the ref Argo CD should track for a placement.
func revisionOf(place placement.Placement) string {
	if place.Branch == "" {
		return "HEAD"
	}
	return place.Branch
}
