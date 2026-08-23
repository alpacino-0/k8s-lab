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

# ValidatingAdmissionPolicy bindings are not effective the instant they are
# applied — the API server has to pick them up. Locally that delay is invisible
# because the policies were applied minutes earlier; in CI, where apply and
# test are seconds apart, four checks silently passed against a policy that was
# not yet enforcing. Poll until a manifest that must be rejected actually is.
wait_for_enforcement() {
  cat > "$WORK/manifest.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: policy-readiness
spec:
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: c
      image: ${IMAGE}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: { drop: ["ALL"] }
      readinessProbe: { httpGet: { path: /healthz, port: 3000 } }
      livenessProbe: { httpGet: { path: /healthz, port: 3000 } }
EOF
  for _ in $(seq 1 60); do
    if [[ "$(apply_dry)" == *"requests and a memory limit"* ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

echo "admission policy checks in namespace '$NAMESPACE'"

if wait_for_enforcement; then
  pass "policies are being enforced"
else
  fail "policies never took effect — every check below would be meaningless"
  echo; echo "some policy checks failed"; exit 1
fi

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

# ---- image signatures -------------------------------------------------------
# Only when Kyverno is installed: this is the one rule that needs it, because
# verifying a signature means reaching a registry and a transparency log, which
# the built-in admission engine cannot do.
if kubectl get clusterpolicy verify-image-signatures >/dev/null 2>&1; then
  signed_pod() {
    sed "s|IMAGE_REF|$1|; s|POD_NAME|$2|" > "$WORK/manifest.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: POD_NAME
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: c
      image: IMAGE_REF
      command: ["node", "-e", "0"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: { drop: ["ALL"] }
      resources:
        requests: { cpu: 10m, memory: 32Mi }
        limits: { memory: 64Mi }
      readinessProbe: { exec: { command: ["true"] } }
      livenessProbe: { exec: { command: ["true"] } }
EOF
  }

  signed_pod "${SIGNED_IMAGE:-ghcr.io/alpacino-0/k8s-lab:1.0.0}" sig-ok
  must_admit "an image signed by the pipeline is admitted"

  # An image that exists but was published before signing was added. Testing
  # with a tag that does not exist would prove nothing: the rejection would say
  # "manifest unknown", which is a different failure wearing the same colour.
  if [[ -n "${UNSIGNED_IMAGE:-}" ]]; then
    signed_pod "$UNSIGNED_IMAGE" sig-unsigned
    must_reject "an unsigned image is rejected" "no signatures found"
  else
    echo "  ....  skipping the unsigned case: set UNSIGNED_IMAGE to an image that exists but is not signed"
  fi
fi

echo
if [[ "$FAILED" -eq 0 ]]; then echo "all policy checks passed"; else echo "some policy checks failed"; fi
exit "$FAILED"
