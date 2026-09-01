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

	// The same two for Redis. The name is also the value of the
	// app.kubernetes.io/name label, which is part of a StatefulSet's selector
	// and therefore immutable — so these two strings are the reason a Database
	// cannot change engine after it exists.
	redisPortName = "redis"
	redisPort     = 6379

	// The uid "redis" has in the official images, the way postgresUID is the
	// one "postgres" has. The data directory is created with it.
	redisUID int64 = 999

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

	// Where each engine's password lives in the Secret. Named because the
	// reconcile reads the key back before writing, and reading the wrong one
	// mints a new password on every pass — see passwordKey.
	postgresPasswordKey = "POSTGRES_PASSWORD"
	redisPasswordKey    = "REDIS_PASSWORD"
)

// engineName is the server's own name, and it is the value of the
// app.kubernetes.io/name label on everything a Database renders.
//
// Empty means postgres rather than an error, because an object built in Go
// never passes through the API server's defaulting and the field defaults to
// postgres there. The two have to agree or a Database created one way is
// labelled differently from the same Database created the other way — and that
// label is inside an immutable selector.
func engineName(db *platformv1alpha1.Database) string {
	if db.Spec.Engine == platformv1alpha1.EngineRedis {
		return redisPortName
	}
	return postgresPortName
}

// enginePort is where the server answers.
func enginePort(db *platformv1alpha1.Database) int32 {
	if db.Spec.Engine == platformv1alpha1.EngineRedis {
		return redisPort
	}
	return postgresPort
}

func databaseLabels(db *platformv1alpha1.Database) map[string]string {
	return map[string]string{
		nameLabel:      engineName(db),
		instanceLabel:  db.Name,
		managedByLabel: managedByDamga,
		componentLabel: "database",
	}
}

func databaseSelector(db *platformv1alpha1.Database) map[string]string {
	return map[string]string{
		nameLabel:     engineName(db),
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
		password = string(existing.Data[passwordKey(db)])
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

	// The host and the port, which neither server reads and every application
	// needs. A Secret the workload already mounts is one fewer thing for it to
	// be told separately.
	data := map[string]string{
		"DB_HOST": databaseHost(db),
		"DB_PORT": fmt.Sprintf("%d", enginePort(db)),
	}
	if db.Spec.Engine == platformv1alpha1.EngineRedis {
		data[redisPasswordKey] = password
		// A URL as well as the parts, and only for Redis. Every Redis client
		// worth using takes one, none of them agree on what to call the three
		// separate variables, and the catalogue applications this exists for
		// ask for REDIS_URL by that name.
		//
		// The password is in it, which is the same exposure the password key
		// beside it already is: this is a Secret, and a URL that omitted the
		// credential would just be a host the application still cannot use.
		data["REDIS_URL"] = fmt.Sprintf("redis://:%s@%s:%d", password, databaseHost(db), redisPort)
		return secretOf(db, data), nil
	}
	// The three names the postgres image reads on first boot, spelled the way
	// the image spells them because it is the image that reads them.
	data["POSTGRES_USER"] = db.Spec.Username
	data["POSTGRES_DB"] = db.Spec.Database
	data[postgresPasswordKey] = password
	return secretOf(db, data), nil
}

// passwordKey is where this engine's password lives in the Secret.
//
// Read before it is written, and that is the whole reason this function is
// separate: a password minted on every pass locks the application out of a
// server whose storage still holds the first one. Looking under the wrong key
// is indistinguishable from finding nothing, so a Redis Database reading
// POSTGRES_PASSWORD would mint a new password on every single reconcile.
func passwordKey(db *platformv1alpha1.Database) string {
	if db.Spec.Engine == platformv1alpha1.EngineRedis {
		return redisPasswordKey
	}
	return postgresPasswordKey
}

func secretOf(db *platformv1alpha1.Database, data map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: db.Name, Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
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
				Name: engineName(db), Port: enginePort(db),
				TargetPort: intstr.FromString(engineName(db)),
			}},
		},
	}
}

