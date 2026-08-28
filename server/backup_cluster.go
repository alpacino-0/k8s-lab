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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// clusterBackups reads the Database a Workload names, from the cluster.
//
// Two reads and a hop between them, which is the shape the model asks for: the
// Workload names a database and nothing else, so finding the status means
// following that name. Neither object references the other with an owner
// reference, and that is the point — an app is redeployed constantly and its
// data outlives every one of those deploys.
type clusterBackups struct{ client.Reader }

// NewClusterBackups builds a BackupReader over a Kubernetes reader.
//
// Exported because the seam is only useful if something outside this package
// can fill it; the free build fills it from the manager's own client.
func NewClusterBackups(r client.Reader) BackupReader { return clusterBackups{r} }

func (c clusterBackups) ForApp(ctx context.Context, namespace, app string) (BackupStatus, error) {
	var workload platformv1alpha1.Workload
	err := c.Get(ctx, client.ObjectKey{Name: app, Namespace: namespace}, &workload)
	switch {
	case apierrors.IsNotFound(err):
		// The Workload is not there yet. Argo CD has not applied the commit, or
		// this environment has never been deployed — either way the app has no
		// database in the only place that could say so, and that is the same
		// answer as naming none.
		return BackupStatus{}, ErrNoDatabase
	case err != nil:
		return BackupStatus{}, fmt.Errorf("reading the workload: %w", err)
	}
	if workload.Spec.Database == "" {
		return BackupStatus{}, ErrNoDatabase
	}

	var db platformv1alpha1.Database
	err = c.Get(ctx, client.ObjectKey{Name: workload.Spec.Database, Namespace: namespace}, &db)
	switch {
	case apierrors.IsNotFound(err):
		// Named and not there. Deliberately not ErrNoDatabase: an app pointing
		// at a database that does not exist is a misconfiguration somebody
		// should see, and reporting it as "no database" hides it behind the
		// ordinary case.
		return BackupStatus{}, fmt.Errorf("the workload names database %q, which does not exist",
			workload.Spec.Database)
	case err != nil:
		return BackupStatus{}, fmt.Errorf("reading the database: %w", err)
	}

	out := BackupStatus{Database: db.Name}
	if db.Status.LastRestore == nil {
		// Backups are configured or they are not, and either way nothing has
		// run yet. The zero FinishedAt is what the handler renders as "none
		// yet" — a third answer that is neither success nor failure.
		return out, nil
	}
	out.Rehearsed = true
	out.FinishedAt = db.Status.LastRestore.FinishedAt.Time
	out.Rows = db.Status.LastRestore.Rows
	out.SourceRows = db.Status.LastRestore.SourceRows
	out.Tables = db.Status.LastRestore.Tables
	return out, nil
}
