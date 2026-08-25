#!/usr/bin/env bash
# Proves the policy results this cluster produces actually reach a report.
#
# The reports controller is the only component here whose output is data rather
# than a decision, and that makes it the easiest one to be wrong about
# quietly. It declares no readiness, liveness or startup probe, so a wedged
# controller stays 1/1 Running for as long as you leave it. Nothing restarts
# it, nothing alerts, and `kubectl get policyreport` keeps answering — with
# whatever it last wrote.
#
# So "Running" is not a check, and neither is "there are rows". Every rule in
# this repository passes on every workload in it, which means an empty result
# set and a correct one look identical from the outside. Four things are
# checked separately because they break separately:
#
#   1. The controller is reconciling now — a report newer than the pod.
#   2. The ValidatingAdmissionPolicies reach the report at all. They are the
#      reason the controller is worth a third pod: a VAP keeps no results of
#      its own, and the Audit action on its binding writes to an API server
#      audit log this cluster does not configure. Without this, seven of the
#      eight rules here produce evidence nowhere.
#   3. A row says how serious it is. Kyverno reads category and severity off
#      annotations, including from policies it does not own; without them
#      every row is a null-severity "fail" that no page can rank.
#   4. A failure is reportable. The bindings say Deny, so a violating resource
#      never persists and the steady state is pass-forever. The only honest way
#      to see a fail row is to create the resource out of scope and then pull
#      scope over it.
set -uo pipefail

NAMESPACE="${NAMESPACE:-damga}"
KYVERNO_NS="${KYVERNO_NS:-kyverno}"
PROOF_NS="${PROOF_NS:-damga-report-proof-$$}"
# Any image works: the case under test is the absent resources block, and this
# never runs long enough to pull. It must not match the signature policy's
# imageReferences, or the pod is rejected for the wrong reason.
PROOF_IMAGE="${PROOF_IMAGE:-registry.k8s.io/pause:3.10}"
FAILED=0

pass() { printf '  \033[0;32mPASS\033[0m  %s\n' "$1"; }
fail() { printf '  \033[0;31mFAIL\033[0m  %s\n' "$1"; FAILED=1; }
note() { printf '  ....  %s\n' "$1"; }

