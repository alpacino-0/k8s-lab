#!/usr/bin/env bash
# One command that puts damga on a single k3s node.
#
# docs/DEPLOY.md is this sequence written for a person to follow; this is the
# same sequence written for a machine, and the two are kept level on purpose. A
# runbook and an installer that drift are two different installs, and only one
# of them is ever exercised.
#
# Run it on the server, as root:
#
#   sudo ./scripts/install.sh --domain demo.example.com \
#        --email you@example.com --tenant acme
#
# DRY_RUN=1 prints, in order, every command it would run and changes nothing.
# That is not only a courtesy to whoever is about to hand it a machine: it is
# what binds this script's one hard ordering rule — the CRDs go in before the
# quota that counts them — to a test. See scripts/install_test.go.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

DOMAIN=""
EMAIL=""
TENANT=""
NAMESPACE="${NAMESPACE:-damga}"
RELEASE="${RELEASE:-app}"
SYSTEM_NAMESPACE="damga-system"
# Staging by default, and this is the one default chosen to be inconvenient.
# Let's Encrypt rate-limits failed authorisations hard and the usual cause of
# failure is DNS that has not propagated yet, so the first run of a new install
# should be against the server that does not count it. --issuer letsencrypt-prod
# is the deliberate second run.
ISSUER="letsencrypt-staging"
INSTALL_K3S="auto"
CONFIGURE_NODE="yes"
CONTROL_PLANE="yes"
CONTROL_PLANE_IMAGE=""
BOOTSTRAP="yes"
DRY_RUN="${DRY_RUN:-0}"

# The control plane's binary inside its image, by absolute path.
#
# /damga and not damga. The image is gcr.io/distroless/static: no shell, and no
# PATH lookup for a bare name, so `kubectl exec -- damga bootstrap` fails with
# "executable file not found in $PATH". Measured on k3s on 2026-09-02 — the
# installer reached its last step and could not create the first owner, which
# ends an install nobody can log into.
#
# Named once because it was spelled twice in this file and both were wrong,
# while a third copy in ci.yml was right — and only the right one ever ran, so
# CI stayed green for two days over a script no CI job executes.
# scripts/install_test.go holds this against the Dockerfile's ENTRYPOINT.
CONTROL_PLANE_BIN="/damga"

# Versions, pinned here and nowhere else in this file.
INGRESS_VERSION="controller-v1.13.0"
CERT_MANAGER_VERSION="v1.16.2"
# The argo-helm chart version, and it has to equal terraform's argocd_version:
# two installers of the same component that disagree produce two clusters that
# are not the same cluster. scripts/install_test.go reads both and fails when
# they drift.
ARGOCD_CHART_VERSION="8.5.10"
# The operator's published image. Overridable for the same reason
# --control-plane-image is: an installer pointed at a tree that has not been
# published yet needs somewhere to get one.
OPERATOR_IMAGE="${OPERATOR_IMAGE:-ghcr.io/damgahq/damga-operator:1.0.0}"

