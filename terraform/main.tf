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

# ---------------------------------------------------------------- kyverno

# Kyverno is installed for exactly one capability: verifying that an image was
# signed by this repository's pipeline. Everything else it could do here is
# already done by ValidatingAdmissionPolicy, which runs in the API server and
# costs no pods — the earlier version of these policies was Kyverno's and was
# moved for that reason.
#
# Signature verification is the one thing the built-in engine cannot do: it has
# no way to reach a registry, fetch a signature and check it against an
# identity.
resource "helm_release" "kyverno" {
  count = var.install_kyverno ? 1 : 0

  name             = "kyverno"
  repository       = "https://kyverno.github.io/kyverno/"
  chart            = "kyverno"
  version          = var.kyverno_version
  namespace        = "kyverno"
  create_namespace = true
  wait             = true
  timeout          = 600

  set = [
    { name = "admissionController.replicas", value = "1" },
    { name = "backgroundController.replicas", value = "1" },
    { name = "cleanupController.enabled", value = "false" },
    { name = "reportsController.enabled", value = "false" },
    { name = "admissionController.container.resources.requests.memory", value = "128Mi" },
    { name = "admissionController.container.resources.limits.memory", value = "384Mi" },
  ]
}

# ---------------------------------------------------------------- policies

# The admission policies use built-in APIs, so there is no CRD to wait for and
# kubernetes_manifest can plan against them like any core resource. They are
# applied before anything else so a workload cannot be created that would not
# satisfy them.
resource "kubernetes_manifest" "admission_policies" {
  for_each = {
    for doc in provider::kubernetes::manifest_decode_multi(
      file("${path.module}/../policies/admission-policies.yaml")
    ) : doc.metadata.name => doc
  }

  manifest = each.value
}

resource "kubernetes_manifest" "admission_bindings" {
  for_each = {
    for doc in provider::kubernetes::manifest_decode_multi(
      file("${path.module}/../policies/admission-bindings.yaml")
    ) : doc.metadata.name => doc
  }

  manifest = each.value

  depends_on = [kubernetes_manifest.admission_policies]
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
