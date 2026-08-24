#!/usr/bin/env bash
# End-to-end checks against a deployed release. Used locally and in CI.
#
# Traffic goes through `kubectl port-forward`, which the kubelet proxies
# straight into the pod's network namespace. Arbitrary in-cluster pods cannot
# reach the app at all — that is the NetworkPolicy doing its job, and it is
# verified explicitly below.
set -uo pipefail

NAMESPACE="${NAMESPACE:-damga}"
RELEASE="${RELEASE:-app}"
SVC="${RELEASE}-damga-app"
PORT="${PORT:-18080}"
METRICS_PORT="${METRICS_PORT:-18091}"
BASE="http://127.0.0.1:${PORT}"
FAILED=0
PF_PID=""
METRICS_PF_PID=""
cleanup() {
  for pid in "$PF_PID" "$METRICS_PF_PID"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null
  done
  kubectl delete namespace "${PROBE_NS:-damga-probes}" --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

pass() { printf '  \033[0;32mPASS\033[0m  %s\n' "$1"; }

# Probes run in a namespace of their own.
#
# The application namespace deliberately refuses anything that is not a
# compliant, pinned, probed workload from a known registry — including a
# throwaway curl pod. That is the policy working, but it also means an
# isolation probe cannot live there. Running it elsewhere is the more faithful
# test anyway: an unrelated workload is in another namespace, not this one.
#
# Each probe prints a sentinel because a pod refused at admission and a pod
# blocked by the NetworkPolicy look identical from the outside — one of them
# proves isolation, the other proves nothing.
PROBE_NS="${PROBE_NS:-damga-probes}"

# A namespace deleted at the end of the previous run can still be Terminating,
# and pods cannot be created in one. Wait it out rather than racing it.
ensure_probe_namespace() {
  for _ in $(seq 1 60); do
    local phase
    phase=$(kubectl get namespace "$PROBE_NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case "$phase" in
      Active) return 0 ;;
      Terminating) sleep 2 ;;
      *) kubectl create namespace "$PROBE_NS" >/dev/null 2>&1 && sleep 1 ;;
    esac
  done
  return 1
}
ensure_probe_namespace || { echo "could not prepare the probe namespace"; exit 1; }
PROBE_SPEC='{"apiVersion":"v1","spec":{"automountServiceAccountToken":false,
"securityContext":{"runAsNonRoot":true,"runAsUser":1000,
"seccompProfile":{"type":"RuntimeDefault"}},
"containers":[{"name":"probe","image":"IMAGE_PLACEHOLDER",
"securityContext":{"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,
"capabilities":{"drop":["ALL"]}},
"resources":{"requests":{"cpu":"10m","memory":"16Mi"},"limits":{"memory":"32Mi"}},
"command":COMMAND_PLACEHOLDER}]}}'

# run_probe <image> <json command array> -> stdout of the pod
#
# Creates the pod, waits for it to finish, then reads its logs. Attaching with
# `kubectl run -i` looked simpler and lost the output whenever the image had to
# be pulled first: the probe reported "never ran" for a pod that ran fine.
run_probe() {
  local image="$1" command="$2" name="probe-$RANDOM"
  local overrides="${PROBE_SPEC//IMAGE_PLACEHOLDER/$image}"
  overrides="${overrides//COMMAND_PLACEHOLDER/$command}"

  if ! kubectl -n "$PROBE_NS" run "$name" --restart=Never --quiet \
        --image="$image" --overrides="$overrides" >/dev/null 2>&1; then
    echo "probe pod was refused at admission"
    return
  fi

  local phase=""
  for _ in $(seq 1 90); do
    phase=$(kubectl -n "$PROBE_NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [[ "$phase" == "Succeeded" || "$phase" == "Failed" ]] && break
    sleep 2
  done

  if [[ "$phase" != "Succeeded" && "$phase" != "Failed" ]]; then
    echo "probe pod never finished (phase=${phase:-unknown}): $(kubectl -n "$PROBE_NS" get pod "$name" \
      -o jsonpath='{.status.containerStatuses[0].state}' 2>/dev/null)"
  else
    kubectl -n "$PROBE_NS" logs "$name" 2>&1
  fi
  kubectl -n "$PROBE_NS" delete pod "$name" --wait=false >/dev/null 2>&1
}
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

# Notes are scoped to a visitor cookie, so write and read must share one jar.
JAR=$(mktemp)
curl -sf -c "$JAR" -o /dev/null "${BASE}/notes" || true

check "note can be written"      sh -c "curl -sf -b '$JAR' -X POST -H 'Content-Type: application/json' \
                                   -d '{\"text\":\"smoke-test\"}' ${BASE}/notes | grep -q smoke-test"
check "note can be read back"    sh -c "curl -sf -b '$JAR' ${BASE}/notes | grep -q smoke-test"
check "empty note is rejected"   sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -b '$JAR' -X POST \
                                   -H 'Content-Type: application/json' -d '{\"text\":\"\"}' ${BASE}/notes)\" = 400 ]"
check "unknown route returns 404" sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' ${BASE}/nope)\" = 404 ]"
check "demo endpoint is disabled" sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' ${BASE}/burn)\" = 404 ]"
check "no credentials in /config" sh -c "! curl -s ${BASE}/config | grep -qiE 'password|secret|token'"

# ---- public-exposure guard rails ------------------------------------------
# Notes are scoped to an anonymous visitor cookie: no signup, but no shared
# graffiti wall either.
JAR_A=$(mktemp); JAR_B=$(mktemp)
curl -sf -c "$JAR_A" -o /dev/null "${BASE}/notes" || true
curl -sf -c "$JAR_B" -o /dev/null "${BASE}/notes" || true

