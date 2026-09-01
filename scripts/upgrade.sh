#!/usr/bin/env bash
# Move an installed control plane to the version in this checkout.
#
# An install that cannot take a new version is a demo with a long life. This is
# the other half of scripts/install.sh and it is deliberately much smaller: it
# re-applies exactly the two things install.sh applies that carry this
# repository's own code — the custom resource definitions and the control plane
# — and touches nothing else. k3s, the ingress controller, cert-manager, the
# registry and the tenant's own applications are not this script's business,
# and re-applying them to move one Deployment forward is how an upgrade
# acquires failure modes it did not need.
#
#   git pull
#   ./scripts/upgrade.sh
#
# The reference tenant is not upgraded here either, and that is the product
# working rather than a gap: an application moves to a new image through the
# deploy path — a commit, then Argo CD — which is the thing this platform is
# for.
#
# DRY_RUN=1 prints every command it would run, in order, and changes nothing.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MANIFEST="$ROOT/cluster/control-plane.yaml"
CRD_DIR="$ROOT/config/crd/bases"

# Hardcoded, and checked against the manifest below rather than trusted. These
# two names are the address of the thing being upgraded, and a script that
# guesses them wrongly reports "deployment not found" — which reads as "you
# have not installed this" and sends the reader to the wrong page.
NAMESPACE="damga-system"
DEPLOYMENT="damga"
CONTAINER="damga"

IMAGE=""
TIMEOUT="300s"
SKIP_CRDS="no"
DRY_RUN="${DRY_RUN:-0}"

usage() {
  cat <<TXT
damga upgrade — move an installed control plane to this checkout's version.

  git pull && ./scripts/upgrade.sh

Optional:
  --image <ref>    run this image instead of the one cluster/control-plane.yaml
                   names. An install that builds its own control plane passes
                   the same reference it passed to install.sh
                   --control-plane-image
  --skip-crds      do not apply config/crd/bases
  --timeout <dur>  how long to wait for the rollout (default: ${TIMEOUT})
  -h, --help       this

Environment:
  DRY_RUN=1        print every command in order and change nothing
TXT
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image)     IMAGE="${2:-}"; shift 2 ;;
    --skip-crds) SKIP_CRDS="yes"; shift ;;
    --timeout)   TIMEOUT="${2:-}"; shift 2 ;;
    -h|--help)   usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

# The manifest has to be the one this script thinks it is. A rename in
# cluster/control-plane.yaml that this file does not follow would otherwise
# apply the new namespace and then wait for a rollout in the old one, and the
# message for that is "deployments.apps \"damga\" not found" — which names
# neither the rename nor this script.
for expected in "name: ${DEPLOYMENT}" "namespace: ${NAMESPACE}"; do
  grep -q "^[[:space:]]*${expected}\$" "$MANIFEST" || {
    echo "upgrade.sh: cluster/control-plane.yaml no longer contains '${expected}';" >&2
    echo "            this script addresses ${NAMESPACE}/${DEPLOYMENT} and would upgrade nothing" >&2
    exit 1
  }
done

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
note() { printf '    %s\n' "$*"; }

run() {
  if [[ "$DRY_RUN" == "1" ]]; then
    local rendered="" arg
    for arg in "$@"; do
      if [[ "$arg" =~ ^[A-Za-z0-9_./:=@,-]+$ ]]; then
        rendered="$rendered $arg"
      else
        rendered="$rendered '$(printf '%s' "$arg" | sed "s/'/'\\\\''/g")'"
      fi
    done
    printf 'RUN%s\n' "$rendered"
    return 0
  fi
  "$@"
}

# ask reads one thing out of the cluster, and answers the empty string rather
# than failing when it is not there. Every caller is reporting rather than
# deciding, and a `set -e` abort in the middle of a report is worse than a blank.
ask() {
  if [[ "$DRY_RUN" == "1" ]]; then
    printf ''
    return 0
  fi
  kubectl "$@" 2>/dev/null || true
}

running_image() {
  ask -n "$NAMESPACE" get deployment "$DEPLOYMENT" \
    -o "jsonpath={.spec.template.spec.containers[?(@.name=='${CONTAINER}')].image}"
}

# What actually ran, which is not the same question. A tag is resolved by the
# kubelet at pull time and `imagePullPolicy: IfNotPresent` never resolves it
# again, so two nodes that pulled `:1.0.0` on different days run different code
# while the Deployment says one thing. This is the only field that can tell.
running_digest() {
  ask -n "$NAMESPACE" get pod -l app.kubernetes.io/name="$DEPLOYMENT" \
    -o "jsonpath={.items[*].status.containerStatuses[?(@.name=='${CONTAINER}')].imageID}"
}

claim_uid() {
  ask -n "$NAMESPACE" get pvc damga-data -o jsonpath='{.metadata.uid}'
}

# render writes the manifest that will be applied.
#
# The image is substituted here rather than patched after the apply, and the
# difference is measurable rather than stylistic: applying the manifest first
# would set the image to the reference in git, which on an install that builds
# its own control plane is one no node can pull. With `Recreate` the old pod is
# gone before the replacement fails, so the correction that follows arrives
# after the outage it caused — and the outage would be this script's, not the
# upgrade's.
render() {
  if [[ -z "$IMAGE" ]]; then
    cat "$MANIFEST"
    return 0
  fi
  local found
  found=$(grep -c "^[[:space:]]*image:[[:space:]]*.*damga-control-plane" "$MANIFEST" || true)
  if [[ "$found" != "1" ]]; then
    echo "upgrade.sh: expected exactly one control-plane image line in cluster/control-plane.yaml, found ${found}" >&2
    exit 1
  fi
  sed "s|^\([[:space:]]*image:[[:space:]]*\).*damga-control-plane.*\$|\1${IMAGE}|" "$MANIFEST"
}

