variable "kubeconfig_path" {
  description = "Path to the kubeconfig for the cluster to configure."
  type        = string
  default     = "~/.kube/config"
}

variable "kube_context" {
  description = <<-EOT
    Context to use. This is the only variable that changes between a local kind
    cluster and a remote one — the platform below is the same either way, which
    is the point of keeping cluster creation out of here.
  EOT
  type        = string
  default     = "kind-k8s-lab"
}

variable "ingress_nginx_version" {
  description = "ingress-nginx chart version."
  type        = string
  default     = "4.13.0"
}

variable "cert_manager_version" {
  description = "cert-manager chart version."
  type        = string
  default     = "v1.16.2"
}

variable "argocd_version" {
  description = "Argo CD chart version."
  type        = string
  default     = "8.5.10"
}

variable "ingress_node_ports" {
  description = <<-EOT
    Fixed NodePorts for the ingress controller, matching the extraPortMappings
    in kind-config.yaml. Set to null on a cluster with a real load balancer,
    where the ports are assigned rather than pinned.
  EOT
  type = object({
    http  = number
    https = number
  })
  default = {
    http  = 30080
    https = 30443
  }
}

variable "kyverno_version" {
  description = "Kyverno chart version. Kyverno is here for one thing only — verifying image signatures, which ValidatingAdmissionPolicy cannot do."
  type        = string
  default     = "3.5.2"
}

variable "install_kyverno" {
  description = "Install Kyverno for image signature verification."
  type        = bool
  default     = true
}

variable "install_argocd" {
  description = "Install Argo CD. The Application itself is applied separately — see README."
  type        = bool
  default     = true
}
