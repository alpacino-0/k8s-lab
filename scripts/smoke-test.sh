#!/usr/bin/env bash
# End-to-end checks against a deployed release. Used locally and in CI.
#
# Traffic goes through `kubectl port-forward`, which the kubelet proxies
# straight into the pod's network namespace. Arbitrary in-cluster pods cannot
# reach the app at all — that is the NetworkPolicy doing its job, and it is
# verified explicitly below.
set -uo pipefail

NAMESPACE="${NAMESPACE:-k8s-lab}"
RELEASE="${RELEASE:-app}"
SVC="${RELEASE}-k8s-lab-app"
WEB_SVC="${RELEASE}-k8s-lab-app-web"
PORT="${PORT:-18080}"
WEB_PORT="${WEB_PORT:-18090}"
METRICS_PORT="${METRICS_PORT:-18091}"
BASE="http://127.0.0.1:${PORT}"
FAILED=0
PF_PID=""

WEB_PF_PID=""
METRICS_PF_PID=""
cleanup() {
  for pid in "$PF_PID" "$WEB_PF_PID" "$METRICS_PF_PID"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null
  done
}
trap cleanup EXIT

pass() { printf '  \033[0;32mPASS\033[0m  %s\n' "$1"; }
fail() { printf '  \033[0;31mFAIL\033[0m  %s\n' "$1"; FAILED=1; }
check() { local name="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name"; fi; }

echo "smoke testing release '$RELEASE' in namespace '$NAMESPACE'"

kubectl -n "$NAMESPACE" rollout status "deployment/${SVC}" --timeout=300s >/dev/null \
  && pass "deployment rolled out" || fail "deployment rolled out"

kubectl -n "$NAMESPACE" port-forward "svc/${SVC}" "${PORT}:80" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do curl -sf "${BASE}/healthz" >/dev/null 2>&1 && break; sleep 1; done

check "liveness responds"        sh -c "curl -sf ${BASE}/healthz | grep -q ok"
check "readiness responds"       sh -c "curl -sf ${BASE}/readyz  | grep -q ok"
check "index responds"           sh -c "curl -sf ${BASE}/ | grep -q 'Served by'"
check "app port does not serve metrics" \
  sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' ${BASE}/metrics)\" = 404 ]"
# Telemetry lives on its own port; the public ingress never routes to it.
kubectl -n "$NAMESPACE" port-forward "svc/${SVC}" "${METRICS_PORT}:9090" >/dev/null 2>&1 &
METRICS_PF_PID=$!
METRICS="http://127.0.0.1:${METRICS_PORT}"
for _ in $(seq 1 30); do curl -sf "${METRICS}/metrics" >/dev/null 2>&1 && break; sleep 1; done

check "telemetry port exposes metrics" \
  sh -c "curl -sf ${METRICS}/metrics | grep -q http_requests_total"
check "database gauge is healthy" \
  sh -c "curl -sf ${METRICS}/metrics | grep -qE '^database_up\{[^}]*\} 1'"
check "telemetry port serves nothing else" \
  sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' ${METRICS}/notes)\" = 404 ]"

check "note can be written"      sh -c "curl -sf -X POST -H 'Content-Type: application/json' \
                                   -d '{\"text\":\"smoke-test\"}' ${BASE}/notes | grep -q smoke-test"
check "note can be read back"    sh -c "curl -sf ${BASE}/notes | grep -q smoke-test"
check "empty note is rejected"   sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -X POST \
                                   -H 'Content-Type: application/json' -d '{\"text\":\"\"}' ${BASE}/notes)\" = 400 ]"
check "unknown route returns 404" sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' ${BASE}/nope)\" = 404 ]"
check "demo endpoint is disabled" sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' ${BASE}/burn)\" = 404 ]"
check "no credentials in /config" sh -c "! curl -s ${BASE}/config | grep -qiE 'password|secret|token'"

# Security posture, read straight from the running pod spec.
POD=$(kubectl -n "$NAMESPACE" get pod -l "app.kubernetes.io/name=k8s-lab-app" \
        --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
q() { kubectl -n "$NAMESPACE" get pod "$POD" -o jsonpath="$1" 2>/dev/null; }
check "runs as non-root"          sh -c "[ \"$(q '{.spec.securityContext.runAsNonRoot}')\" = true ]"
check "root filesystem read-only" sh -c "[ \"$(q '{.spec.containers[0].securityContext.readOnlyRootFilesystem}')\" = true ]"
check "all capabilities dropped"  sh -c "[ \"$(q '{.spec.containers[0].securityContext.capabilities.drop[0]}')\" = ALL ]"
check "no service-account token"  sh -c "[ \"$(q '{.spec.automountServiceAccountToken}')\" = false ]"
check "effective uid is not 0"    sh -c "[ \"$(kubectl -n $NAMESPACE exec $POD -- id -u 2>/dev/null)\" != 0 ]"

# ---- interface tier -------------------------------------------------------
kubectl -n "$NAMESPACE" rollout status "deployment/${WEB_SVC}" --timeout=300s >/dev/null \
  && pass "interface rolled out" || fail "interface rolled out"

kubectl -n "$NAMESPACE" port-forward "svc/${WEB_SVC}" "${WEB_PORT}:80" >/dev/null 2>&1 &
WEB_PF_PID=$!
WEB="http://127.0.0.1:${WEB_PORT}"
for _ in $(seq 1 30); do curl -sf "${WEB}/healthz" >/dev/null 2>&1 && break; sleep 1; done

check "interface serves the app shell" sh -c "curl -sf ${WEB}/ | grep -q '<div id=\"root\"'"
check "unknown paths return the shell" sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' ${WEB}/any/deep/route)\" = 200 ]"
check "security headers are present"   sh -c "curl -s -D - -o /dev/null ${WEB}/ | grep -qi 'content-security-policy'"
check "interface runs as non-root"     sh -c "[ \"\$(kubectl -n $NAMESPACE get pod -l app.kubernetes.io/name=k8s-lab-web \
                                          -o jsonpath='{.items[0].spec.securityContext.runAsNonRoot}')\" = true ]"

# NetworkPolicy: an unrelated pod must NOT be able to reach the app or the database.
echo "  ...verifying network isolation (takes ~20s)"
BLOCKED=$(kubectl -n "$NAMESPACE" run "netpol-$RANDOM" --rm -i --restart=Never --quiet \
  --image=curlimages/curl:8.11.1 --command -- \
  curl -s -m 8 -o /dev/null -w '%{http_code}' "http://${SVC}/healthz" 2>/dev/null)
[[ "$BLOCKED" == "000" || -z "$BLOCKED" ]] \
  && pass "unauthorized pod cannot reach the app" \
  || fail "unauthorized pod reached the app (got HTTP $BLOCKED) — NetworkPolicy not enforced"

echo
[[ "$FAILED" -eq 0 ]] && echo "all checks passed" || echo "some checks failed"
exit "$FAILED"
