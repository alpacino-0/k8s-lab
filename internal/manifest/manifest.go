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

// Package manifest turns an app's desired state into the file that is
// committed, and reads that file back.
//
// One Workload per environment and nothing else. The operator renders the
// seven objects a workload actually needs — Deployment, Service,
// ServiceAccount, NetworkPolicy, PodDisruptionBudget, and conditionally an
// Ingress and an autoscaler — from that one resource, which is what keeps the
// hardening a guarantee instead of a template somebody can edit out.
//
// The committed file is the state. There is no second copy in the control
// plane's database, and Parse is how a deploy that changes one field reads the
// other fields it is not changing. That is only lossless because damga owns
// these repositories and the tenant has no push identity for them; the day
// something else commits here, a round trip through the Go type starts
// deleting whatever it wrote.
package manifest

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/internal/deploywatch"
)

// File is the name written inside the placement's directory.
//
// Fixed rather than derived from the app name. The directory already names the
// app — that is what a placement's Path is — and a second copy of the name in
// the filename means a rename has to move a file as well as a directory, with
// a window where both exist and Argo CD applies both.
const File = "workload.yaml"

// FileFor is where an object other than the primary workload is written.
//
// The primary keeps File, which is a fixed name for the reason above; anything
// beside it is named for what it is, because a directory holding six manifests
// has to be readable by somebody running ls. Lower-cased kind and object name,
// which are both already constrained to what a filename can hold: a Kubernetes
// object name is a DNS label, and the kinds are this package's own.
//
// It never returns File. "workload.yaml" and "workload-<name>.yaml" cannot
// collide, because an object name is never empty.
func FileFor(kind, name string) string {
	return strings.ToLower(kind) + "-" + name + ".yaml"
}

// Owns says whether a committed file is one this platform wrote.
//
// The test is the API group and not the filename. A name is a convention, and
// the first rename or hand-edit makes a convention wrong in the direction that
// costs the most: a file that is ours and does not look it is a file something
// stops maintaining, and a file that looks ours and is not is a file something
// deletes. What is actually decisive is in the content — this platform writes
// its own resources and nothing else.
//
// Everything unreadable is somebody else's: a README, a kustomization, a
// Secret, a file that is not YAML at all. That asymmetry is deliberate, because
// the two mistakes are not the same size. Failing to recognise our own file
// leaves a stale manifest in git where a person can see it; recognising
// somebody else's as ours removes work from a repository they cannot push to.
func Owns(body []byte) bool {
	var head struct {
		APIVersion string `json:"apiVersion"`
	}
	if err := yaml.Unmarshal(body, &head); err != nil {
		return false
	}
	return head.APIVersion == platformv1alpha1.GroupVersion.String()
}

// Render produces the file to commit.
//
// The rollout id goes on the Workload's own annotations, from where the
// operator carries it onto the Deployment and the observer reads it back off
// the live object. That is the whole chain that lets a record opened at commit
// time be closed by what actually happened in the cluster.
func Render(w platformv1alpha1.Workload, rolloutID string) ([]byte, error) {
	if w.Name == "" || w.Namespace == "" {
		return nil, fmt.Errorf("manifest: a workload needs a name and a namespace")
	}
	if w.Spec.Image == "" {
		return nil, fmt.Errorf("manifest: a workload needs an image")
	}

	// Set rather than merged into whatever was there. These two identify the
	// resource, and a file that arrived with a different apiVersion is a file
	// this package did not write.
	w.APIVersion = platformv1alpha1.GroupVersion.String()
	w.Kind = "Workload"

	if rolloutID != "" {
		if w.Annotations == nil {
			w.Annotations = map[string]string{}
		}
		w.Annotations[deploywatch.RolloutAnnotation] = rolloutID
	}

	// Nothing about status is written. It is the cluster's answer, and a
	// committed status field is a claim about the world made by whoever last
	// edited a file.
	w.Status = platformv1alpha1.WorkloadStatus{}

	out, err := yaml.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("manifest: rendering: %w", err)
	}
	return out, nil
}

// RenderDatabase produces the file one Database is committed to.
//
// Separate from Render rather than a generic object renderer, because the two
// differ in the one thing that matters here: no rollout annotation. The id on a
// Workload is what lets a record opened at commit time be closed by the
// Deployment the operator derives from it — the observer finds a record by
// reading damga.co/rollout off the live object. A Database's StatefulSet
// carrying the same id would be a second object answering to one record, and
// the observer would close a deploy on whichever it saw first.
//
// It is here rather than in the caller so that "what a committed file looks
// like" stays one answer. The apiVersion this sets is the same string Owns
// recognises, and a second renderer somewhere else is how those two drift into
// a file the platform writes and does not recognise as its own.
func RenderDatabase(db platformv1alpha1.Database) ([]byte, error) {
	if db.Name == "" || db.Namespace == "" {
		return nil, fmt.Errorf("manifest: a database needs a name and a namespace")
	}
	db.APIVersion = platformv1alpha1.GroupVersion.String()
	db.Kind = "Database"

	// Nothing about status, for the reason Render gives: it is the cluster's
	// answer, and a committed status is a claim about the world made by
	// whoever last edited a file.
	db.Status = platformv1alpha1.DatabaseStatus{}

	out, err := yaml.Marshal(db)
	if err != nil {
		return nil, fmt.Errorf("manifest: rendering the database: %w", err)
	}
	return out, nil
}

// Parse reads a committed file back.
//
// Strict: a file with a field this build does not know is an error rather than
// a silent drop. The alternative is a deploy that changes the image and
// removes a setting somebody added with a newer version of damga, which is a
// data loss nobody would attribute to the deploy that caused it.
func Parse(body []byte) (platformv1alpha1.Workload, error) {
	var w platformv1alpha1.Workload
	if err := yaml.UnmarshalStrict(body, &w); err != nil {
		return platformv1alpha1.Workload{}, fmt.Errorf("manifest: reading: %w", err)
	}
	if w.Kind != "" && w.Kind != "Workload" {
		return platformv1alpha1.Workload{}, fmt.Errorf("manifest: %s is not a Workload", w.Kind)
	}
	return w, nil
}
