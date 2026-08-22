#!/usr/bin/env bash
# Create a local cluster, install the ingress controller, build and deploy.
# Idempotent: safe to re-run.
set -euo pipefail

CLUSTER="${CLUSTER:-k8s-lab}"
NAMESPACE="${NAMESPACE:-k8s-lab}"
RELEASE="${RELEASE:-app}"
IMAGE_TAG="${IMAGE_TAG:-1.0.0}"
INGRESS_VERSION="${INGRESS_VERSION:-controller-v1.13.0}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

for tool in docker kind kubectl helm; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER" --config "$ROOT/kind-config.yaml"
else
  log "cluster '$CLUSTER' already exists"
fi

log "building images"
docker build -q -t "k8s-lab-app:$IMAGE_TAG" "$ROOT/app" >/dev/null
docker build -q -t "k8s-lab-web:$IMAGE_TAG" "$ROOT/web" >/dev/null

log "loading images into cluster nodes"
kind load docker-image "k8s-lab-app:$IMAGE_TAG" "k8s-lab-web:$IMAGE_TAG" --name "$CLUSTER" >/dev/null

if ! kubectl get ns ingress-nginx >/dev/null 2>&1; then
  log "installing ingress-nginx"
  kubectl apply -f "https://raw.githubusercontent.com/kubernetes/ingress-nginx/${INGRESS_VERSION}/deploy/static/provider/baremetal/deploy.yaml"
  kubectl wait -n ingress-nginx --for=condition=Ready pod \
    -l app.kubernetes.io/component=controller --timeout=300s
  # Pin the controller to the port kind maps to the host.
  kubectl patch svc ingress-nginx-controller -n ingress-nginx --type=json \
    -p='[{"op":"replace","path":"/spec/ports/0/nodePort","value":30080}]'
else
  log "ingress-nginx already installed"
fi

log "deploying release '$RELEASE' to namespace '$NAMESPACE'"
# Deliberately no --wait: it also waits for the backup PVC, which stays Pending
# under a WaitForFirstConsumer StorageClass until its first consumer runs.
# We wait on the workloads that actually matter instead.
helm upgrade --install "$RELEASE" "$ROOT/chart" \
  --namespace "$NAMESPACE" --create-namespace \
  --set image.tag="$IMAGE_TAG" \
  --set web.image.tag="$IMAGE_TAG" \
  --set postgres.auth.password="${PGPASSWORD:-local-dev-password}" \
  --set 'ingress.extraHosts[0]=localhost' \
  --timeout 10m "$@"

log "waiting for workloads"
kubectl -n "$NAMESPACE" rollout status "statefulset/${RELEASE}-postgres" --timeout=300s
kubectl -n "$NAMESPACE" rollout status "deployment/${RELEASE}-k8s-lab-app" --timeout=300s
kubectl -n "$NAMESPACE" rollout status "deployment/${RELEASE}-k8s-lab-app-web" --timeout=300s

log "done"
kubectl -n "$NAMESPACE" get deploy,sts,svc,ingress,hpa
cat <<TXT

  Open http://localhost:8080

  The chart also serves app.local. To use that name instead, add it once:
    echo "127.0.0.1 app.local" | sudo tee -a /etc/hosts

TXT
