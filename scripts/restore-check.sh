#!/usr/bin/env bash
# Asserts what the restore rehearsal found, not that it ran.
#
# The chart's hook already fails the install when the rehearsal fails, so
# "it ran" is not worth a step. What is worth asserting is the finding: a
# restore that produces an empty database is still a restore, and "the backup
# was restored" should not be sayable about one.
#
# MIN_ROWS is the point. Straight after an install the database is empty and
# zero rows is the correct answer — asserting otherwise fails a working system.
# After something has written to it, zero rows means the archive carries a
# schema and no data, which is precisely the failure gzip -t cannot see. So the
# caller says which of the two situations it is in, and a run that gets it wrong
# is a wrong expectation rather than a tolerance quietly widened to fit.
set -uo pipefail

NAMESPACE="${NAMESPACE:-damga}"
JOB="${JOB:-app-postgres-backup-verify}"
MIN_ROWS="${MIN_ROWS:-0}"

log=$(kubectl -n "$NAMESPACE" logs "job/$JOB" --tail=-1 2>/dev/null)
if [ -z "$log" ]; then
  echo "::error::no log from $JOB; the rehearsal did not run"
  exit 1
fi
echo "$log"

line=$(printf '%s\n' "$log" | grep '^restored ' || true)
if [ -z "$line" ]; then
  echo "::error::nothing in the log says a backup was restored"
  exit 1
fi

tables=$(printf '%s' "$line" | sed -n 's/.*: \([0-9][0-9]*\) tables.*/\1/p')
rows=$(printf '%s' "$line" | sed -n 's/.*, \([0-9][0-9]*\) rows.*/\1/p')
echo "rehearsal: ${tables:-?} tables, ${rows:-?} rows (expected at least $MIN_ROWS)"

if [ "${tables:-0}" -lt 1 ]; then
  echo "::error::the restore produced no tables, so the archive holds no schema"
  exit 1
fi
if [ "${rows:-0}" -lt "$MIN_ROWS" ]; then
  echo "::error::the restore produced ${rows:-0} rows and something had written to \
this database, so the archive carries a schema and no data — which is exactly \
what a readable-but-truncated dump looks like"
  exit 1
fi
