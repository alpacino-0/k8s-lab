#!/usr/bin/env bash
# Create a local cluster, install the ingress controller, build and deploy.
# Idempotent: safe to re-run.
set -euo pipefail

# The registry's name as every image reference spells it. One string, because
# the build pod and the kubelet have to agree on it — see cluster/registry.yaml.
REGISTRY_HOST="registry.damga-registry.svc:5000"

CLUSTER="${CLUSTER:-damga}"
NAMESPACE="${NAMESPACE:-damga}"
RELEASE="${RELEASE:-app}"
IMAGE_TAG="${IMAGE_TAG:-1.0.0}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

for tool in docker kind kubectl helm terraform jq; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER" --config "$ROOT/kind-config.yaml"
else
  log "cluster '$CLUSTER' already exists"
fi

log "approving kubelet serving certificates"
"$ROOT/scripts/approve-kubelet-certs.sh"

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

# The registry builds push to and the kubelet pulls from.
#
# Applied here rather than by Terraform for the same reason the issuers are:
# it is a plain manifest and Terraform's kubernetes_manifest wants a schema at
# plan time. The wait is on the deployment rather than the release, because a
# registry that is not serving yet turns the first build into a push timeout.
# The CRDs before anything that counts them.
#
# cluster/build-namespace.yaml puts a quota on count/builds.platform.damga.co,
# and a ResourceQuota that counts a type the API server has never heard of
# cannot compute a usage for it — until it does, every create is refused with
# "status unknown for quota", which names the quota and not the missing type.
log "installing the CRDs"
make -f "$ROOT/Makefile.operator" -C "$ROOT" install >/dev/null

log "installing the image registry and the build namespace"
kubectl apply -f "$ROOT/cluster/registry.yaml" -f "$ROOT/cluster/build-namespace.yaml"
kubectl -n damga-registry rollout status deployment/registry --timeout=300s

# The half of the registry that lives on the nodes, and the half that is easy to
# forget: containerd cannot resolve cluster DNS, so without this file the
# kubelet answers every pull with "no such host" while the build that produced
# the image succeeded. kind-config.yaml points containerd at this directory; the
# file itself has to be written per node, because kind has no way to place it.
#
# Written on every reconcile of this script rather than once at creation, so a
# node added later gets it by re-running bootstrap.
log "teaching containerd where the registry is"
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/${REGISTRY_HOST}"
  docker exec -i "$node" tee "/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml" >/dev/null <<TOML
server = "http://${REGISTRY_HOST}"

[host."http://localhost:30500"]
  capabilities = ["pull", "resolve"]
TOML
done

# The control plane itself, in the cluster it manages. Built here rather than
# pulled, because the published image is the reference tenant — see
# cluster/control-plane.yaml for why that was true for so long.
log "building and deploying the control plane"
docker build -q -t "damga-control-plane:$IMAGE_TAG" "$ROOT" >/dev/null
kind load docker-image "damga-control-plane:$IMAGE_TAG" --name "$CLUSTER" >/dev/null
kubectl apply -f "$ROOT/cluster/control-plane.yaml"
kubectl -n damga-system set image deployment/damga "damga=damga-control-plane:$IMAGE_TAG"
kubectl -n damga-system patch deployment damga --type=json \
  -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
kubectl -n damga-system rollout status deployment/damga --timeout=300s

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

  The control plane's own panel:
    kubectl -n damga-system port-forward svc/damga 9000:80
    then http://localhost:9000

  The chart also serves app.local. To use that name instead, add it once:
    echo "127.0.0.1 app.local" | sudo tee -a /etc/hosts

TXT
