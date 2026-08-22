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
    "app.kubernetes.io/part-of"    = "k8s-lab"
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

resource "kubernetes_namespace" "app" {
  metadata {
    name = "k8s-lab"
    labels = merge(local.common_labels, {
      "pod-security.kubernetes.io/enforce"         = "restricted"
      "pod-security.kubernetes.io/enforce-version" = "latest"
      "pod-security.kubernetes.io/audit"           = "restricted"
      "pod-security.kubernetes.io/warn"            = "restricted"
      "k8s-lab.dev/policies"                       = "enforced"
    })
  }
}
