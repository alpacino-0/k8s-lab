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
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	// buildsKeptPerApp is how many finished builds of one app survive.
	//
	// Ten, and the number is not chosen here: cluster/registry.yaml keeps the
	// newest ten tags of each repository, so the eleventh Build is a record
	// whose image the registry has already collected. Keeping it would keep the
	// paperwork for something nobody can pull — and the two numbers have to
	// move together, which is easier to notice when they are the same number
	// for one stated reason than when they are two numbers that happen to
	// agree.
	buildsKeptPerApp = 10

	// buildsKeptInNamespace is the ceiling across every tenant.
	//
	// Every Build of every app lives in one namespace under one quota, so a
	// per-app rule alone bounds nothing: twenty apps keeping ten each is two
	// hundred, which is the quota exactly. This is the rule that keeps the
	// namespace off the wall.
	//
	// 150 of the 200 that cluster/build-namespace.yaml permits. The 50 left
	// over is five times the ten concurrent builds the same file allows, so a
	// burst that arrives while a sweep is running still has somewhere to land.
	buildsKeptInNamespace = 150
)

// collectBuilds removes finished Build records that nothing needs any more.
//
// # Why the operator and not the control plane
//
// The control plane is granted create on builds and deliberately not delete —
// cluster/build-namespace.yaml says why: a Build is the record of what produced
// a running image, and a server that can remove one can remove the answer to
// "what is this". The operator already owns this object's lifecycle, so the
// retention rule lives with the thing that reconciles it.
//
// # Why anything has to
//
// Nothing collected these before. The namespace quota admits 200 and a Build is
// created per push, so an application that is deployed a few times a day walks
// into that wall in a fortnight — and what a user sees when it arrives is not
// "the platform is full", it is "deploy does not work".
//
// # Failures do not live longer than successes
//
// The obvious asymmetry is to keep failures longer because they are what
// somebody is debugging. It is backwards. A failed build produced no image, so
// it is the provenance of nothing; a successful one is the provenance of
// something that may still be running. And a failing repository is exactly what
// fills this namespace fastest — a CI loop against a broken Dockerfile is the
// case that reaches 200 first. Keeping failures longer would evict the records
// that answer "what is running" in order to keep the records nobody read.
//
// What a failure gets instead is recency: it is collected by the same rule as
// everything else, so the last ten attempts of an app are always there, which
// is the window in which anybody actually looks.
func (r *BuildReconciler) collectBuilds(ctx context.Context, namespace string) error {
	var all platformv1alpha1.BuildList
	if err := r.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return err
	}

	// Only what has finished. A build that is running is the one thing here
	// that cannot be reconstructed, and a sweep that deletes it takes the job
	// with it through the owner reference.
	finished := make([]platformv1alpha1.Build, 0, len(all.Items))
	for _, b := range all.Items {
		if b.Status.Phase == platformv1alpha1.BuildSucceeded ||
			b.Status.Phase == platformv1alpha1.BuildFailed {
			finished = append(finished, b)
		}
	}
	slices.SortFunc(finished, func(a, b platformv1alpha1.Build) int {
		return endedAt(b).Compare(endedAt(a))
	})

	perAppLimit, ceiling := r.KeptPerApp, r.KeptInNamespace
	if perAppLimit <= 0 {
		perAppLimit = buildsKeptPerApp
	}
	if ceiling <= 0 {
		ceiling = buildsKeptInNamespace
	}

	// Newest first, so counting forwards is counting what to keep.
	doomed := map[string]bool{}
	perApp := map[string]int{}
	for _, b := range finished {
		owner := b.Labels[platformv1alpha1.BuildTenantLabel] + "/" + b.Labels[platformv1alpha1.BuildAppLabel]
		perApp[owner]++
		if perApp[owner] > perAppLimit {
			doomed[b.Name] = true
		}
	}
	// And then the ceiling, which can take a build that its own app would have
	// kept. That order is deliberate: the per-app rule is fairness between
	// tenants and the ceiling is the wall, and the wall wins.
	kept := 0
	for _, b := range finished {
		if doomed[b.Name] {
			continue
		}
		kept++
		if kept > ceiling {
			doomed[b.Name] = true
		}
	}

	for i := range finished {
		b := finished[i]
		if !doomed[b.Name] {
			continue
		}
		// Deleting the Build takes its Job and the Job's pods with it, through
		// the owner reference the reconciler set — which is also what frees the
		// pod and job counts in the same quota.
		if err := r.Delete(ctx, &b); err != nil && !apierrors.IsNotFound(err) {
			// Not fatal to the reconcile. The build this sweep was triggered by
			// has already been recorded, and a sweep that fails half way is one
			// the next finished build repeats.
			return err
		}
	}
	return nil
}

// endedAt is when a build stopped, for ordering.
//
// FinishedAt is written by this controller and is the truth; creation time is
// the fallback for a record that reached a terminal phase without one, which is
// possible for a build that was refused before it ever ran.
func endedAt(b platformv1alpha1.Build) time.Time {
	if b.Status.FinishedAt != nil {
		return b.Status.FinishedAt.Time
	}
	return b.CreationTimestamp.Time
}
