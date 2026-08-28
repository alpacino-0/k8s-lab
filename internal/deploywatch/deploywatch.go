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

// Package deploywatch closes the evidence record that the git write path
// opened.
//
// It exists because of principle 1. The platform is the only thing that writes
// to git, so it is the only thing that knows who asked for a deploy — but it is
// not present when the deploy happens, because Argo CD is what applies it. So a
// record is opened at commit time with the human's name on it and closed here,
// by reading the cluster.
//
// Reading is not writing: this package holds no write verb on anything, and the
// principle constrains the write path.
//
// It is level-triggered rather than event-driven, and that is the whole design
// rather than a preference. The obvious alternative — watching Argo CD's
// Application and following .status.history — was measured and is lossy: Argo
// CD builds a self-heal sync as a *partial* sync, and partial syncs never
// append a history entry, so every drift correction is invisible there. History
// is also a ring buffer capped at ten. A reconciler that re-derives from live
// state has no such hole: an outage costs latency, not a record.
package deploywatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/forge"
)

const (
	// RolloutAnnotation carries the id of the record this object belongs to.
	// Damga mints it before writing the commit and stamps it here.
	//
	// On object metadata, never on the pod template. Verified: the deployment
	// controller hashes .spec.template alone, so an object annotation cannot
	// roll pods — and it could not, or `deployment.kubernetes.io/revision`,
	// which is itself an object annotation the controller rewrites on every
	// rollout, would be an infinite rollout loop.
	RolloutAnnotation = "damga.co/rollout"

	// VerifyImagesAnnotation is Kyverno's, not ours. It binds the digest that
	// was admitted to the verdict on it, in one field:
	//
	//	{"ghcr.io/damgahq/damga@sha256:f8b41ad0…":"pass"}
	//
	// Read off the Deployment, and that is measured rather than assumed. The
	// policy matches Pods and Kyverno autogenerates the rule that covers pod
	// controllers, so where the annotation lands is upstream's decision. On the
	// installed version, with a signed image and an auditing policy:
	//
	//	deployment metadata : {"ghcr.io/…@sha256:83b4496a…":"pass"}
	//	pod template        : <absent>
	//	pod metadata        : {"ghcr.io/…@sha256:83b4496a…":"pass"}
	//
	// The Deployment and the Pod carry it; the pod template does not — which is
	// the one that would have mattered, because a template annotation is part
	// of the hash the deployment controller rolls on. scripts/verdict-probe.sh
	// is where that comes from and it runs in CI, so a version of Kyverno that
	// moves it fails there rather than here.
	//
	// Its ABSENCE is a signal in its own right, and the one this project got
	// wrong once already: an image no rule matched produces no annotation, and
	// rendering that the same as "verified" is how an evidence page lies.
	VerifyImagesAnnotation = "kyverno.io/verify-images"
)

// Reconciler observes Deployments and moves the record they name.
type Reconciler struct {
	client.Client
	Evidence evidence.Store

	// Connections is where the signing identity for an app lives, and it is
	// optional. Without it the verdict still reaches the record — the digest
	// and the pass are on the Deployment — but with no subject or issuer
	// attached, and nothing ever leaves audit mode. An installation that
	// connects no repositories is exactly that installation.
	Connections forge.Store
}

// SetupWithManager registers the reconciler. It watches Deployments only.
// Pods carry the same Kyverno verdict, but a Deployment is the object a rollout
// is, and a per-pod watch would multiply the work by the replica count to learn
// nothing the Deployment does not already say.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Named("deploywatch").
		Complete(r)
}

