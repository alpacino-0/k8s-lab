# The platform layer: what a cluster needs before this project's chart can be
# installed into it.
#
# Cluster creation is deliberately not here. It comes from kind locally and
# from a cloud provider or k3s remotely, and the only thing that changes
# between them is which context this configuration points at. Keeping creation
# out also keeps a community-maintained provider out of the dependency list.
#
# What is not managed here, and why:
#   - cert-manager ClusterIssuers and the Argo CD Application are custom
#     resources whose CRDs do not exist until the release above them is
#     installed. kubernetes_manifest resolves the schema at plan time, so it
#     cannot plan against a CRD that is not there yet. They are plain YAML,
#     applied after this runs — see terraform/README.md.
#   - The application release itself belongs to Argo CD, not to Terraform.
#     Two controllers reconciling the same objects is how you get a fight.

locals {
  common_labels = {
    "app.kubernetes.io/part-of"    = "damga"
    "app.kubernetes.io/managed-by" = "terraform"
  }
}

# ---------------------------------------------------------------- ingress

resource "helm_release" "ingress_nginx" {
  name             = "ingress-nginx"
  repository       = "https://kubernetes.github.io/ingress-nginx"
  chart            = "ingress-nginx"
  version          = var.ingress_nginx_version
  namespace        = "ingress-nginx"
  create_namespace = true
  wait             = true
  timeout          = 600

  # Pinned NodePorts, expressed as chart values rather than a kubectl patch
  # applied afterwards. The patch worked and left nothing that would put the
  # ports back if the Service were ever recreated.
  set = var.ingress_node_ports == null ? [] : [
    {
      name  = "controller.service.type"
      value = "NodePort"
    },
    {
      name  = "controller.service.nodePorts.http"
      value = tostring(var.ingress_node_ports.http)
    },
    {
      name  = "controller.service.nodePorts.https"
      value = tostring(var.ingress_node_ports.https)
    },
  ]
}

# ---------------------------------------------------------------- cert-manager

resource "helm_release" "cert_manager" {
  name             = "cert-manager"
  repository       = "https://charts.jetstack.io"
  chart            = "cert-manager"
  version          = var.cert_manager_version
  namespace        = "cert-manager"
  create_namespace = true
  wait             = true
  timeout          = 600

  set = [{
    name  = "crds.enabled"
    value = "true"
  }]
}

# ---------------------------------------------------------------- argo cd

resource "helm_release" "argocd" {
  count = var.install_argocd ? 1 : 0

  name             = "argocd"
  repository       = "https://argoproj.github.io/argo-helm"
  chart            = "argo-cd"
  version          = var.argocd_version
  namespace        = "argocd"
  create_namespace = true
  wait             = true
  timeout          = 900

  values = [file("${path.module}/../cluster/argocd-values.yaml")]
}

# The one thing GitOps could not describe in git. Everything else the Argo CD
# Application needs is a manifest; the database password was a `kubectl create
# secret` run by hand beforehand, which meant a cluster rebuilt from the
# repository alone came up incomplete.
#
# This does not remove that out-of-band secret so much as move it: what a
# SealedSecret is encrypted against is a key this controller generates and keeps,
# so nothing else can read it and a cluster rebuilt from scratch reads none of
# what an older one sealed. The password stops being the thing you have to carry;
# the key becomes it.
resource "helm_release" "sealed_secrets" {
  count = var.install_sealed_secrets ? 1 : 0

  name             = "sealed-secrets"
  repository       = "https://charts.bitnami.com/bitnami"
  chart            = "sealed-secrets"
  version          = var.sealed_secrets_version
  namespace        = "sealed-secrets"
  create_namespace = true
  wait             = true
  timeout          = 300

  values = [file("${path.module}/../cluster/sealed-secrets-values.yaml")]
}

# The HorizontalPodAutoscaler reads metrics.k8s.io, and nothing registers that
# API until this runs. Measured before it existed: the app's HPA reported
# ScalingActive=False and had logged the same FailedGetResourceMetric 837 times
# over 5h49m, while the Deployment beside it looked healthy — an autoscaler that
# has never made a decision does not announce itself anywhere a dashboard looks.
resource "helm_release" "metrics_server" {
  count = var.install_metrics_server ? 1 : 0

  name       = "metrics-server"
  repository = "https://kubernetes-sigs.github.io/metrics-server/"
  chart      = "metrics-server"
  version    = var.metrics_server_version
  namespace  = "kube-system"
  wait       = true
  timeout    = 300

  values = [file("${path.module}/../cluster/metrics-server-values.yaml")]
}

# The tenant fence. Applied from the same file `make policies` uses, so the two
# entry points cannot drift. It has to come after the namespace exists, and it
# is deliberately not a kubernetes_resource_quota_v1: keeping it as YAML means
# one definition, reviewed in one place, whichever path applied it.
resource "kubernetes_manifest" "tenant_quota" {
  for_each = {
    for doc in provider::kubernetes::manifest_decode_multi(
      file("${path.module}/../policies/tenant-quota.yaml")
    ) : doc.metadata.name => doc
  }

  manifest = each.value

  depends_on = [kubernetes_namespace_v1.app]
}

resource "kubernetes_namespace_v1" "app" {
  metadata {
    name = "damga"
    labels = merge(local.common_labels, {
      "pod-security.kubernetes.io/enforce"         = "restricted"
      "pod-security.kubernetes.io/enforce-version" = "latest"
      "pod-security.kubernetes.io/audit"           = "restricted"
      "pod-security.kubernetes.io/warn"            = "restricted"
      "damga.co/policies"                          = "enforced"
    })
  }
}
