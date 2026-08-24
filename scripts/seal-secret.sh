#!/usr/bin/env bash
# Reads a Secret manifest on stdin, writes a SealedSecret on stdout.
#
#   kubectl create secret generic db --from-literal=PASSWORD=... \
#     --dry-run=client -o yaml | scripts/seal-secret.sh <namespace>
#
# The point: a SealedSecret is safe to commit, so the one thing GitOps could not
# describe in git stops being an exception. What it does not do is remove the
# out-of-band secret — it moves it. The sealing key lives in the cluster and
# nothing else can decrypt what it sealed, so a cluster rebuilt from git alone
# reads none of what an older one sealed. The password stops being the thing you
# have to carry; the key becomes it.
#
# The namespace matters and is not cosmetic: the default scope binds the
# ciphertext to one name in one namespace, so a sealed secret cannot be lifted
# into another namespace to be read there.
#
# kubeseal runs in a container rather than as one more thing to install — docker
# is required already. It gets the sealing certificate, which is public, and no
# cluster access at all. The empty kubeconfig is there because kubeseal builds a
# client before it notices --cert means it will never use one.
set -euo pipefail

NS_TARGET="${1:-}"
[[ -n "$NS_TARGET" ]] || { echo "usage: $0 <namespace>  (Secret manifest on stdin)" >&2; exit 2; }

CONTROLLER_NS="${SEALED_SECRETS_NAMESPACE:-sealed-secrets}"
IMAGE="${KUBESEAL_IMAGE:-docker.io/bitnami/sealed-secrets-kubeseal:0.31.0}"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

kubectl -n "$CONTROLLER_NS" get secret -l sealedsecrets.bitnami.com/sealed-secrets-key \
  -o jsonpath='{.items[0].data.tls\.crt}' 2>/dev/null | base64 -d > "$work/cert.pem" || true

if [[ ! -s "$work/cert.pem" ]]; then
  echo "no sealing certificate in namespace '$CONTROLLER_NS' — is the controller installed?" >&2
  echo "it comes from the platform layer: make platform" >&2
  exit 1
fi

cat > "$work/kubeconfig" <<'KC'
apiVersion: v1
kind: Config
clusters: [{name: none, cluster: {server: https://127.0.0.1:1}}]
contexts: [{name: none, context: {cluster: none, user: none, namespace: default}}]
current-context: none
users: [{name: none, user: {}}]
KC

docker run --rm -i \
  -v "$work/cert.pem:/cert.pem:ro" \
  -v "$work/kubeconfig:/kubeconfig:ro" \
  "$IMAGE" --cert /cert.pem --kubeconfig /kubeconfig -n "$NS_TARGET" -o yaml