usage() {
  cat <<TXT
damga installer — k3s, on one machine.

  sudo ./scripts/install.sh --domain demo.example.com --email you@example.com --tenant acme

Required:
  --domain <host>     the name the demo is served on; its A record must already
                      resolve to this machine
  --email <address>   receives certificate expiry warnings, and becomes the
                      first owner's login address
  --tenant <slug>     the first tenant

Optional:
  --issuer <name>     letsencrypt-staging (default) or letsencrypt-prod
  --namespace <name>  the tenant namespace (default: ${NAMESPACE})
  --release <name>    the Helm release (default: ${RELEASE})
  --skip-k3s          do not install k3s; use the kubeconfig already in scope
  --skip-node-config  do not write the containerd redirect or restart k3s
  --control-plane-image <ref>
                      run the control plane from this image instead of the one
                      cluster/control-plane.yaml names. For an install that
                      builds its own; the manifest's reference is published
  --skip-control-plane
                      install only the platform and the reference tenant
  --skip-bootstrap    do not create the first owner
  -h, --help          this

Environment:
  DRY_RUN=1           print every command in order and change nothing
TXT
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain)           DOMAIN="${2:-}"; shift 2 ;;
    --email)            EMAIL="${2:-}"; shift 2 ;;
    --tenant)           TENANT="${2:-}"; shift 2 ;;
    --issuer)           ISSUER="${2:-}"; shift 2 ;;
    --namespace)        NAMESPACE="${2:-}"; shift 2 ;;
    --release)          RELEASE="${2:-}"; shift 2 ;;
    --skip-k3s)              INSTALL_K3S="no"; shift ;;
    --skip-node-config)      CONFIGURE_NODE="no"; shift ;;
    --control-plane-image)   CONTROL_PLANE_IMAGE="${2:-}"; shift 2 ;;
    --skip-control-plane)    CONTROL_PLANE="no"; BOOTSTRAP="no"; shift ;;
    --skip-bootstrap)        BOOTSTRAP="no"; shift ;;
    -h|--help)          usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

for required in DOMAIN:--domain EMAIL:--email TENANT:--tenant; do
  name="${required%%:*}"
  flag="${required#*:}"
  if [[ -z "${!name}" ]]; then
    echo "install.sh: $flag is required" >&2
    usage >&2
    exit 2
  fi
done
case "$ISSUER" in
  letsencrypt-staging|letsencrypt-prod) ;;
  *) echo "install.sh: --issuer must be letsencrypt-staging or letsencrypt-prod" >&2; exit 2 ;;
esac

# The registry every image reference spells, read from the manifest that already
# holds it rather than written out a fourth time.
#
# This string is in cluster/registry.yaml, in scripts/bootstrap.sh, in CI and in
# the control plane's -registry flag. This repository has twice paid for one
# value living in several places and drifting — buildHome cost two CI rounds,
# and a registry host cost a whole build path — so this script reads the
# deployed answer instead of adding to the pile. A parse that finds nothing
# fails here, loudly, rather than composing an image reference with a hole in it.
REGISTRY_HOST="$(sed -n 's/^[[:space:]]*-[[:space:]]*-registry=\(.*\)$/\1/p' \
  "$ROOT/cluster/control-plane.yaml" | head -1 | tr -d ' \r')"
if [[ ! "$REGISTRY_HOST" =~ ^[a-z0-9.-]+(:[0-9]+)?$ ]]; then
  echo "install.sh: could not read -registry from cluster/control-plane.yaml (got '${REGISTRY_HOST}')" >&2
  exit 1
fi

# Where the control plane will look for the one-click catalogue. Read from the
# same manifest and for the same reason as the two below: the value is deployed
# there, and a second copy here is a second thing to drift.
#
# Empty is a legitimate configuration — the server answers 503 and names the
# flag — but it is not a legitimate *install*, because the catalogue is the
# product's headline feature and an operator who removed the flag by accident
# would find out from a page that says nothing is available. So this fails here,
# where the message can say which line to put back.
CATALOG_DIR="$(sed -n 's/^[[:space:]]*-[[:space:]]*-catalog-dir=\(.*\)$/\1/p' \
  "$ROOT/cluster/control-plane.yaml" | head -1 | tr -d ' \r')"
if [[ -z "$CATALOG_DIR" ]]; then
  echo "install.sh: cluster/control-plane.yaml has no -catalog-dir argument, so this" >&2
  echo "install would come up with no one-click catalogue at all. Restore the flag" >&2
  echo "(it should read -catalog-dir=/catalog, where the image puts the templates)." >&2
  exit 1
fi

# The DSN the control plane is deployed with. Same reasoning, same file.
CONTROL_PLANE_DSN="$(sed -n 's/^[[:space:]]*-[[:space:]]*-evidence-dsn=\(.*\)$/\1/p' \
  "$ROOT/cluster/control-plane.yaml" | head -1 | tr -d ' \r')"