// redisArgs is how the server is started, absorbed from
// chart/templates/redis.yaml with one line deliberately reversed.
//
// The chart runs `--maxmemory-policy allkeys-lru`, and its comment says why:
// everything the chart's Redis holds is "derivable and short-lived. Losing it
// costs a cache miss and a looser rate limit window, not data — which is why an
// eviction policy is safe". That is true of the platform's own rate-limit
// counters and it is not true here. This Redis is handed to a tenant through
// envFrom, and the catalogue applications it exists for keep queues and
// sessions in it. allkeys-lru would silently delete a tenant's data when the
// memory limit was reached, and the application would see a key that used to be
// there simply not be there.
//
// noeviction refuses the write instead, with an error the client raises. A
// tenant who is out of memory finds out from an error rather than from missing
// data, which is the same trade the rest of this platform already makes.
//
// Persistence for the same reason. The chart turns both save and appendonly off
// because losing the lot costs nothing there; here it costs the tenant's data,
// so the volume the CRD already requires is used for what it is for. appendonly
// rather than snapshots alone: a snapshot loses everything since the last one.
//
// The password arrives through the environment rather than as a literal,
// because the argument list is the one place a Secret's value would end up in
// `kubectl describe`. It is still visible in this container's own process list,
// which is a real limit and one Redis gives no way around short of an ACL file.
func redisArgs() []string {
	return []string{"sh", "-c", `exec redis-server   --appendonly yes   --dir /data   --maxmemory-policy noeviction   --requirepass "$` + redisPasswordKey + `"`}
}

// desiredDatabaseStatefulSet renders the server the tenant asked for.
func desiredDatabaseStatefulSet(db *platformv1alpha1.Database) *appsv1.StatefulSet {
	if db.Spec.Engine == platformv1alpha1.EngineRedis {
		return redisStatefulSet(db)
	}
	return postgresStatefulSet(db)
}

// databaseClaim is the data volume, which both engines take on the same terms:
// the size the tenant named, the class they named, and ReadWriteOnce because
// there is one replica and there is going to be one replica.
func databaseClaim(db *platformv1alpha1.Database) corev1.PersistentVolumeClaim {
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
	return claim
}

// redisStatefulSet renders a Redis a tenant can keep data in.
func redisStatefulSet(db *platformv1alpha1.Database) *appsv1.StatefulSet {
	// redis-cli reads REDISCLI_AUTH, which is the only way to authenticate a
	// probe without putting the password in the command line — where it would
	// appear in `kubectl describe pod` and in every event about a failing
	// probe. Without it both probes answer NOAUTH and the pod never goes ready.
	probe := func(delay, period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"redis-cli", "ping"},
			}},
			InitialDelaySeconds: delay, PeriodSeconds: period, FailureThreshold: failures,
		}
	}
	auth := corev1.EnvVar{Name: "REDISCLI_AUTH", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: db.Name},
			Key:                  redisPasswordKey,
		},
	}}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: db.Name, Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: db.Name,
			Replicas:    ptr.To(int32(1)),
			Selector:    &metav1.LabelSelector{MatchLabels: databaseSelector(db)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: databaseSelector(db)},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					// Shorter than PostgreSQL's sixty. Redis rewrites the
					// append-only file on shutdown and there is no WAL replay to
					// interrupt, so the thing the long grace period buys does
					// not exist here.
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(redisUID),
						RunAsGroup:   ptr.To(redisUID),
						// Without this the mounted volume belongs to root and
						// the append-only file cannot be written.
						FSGroup:        ptr.To(redisUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    redisPortName,
						Image:   db.Spec.Image,
						Command: redisArgs(),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
						},
						// The password, which the shell in Command expands into
						// --requirepass, and the same value again under the name
						// redis-cli reads.
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: db.Name},
							},
						}},
						Env: []corev1.EnvVar{auth},
						Ports: []corev1.ContainerPort{{
							Name: redisPortName, ContainerPort: redisPort,
						}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: dataVolume, MountPath: "/data"},
							{Name: tmpVolume, MountPath: tmpPath},
						},
						ReadinessProbe: probe(2, 5, 3),
						// Slower and more forgiving than readiness, for the
						// reason PostgreSQL's is: readiness taking the pod out
						// of service is cheap, liveness killing it while it is
						// loading a large append-only file is not.
						LivenessProbe: probe(10, 15, 5),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    db.Spec.Resources.CPURequest,
								corev1.ResourceMemory: db.Spec.Resources.MemoryRequest,
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: db.Spec.Resources.MemoryLimit,
							},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: tmpVolume, VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{
								SizeLimit: ptr.To(resource.MustParse("64Mi")),
							},
						}},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{databaseClaim(db)},
		},
	}
}

func postgresStatefulSet(db *platformv1alpha1.Database) *appsv1.StatefulSet {
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
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{databaseClaim(db)},
		},
	}
}