// The permissions this needs, and only these. Read-only, because the write path
// is git.
//
// A caveat that matters more than the line itself: controller-gen is pointed at
// ./api/... and ./internal/..., and it collects every marker under them into the
// *operator's* ClusterRole. This one happens to be a strict subset of what the
// operator already holds, so regenerating changes nothing — verified. But the
// server is a different workload with a different ServiceAccount, and it has no
// role of its own because nothing deploys it in-cluster yet. When something
// does, this marker moves with it; leaving it here to be silently merged into
// the operator's role would grant the wrong pod the right permission.
//
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile derives the deploy's state from the live object and moves the
// record to match, at most once per real change.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var deploy appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deploy); err != nil {
		// A deleted Deployment is not a state change for the record. What ran,
		// ran; the evidence of it does not become false because the object is
		// gone, and inventing a transition here would rewrite history on a
		// `kubectl delete`.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	id := deploy.Annotations[RolloutAnnotation]
	if id == "" {
		// Not ours, or an out-of-band change. Either way there is nothing to
		// move: a record this object does not name cannot be found, and
		// creating one would be the observer inventing a deploy nobody asked
		// for. Recording it as a finding needs somewhere to put it — the app
		// model — which does not exist yet.
		return ctrl.Result{}, nil
	}

	rec, err := r.Evidence.Get(ctx, evidence.ID(id))
	switch {
	case errors.Is(err, evidence.ErrNotFound):
		// The annotation names a record that is not there. Worth saying out
		// loud exactly once rather than requeuing: retrying cannot conjure the
		// record, and this is what a restored cluster or a wiped store looks
		// like.
		log.Info("deployment names a record the store does not have",
			"rollout", id, "deployment", req.NamespacedName)
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("reading the evidence record: %w", err)
	}

	want, reason := derive(&deploy)
	if want == "" {
		// Mid-rollout, or the status has not caught up with the spec. Nothing
		// is known yet, so nothing is written. A watch will bring us back.
		return ctrl.Result{}, nil
	}
	if rec.State == want {
		return ctrl.Result{}, nil
	}

	from := allowedFrom(want)
	if len(from) == 0 {
		return ctrl.Result{}, nil
	}

	// The connection, if this app has one. Read before the transition so the
	// verdict can name the identity, and a failure to read it is not a failure
	// to record what was observed: the pass on the object is true whether or
	// not the control plane can say whose signature it was.
	var conn *forge.Connection
	if r.Connections != nil {
		got, cErr := r.Connections.Get(ctx, forge.Key{TenantID: rec.Ref.TenantID, App: rec.Ref.App})
		if cErr == nil {
			conn = &got
		} else if !errors.Is(cErr, forge.ErrNotFound) {
			log.V(1).Info("could not read the connection; recording the verdict without it",
				"app", rec.Ref.App, "error", cErr)
		}
	}
	verdict := signatureVerdict(&deploy, conn)

	// Fenced on the version the derivation was made from. Without this, a
	// write computed before a leader handover can land after it and set a
	// state on the strength of an observation that is already stale — and the
	// hash chain would still verify, because it proves the row was not edited
	// rather than the order rows were written in.
	version := len(rec.Transitions)

	_, err = r.Evidence.Transition(ctx, rec.ID, evidence.Transition{
		From: from, To: want, At: time.Now().UTC(), Reason: reason,
		Observation: evidence.Observation{
			Source: evidence.ObservedFromWorkload,
			At:     time.Now().UTC(),
		},
		Image:        admittedImage(&deploy),
		Signature:    verdict,
		ExpectEvents: &version,
	})
	switch {
	case errors.Is(err, evidence.ErrConflict):
		// The normal outcome, not an error. Something else moved the record
		// between the read and the write, or it is in a state this transition
		// is not allowed from. Requeuing would be a hot loop against the store
		// for a derivation that is now stale; the next watch event carries a
		// fresh one.
		log.V(1).Info("record moved under us; leaving it alone",
			"rollout", id, "want", want, "have", rec.State)
		return ctrl.Result{}, nil
	case apierrors.IsConflict(err):
		return ctrl.Result{Requeue: true}, nil
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("transitioning the record to %s: %w", want, err)
	}

	// The first signature is what ends the policy's recording state, and this
	// is the only place that ever learns of one. Written after the transition
	// rather than before: the record is the evidence, and marking a connection
	// verified on the strength of an observation that then failed to record
	// would enforce a policy against a chain nothing can show working.
	//
	// Once, and never moved. A later deploy is another signature, not a
	// different first one, and rewriting it would make "since when has this
	// been enforcing" unanswerable.
	if conn != nil && verdict != nil && verdict.Verified && !conn.Verified() {
		conn.FirstSignatureAt = time.Now().UTC()
		if _, err := r.Connections.Put(ctx, *conn); err != nil {
			// Not fatal, and not retried here. The record moved, which is the
			// thing that had to happen; the next deploy sees the same pass and
			// tries again. Failing the reconcile would roll back nothing and
			// requeue an observation that has already been written.
			log.Error(err, "could not mark the connection verified",
				"app", rec.Ref.App)
		} else {
			log.Info("first signature seen; the signature policy will enforce from the next deploy",
				"app", rec.Ref.App, "identity", conn.Identity())
		}
	}

	log.Info("evidence record moved", "rollout", id, "to", want, "reason", reason)
	return ctrl.Result{}, nil
}

