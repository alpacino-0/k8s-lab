#!/usr/bin/env bash
# Proves each admission rule rejects what it is meant to reject.
#
# A policy that is installed but never tested is decoration: it might match
# nothing, it might be bound to the wrong namespace, its expression might
# always evaluate true. Each case below is a manifest that must fail, plus one
# that must pass — a rule that rejects everything is as broken as one that
# rejects nothing.
set -uo pipefail
NAMESPACE="${NAMESPACE:-k8s-lab}"
IMAGE="${IMAGE:-k8s-lab-app:1.0.0}"
FAILED=0
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass() { printf '  \033[0;32mPASS\033[0m  %s\n' "$1"; }
fail() { printf '  \033[0;31mFAIL\033[0m  %s\n' "$1"; FAILED=1; }

# Written to a file rather than piped: a pipeline runs its last stage in a
# subshell, so a failure counter incremented there never reaches this script
# and it would always report success.
apply_dry() { kubectl -n "$NAMESPACE" apply --dry-run=server -f "$WORK/manifest.yaml" 2>&1; }

must_reject() {
  local what="$1" expect="$2" out
  out=$(apply_dry)
  if [[ "$out" == *"$expect"* ]]; then
    pass "$what"
  else
    fail "$what — expected a rejection mentioning '$expect', got: ${out:0:150}"
  fi
}

must_admit() {
  local what="$1" out
  out=$(apply_dry)
  if [[ "$out" == *"(server dry run)"* ]]; then
    pass "$what"
  else
    fail "$what — expected admission, got: ${out:0:150}"
  fi
}

# One compliant baseline; every case below breaks exactly one thing.
write_pod() {
  local image="${1:-$IMAGE}" readonly_fs="${2:-true}" resources="${3:-full}"
  {
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: policy-probe
spec:
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: c
      image: ${image}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: ${readonly_fs}
        capabilities: { drop: ["ALL"] }
EOF
    if [[ "$resources" == "full" ]]; then
      cat <<'EOF'
      resources:
        requests: { cpu: 10m, memory: 16Mi }
        limits: { memory: 32Mi }
EOF
    fi
    cat <<'EOF'
      readinessProbe: { httpGet: { path: /healthz, port: 3000 } }
      livenessProbe: { httpGet: { path: /healthz, port: 3000 } }
EOF
  } > "$WORK/manifest.yaml"
}

echo "admission policy checks in namespace '$NAMESPACE'"

write_pod;                             must_admit  "a compliant pod is admitted"
write_pod "$IMAGE" true none;          must_reject "a pod without resource bounds is rejected" "requests and a memory limit"
write_pod "alpine:latest";             must_reject "a :latest image is rejected" "explicit version"
write_pod "quay.io/someone/thing:1.0"; must_reject "an unknown registry is rejected" "must come from"
write_pod "$IMAGE" false;              must_reject "a writable root filesystem is rejected" "read-only root filesystem"

# Pod Security Admission runs before any of the above.
cat > "$WORK/manifest.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: policy-probe-root
spec:
  containers:
    - name: c
      image: ${IMAGE}
      securityContext: { runAsUser: 0 }
EOF
must_reject "a root container is rejected" "PodSecurity"

cat > "$WORK/manifest.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: policy-probe-hostpath
spec:
  containers:
    - name: c
      image: ${IMAGE}
      volumeMounts: [{ name: h, mountPath: /host }]
  volumes:
    - name: h
      hostPath: { path: / }
EOF
must_reject "a hostPath volume is rejected" "PodSecurity"

# Controllers are checked too, so a bad Deployment fails at apply time instead
# of becoming a Deployment that silently never produces a pod.
cat > "$WORK/manifest.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: policy-probe-deploy
spec:
  replicas: 1
  selector: { matchLabels: { app: probe } }
  template:
    metadata: { labels: { app: probe } }
    spec:
      automountServiceAccountToken: false
      containers:
        - name: c
          image: ${IMAGE}
          securityContext: { readOnlyRootFilesystem: true }
EOF
must_reject "a bad Deployment fails at apply time" "ValidatingAdmissionPolicy"

echo
if [[ "$FAILED" -eq 0 ]]; then echo "all policy checks passed"; else echo "some policy checks failed"; fi
exit "$FAILED"
