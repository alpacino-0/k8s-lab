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

package deploywatch

import (
	"context"
	"errors"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/damgahq/damga/evidence"
)

// Sweep gives up on records nothing ever came back about, and says so.
//
// It exists because the alternative is worse than a gap. A record opened at
// commit time and never observed means the commit may have been rejected at
// admission, or the observer may have been down, or the cluster may never have
// been reachable — and there is no way to tell which afterwards. A rejected
// deploy leaves almost nothing behind: the object never persists, so there is
// no annotation and no report, and the only trace measured on a live cluster
// was a Warning event on a policy object in another namespace with an hour's
// retention.
//
// So the sweep does not guess. It writes unknown, which is a true statement
// about what the platform saw, and the page renders it as unconfirmed. An audit
// record may be incomplete; it may not be confidently wrong.
type Sweep struct {
	Evidence evidence.Store

	// After is how long a record may sit unobserved before it is given up on.
	// It has to exceed the cluster's progress deadline, which is ten minutes on
	// every Deployment this platform renders — a sweep faster than that marks
	// a rollout unknown while it is still legitimately rolling.
	After time.Duration

	// Every is the interval between passes.
	Every time.Duration

	// Now is injected so the tests do not have to sleep. nil means time.Now.
	Now func() time.Time
}

// NeedLeaderElection puts the sweep in the leader-election runnable group.
// Several replicas racing to give up on the same record would be harmless — the
// version fence lets one win — but it would be several stores' worth of work to
// reach the same answer.
func (s *Sweep) NeedLeaderElection() bool { return true }

// Start runs until ctx is cancelled. It satisfies manager.Runnable.
func (s *Sweep) Start(ctx context.Context) error {
	every := s.Every
	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Not an error. A cancelled context is how a manager stops, and
			// returning one here makes every clean shutdown look failed.
			return nil
		case <-ticker.C:
			if err := s.Once(ctx); err != nil {
				logf.FromContext(ctx).Error(err, "evidence sweep failed")
			}
		}
	}
}

// Once makes a single pass. Exported so a test can drive it without a clock.
func (s *Sweep) Once(ctx context.Context) error {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	after := s.After
	if after <= 0 {
		after = 30 * time.Minute
	}
	cutoff := now().Add(-after)
	log := logf.FromContext(ctx)

	// Only the states that mean "opened and not yet answered". Everything else
	// has been observed, and a record the platform has an answer about is not
	// the sweep's business.
	q := evidence.Query{
		States: []evidence.State{evidence.StatePending, evidence.StateSyncing},
		Until:  cutoff,
		Order:  evidence.OrderOldest,
		Limit:  100,
	}

	for {
		page, err := s.Evidence.History(ctx, q)
		if err != nil {
			return err
		}
		for _, rec := range page.Records {
			// Re-checked rather than trusted from the query, because the two
			// stores disagree by one about whether Until is inclusive, and a
			// record given up on a minute early is a wrong record.
			if !rec.CreatedAt.Before(cutoff) {
				continue
			}
			version := len(rec.Transitions)
			_, err := s.Evidence.Transition(ctx, rec.ID, evidence.Transition{
				From: []evidence.State{evidence.StatePending, evidence.StateSyncing},
				To:   evidence.StateUnknown,
				At:   now().UTC(),
				Reason: "nothing was observed within " + after.String() +
					"; the platform does not know whether this reached the cluster",
				Observation:  evidence.Observation{Source: evidence.ObservedFromSweep, At: now().UTC()},
				ExpectEvents: &version,
			})
			switch {
			case errors.Is(err, evidence.ErrConflict):
				// The observer got there first, which is the outcome the sweep
				// exists to lose to.
				continue
			case err != nil:
				return err
			}
			log.Info("gave up on an unobserved record", "record", rec.ID, "app", rec.Ref.App)
		}
		if page.Next == "" {
			return nil
		}
		q.After = page.Next
	}
}
