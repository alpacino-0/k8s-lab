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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=db
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.status.host`
// +kubebuilder:printcolumn:name="Restored",type=date,JSONPath=`.status.lastRestore.finishedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Database is a PostgreSQL server this platform runs for one app environment.
//
// # Why it is not a field on Workload
//
// The obvious shape is spec.database on the Workload, and it is the shape that
// loses data. Everything a Workload renders carries an owner reference to it,
// which is what makes `kubectl delete workload` clean up — and a StatefulSet's
// PersistentVolumeClaims would go the same way. Deleting an app would delete
// its database, permanently, through a cascade nobody typed.
//
// The lifecycles are genuinely different, which is the real reason rather than
// the consequence. An app is redeployed many times a day and its database
// outlives every one of those deploys; an app can be deleted and recreated
// against data that has to still be there. Two objects with different
// lifetimes do not belong in one.
//
// # What connects them
//
// The Workload names a Database by name in its own namespace, and nothing more
// — no owner reference in either direction. The Database publishes its
// credentials as a Secret, which the Workload already knows how to consume
// through envFrom. That is the whole coupling: a name and a Secret.
//
// Deliberately not a cross-namespace reference. A Secret readable from another
// namespace is a tenant boundary with a hole in it, and the namespace-per-tenant
// arrangement is what makes the reference safe without another check.
//
// # Why the platform runs it at all
//
// One replica, no failover, no connection pooler. That is a real limit and it
// is stated rather than implied: this is the database a small team gets for
// free without operating one, and it is not a managed service. An install that
// needs high availability points its apps at something else, which the same
// envFrom seam already allows.
type Database struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseSpec   `json:"spec,omitempty"`
	Status DatabaseStatus `json:"status,omitempty"`
}

// DatabaseSpec is what the tenant asks for.
type DatabaseSpec struct {
	// Image is the PostgreSQL image to run.
	//
	// Pinned by the tenant rather than chosen by the platform, and refused if
	// it floats: PostgreSQL will not start on a data directory written by a
	// newer major version, so an image that moves is an outage on the next pod
	// restart, at a moment nothing connects to the change that caused it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self.contains('@') || self.split('/')[int(self.split('/').size()) - 1].contains(':')",message="image must carry an explicit tag or digest"
	// +kubebuilder:validation:XValidation:rule="!self.endsWith(':latest')",message="image must not use the :latest tag"
	Image string `json:"image"`

	// Database is the database name inside the server.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	// +kubebuilder:default=app
	Database string `json:"database,omitempty"`

	// Username is the role the application connects as.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	// +kubebuilder:default=app
	Username string `json:"username,omitempty"`

	// Storage is how much disk the data directory gets.
	//
	// No default, on purpose. Every other size in this API has one because
	// getting it wrong costs a restart; this one cannot be changed downwards at
	// all and is expensive to change upwards, so it is the tenant's answer and
	// not the platform's guess.
	//
	// Required is not enough on its own, which a test found rather than a
	// review: a zero Quantity serialises as "0", so the field is present, the
	// required list is satisfied, and the platform goes on to ask for a
	// zero-byte volume. Absent and zero are both "no answer" here and both have
	// to be refused.
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="storage must be greater than zero"
	Storage resource.Quantity `json:"storage"`

	// StorageClass selects the volume type. Empty means the cluster default,
	// which is what a single-node install has.
	StorageClass string `json:"storageClass,omitempty"`

	Resources Resources `json:"resources,omitempty"`

	// Backup is what is taken, how often, and how long it is kept. Absent means
	// no backups, which is a choice a tenant is allowed to make and not a
	// default they can arrive at by not reading.
	Backup *DatabaseBackup `json:"backup,omitempty"`
}

// DatabaseBackup describes the schedule and the rehearsal.
type DatabaseBackup struct {
	// Schedule is a cron expression in the cluster's timezone.
	// +kubebuilder:default="0 2 * * *"
	Schedule string `json:"schedule,omitempty"`

	// RetainDays is how long an archive is kept.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=7
	RetainDays int32 `json:"retainDays,omitempty"`

	// Storage is the size of the volume the archives live on. It is separate
	// from the data volume so that filling it with backups cannot stop the
	// database from accepting writes.
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="storage must be greater than zero"
	Storage resource.Quantity `json:"storage"`

	// Rehearse restores each backup into a throwaway instance and counts the
	// rows, instead of only checking that the archive is readable.
	//
	// On by default, and that is the product's whole claim about backups: an
	// archive that decompresses is a file, and a dump truncated after the
	// schema is a readable file holding nothing. Turning it off is allowed —
	// it costs CPU and time on a large database — and the evidence surface says
	// which of the two claims an install is entitled to make.
	// +kubebuilder:default=true
	Rehearse *bool `json:"rehearse,omitempty"`
}

// DatabaseStatus reports what the platform observed.
type DatabaseStatus struct {
	// Conditions follows the standard convention: Ready says whether the server
	// is accepting connections.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// SecretName is where the credentials are, for a Workload to name in
	// envFrom. Reported rather than assumed, so the name can change without
	// every consumer having to know the rule that produced it.
	SecretName string `json:"secretName,omitempty"`

	// Host is where the server answers inside the cluster.
	Host string `json:"host,omitempty"`

	// LastRestore is the most recent rehearsal, and it is the reason this type
	// carries a status at all beyond Ready.
	//
	// "The backup was last restored three hours ago, 1,284 rows verified" is a
	// sentence about the past, and a page that has to say it cannot compute it
	// from anything live. Somebody has to have written down that it happened.
	LastRestore *RestoreRehearsal `json:"lastRestore,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// RestoreRehearsal is one proof that the backup can be restored.
type RestoreRehearsal struct {
	// FinishedAt is when the rehearsal completed.
	FinishedAt metav1.Time `json:"finishedAt"`

	// Archive is the file that was restored.
	Archive string `json:"archive,omitempty"`

	// Rows and Tables are what came back out of it.
	//
	// Rows is a count and not an estimate, and zero is a legitimate answer for
	// a database nothing has written to. It is compared against the source
	// rather than against a threshold, because any threshold is wrong for one
	// of those two cases.
	Rows   int64 `json:"rows"`
	Tables int32 `json:"tables"`

	// SourceRows is what the live database held when the archive was taken, so
	// a reader can see that everything came back rather than merely that
	// something did.
	SourceRows int64 `json:"sourceRows"`
}

// DatabaseList contains a list of Database.
// +kubebuilder:object:root=true
type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Database `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Database{}, &DatabaseList{})
		return nil
	})
}