// derive reads a state out of the Deployment, or "" when nothing is known yet.
//
// Every signal here is generation-stable on purpose. The Available condition is
// not used, and that is measured rather than stylistic: every Deployment this
// platform renders runs maxUnavailable 0, so Available requires *all* replicas
// and one restarting pod flips it — while metadata.generation does not move.
// Deriving from it would flap the record between running and degraded for the
// lifetime of the app.
func derive(d *appsv1.Deployment) (evidence.State, string) {
	if d.Status.ObservedGeneration != d.Generation {
		// The controller has not looked at this spec yet. Anything read now
		// describes the previous one.
		return "", ""
	}

	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	switch {
	case want == 0:
		// Scaled to nothing on purpose. Not a failure and not running.
		return "", ""
	case d.Status.UpdatedReplicas == want &&
		d.Status.ReadyReplicas == want &&
		d.Status.Replicas == want:
		return evidence.StateRunning, "every replica is updated and ready"
	case progressDeadlineExceeded(d):
		// The only failure a Deployment reports about itself, and it can only
		// arrive while a rollout is in flight: the deployment controller stops
		// evaluating the deadline once Progressing latches on
		// NewReplicaSetAvailable. So this is a rollout that never landed, which
		// is exactly what the record should say.
		return evidence.StateFailed, "the rollout did not complete before its deadline"
	default:
		// Objects exist and admission accepted them; the rollout is still
		// moving. This is the honest floor: something was applied.
		return evidence.StateApplied, "the objects were admitted"
	}
}

func progressDeadlineExceeded(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

// allowedFrom is the compare-and-set target set, written out per destination so
// that the destination never appears in its own From — which is what stops a
// level-triggered reconciler appending an identical transition every loop.
//
// Running is terminal for a rollout id, and that is a product decision rather
// than a storage one. A Deployment's progress deadline is ten minutes, so
// admitting running to failed would admit a failure written ten minutes after
// the fact and open unbounded alternation between the two. The record answers
// "did this deploy land", not "is this app healthy now"; the second question is
// the monitoring stack's, and it already has one.
func allowedFrom(to evidence.State) []evidence.State {
	switch to {
	case evidence.StateApplied:
		return []evidence.State{evidence.StatePending, evidence.StateSyncing, evidence.StateUnknown}
	case evidence.StateRunning:
		return []evidence.State{
			evidence.StatePending, evidence.StateSyncing,
			evidence.StateApplied, evidence.StateUnknown,
		}
	case evidence.StateFailed:
		return []evidence.State{evidence.StatePending, evidence.StateSyncing, evidence.StateApplied}
	default:
		return nil
	}
}

// admittedImage reports what actually ran, taken from the field Kyverno
// rewrote, together with the verdict it recorded.
//
// nil when the object carries no verdict, which is not the same as a failure:
// it is an image no rule matched. Reporting the two identically is the defect
// this project already measured once, in a namespace that claimed to enforce.
func admittedImage(d *appsv1.Deployment) *evidence.Image {
	if ref, ok := passedDigest(d); ok {
		return &evidence.Image{AdmittedDigest: ref}
	}
	return nil
}

// passedDigest is the digest the admission controller says it verified.
//
// Only a pass counts. Kyverno's own helper treats "skip" as verified, and a
// record that has to survive an auditor must not — an image no rule matched
// produces no annotation at all, and rendering that the same as verified is how
// an evidence page lies.
func passedDigest(d *appsv1.Deployment) (string, bool) {
	raw := d.Annotations[VerifyImagesAnnotation]
	if raw == "" {
		return "", false
	}
	verdicts := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &verdicts); err != nil {
		return "", false
	}
	for ref, verdict := range verdicts {
		if verdict == "pass" {
			return ref, true
		}
	}
	return "", false
}

// signatureVerdict is what the record gets to say about the supply chain.
//
// The pass and the digest come off the live object, from the controller that
// actually did the checking. The issuer and the subject come from the
// connection, and they have to: the annotation records that a rule passed and
// not which identity satisfied it. That is sound only because the rule in that
// namespace is the one rendered from this connection, pinned to this subject
// and scoped to this image repository — so a pass on a matching image is a pass
// against that identity and no other.
//
// Without a connection there is still a verdict, carrying the digest and the
// pass and nothing about who signed. Weaker, and said as such rather than left
// out: a record with no verdict at all is indistinguishable from a deploy
// nothing checked.
func signatureVerdict(d *appsv1.Deployment, conn *forge.Connection) *evidence.SignatureVerdict {
	digest, ok := passedDigest(d)
	if !ok {
		return nil
	}
	v := &evidence.SignatureVerdict{
		Verified: true, Digest: digest,
		Message: "admission verified the signature on this digest",
	}
	if conn != nil {
		v.Issuer = forge.OIDCIssuer
		v.Subject = conn.Identity()
	}
	return v
}