check "visitor cookie is issued" sh -c "grep -q visitor '$JAR_A'"

NOTE_ID=$(curl -sf -b "$JAR_A" -X POST -H 'Content-Type: application/json' \
  -d '{"text":"isolation probe"}' "${BASE}/notes" \
  | sed -n 's/.*"id":\([0-9]*\).*/\1/p')

check "a second visitor cannot see the first one's notes" \
  sh -c "! curl -sf -b '$JAR_B' ${BASE}/notes | grep -q 'isolation probe'"
check "a second visitor cannot delete them" \
  sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -b '$JAR_B' -X DELETE ${BASE}/notes/${NOTE_ID})\" = 404 ]"
check "the owner can delete their own note" \
  sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -b '$JAR_A' -X DELETE ${BASE}/notes/${NOTE_ID})\" = 200 ]"
check "rate limit headers are returned" \
  sh -c "curl -s -D - -o /dev/null -b '$JAR_A' ${BASE}/notes | grep -qi 'ratelimit-remaining'"
check "oversized notes are rejected" \
  sh -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' -b '$JAR_A' -X POST \
     -H 'Content-Type: application/json' \
     -d \"{\\\"text\\\":\\\"\$(head -c 600 /dev/zero | tr '\\0' 'x')\\\"}\" ${BASE}/notes)\" = 400 ]"
rm -f "$JAR_A" "$JAR_B"

# ---- shared state ----------------------------------------------------------
if kubectl -n "$NAMESPACE" get deploy "${RELEASE}-redis" >/dev/null 2>&1; then
  kubectl -n "$NAMESPACE" port-forward "svc/${SVC}" "${METRICS_PORT}:9090" >/dev/null 2>&1 &
  check "shared store is reachable from the app" \
    sh -c "curl -sf ${METRICS}/metrics | grep -qE '^redis_up\{[^}]*\} 1'"
  check "the rate limiter is using the shared store" \
    sh -c "curl -sf ${METRICS}/metrics | grep -q 'rate_limiter_decisions_total{backend=\"redis\"'"
  check "the note count is served from cache" \
    sh -c "curl -sf -b '$JAR' ${BASE}/stats >/dev/null; curl -sf -b '$JAR' ${BASE}/stats | grep -q '\"cached\":true'"
  # Redis does not speak HTTP, so a status code proves nothing: curl reports
  # 000 whether the connection was refused or merely produced no HTTP response.
  # What distinguishes the two is whether the TCP connection was established at
  # all, which curl states in its verbose output.
  REDIS_OUT=$(run_probe "curlimages/curl:8.11.1" \
    '["sh","-c","if curl -sv -m 8 telnet://'"${RELEASE}-redis.${NAMESPACE}"':6379 2>\u00261 | grep -q \"Connected to\"; then echo PROBE_RAN:connected; else echo PROBE_RAN:blocked; fi"]')
  if [[ "$REDIS_OUT" != *"PROBE_RAN:"* ]]; then
    fail "the redis isolation probe never ran: ${REDIS_OUT:0:140}"
  elif [[ "$REDIS_OUT" == *"PROBE_RAN:blocked"* ]]; then
    pass "redis refuses connections from unrelated pods"
  else
    fail "an unrelated pod opened a connection to redis — NetworkPolicy not enforced"
  fi
fi

# Security posture, read straight from the running pod spec.
POD=$(kubectl -n "$NAMESPACE" get pod -l "app.kubernetes.io/name=damga-app" \
        --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
q() { kubectl -n "$NAMESPACE" get pod "$POD" -o jsonpath="$1" 2>/dev/null; }
check "runs as non-root"          sh -c "[ \"$(q '{.spec.securityContext.runAsNonRoot}')\" = true ]"
check "root filesystem read-only" sh -c "[ \"$(q '{.spec.containers[0].securityContext.readOnlyRootFilesystem}')\" = true ]"
check "all capabilities dropped"  sh -c "[ \"$(q '{.spec.containers[0].securityContext.capabilities.drop[0]}')\" = ALL ]"
check "no service-account token"  sh -c "[ \"$(q '{.spec.automountServiceAccountToken}')\" = false ]"
check "effective uid is not 0"    sh -c "[ \"$(kubectl -n $NAMESPACE exec $POD -- id -u 2>/dev/null)\" != 0 ]"

# NetworkPolicy: an unrelated pod must NOT be able to reach the app or the
# database. The sentinel proves the probe actually ran; without it a pod that
# was refused at admission looks identical to one that was blocked by the
# network policy.
echo "  ...verifying network isolation (takes ~20s)"
NETPOL_OUT=$(run_probe "curlimages/curl:8.11.1" \
  '["sh","-c","code=$(curl -s -m 8 -o /dev/null -w %{http_code} http://'"${SVC}.${NAMESPACE}"'/healthz); echo PROBE_RAN:$code"]')

if [[ "$NETPOL_OUT" != *"PROBE_RAN:"* ]]; then
  fail "the isolation probe never ran, so nothing was proved: ${NETPOL_OUT:0:140}"
elif [[ "$NETPOL_OUT" == *"PROBE_RAN:000"* ]]; then
  pass "unauthorized pod cannot reach the app"
else
  fail "unauthorized pod reached the app (${NETPOL_OUT##*PROBE_RAN:}) — NetworkPolicy not enforced"
fi

echo
[[ "$FAILED" -eq 0 ]] && echo "all checks passed" || echo "some checks failed"
exit "$FAILED"
