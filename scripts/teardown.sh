#!/usr/bin/env bash
# Remove everything this project created.
set -euo pipefail
CLUSTER="${CLUSTER:-k8s-lab}"

read -r -p "Delete kind cluster '$CLUSTER' and all its data? [y/N] " reply
case "$reply" in
  [yY]*) kind delete cluster --name "$CLUSTER" ;;
  *) echo "aborted"; exit 1 ;;
esac