if [[ -z "$CONTROL_PLANE_DSN" ]]; then
  echo "install.sh: could not read -evidence-dsn from cluster/control-plane.yaml" >&2
  exit 1
fi

# ------------------------------------------------------------------ plumbing

STEP=0
step() {
  STEP=$((STEP + 1))
  printf '\n\033[1;34m==> %d. %s\033[0m\n' "$STEP" "$*"
}

note() { printf '    %s\n' "$*"; }

# run is the only thing in this file that changes anything.
#
# Every mutation goes through it, so DRY_RUN=1 prints the real sequence rather
# than a second list maintained beside it — a plan written out separately is a
# plan that stops matching the script, and it stops matching it silently.
run() {
  if [[ "$DRY_RUN" == "1" ]]; then
    # Quoted the way a shell would need it back. The plan is something an
    # operator copies a line out of before handing this script a machine, and an
    # unquoted JSON patch pasted into a terminal is a different command.
    local rendered="" arg
    for arg in "$@"; do
      if [[ "$arg" =~ ^[A-Za-z0-9_./:=@,-]+$ ]]; then
        rendered="$rendered $arg"
      else
        # Single-quoted, with any embedded quote closed and reopened. printf %q
        # would do it too, and on bash 3.2 it renders as backslash escapes —
        # correct, and unreadable. This line exists to be read.
        rendered="$rendered '$(printf '%s' "$arg" | sed "s/'/'\\\\''/g")'"
      fi
    done
    printf 'RUN%s\n' "$rendered"
    return 0
  fi
  "$@"
}

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "install.sh: missing required tool: $1" >&2; exit 1; }
}

require_root() {
  if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    echo "install.sh: $1 needs root; re-run under sudo, or pass --skip-k3s --skip-node-config" >&2
    exit 1
  fi
}

# ------------------------------------------------------------------ steps

install_k3s() {
  # Traefik is disabled because the chart depends on ingress-nginx annotations
  # (limit-rps, limit-burst-multiplier, limit-connections) that Traefik does not
  # implement. Installing both and choosing later is not an option: two
  # controllers claiming the same Ingress is a coin toss.
  run bash -c "curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC='--disable traefik' sh -"
}

wait_for_ingress_controller() {
  # Two waits, because they fail differently and only one of them is a timeout.
  #
  # `kubectl wait` against a label selector that matches nothing answers "no
  # matching resources found" straight away — it does not spend its own
  # --timeout waiting for the object to turn up. So a wait placed directly after
  # an apply races the API server writing the Pod, and loses. Measured on k3s on
  # 2026-09-02: the Deployment and the Pod both carry creationTimestamp
  # 00:20:52 and the error printed in that same second, twenty seconds into a
  # fresh install. Nobody had seen it because no CI job runs this script.
  #
  # Deliberately not a sleep. A sleep is a guess about somebody else's
  # scheduler, and the repository has paid three times this week for a check
  # whose claim was right and whose timing was wrong. This waits for the object
  # to exist, then for it to be ready, and prints how long each took — so the
  # next person to read a slow install has the number rather than a suspicion.
  # The first clean run through this function printed 2s and 124s: the Pod took
  # two seconds to exist, and two minutes to be ready. The old wait asked at
  # zero and was answered at zero, so those two seconds were the whole bug and
  # the two minutes were never the part that needed waiting for.
  local start=$SECONDS deadline=$((SECONDS + 300))
  until [[ -n "$(kubectl -n ingress-nginx get pod \
      -l app.kubernetes.io/component=controller -o name 2>/dev/null)" ]]; do
    if (( SECONDS >= deadline )); then
      echo "install.sh: no ingress-nginx controller pod exists after $((SECONDS - start))s." >&2
      echo "The manifest applied, so this is the Deployment not producing a Pod:" >&2
      kubectl -n ingress-nginx get deploy,replicaset,pod >&2 || true
      return 1
    fi
    sleep 2
  done
  note "the controller pod existed after $((SECONDS - start))s"
  kubectl wait -n ingress-nginx --for=condition=Ready pod \
    -l app.kubernetes.io/component=controller --timeout=300s
  note "it was ready after $((SECONDS - start))s"
}

