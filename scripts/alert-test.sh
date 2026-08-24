#!/usr/bin/env bash
# Proves the alerting path does what the configuration claims.
#
# Two separate things, because they fail separately:
#
#   1. A rule that fires reaches Alertmanager. This breaks the application for
#      real and waits, because the only honest way to know an alert arrives is
#      to cause one. A rule that matches nothing, a ServiceMonitor that selects
#      nothing and an Alertmanager nothing routes to all look identical from a
#      dashboard.
#
#   2. A critical suppresses the warnings describing the same failure. That is
#      configuration rather than metrics, so it is exercised by handing
#      Alertmanager two alerts directly — no waiting on a real outage that
#      happens to light both.
set -uo pipefail

NAMESPACE="${NAMESPACE:-damga}"
MON_NS="${MON_NS:-monitoring}"
DEPLOY="${DEPLOY:-app-damga-app}"
AM_SVC="${AM_SVC:-monitoring-kube-prometheus-alertmanager}"
PORT="${PORT:-9093}"
FAILED=0

pass() { printf '  \033[0;32mPASS\033[0m  %s\n' "$1"; }
fail() { printf '  \033[0;31mFAIL\033[0m  %s\n' "$1"; FAILED=1; }
note() { printf '  ....  %s\n' "$1"; }

cleanup() {
  [[ -n "${PF_PID:-}" ]] && kill "$PF_PID" 2>/dev/null
  if [[ -n "${ORIGINAL:-}" ]]; then
    kubectl -n "$NAMESPACE" scale "deploy/$DEPLOY" --replicas="$ORIGINAL" >/dev/null 2>&1
  fi
}
trap cleanup EXIT

kubectl -n "$MON_NS" get svc "$AM_SVC" >/dev/null 2>&1 || {
  echo "no Alertmanager service '$AM_SVC' in '$MON_NS' — run: make monitoring" >&2
  exit 1
}

# Without the rule the first check would wait four minutes and then report that
# the alert never arrived, which is true and useless: nothing was ever asked to
# fire. The rules ship disabled because they need CRDs that only exist after the
# monitoring stack is installed.
kubectl -n "$NAMESPACE" get prometheusrule "$DEPLOY" >/dev/null 2>&1 || {
  echo "no PrometheusRule '$DEPLOY' in '$NAMESPACE' — the rules are off in this release" >&2
  echo "turn them on:  helm upgrade app chart -n $NAMESPACE --reuse-values \\" >&2
  echo "                 --set prometheusRule.enabled=true \\" >&2
  echo "                 --set serviceMonitor.enabled=true \\" >&2
  echo "                 --set serviceMonitor.labels.release=monitoring" >&2
  exit 1
}

kubectl -n "$MON_NS" port-forward "svc/$AM_SVC" "$PORT:9093" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:$PORT/-/ready" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "http://127.0.0.1:$PORT/-/ready" >/dev/null 2>&1 || { echo "Alertmanager did not become reachable" >&2; exit 1; }

echo "alerting checks against '$AM_SVC'"

# ---------------------------------------------------------------- 1. delivery
ORIGINAL=$(kubectl -n "$NAMESPACE" get "deploy/$DEPLOY" -o jsonpath='{.spec.replicas}')
note "taking $DEPLOY to zero replicas; NoReadyReplicas fires after 1m"
kubectl -n "$NAMESPACE" scale "deploy/$DEPLOY" --replicas=0 >/dev/null

found=0
for _ in $(seq 1 48); do   # up to 4 minutes
  if curl -sf "http://127.0.0.1:$PORT/api/v2/alerts" \
     | jq -e '.[] | select(.labels.alertname == "NoReadyReplicas")' >/dev/null 2>&1; then
    found=1; break
  fi
  sleep 5
done
[[ "$found" -eq 1 ]] \
  && pass "a rule that fires reaches Alertmanager" \
  || fail "NoReadyReplicas never arrived — the rule, the scrape or the route is broken"

kubectl -n "$NAMESPACE" scale "deploy/$DEPLOY" --replicas="$ORIGINAL" >/dev/null
ORIGINAL=""
kubectl -n "$NAMESPACE" rollout status "deploy/$DEPLOY" --timeout=120s >/dev/null 2>&1

# -------------------------------------------------------------- 2. inhibition
job="alert-test-$$"
ends=$(date -u -v+10M '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '+10 minutes' '+%Y-%m-%dT%H:%M:%SZ')
curl -sf -XPOST "http://127.0.0.1:$PORT/api/v2/alerts" -H 'Content-Type: application/json' -d "[
  {\"labels\":{\"alertname\":\"ProbeCritical\",\"severity\":\"critical\",\"job\":\"$job\"},\"endsAt\":\"$ends\"},
  {\"labels\":{\"alertname\":\"ProbeWarning\",\"severity\":\"warning\",\"job\":\"$job\"},\"endsAt\":\"$ends\"}
]" >/dev/null

suppressed=""
for _ in $(seq 1 20); do
  suppressed=$(curl -sf "http://127.0.0.1:$PORT/api/v2/alerts" \
    | jq -r --arg j "$job" '.[] | select(.labels.job == $j and .labels.alertname == "ProbeWarning")
             | .status.state' | head -1)
  [[ "$suppressed" == "suppressed" ]] && break
  sleep 2
done

critical=$(curl -sf "http://127.0.0.1:$PORT/api/v2/alerts" \
  | jq -r --arg j "$job" '.[] | select(.labels.job == $j and .labels.alertname == "ProbeCritical")
           | .status.state' | head -1)

[[ "$suppressed" == "suppressed" ]] \
  && pass "a critical suppresses the warning describing the same failure" \
  || fail "the warning was '$suppressed', not suppressed — the inhibit rule is not matching"

[[ "$critical" == "active" ]] \
  && pass "the critical itself still pages" \
  || fail "the critical was '$critical' — an inhibit rule that silences its own source is worse than none"

echo
[[ "$FAILED" -eq 0 ]] && echo "all alerting checks passed" || echo "some alerting checks failed"
exit "$FAILED"
