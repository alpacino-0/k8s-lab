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
PORT="${PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
FAILED=0
PF_PID=""

cleanup() { [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null; }
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
check "metrics are exposed"      sh -c "curl -sf ${BASE}/metrics | grep -q http_requests_total"
check "database gauge is healthy" sh -c "curl -sf ${BASE}/metrics | grep -q '^database_up{[^}]*} 1'"
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