write_containerd_redirect() {
  # The half of the registry that lives on the node, and the half that gets
  # missed — because the build that produced the image succeeds either way and
  # the failure arrives later, as a pull.
  #
  # A build pushes to the in-cluster registry by its Service name, and that is
  # the reference recorded for the deploy. containerd runs on the host, does not
  # use cluster DNS, and answers every pull of that name with "no such host". So
  # the node is told once that this one name is served by the registry's
  # NodePort.
  install -d -m 0755 /etc/rancher/k3s
  cat > /etc/rancher/k3s/registries.yaml <<YAML
mirrors:
  "${REGISTRY_HOST}":
    endpoint:
      - "http://127.0.0.1:30500"
YAML
}

restart_k3s() {
  # The restart is what applies it. k3s reads registries.yaml at start-up and
  # writes containerd's own configuration from it; edited afterwards it changes
  # nothing, and nothing says so.
  if systemctl list-unit-files k3s.service >/dev/null 2>&1; then
    systemctl restart k3s
  else
    systemctl restart k3s-agent
  fi
}

apply_issuers() {
  # Rendered from the checked-in template rather than written out again here,
  # and never saved: the only thing this substitutes is the address that
  # receives expiry warnings.
  sed "s/you@example.com/${EMAIL}/g" "$ROOT/cluster/issuers-letsencrypt.yaml.example" |
    kubectl apply -f -
}

create_db_secret() {
  # Created once and never rotated by re-running this script. PostgreSQL sets
  # the password at first boot from what the secret said then; replacing it
  # later leaves a database whose password no longer matches the one the app
  # presents, and the failure is a connection refused with nothing anywhere
  # saying the credential was changed underneath it.
  if kubectl -n "$NAMESPACE" get secret db-credentials >/dev/null 2>&1; then
    note "db-credentials already exists; leaving it alone"
    return 0
  fi
  # The password is generated here and never printed, never passed as an
  # argument to anything this script echoes, and never written to a values file.
  kubectl -n "$NAMESPACE" create secret generic db-credentials \
    --from-literal=POSTGRES_USER=labuser \
    --from-literal=POSTGRES_DB=labdb \
    --from-literal=POSTGRES_PASSWORD="$(openssl rand -base64 24)"
}

bootstrap_owner() {
  # Run unconditionally, which is what exit code 3 is for: "this install already
  # has an owner" is a fact, not a failure, and a deployment script that cannot
  # tell the two apart has to either parse the message or never re-run.
  local status=0
  kubectl -n "$SYSTEM_NAMESPACE" exec -i deploy/damga -- \
    "$CONTROL_PLANE_BIN" bootstrap -evidence-dsn "$CONTROL_PLANE_DSN" \
    -email "$EMAIL" -tenant "$TENANT" || status=$?
  case "$status" in
    0) ;;
    3) note "this install already has an owner; nothing to do" ;;
    *) return "$status" ;;
  esac
}

# ------------------------------------------------------------------ install

# Printed in both modes, because these two values are read out of
# cluster/control-plane.yaml rather than written here, and a reader has no other
# way to see what they came out as.
printf 'PLAN registry=%s evidence-dsn=%s issuer=%s\n' \
  "$REGISTRY_HOST" "$CONTROL_PLANE_DSN" "$ISSUER"

if [[ "$DRY_RUN" != "1" ]]; then
  step "checking what is here"
  need kubectl
  need helm
  need sed
  need openssl
  if [[ "$INSTALL_K3S" != "no" ]] && ! command -v k3s >/dev/null 2>&1; then
    need curl
    require_root "installing k3s"
  fi
  if [[ "$CONFIGURE_NODE" == "yes" ]]; then
    require_root "writing the containerd redirect"
  fi
