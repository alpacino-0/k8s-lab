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

// Package notify tells somebody when a deploy stops going well.
//
// A platform that cannot say "your app is down" is missing the thing people
// notice first. This repository already had most of an alerting path — rules
// that fire, an Alertmanager that groups and inhibits, and scripts/alert-test.sh
// proving both — and it stopped one step short: the receiver in
// cluster/monitoring-values.yaml is "null", so an alert reaches Alertmanager
// and goes nowhere. That path is still where infrastructure alerts belong and
// is untouched here.
//
// This is the other source, and it is the one every install has. Alertmanager
// arrives with the monitoring stack, which is optional and which
// scripts/install.sh does not install; the control plane is in every
// installation and already knows the thing a user most wants to hear about —
// what happened to the deploy they just asked for.
package notify

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/damgahq/damga/evidence"
)

// Event is one thing worth telling somebody about.
//
// Flat strings and no evidence types, because this crosses a boundary: what a
// webhook body carries is a product surface that will change, and the record's
// shape is hashed into a chain that must not. server/wire.go makes the same
// separation for the same reason.
type Event struct {
	Tenant string
	App    string
	Env    string

	// State is the evidence state the record reached, as its own word:
	// "running", "failed", "rejected", "unknown".
	State string
	Seq   int64

	Image  string
	Commit string
	Actor  string

	// Reason is the transition's own words — "the commit was never pushed:
	// …", "nothing was observed within 30m…". It is what makes the message
	// answerable rather than merely alarming.
	Reason string

	At time.Time
}

// Notifier delivers one event.
//
// An interface with one method, so a paid build, a test and a second transport
// are the same substitution. It returns an error rather than swallowing one:
// who logs a failed notification is the caller's decision, and the caller is
// the only one that knows a notification nobody received is not a reason to
// fail the deploy it was about.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// worthTelling is the states a person wants woken for.
//
// Four of the nine, and the five left out are left out for one reason each.
// pending and syncing are damga narrating its own progress. applied is the
// cluster having accepted the manifest, which is not yet the app working.
// superseded means a newer deploy replaced this one, and the newer one will
// send its own message. Sending all nine would be the platform teaching people
// to filter it out, which is the same as not sending anything.
var worthTelling = map[evidence.State]bool{
	// It worked. Said out loud because "did my deploy land" is the question,
	// and a platform that only speaks up on failure leaves it unanswered.
	evidence.StateRunning: true,
	// It did not.
	evidence.StateFailed:   true,
	evidence.StateRejected: true,
	// Nobody knows, which is its own answer and the one most worth having: the
	// sweep reaches this after nothing observed the deploy for the pending
	// timeout, and silence here would be indistinguishable from success.
	evidence.StateUnknown: true,
}

// Store wraps an evidence.Store so that every path that moves a record tells
// somebody about it.
//
// A decorator and not a call at each site, because there are three of them —
// internal/gitwrite when a push fails, internal/deploywatch when the cluster
// answers, and the sweep when nothing does — and they are in three packages
// written at three different times. A fourth is a matter of time, and the way
// that fourth would announce itself is a deploy that failed in silence.
//
// It notifies after the store has committed the transition, never before: a
// message about a state change that then failed to persist is worse than no
// message, because it cannot be reconciled with the page.
type Store struct {
	evidence.Store
	Notifier Notifier
	Log      *slog.Logger
}

// Wrap returns s unchanged when there is nothing configured to notify.
//
// Nil rather than a no-op implementation, so an install with no webhook pays
// nothing and reads nothing in its logs. The seam is the Notifier; the absence
// of one is not a degraded mode, it is the default.
func Wrap(s evidence.Store, n Notifier, log *slog.Logger) evidence.Store {
	if n == nil {
		return s
	}
	if log == nil {
		log = slog.Default()
	}
	return &Store{Store: s, Notifier: n, Log: log}
}

// Transition moves the record and then, if the state it reached is one worth
// telling somebody about, tells them.
func (s *Store) Transition(
	ctx context.Context, id evidence.ID, t evidence.Transition,
) (evidence.Record, error) {
	rec, err := s.Store.Transition(ctx, id, t)
	if err != nil {
		return rec, err
	}
	s.announce(ctx, rec)
	return rec, nil
}

// announce sends, and says which way it failed when it fails.
//
// The failure is logged and never returned, because the deploy is not less
// successful for a webhook being down. What it must not be is silent: "the
// notification was not sent" is true of a wrong URL, a receiver that refused
// the body and a network that never carried it, and those are three different
// things to go and fix. Notify's own errors name which; this adds the app they
// were about.
func (s *Store) announce(ctx context.Context, rec evidence.Record) {
	if !worthTelling[rec.State] {
		return
	}
	e := EventFrom(rec)
	if err := s.Notifier.Notify(ctx, e); err != nil {
		s.Log.Error("the deploy notification was not delivered",
			"app", e.App, "env", e.Env, "state", e.State, "error", err)
	}
}

// EventFrom projects a record onto the wire.
//
// Exported because a second sender — a mail transport, a paid build's
// integration — needs the same projection, and two of them would disagree about
// which commit a record is about the first time one of them was written by
// somebody who had not read effectiveCommit.
func EventFrom(rec evidence.Record) Event {
	return Event{
		Tenant: rec.Ref.TenantID,
		App:    rec.Ref.App,
		Env:    rec.Ref.Env,
		State:  string(rec.State),
		Seq:    rec.Seq,
		Image:  rec.Image.RequestedRef,
		Commit: commitOf(rec),
		// The instance-local audit alias the record carries, which for a
		// damga-originated deploy is the account id and never the login
		// address. That is deliberate here as well as in the commit it comes
		// from: a webhook body is posted to a third party, kept by them, and
		// visible to everyone in the channel — which is the last place a
		// person's email address should turn up because they pressed deploy.
		Actor:  rec.Actor.DisplayName,
		Reason: reasonOf(rec),
		At:     rec.UpdatedAt,
	}
}

// commitOf is the commit a record is about, wherever it was written down.
//
// A record opened by the git write path cannot carry the SHA in Source: the
// record has to exist before the commit does, because the rollout id is minted
// from it and has to be inside the manifests. The SHA lands on a transition
// instead. server/wire.go answers the same question the same way, and this is
// deliberately a copy rather than an import — that one is the panel's
// presentation and this is a message body, and coupling them would mean a
// change to either being made against both.
func commitOf(rec evidence.Record) string {
	if rec.Source.CommitSHA != "" {
		return rec.Source.CommitSHA
	}
	// Newest first: a record can be observed more than once, and the commit it
	// is about is the one it was pushed as.
	for _, e := range slices.Backward(rec.Transitions) {
		if rev := e.Observation.Revision; rev != "" {
			return rev
		}
	}
	return ""
}

// maxReason bounds what travels. Slack refuses a message over 4000 characters
// outright, and a reason long enough to approach that is a stack trace nobody
// reads in a chat window — the page has the whole of it.
const maxReason = 500

// reasonOf is the newest transition's reason, which is the one that explains
// the state the record is in now.
func reasonOf(rec evidence.Record) string {
	if len(rec.Transitions) == 0 {
		return ""
	}
	reason := strings.TrimSpace(rec.Transitions[len(rec.Transitions)-1].Reason)
	if len(reason) > maxReason {
		// Marked rather than silently cut, so nobody debugs a truncated
		// sentence believing it is the whole one.
		return reason[:maxReason] + "… (truncated; the deploy page has the rest)"
	}
	return reason
}
