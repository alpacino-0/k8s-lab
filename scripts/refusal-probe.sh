#!/usr/bin/env bash
# Measures how a Deployment reports that admission refused its pods.
#
# The control plane's observer reads ReplicaFailure off the Deployment, treats it
# as the refusal, and quotes its Message to the person asking why their deploy is
# not running. Every part of that is Kubernetes' behaviour rather than this
# repository's: the ReplicaSet controller sets the condition when it cannot
# create pods, the deployment controller copies it up, and whether the webhook's
# own sentence survives that journey is not something the Go code can decide.
#
# envtest runs neither controller, so the unit tests write the condition by hand
# and prove the reading. This proves there is something to read.
#
# It asserts rather than reports. A step that prints a measurement nobody checks
# is a step that proves nothing — and if this fails, the output says what the
# object actually carried and the Go code is what changes.
set -uo pipefail

NS="${NS:-damga}"
NAME="${NAME:-refusal-probe}"
IMAGE="${IMAGE:?set IMAGE}"

FAILED=0
trap 'kubectl -n "$NS" delete deployment "$NAME" --ignore-not-found --wait=false >/dev/null 2>&1' EXIT

echo "== deploying something admission must refuse at the pod, not the deployment =="
# There are two refusal paths in this platform and they surface in different
# places. Measured, after this probe was first written against the wrong one:
#
#   Deployment-level. The ValidatingAdmissionPolicies bind to pods AND to
#   deployments/statefulsets/daemonsets/jobs, and Kyverno autogenerates rules
#   covering the same controllers. A violation they catch fails the apply
#   itself — "the deployments X is invalid: ... denied request" — so no
#   Deployment is ever created and no condition exists to read. Nothing that
#   watches Deployments can see this one. See the note in deploywatch.
#
#   Pod-level. Pod Security Admission enforces on pods and only *warns* on the
#   controllers that make them, so the Deployment is admitted, the ReplicaSet
#   is refused when it tries to create a pod, and the refusal arrives as
#   ReplicaFailure with the message attached. That is the path the observer
#   reads and the one this probe has to exercise.
#
# So the spec below satisfies every VAP — token off, read-only root, resources
# set — and violates PSA restricted by running as root. Anything the VAPs catch
# would be measuring the wrong path.
manifest=$(mktemp)
cat > "$manifest" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $NAME
  namespace: $NS
spec:
  replicas: 1
  selector:
    matchLabels: {app: $NAME}
  template:
    metadata:
      labels: {app: $NAME}
    spec:
      automountServiceAccountToken: false
      containers:
        - name: app
          image: $IMAGE
          securityContext:
            runAsUser: 0
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
          resources:
            requests: {cpu: 50m, memory: 64Mi}
            limits: {memory: 128Mi}
          # Present because damga-workload-hygiene requires both on anything
          # long-running. Everything except runAsUser satisfies a rule that
          # binds to deployments; miss one and the probe measures which rule it
          # forgot instead of the path it is about.
          readinessProbe:
            httpGet: {path: /readyz, port: 3000}
          livenessProbe:
            httpGet: {path: /healthz, port: 3000}
EOF

if ! applied=$(kubectl apply -f "$manifest" 2>&1); then
  echo "::error::the deployment was refused at the deployment level, so no ReplicaSet"
  echo "::error::was created and there is no ReplicaFailure to read. The probe is"
  echo "::error::measuring the wrong path — what admission actually said:"
  printf '%s\n' "$applied"
  rm -f "$manifest"
  exit 1
fi
rm -f "$manifest"

echo "== waiting for the refusal to reach the Deployment =="
for _ in $(seq 1 30); do
  status=$(kubectl -n "$NS" get "deployment/$NAME" \
    -o jsonpath='{.status.conditions[?(@.type=="ReplicaFailure")].status}' 2>/dev/null)
  [ "$status" = "True" ] && break
  sleep 2
done

reason=$(kubectl -n "$NS" get "deployment/$NAME" \
  -o jsonpath='{.status.conditions[?(@.type=="ReplicaFailure")].reason}' 2>/dev/null)
message=$(kubectl -n "$NS" get "deployment/$NAME" \
  -o jsonpath='{.status.conditions[?(@.type=="ReplicaFailure")].message}' 2>/dev/null)

echo "  ReplicaFailure : ${status:-<absent>}"
echo "  reason         : ${reason:-<absent>}"
echo "  message        : ${message:-<absent>}"

if [ "$status" != "True" ]; then
  echo "::error::the Deployment carries no ReplicaFailure, so the observer has nothing \
to read and a refused deploy would sit at \"applied\" until the sweep called it \
unknown. Whatever the conditions below say is where the refusal actually went."
  kubectl -n "$NS" get "deployment/$NAME" -o jsonpath='{.status.conditions}' | tr ',' '\n'
  FAILED=1
elif [ -z "$message" ]; then
  echo "::error::ReplicaFailure carries no message, so the record can say a deploy was \
refused and not why — which is the half that is worth having"
  FAILED=1
else
  echo "  the refusal reached the Deployment with a message the record can quote"
fi

# What the pods themselves were told, printed either way. When the condition is
# missing this is what says whether admission refused anything at all.
echo "== what the replicaset was told =="
kubectl -n "$NS" get events --field-selector "involvedObject.kind=ReplicaSet" \
  --sort-by=.lastTimestamp 2>/dev/null | grep -i "$NAME" | tail -5 || true

exit "$FAILED"
