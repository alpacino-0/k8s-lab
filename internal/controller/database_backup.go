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
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// backupSuffix keeps the backup objects out of the way of the server's own.
const (
	backupSuffix = "-backup"

	// The label value that finds the pods a backup left behind, and the
	// container inside them. Both are read back by latestRehearsal, so the
	// renderer and the reader have to spell them the same way.
	backupComponent = "database-backup"
	backupContainer = "dump"
)

func backupName(db *platformv1alpha1.Database) string { return db.Name + backupSuffix }

// desiredBackupClaim is where archives live.
//
// A volume of its own and not the data volume, so that filling it with backups
// cannot stop the database accepting writes — which would be a backup policy
// that causes the outage it exists to recover from.
func desiredBackupClaim(db *platformv1alpha1.Database) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: backupName(db), Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: db.Spec.Backup.Storage},
			},
		},
	}
	if db.Spec.StorageClass != "" {
		claim.Spec.StorageClassName = ptr.To(db.Spec.StorageClass)
	}
	return claim
}

// RestoreResultPath is where the rehearsal writes its answer for the operator
// to read.
//
// The termination log, and not the backup volume it also writes to. The job
// runs with no service-account token by design — PostgreSQL tooling has no
// business holding one — so it cannot write a status, and the operator cannot
// read a volume mounted by somebody else's pod. What Kubernetes already carries
// between the two is this file: whatever a container writes here appears in its
// terminated state, which the operator is watching anyway.
//
// The default path, stated because the manifest below has to agree with it and
// a default that moves would be a silent loss of the only channel there is.
const RestoreResultPath = "/dev/termination-log"

// backupScript dumps, and then restores what it dumped.
//
// Absorbed from chart/templates/backup-verify-job.yaml, where it was proven
// against a real cluster in CI. Two flags carry the whole proof and neither is
// decoration:
//
// ON_ERROR_STOP, because psql exits zero after every statement in a file has
// failed — without it the rehearsal is a slower way of asserting that the
// archive decompresses, which is the check it exists to replace.
//
// pipefail, because the exit status of "pg_dump | gzip" is gzip's, and gzip
// succeeds on the partial output of a pg_dump that died. That is how a
// truncated dump becomes a backup passing every check made on the file.
//
// The restore runs against a PostgreSQL this job starts in its own container,
// on a socket, in a directory that dies with the pod. Not a throwaway database
// on the live server: a rehearsal that runs on a schedule must not be able to
// damage the thing it rehearses for.
const backupScript = `set -eu
set -o pipefail

until pg_isready -h "$DB_HOST" -U "$POSTGRES_USER" -d "$POSTGRES_DB" -q; do sleep 2; done

count_rows() {
  _rows=0
  for _t in $(eval "$1 -At -c \"SELECT schemaname||'.'||tablename FROM pg_tables
      WHERE schemaname NOT IN ('pg_catalog','information_schema')\""); do
    _n=$(eval "$1 -At -c \"SELECT count(*) FROM $_t\"")
    _rows=$((_rows + _n))
  done
  echo "$_rows"
}

SRC="psql -h $DB_HOST -U $POSTGRES_USER -d $POSTGRES_DB -v ON_ERROR_STOP=1 -q"
SRC_ROWS=$(count_rows "$SRC")

STARTED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUT="/backup/${POSTGRES_DB}-$(date +%Y%m%d-%H%M%S).sql.gz"
pg_dump -h "$DB_HOST" -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  --no-owner --no-privileges | gzip > "$OUT"
gzip -t "$OUT"
SIZE=$(wc -c < "$OUT")
[ "$SIZE" -gt 100 ] || { echo "backup suspiciously small: $SIZE bytes"; exit 1; }

find /backup -name '*.sql.gz' -mtime "+${RETAIN_DAYS}" -delete

if [ "${REHEARSE:-true}" != "true" ]; then
  echo "backed up $OUT ($SIZE bytes); no rehearsal was asked for"
  printf '{"finishedAt":"%s","archive":"%s","archiveBytes":%s,"restored":false}' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$OUT" "$SIZE" > ` + RestoreResultPath + `
  exit 0
fi

export PGDATA=/rehearsal/data
initdb --username=rehearsal --auth=trust >/dev/null
pg_ctl -o "-k /tmp -c listen_addresses=''" -w -l /tmp/pg.log start >/dev/null
trap 'pg_ctl -w stop >/dev/null 2>&1 || true' EXIT

PSQL="psql -h /tmp -U rehearsal -v ON_ERROR_STOP=1 -q"
$PSQL -d postgres -c "CREATE DATABASE rehearsal" >/dev/null
gunzip -c "$OUT" | $PSQL -d rehearsal >/dev/null

TABLES=$($PSQL -At -d rehearsal -c \
  "SELECT schemaname||'.'||tablename FROM pg_tables
    WHERE schemaname NOT IN ('pg_catalog','information_schema')")
[ -n "$TABLES" ] || { echo "the restore produced no tables"; exit 1; }
TABLE_COUNT=$(printf '%s\n' $TABLES | wc -l | tr -d ' ')
ROWS=$(count_rows "$PSQL -d rehearsal")

if [ "$SRC_ROWS" -gt 0 ] && [ "$ROWS" -eq 0 ]; then
  echo "the source holds $SRC_ROWS rows and the restore holds none:"
  echo "the archive is readable and carries no data"
  exit 1
fi

FINISHED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "restored $OUT into a private instance: $TABLE_COUNT tables, $ROWS rows"

# Written twice on purpose. The volume keeps the history; the termination log is
# the only thing the operator can read, and it holds one run.
printf '{"startedAt":"%s","finishedAt":"%s","archive":"%s","archiveBytes":%s,"tables":%s,"rows":%s,"sourceRows":%s,"restored":true}' \
  "$STARTED" "$FINISHED" "$OUT" "$SIZE" "$TABLE_COUNT" "$ROWS" "$SRC_ROWS" > /backup/last-restore.json
cat /backup/last-restore.json > ` + RestoreResultPath + `
`

