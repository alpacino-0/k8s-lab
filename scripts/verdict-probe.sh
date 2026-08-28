#!/usr/bin/env bash
# Measures where Kyverno records a signature verdict, and whether it records one
# at all while the policy is auditing rather than enforcing.
#
# Two claims in the Go code rest on this and neither can be tested below it.
#
# The first: deploywatch reads kyverno.io/verify-images off the *Deployment*.
# The policy matches Pods, and Kyverno's autogen writes the rules that cover
# pod controllers — so which object ends up carrying the annotation is upstream
# behaviour, not a decision this repository made. envtest runs no admission
# controller, so nothing under go test can see it.
#
# The second, and the one that decides whether the whole signing chain
# terminates: a connection's policy audits until a signature carrying its
# identity has been seen, and the only thing that ever sees one is that
# annotation. If Kyverno writes it only while enforcing, then no connection ever
# leaves audit mode, and the chain stalls one step from the end — silently, in
# the direction where nothing fails.
#
# This asserts what the code assumes. If it fails, the measurement in the output
# is the answer and the code is what changes.
set -uo pipefail

IMAGE="${IMAGE:?set IMAGE to a signed image reference}"
# The identity that image was signed with. Not derived from the image: the whole
# point of a keyless subject is that it is a claim about who built something,
# and deriving it from the thing being checked would prove nothing.
SUBJECT="${SUBJECT:?set SUBJECT to the certificate identity the image carries}"
ISSUER="${ISSUER:-https://token.actions.githubusercontent.com}"

# Deliberately without damga.co/policies: enforced. The cluster-wide signature
# policy selects on that label, and this probe has to be the only rule that
# matches — otherwise a pass could come from the enforcing policy and the audit
# question would go unanswered.
NS="${NS:-verdict-probe}"
NAME=probe

FAILED=0
trap 'kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1' EXIT

fail() { echo "::error::$*"; FAILED=1; }
note() { echo "  $*"; }

kubectl create namespace "$NS" >/dev/null 2>&1 || true

echo "== applying an auditing, namespace-scoped signature policy =="
# The same shape forge.Policy renders for a connection nothing has verified yet:
# namespace-scoped kyverno.io/v1 Policy, Audit, one keyless attestor.
cat <<EOF | kubectl apply -f - || fail "the audit policy was rejected"
apiVersion: kyverno.io/v1
kind: Policy
metadata:
  name: damga-image-signature
  namespace: $NS
spec:
  validationFailureAction: Audit
  background: false
  rules:
    - name: verify-tenant-signature
      match:
        any:
          - resources:
              kinds: [Pod]
      verifyImages:
        - imageReferences:
            - "${IMAGE%@*}*"
          # False, because the action is Audit. Kyverno refuses the policy
          # outright otherwise, and forge.Policy renders it the same way for
          # the same reason.
          mutateDigest: false
          required: true
          attestors:
            - count: 1
              entries:
                - keyless:
                    subject: "$SUBJECT"
                    issuer: "$ISSUER"
                    rekor:
                      url: https://rekor.sigstore.dev
EOF

# Stop here if it was refused, rather than carrying on to blame the annotation.
#
# The first run of this probe did exactly that: Kyverno rejected the policy for
# an unrelated reason, no rule was ever installed, and the assertion at the
# bottom reported "Kyverno does not annotate while auditing" — a conclusion
# about behaviour that had never been exercised. The apply error was three lines
# above it and said what actually happened. A probe that keeps going after its
# subject failed to exist is a probe that measures nothing and says something.
if [ "$FAILED" -ne 0 ]; then
  echo "::error::the policy was refused, so nothing below was measured. The apply error above is the finding."
  exit 1
fi

# The webhook is registered asynchronously after the Policy is accepted, and a
# Deployment created in that window is admitted by nobody — which reads exactly
# like "Kyverno does not annotate in audit mode".
echo "== waiting for the policy to be ready =="
for _ in $(seq 1 30); do
  ready=$(kubectl -n "$NS" get policy damga-image-signature \
    -o jsonpath='{.status.ready}' 2>/dev/null)
  [ "$ready" = "true" ] && break
  sleep 2
done
note "policy ready: ${ready:-unknown}"

echo "== deploying the signed image =="
kubectl -n "$NS" create deployment "$NAME" --image="$IMAGE" >/dev/null 2>&1 \
  || fail "the deployment was not created"
kubectl -n "$NS" rollout status "deployment/$NAME" --timeout=180s >/dev/null 2>&1 \
  || note "the rollout did not settle; the annotation is still worth reading"

echo "== where the verdict landed =="
on_deployment=$(kubectl -n "$NS" get "deployment/$NAME" \
  -o jsonpath='{.metadata.annotations.kyverno\.io/verify-images}' 2>/dev/null)
on_template=$(kubectl -n "$NS" get "deployment/$NAME" \
  -o jsonpath='{.spec.template.metadata.annotations.kyverno\.io/verify-images}' 2>/dev/null)
pod=$(kubectl -n "$NS" get pods -l app="$NAME" -o name 2>/dev/null | head -1)
on_pod=""
[ -n "$pod" ] && on_pod=$(kubectl -n "$NS" get "$pod" \
  -o jsonpath='{.metadata.annotations.kyverno\.io/verify-images}' 2>/dev/null)

note "deployment metadata : ${on_deployment:-<absent>}"
note "pod template        : ${on_template:-<absent>}"
note "pod metadata        : ${on_pod:-<absent>}"

# What the code requires, stated as the two things it needs to be true.
if [ -z "$on_deployment" ]; then
  fail "no verdict on the Deployment. deploywatch reads it there, so with the \
policy auditing no connection would ever be marked verified and every rendered \
policy would stay in audit mode for ever. If the verdict is on the pod above, \
the observer is reading the wrong object; if it is absent everywhere, Kyverno \
does not annotate while auditing and the chain needs a different signal."
elif ! printf '%s' "$on_deployment" | grep -q '"pass"'; then
  fail "the Deployment carries a verdict that is not a pass: $on_deployment"
else
  echo "  the signed image was recorded as a pass on the Deployment while the policy audits"
fi

# Kyverno's own report, printed whether or not the assertion held. When the
# annotation is absent this is what says why — a policy that matched nothing
# reports nothing, and that is a different problem from one that matched and
# declined to annotate.
echo "== kyverno's own view =="
kubectl -n "$NS" get policyreport -o wide 2>/dev/null || note "no policy report"
kubectl -n kyverno logs -l app.kubernetes.io/component=admission-controller \
  --tail=40 2>/dev/null | grep -i "verify\|$NS" | tail -20 || true

exit "$FAILED"
