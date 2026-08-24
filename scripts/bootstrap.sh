#!/usr/bin/env bash
# Create a local cluster, install the ingress controller, build and deploy.
# Idempotent: safe to re-run.
set -euo pipefail

CLUSTER="${CLUSTER:-damga}"
NAMESPACE="${NAMESPACE:-damga}"
RELEASE="${RELEASE:-app}"
IMAGE_TAG="${IMAGE_TAG:-1.0.0}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

for tool in docker kind kubectl helm terraform; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER" --config "$ROOT/kind-config.yaml"
else
  log "cluster '$CLUSTER' already exists"
fi

log "building the image"
docker build -q -t "damga-app:$IMAGE_TAG" "$ROOT/app" >/dev/null

log "loading the image into cluster nodes"
kind load docker-image "damga-app:$IMAGE_TAG" --name "$CLUSTER" >/dev/null

# The platform layer — ingress controller, cert-manager, Argo CD, the Kyverno
# engine, the admission policies and the namespace they apply to — is
# Terraform's. Installing the same components from a shell script as well
# would be two sources of truth disagreeing at the worst possible moment.
log "applying the platform with terraform"
terraform -chdir="$ROOT/terraform" init -input=false >/dev/null
terraform -chdir="$ROOT/terraform" apply -input=false -auto-approve \
  -var "kube_context=kind-${CLUSTER}"

# Custom resources whose CRDs are installed by the releases above. Terraform's
# kubernetes_manifest resolves a schema at plan time and cannot plan against a
# CRD that does not exist yet, so these are applied afterwards.
log "applying certificate issuers"
kubectl apply -f "$ROOT/cluster/issuers.yaml"
kubectl wait --for=condition=Ready clusterissuer/selfsigned-ca --timeout=180s

# The signature rule belongs to the same category and had been left out of it.
# Terraform installs the Kyverno engine and cannot install this, so a cluster
# built by the documented path ran two Kyverno pods enforcing nothing at all —
# the one capability the engine is here for, switched off by omission.
#
# Guarded rather than unconditional: install_kyverno can be turned off, and a
# missing CRD should skip the rule, not fail the bootstrap.
if kubectl get crd clusterpolicies.kyverno.io >/dev/null 2>&1; then
  log "applying the image signature policy"
  # helm --wait returns when the pods report Ready, which is earlier than the
  # moment Kyverno's webhook accepts connections — and this policy travels
  # through that webhook.
  for attempt in $(seq 1 30); do
    kubectl apply -f "$ROOT/policies/kyverno-image-signatures.yaml" && break
    [ "$attempt" -eq 30 ] && { echo "kyverno webhook never became reachable" >&2; exit 1; }
    sleep 2
  done
else
  log "kyverno not installed — skipping the image signature policy"
fi

log "deploying release '$RELEASE' to namespace '$NAMESPACE'"
# Deliberately no --wait: it also waits for the backup PVC, which stays Pending
# under a WaitForFirstConsumer StorageClass until its first consumer runs.
# We wait on the workloads that actually matter instead.
helm upgrade --install "$RELEASE" "$ROOT/chart" \
  --namespace "$NAMESPACE" --create-namespace \
  --set image.tag="$IMAGE_TAG" \
  --set postgres.auth.password="${PGPASSWORD:-local-dev-password}" \
  --set 'ingress.extraHosts[0]=localhost' \
  --timeout 10m "$@"

log "waiting for workloads"
kubectl -n "$NAMESPACE" rollout status "statefulset/${RELEASE}-postgres" --timeout=300s
kubectl -n "$NAMESPACE" rollout status "deployment/${RELEASE}-damga-app" --timeout=300s

log "done"
kubectl -n "$NAMESPACE" get deploy,sts,svc,ingress,hpa
cat <<TXT

  Open http://localhost:8080

  The chart also serves app.local. To use that name instead, add it once:
    echo "127.0.0.1 app.local" | sudo tee -a /etc/hosts

TXT