fi

step "k3s"
if [[ "$INSTALL_K3S" == "no" ]]; then
  note "skipped; using the kubeconfig already in scope"
elif [[ "$DRY_RUN" != "1" ]] && command -v k3s >/dev/null 2>&1; then
  note "k3s is already installed"
else
  install_k3s
fi

step "ingress controller"
run kubectl apply -f \
  "https://raw.githubusercontent.com/kubernetes/ingress-nginx/${INGRESS_VERSION}/deploy/static/provider/baremetal/deploy.yaml"
run wait_for_ingress_controller
# k3s ships klipper-lb, which binds the node's ports, so a LoadBalancer Service
# works with no cloud provider behind it. The baremetal manifest ships NodePort.
run kubectl patch svc ingress-nginx-controller -n ingress-nginx --type=json \
  -p '[{"op":"replace","path":"/spec/type","value":"LoadBalancer"}]'

step "TLS"
run kubectl apply -f \
  "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
run kubectl wait -n cert-manager --for=condition=Available deployment --all --timeout=300s
run apply_issuers

step "namespace and the tenant fence"
# From the manifests and never `kubectl create namespace`. The labels are the
# whole point of the file: they put the namespace under Pod Security Admission
# at restricted, and the quota is the neighbour guard. A namespace created
# without either is not one with weaker rules — it is one with no rules, and
# nothing says so.
run kubectl apply -f "$ROOT/policies/namespace.yaml"
run kubectl apply -f "$ROOT/policies/tenant-quota.yaml"

step "custom resource definitions"
# BEFORE the build namespace below, and this is the one ordering in this script
# that is not a matter of taste.
#
# cluster/build-namespace.yaml puts a quota on count/builds.platform.damga.co. A
# ResourceQuota that counts a type the API server has never heard of cannot
# compute a usage for it, and until it can, every create in that namespace is
# refused with "status unknown for quota" — a message that names the quota and
# not the missing type. Measured in this repository: applying the quota first
# made the first build of every CI run fail on the platform's own guard rail.
#
# The bases directly rather than `make -f Makefile.operator install`, because
# that target needs Go, make and a kustomize download on a machine whose only
# job is to run one server. Measured equivalent: `bin/kustomize build config/crd`
# and the three files in config/crd/bases parse to the same three objects, field
# for field — config/crd/kustomization.yaml has no active patches.
run kubectl apply -f "$ROOT/config/crd/bases"

step "the operator"
# Without this, a Workload is a row in etcd: nothing renders it into a
# Deployment, and a deploy commits and stays pending for ever. This was in the
# list of what the installer does not do, and that list was honest — which is
# why the fix is here rather than in the list.
#
# `kubectl apply -k` rather than kustomize or make: kubectl carries its own
# kustomize, so this needs nothing on the machine that is not already needed.
# It re-applies the CRDs the step above installed, which is idempotent and keeps
# the ordering rule intact — the CRDs still land before the build namespace.
run kubectl apply -k "$ROOT/config/default"
run kubectl -n damga-platform-system set image deployment/damga-platform-controller-manager \
  "manager=${OPERATOR_IMAGE}"
run kubectl -n damga-platform-system rollout status \
  deployment/damga-platform-controller-manager --timeout=300s

step "Argo CD"
# The other half of the same absence. The operator turns a Workload into a
# Deployment; Argo CD is what turns a commit into a Workload. With neither, the
# product's write path ends in a git repository nobody reads.
#
# The same chart and the same values terraform uses, at the same pinned version,
# so an installer-built cluster and a terraform-built one are the same cluster.
run helm repo add argo https://argoproj.github.io/argo-helm
run helm repo update argo
run helm upgrade --install argocd argo/argo-cd \
  --version "$ARGOCD_CHART_VERSION" \
  --namespace argocd --create-namespace \
  --values "$ROOT/cluster/argocd-values.yaml" \
  --wait --timeout 12m