cleanup() {
  kubectl delete namespace "$PROOF_NS" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

kubectl -n "$KYVERNO_NS" get deploy kyverno-reports-controller >/dev/null 2>&1 || {
  echo "no reports controller in '$KYVERNO_NS' — policy results are being produced and discarded" >&2
  echo "run: make platform" >&2
  echo "or, on a cluster built by hand:" >&2
  echo "  helm upgrade --install kyverno kyverno/kyverno -n kyverno --version 3.5.2 \\" >&2
  echo "    --set reportsController.enabled=true \\" >&2
  echo "    --set reportsController.resources.limits.memory=256Mi" >&2
  exit 1
}

# Without a bound policy every check below would wait, then report that no
# results arrived — true, and useless: nothing was ever asked to produce one.
kubectl get validatingadmissionpolicybinding damga-resource-bounds >/dev/null 2>&1 || {
  echo "no 'damga-resource-bounds' binding — the policies are not installed in this cluster" >&2
  echo "run: make policies" >&2
  exit 1
}

echo "report checks against '$KYVERNO_NS'"

polr() { kubectl get policyreports.wgpolicyk8s.io -A -o json 2>/dev/null; }
vap_results() {
  polr | jq -r '.items[].results[] | select(.source=="ValidatingAdmissionPolicy")'
}

# ------------------------------------------------------- 1. actually reconciling
POD_START=$(kubectl -n "$KYVERNO_NS" get pods -l app.kubernetes.io/component=reports-controller \
  -o jsonpath='{.items[0].status.startTime}' 2>/dev/null)
NEWEST=$(polr | jq -r '[.items[].metadata.creationTimestamp] | max // empty')

if [[ -z "$NEWEST" ]]; then
  fail "the controller has produced no report at all — it has no liveness probe, so Running proves nothing"
elif [[ -n "$POD_START" && "$NEWEST" > "$POD_START" ]]; then
  pass "the controller is reconciling — a report is newer than the pod that writes them"
else
  fail "every report predates the running pod ($NEWEST <= $POD_START) — the controller is up and stuck"
fi

# --------------------------------------------------------- 2. the VAPs are covered
VAP_COUNT=$(vap_results | jq -s 'length')
[[ "${VAP_COUNT:-0}" -gt 0 ]] \
  && pass "ValidatingAdmissionPolicy results reach the report ($VAP_COUNT rows)" \
  || fail "no VAP rows — check the binding's namespaceSelector before suspecting the chart"

# Named individually. A policy that silently drops out of scope leaves the total
# looking healthy, because the other two keep writing.
for p in damga-resource-bounds damga-image-provenance damga-workload-hygiene; do
  n=$(vap_results | jq -s --arg p "$p" '[.[]|select(.policy==$p)]|length')
  [[ "${n:-0}" -gt 0 ]] \
    && pass "$p is represented ($n rows)" \
    || fail "$p appears in no report — it is installed but nothing records what it decided"
done

# ------------------------------------------------------------ 3. rows are readable
NULLS=$(vap_results | jq -s '[.[]|select(.severity==null or .category==null)]|length')
[[ "${NULLS:-1}" -eq 0 ]] \
  && pass "every VAP row carries a category and a severity" \
  || fail "$NULLS VAP rows have a null category or severity — add policies.kyverno.io/category and /severity to the policy"

# ------------------------------------------------------------ 4. a failure reports
# Created unlabelled, then pulled into scope. Applying it to a namespace the
# bindings already select would be rejected at admission and never exist, which
# is the enforcement working and tells us nothing about reporting.
note "creating '$PROOF_NS' out of scope, then labelling it, to force a fail row"
kubectl create namespace "$PROOF_NS" >/dev/null 2>&1
kubectl -n "$PROOF_NS" create deployment nolimits --image="$PROOF_IMAGE" >/dev/null 2>&1
kubectl -n "$PROOF_NS" rollout status deploy/nolimits --timeout=90s >/dev/null 2>&1
kubectl label namespace "$PROOF_NS" damga.co/policies=enforced --overwrite >/dev/null 2>&1

FAILS=0
for _ in $(seq 1 30); do   # up to 2.5 minutes; the enqueue delay is ~30s
  FAILS=$(kubectl -n "$PROOF_NS" get policyreports.wgpolicyk8s.io -o json 2>/dev/null \
    | jq '[.items[].results[]|select(.result=="fail" and .policy=="damga-resource-bounds")]|length')
  [[ "${FAILS:-0}" -gt 0 ]] && break
  sleep 5
done

if [[ "${FAILS:-0}" -gt 0 ]]; then
  pass "a violation is recorded as a fail row, not only rejected at admission"
  MSG=$(kubectl -n "$PROOF_NS" get policyreports.wgpolicyk8s.io -o json 2>/dev/null \
    | jq -r '[.items[].results[]|select(.result=="fail")][0].message // ""')
  [[ -n "$MSG" ]] \
    && pass "the fail row carries the policy's own message: \"${MSG:0:60}...\"" \
    || fail "the fail row has no message — a finding nobody can act on"
else
  fail "no fail row appeared for a deployment with no resources — reporting covers passes only"
fi

# --------------------------------------------------------------- 5. no leak left
# EphemeralReports are the admission controller's intermediate output. They
# carry no ownerReferences, so nothing garbage-collects them: with no reports
# controller running they accumulate for the life of the cluster. Measured
# before this controller was turned on — 14 of them, 23 hours old, 9 belonging
# to resources that no longer existed.
CUTOFF=$(date -u -v-5M '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '-5 minutes' '+%Y-%m-%dT%H:%M:%SZ')
STALE=$(kubectl get ephemeralreports.reports.kyverno.io -A -o json 2>/dev/null \
  | jq --arg c "$CUTOFF" '[.items[]|select(.metadata.creationTimestamp < $c)]|length')
[[ "${STALE:-1}" -eq 0 ]] \
  && pass "no EphemeralReport older than 5 minutes — the intermediate reports are being consumed" \
  || fail "$STALE EphemeralReports are older than 5 minutes — they have no owner and nothing else collects them"

# ------------------------------------------------------- 6. the signature row
# Skipped rather than failed where it cannot apply: the signature policy matches
# ghcr.io/damgahq/damga* only, and a locally built damga-app:1.0.0 — which is
# what `make up` and CI's e2e job run — is unsigned by design.
SIG=$(polr | jq -s -r '[.[0].items[].results[]|select(.policy=="verify-image-signatures")]|length')
if [[ "${SIG:-0}" -gt 0 ]]; then
  pass "the image signature verdict is recorded too ($SIG rows)"
  DIGEST=$(polr | jq -r '[.items[].results[]|select(.policy=="verify-image-signatures")
                          |.message|select(test("sha256:"))][0] // ""')
  [[ -n "$DIGEST" ]] \
    && note "one row names the verified digest; the rest say only \"image verified\"" \
    || note "no row names a digest — read it from the image field, which mutateDigest rewrote"
else
  note "no ghcr.io/damgahq/damga* workload in this cluster — skipping the signature row"
fi

echo
[[ "$FAILED" -eq 0 ]] && echo "all report checks passed" || echo "some report checks failed"
exit "$FAILED"
