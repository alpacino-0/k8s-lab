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
	"crypto/rand"
	"encoding/base64"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// Translated from chart/templates/postgres.yaml, which the plan calls the
// working specification for this — YAML proven on a real cluster rather than a
// design to be invented here. Anything below that reads as a decision was one
// made there and measured; the comments come with it.

const (
	// The port PostgreSQL answers on, named so the Service and the container
	// agree without either restating the number.
	postgresPortName = "postgres"
	postgresPort     = 5432

	// The uid "postgres" has in the alpine images. The data directory is
	// created with it, so it is not a value that can be changed later without
	// the volume becoming unreadable.
	postgresUID int64 = 70

	// Long enough that it is not worth attacking and short enough to paste.
	// Generated once and then preserved; see desiredDatabaseSecret.
	passwordBytes = 30

	// The scratch volume every container here mounts, because a read-only
	// root filesystem leaves nowhere else to write.
	tmpVolume = "tmp"
)

func databaseLabels(db *platformv1alpha1.Database) map[string]string {
	return map[string]string{
		nameLabel:      postgresPortName,
		instanceLabel:  db.Name,
		managedByLabel: managedByDamga,
		componentLabel: "database",
	}
}

func databaseSelector(db *platformv1alpha1.Database) map[string]string {
	return map[string]string{
		nameLabel:     postgresPortName,
		instanceLabel: db.Name,
	}
}

// databaseHost is the stable name of the single pod, not of the Service.
//
// A headless Service has no address of its own, so a client pointed at it gets
// whatever DNS returns; the pod's own name is the thing that stays true, and
// with one replica it is the only one there is.
func databaseHost(db *platformv1alpha1.Database) string {
	return fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", db.Name, db.Name, db.Namespace)
}

// desiredDatabaseSecret renders the credentials, keeping any password that is
// already there.
//
// The keeping is the important half and it is not tidiness. PostgreSQL bakes
// the password into the data directory when it first initialises, so a value
// that changes afterwards locks the application out of its own database while
// the server goes on accepting the old one — a failure that looks like a
// credentials bug in the app and is not.
//
// existing is what the cluster holds, or nil. A caller that cannot read it must
// pass nil rather than an empty Secret, or every reconcile mints a new password.
func desiredDatabaseSecret(db *platformv1alpha1.Database, existing *corev1.Secret) (*corev1.Secret, error) {
	password := ""
	if existing != nil {
		password = string(existing.Data["POSTGRES_PASSWORD"])
	}
	if password == "" {
		raw := make([]byte, passwordBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generating a password: %w", err)
		}
		// Base64 rather than hex: the same entropy in fewer characters, and
		// every byte of it is safe in a connection string and in a shell.
		password = base64.RawURLEncoding.EncodeToString(raw)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: db.Name, Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Type: corev1.SecretTypeOpaque,
		// The three names the postgres image reads on first boot, and the three
		// an application needs. Written as data the workload consumes with
		// envFrom, which is why they are spelled the way the image spells them.
		StringData: map[string]string{
			"POSTGRES_USER":     db.Spec.Username,
			"POSTGRES_DB":       db.Spec.Database,
			"POSTGRES_PASSWORD": password,
			// Not read by PostgreSQL. Here because every application that talks
			// to this needs the host, and a Secret the workload already mounts
			// is one fewer thing for it to be told separately.
			"DB_HOST": databaseHost(db),
			"DB_PORT": fmt.Sprintf("%d", postgresPort),
		},
	}, nil
}

// desiredDatabaseService is headless: it exists to give the pod a stable DNS
// name, not to load-balance across replicas there is only one of.
func desiredDatabaseService(db *platformv1alpha1.Database) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: db.Name, Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  databaseSelector(db),
			Ports: []corev1.ServicePort{{
				Name: postgresPortName, Port: postgresPort,
				TargetPort: intstr.FromString(postgresPortName),
			}},
		},
	}
}

// desiredDatabaseStatefulSet renders the server.
func desiredDatabaseStatefulSet(db *platformv1alpha1.Database) *appsv1.StatefulSet {
	volumeMode := corev1.PersistentVolumeFilesystem
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: dataVolume},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			// Stated rather than left out. The API server defaults it, and a
			// field the manifest omits and the server fills in is a permanent
			// diff for anything comparing desired state against live state.
			VolumeMode: &volumeMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: db.Spec.Storage},
			},
		},
	}
	if db.Spec.StorageClass != "" {
		claim.Spec.StorageClassName = ptr.To(db.Spec.StorageClass)
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: db.Name, Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: db.Name,
			// One. Stated in the API's own doc comment as a limit rather than
			// implied: this is the database a small team gets without operating
			// one, and PostgreSQL replication is not something to add by
			// changing an integer.
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: databaseSelector(db)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: databaseSelector(db)},
				Spec: corev1.PodSpec{
					// PostgreSQL never calls the Kubernetes API, and the
					// admission policy refuses a pod that mounts a token it
					// does not need.
					AutomountServiceAccountToken: ptr.To(false),
					// Long enough for a checkpoint. Killing PostgreSQL mid-write
					// is recoverable and slow; letting it finish is neither.
					TerminationGracePeriodSeconds: ptr.To(int64(60)),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(postgresUID),
						RunAsGroup:   ptr.To(postgresUID),
						// Without this the mounted volume belongs to root and
						// initdb cannot write to it.
						FSGroup:        ptr.To(postgresUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  postgresPortName,
						Image: db.Spec.Image,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							// PostgreSQL writes only to its data directory, its
							// socket directory and /tmp, all of them mounted
							// volumes. Verified before the policy requiring it
							// was written.
							ReadOnlyRootFilesystem: ptr.To(true),
							Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
						},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: db.Name},
							},
						}},
						Env: []corev1.EnvVar{{
							// A subdirectory, not the mount point. A volume
							// mounted at the data directory itself arrives with
							// a lost+found in it, and initdb refuses to
							// initialise a directory that is not empty.
							Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata",
						}},
						Ports: []corev1.ContainerPort{{
							Name: postgresPortName, ContainerPort: postgresPort,
						}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: dataVolume, MountPath: "/var/lib/postgresql/data"},
							{Name: runVolume, MountPath: "/var/run/postgresql"},
							{Name: tmpVolume, MountPath: tmpPath},
						},
						// pg_isready and not a TCP probe. A socket that accepts
						// connections while the server is still replaying WAL is
						// a pod marked ready that answers every query with an
						// error.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
								Command: []string{"sh", "-c",
									"pg_isready -U $POSTGRES_USER -d $POSTGRES_DB -q"},
							}},
							InitialDelaySeconds: 5, PeriodSeconds: 5,
						},
						// Slower and more forgiving than readiness on purpose:
						// readiness taking the pod out of service is cheap,
						// liveness killing it mid-recovery is not.
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
								Command: []string{"sh", "-c", "pg_isready -U $POSTGRES_USER -q"},
							}},
							InitialDelaySeconds: 30, PeriodSeconds: 15, FailureThreshold: 5,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    db.Spec.Resources.CPURequest,
								corev1.ResourceMemory: db.Spec.Resources.MemoryRequest,
							},
							// No CPU limit, for the reason the Workload has
							// none: throttling produces a latency cliff, and the
							// failure it prevents is one the memory limit
							// already covers.
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: db.Spec.Resources.MemoryLimit,
							},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: runVolume, VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						}},
						{Name: tmpVolume, VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{
								SizeLimit: ptr.To(resource.MustParse("64Mi")),
							},
						}},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{claim},
		},
	}
}
