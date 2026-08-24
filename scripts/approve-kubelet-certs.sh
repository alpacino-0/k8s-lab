#!/usr/bin/env bash
# Approves the kubelet serving certificate requests kind's config asks for.
#
# kubernetes.io/kubelet-serving is the one signer kube-controller-manager will
# not auto-approve: granting one means accepting a node's own claim about which
# addresses it answers on. For a cluster this repository just created that claim
# is not in question, and without the approval the kubelets have no certificate
# the cluster CA signed — which leaves scraping them possible only by turning
# verification off, the one thing done nowhere else here.
#
# The requests arrive as each kubelet starts, so this waits for one per node
# rather than approving whatever happens to exist the moment it runs.
set -uo pipefail

signer="kubernetes.io/kubelet-serving"
deadline=$((SECONDS + 120))
want=$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')

pending() {
  kubectl get csr -o json 2>/dev/null \
    | jq -r --arg s "$signer" '.items[]
        | select(.spec.signerName == $s)
        | select((.status.conditions // []) | map(.type) | index("Approved") | not)
        | .metadata.name'
}

approved() {
  kubectl get csr -o json 2>/dev/null \
    | jq -r --arg s "$signer" '[.items[]
        | select(.spec.signerName == $s)
        | select((.status.conditions // []) | map(.type) | index("Approved"))] | length'
}

while (( SECONDS < deadline )); do
  pending | xargs -r -n1 kubectl certificate approve >/dev/null 2>&1
  got=$(approved)
  if [[ "${got:-0}" -ge "${want:-1}" ]]; then
    echo "approved ${got} kubelet serving certificate(s) for ${want} node(s)"
    exit 0
  fi
  sleep 2
done

echo "only $(approved) of ${want} kubelet serving certificates were approved" >&2
echo "metrics-server cannot verify a kubelet that has none" >&2
exit 1
