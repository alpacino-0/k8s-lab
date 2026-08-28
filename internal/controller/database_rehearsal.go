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
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// rehearsalResult is what the backup job writes to its termination log.
//
// A shape shared with a shell script, which is a joint worth being careful
// about: nothing type-checks across it, so a field renamed on one side reads as
// a zero on the other. The names here are the names in backupScript, and the
// test that parses a literal copy of what that script prints is what keeps the
// two honest.
type rehearsalResult struct {
	FinishedAt   string `json:"finishedAt"`
	Archive      string `json:"archive"`
	Tables       int32  `json:"tables"`
	Rows         int64  `json:"rows"`
	SourceRows   int64  `json:"sourceRows"`
	Restored     bool   `json:"restored"`
	ArchiveBytes int64  `json:"archiveBytes"`
}

// latestRehearsal reads the most recent backup run off the pods it left behind.
//
// The pod and not the Job, because the message lives in a container's terminated
// state and a Job only says how many succeeded. Pods outlive their Job for as
// long as the history limit allows, which is what makes "three hours ago"
// answerable at all — the alternative is a status that is only ever correct in
// the seconds after a run.
//
// Nothing is returned when nothing has run, and that is not the same as a
// failed rehearsal. A Database whose first backup is still hours away has no
// answer to give, and inventing one — "not restored" — would read on the page
// as a rehearsal that was tried and did not work.
func (r *DatabaseReconciler) latestRehearsal(
	ctx context.Context, db *platformv1alpha1.Database,
) (*platformv1alpha1.RestoreRehearsal, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(db.Namespace),
		client.MatchingLabels{
			instanceLabel:  db.Name,
			componentLabel: backupComponent,
		},
	); err != nil {
		return nil, err
	}

	var newest *platformv1alpha1.RestoreRehearsal
	for i := range pods.Items {
		got := rehearsalFromPod(&pods.Items[i])
		if got == nil {
			continue
		}
		// Compared on the time the run reported rather than on the pod's own
		// timestamps. A pod that started later can finish earlier — the history
		// limit keeps three, and a retry after a failure overlaps the next
		// scheduled run — so ordering by anything but the answer itself can
		// report an older rehearsal as the latest.
		if newest == nil || got.FinishedAt.After(newest.FinishedAt.Time) {
			newest = got
		}
	}
	return newest, nil
}

// rehearsalFromPod pulls one result out of a finished pod, or nil.
func rehearsalFromPod(pod *corev1.Pod) *platformv1alpha1.RestoreRehearsal {
	for i := range pod.Status.ContainerStatuses {
		term := pod.Status.ContainerStatuses[i].State.Terminated
		if term == nil || term.Message == "" {
			continue
		}
		var res rehearsalResult
		if err := json.Unmarshal([]byte(term.Message), &res); err != nil {
			// Not an error worth surfacing. A container that died before it
			// wrote anything leaves whatever it last printed here, and a
			// backup that failed is reported by the Job failing — not by this
			// function inventing a rehearsal out of a log line.
			continue
		}
		// A run that only took a backup is not a rehearsal, and reporting it as
		// one would put "restored" on a page for a tenant who turned the
		// rehearsal off.
		if !res.Restored {
			continue
		}
		finished, err := time.Parse(time.RFC3339, res.FinishedAt)
		if err != nil {
			continue
		}
		return &platformv1alpha1.RestoreRehearsal{
			FinishedAt: metav1.NewTime(finished),
			Archive:    res.Archive,
			Rows:       res.Rows,
			Tables:     res.Tables,
			SourceRows: res.SourceRows,
		}
	}
	return nil
}