step "the image store"
run kubectl apply -f "$ROOT/cluster/registry.yaml" -f "$ROOT/cluster/build-namespace.yaml"
run kubectl -n damga-registry rollout status deployment/registry --timeout=300s

step "the node's half of the image store"
if [[ "$CONFIGURE_NODE" == "yes" ]]; then
  run write_containerd_redirect
  run restart_k3s
  note "on every other node, run this again with --skip-k3s and --skip-bootstrap"
else
  note "skipped; builds will push successfully and their images will not pull"
fi

step "database credentials"
run create_db_secret

step "the reference tenant"
# Deliberately no --wait: it also waits for the backup volume, which uses a
# WaitForFirstConsumer storage class and stays Pending until the first backup
# job mounts it. That would block the release for ever.
run helm upgrade --install "$RELEASE" "$ROOT/chart" \
  --namespace "$NAMESPACE" \
  -f "$ROOT/chart/values-public.yaml" \
  --set "ingress.host=${DOMAIN}" \
  --set "ingress.tls.clusterIssuer=${ISSUER}" \
  --timeout 10m
run kubectl -n "$NAMESPACE" rollout status "statefulset/${RELEASE}-postgres" --timeout=300s
run kubectl -n "$NAMESPACE" rollout status "deployment/${RELEASE}-redis" --timeout=300s
run kubectl -n "$NAMESPACE" rollout status "deployment/${RELEASE}-damga-app" --timeout=300s

step "the control plane"
if [[ "$CONTROL_PLANE" == "no" ]]; then
  note "skipped; the platform and the reference tenant are installed, the panel is not"
else
  run kubectl apply -f "$ROOT/cluster/control-plane.yaml"
  if [[ -n "$CONTROL_PLANE_IMAGE" ]]; then
    run kubectl -n "$SYSTEM_NAMESPACE" set image deployment/damga "damga=${CONTROL_PLANE_IMAGE}"
  fi
  run kubectl -n "$SYSTEM_NAMESPACE" rollout status deployment/damga --timeout=300s
  # Said now rather than discovered later, and it names one cause rather than
  # the symptom. The flag is in the manifest — checked above — and an image
  # built from this tree cannot lack the templates, because the build fails at
  # the COPY when they are missing. So the one way to reach this note and still
  # have no catalogue is an image built before the templates were vendored,
  # which is exactly what --control-plane-image makes easy to pass in.
  note "the catalogue is served from ${CATALOG_DIR} inside the image;"
  note "if /catalog answers 503 after this, the running image predates the"
  note "vendored templates — rebuild it from this tree"
fi

step "the first owner"
if [[ "$BOOTSTRAP" == "yes" ]]; then
  # Through kubectl exec, which streams over the CRI exec channel rather than
  # the container log a collector tails — so the generated password is shown to
  # whoever ran this command and to nobody else.
  run bootstrap_owner
else
  note "skipped; run \`kubectl -n ${SYSTEM_NAMESPACE} exec -it deploy/damga -- ${CONTROL_PLANE_BIN} bootstrap ...\` yourself"
fi

if [[ "$DRY_RUN" == "1" ]]; then
  printf '\nDRY_RUN: nothing above was executed.\n'
  exit 0
fi

cat <<TXT

Done.

  The demo          https://${DOMAIN}
  The control plane kubectl -n ${SYSTEM_NAMESPACE} port-forward svc/damga 9000:80
                    then http://localhost:9000

  The CLI, against the same API the panel uses:
    damga-cli login --server http://localhost:9000 --email ${EMAIL}

  Later, to move this install to a new version:
    git pull && ./scripts/upgrade.sh

What this did NOT install, said out loud rather than discovered:

  - A git token. The control plane runs without -git-token-file, so the deploy
    endpoint refuses with a message naming that flag. Nothing else is affected.

TXT
