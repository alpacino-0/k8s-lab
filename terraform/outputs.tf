output "ingress_http_url" {
  description = "Where the ingress answers, given the kind port mappings."
  value       = var.ingress_node_ports == null ? "assigned by the load balancer" : "http://localhost:8080"
}

output "next_steps" {
  description = "What Terraform deliberately does not apply."
  value       = <<-EOT
    kubectl apply -f cluster/issuers.yaml                  # needs the cert-manager CRDs
    kubectl apply -f policies/kyverno-image-signatures.yaml  # needs the Kyverno CRDs
    kubectl apply -f gitops/application.yaml               # needs the Argo CD CRDs
  EOT
}