# applied_image is the reference the apply below will carry, from whichever of
# the two places decides it.
applied_image() {
  if [[ -n "$IMAGE" ]]; then
    printf '%s' "$IMAGE"
    return 0
  fi
  sed -n 's|^[[:space:]]*image:[[:space:]]*\(.*damga-control-plane.*\)$|\1|p' "$MANIFEST" | head -1
}

apply_control_plane() {
  # Rendered in both modes, so the guard inside render() runs during a dry run
  # too. A manifest this script can no longer rewrite is something to find out
  # while looking at the plan, not while the old pod is already gone.
  local rendered
  rendered="$(render)"
  if [[ "$DRY_RUN" == "1" ]]; then
    printf 'RUN kubectl apply -f - < cluster/control-plane.yaml (image=%s)\n' "$(applied_image)"
    return 0
  fi
  printf '%s\n' "$rendered" | kubectl apply -f -
}

# ------------------------------------------------------------------ upgrade

BEFORE_IMAGE="$(running_image)"
BEFORE_DIGEST="$(running_digest)"
BEFORE_CLAIM="$(claim_uid)"

if [[ "$DRY_RUN" != "1" && -z "$BEFORE_IMAGE" ]]; then
  echo "upgrade.sh: there is no ${DEPLOYMENT} deployment in ${NAMESPACE} to upgrade;" >&2
  echo "            this installs nothing — run scripts/install.sh first" >&2
  exit 1
fi

printf 'FROM image=%s digest=%s\n' "${BEFORE_IMAGE:-<none>}" "${BEFORE_DIGEST:-<none>}"

step "the types the platform reconciles"
if [[ "$SKIP_CRDS" == "yes" ]]; then
  note "skipped"
else
  # Before the control plane, for the reason install.sh gives at length: a
  # ResourceQuota that counts a type the API server has not heard of refuses
  # every create in that namespace. Here it buys a second thing — a release
  # that adds a field to a type must have the field before the binary that
  # writes it, or the API server silently prunes it out of every object the new
  # control plane creates.
  run kubectl apply -f "$CRD_DIR"
fi

step "the control plane"
if [[ -n "$IMAGE" ]]; then
  note "image override: ${IMAGE}"
fi
apply_control_plane
run kubectl -n "$NAMESPACE" rollout status "deployment/${DEPLOYMENT}" --timeout="$TIMEOUT" || {
  # Diagnostics on the failure rather than after it. A rollout that times out
  # says "0 of 1 updated replicas are available" and nothing else, which is
  # equally true of a pod that will not schedule, will not pull, will not start
  # and will not pass a probe — four problems with four different fixes.
  echo "::group::why the upgraded control plane did not start" >&2
  kubectl -n "$NAMESPACE" get pod,pvc -o wide >&2 || true
  kubectl -n "$NAMESPACE" describe pod -l app.kubernetes.io/name="$DEPLOYMENT" | tail -40 >&2 || true
  kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name="$DEPLOYMENT" --tail=60 >&2 || true
  kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name="$DEPLOYMENT" --previous --tail=60 >&2 || true
  echo "::endgroup::" >&2
  exit 1
}

if [[ "$DRY_RUN" == "1" ]]; then
  printf '\nDRY_RUN: nothing above was executed.\n'
  exit 0
fi

AFTER_IMAGE="$(running_image)"
AFTER_DIGEST="$(running_digest)"
AFTER_CLAIM="$(claim_uid)"

printf 'TO   image=%s digest=%s\n' "${AFTER_IMAGE:-<none>}" "${AFTER_DIGEST:-<none>}"

# The claim is the install. Accounts, sessions, placements and every deploy
# record are one SQLite file on it, so an upgrade that replaces the volume has
# not upgraded anything — it has installed a second, empty platform wearing the
# first one's name, and every symptom of that appears later as "my apps are
# gone" rather than here as a failed upgrade.
if [[ -n "$BEFORE_CLAIM" && "$BEFORE_CLAIM" != "$AFTER_CLAIM" ]]; then
  echo "upgrade.sh: the data volume was replaced during the upgrade" >&2
  echo "            (claim ${BEFORE_CLAIM} -> ${AFTER_CLAIM:-<none>});" >&2
  echo "            the control plane is running against an empty database" >&2
  exit 1
fi

step "what moved"
if [[ "$BEFORE_IMAGE" == "$AFTER_IMAGE" && "$BEFORE_DIGEST" == "$AFTER_DIGEST" ]]; then
  # Said out loud, because `kubectl apply` of an unchanged pod template is a
  # no-op and a rollout status against an unchanged Deployment succeeds
  # immediately. Both report success, and an upgrade that reports success and
  # moves nothing is the failure this message exists to name.
  note "nothing moved: already running ${AFTER_IMAGE}"
  note "if a new version was expected, the reference in cluster/control-plane.yaml"
  note "is a tag that has not changed — pin the digest, or pass --image"
  exit 0
fi
note "image  ${BEFORE_IMAGE:-<none>} -> ${AFTER_IMAGE}"
note "digest ${BEFORE_DIGEST:-<none>} -> ${AFTER_DIGEST}"
note "data volume kept: ${AFTER_CLAIM}"