// desiredBackupCronJob renders the schedule.
func desiredBackupCronJob(db *platformv1alpha1.Database) *batchv1.CronJob {
	rehearse := "true"
	if db.Spec.Backup.Rehearse != nil && !*db.Spec.Backup.Rehearse {
		rehearse = "false"
	}

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: backupName(db), Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: db.Spec.Backup.Schedule,
			// Never two dumps at once. A second one starting while the first
			// still holds the volume is how a backup window turns into a
			// half-written archive.
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: ptr.To(int32(3)),
			FailedJobsHistoryLimit:     ptr.To(int32(3)),
			// A missed window is worth catching up on for ten minutes and not
			// for ever: a controller that was down overnight should take one
			// backup on the way up, not queue every one it slept through.
			StartingDeadlineSeconds: ptr.To(int64(600)),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptr.To(int32(2)),
					ActiveDeadlineSeconds: ptr.To(int64(3600)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
							nameLabel:      postgresPortName,
							instanceLabel:  db.Name,
							componentLabel: backupComponent,
						}},
						Spec: corev1.PodSpec{
							RestartPolicy:                corev1.RestartPolicyOnFailure,
							AutomountServiceAccountToken: ptr.To(false),
							SecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot:   ptr.To(true),
								RunAsUser:      ptr.To(postgresUID),
								RunAsGroup:     ptr.To(postgresUID),
								FSGroup:        ptr.To(postgresUID),
								SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
							},
							Containers: []corev1.Container{{
								Name:  backupContainer,
								Image: db.Spec.Image,
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: ptr.To(false),
									ReadOnlyRootFilesystem:   ptr.To(true),
									Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
								},
								Command: []string{"sh", "-c", backupScript},
								EnvFrom: []corev1.EnvFromSource{{
									SecretRef: &corev1.SecretEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{Name: db.Name},
									},
								}},
								Env: []corev1.EnvVar{
									{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: db.Name},
											Key:                  "POSTGRES_PASSWORD",
										},
									}},
									{Name: "RETAIN_DAYS", Value: strconv.Itoa(int(db.Spec.Backup.RetainDays))},
									{Name: "REHEARSE", Value: rehearse},
								},
								// Stated rather than defaulted, because the
								// script writes to this exact path and a
								// default that moved would silently take away
								// the only channel between this pod and the
								// operator.
								TerminationMessagePath: RestoreResultPath,
								// The message is JSON the operator parses, so it
								// has to be what the container wrote and not
								// the last lines of its log.
								TerminationMessagePolicy: corev1.TerminationMessageReadFile,
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    db.Spec.Resources.CPURequest,
										corev1.ResourceMemory: db.Spec.Resources.MemoryRequest,
									},
									Limits: corev1.ResourceList{
										corev1.ResourceMemory: db.Spec.Resources.MemoryLimit,
									},
								},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "backup", MountPath: "/backup"},
									{Name: tmpVolume, MountPath: tmpPath},
									{Name: "rehearsal", MountPath: "/rehearsal"},
								},
							}},
							Volumes: []corev1.Volume{
								{Name: "backup", VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: backupName(db),
									},
								}},
								{Name: tmpVolume, VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								}},
								// The restore target, and deliberately not the
								// backup volume: a rehearsal that writes its
								// scratch instance beside the archives it is
								// verifying can fill the volume they live on,
								// which turns a failed rehearsal into a failed
								// backup.
								{Name: "rehearsal", VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								}},
							},
						},
					},
				},
			},
		},
	}
}
