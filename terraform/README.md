# Platform, as code

Terraform manages what a cluster needs before this project's chart can be
installed into it: the ingress controller, cert-manager, Argo CD, Kyverno (for
image signature verification only — `install_kyverno`, on by default),
metrics-server (without which the HorizontalPodAutoscaler has no metrics API to
read), sealed-secrets (so a GitOps repository can carry its own secrets), the
admission policies, the namespace they apply to and its quota.

```bash
terraform init
terraform apply -var kube_context=kind-damga
```

`make up` runs this — it is the installation path, not a second one.

## What is deliberately not here

**Cluster creation.** It comes from `kind` locally and from a cloud provider or
k3s remotely. Keeping it out means the only thing that changes between the two
is `kube_context`, and it keeps a community-maintained `kind` provider out of
the dependency list of a repository meant to be read as an example.

**cert-manager `ClusterIssuer`s, the Argo CD `Application` and the Kyverno
`ClusterPolicy`.** All three are custom resources whose CRDs are installed by a
release in this configuration. `kubernetes_manifest` resolves a resource's
schema at plan time, so it cannot plan against a CRD that does not exist yet —
the standard chicken-and-egg of Terraform against Kubernetes. They are plain
YAML applied after this runs. The admission policies do not have the problem:
`ValidatingAdmissionPolicy` is a built-in API, so there is no CRD to wait for.

**The application release.** That belongs to Argo CD. Two controllers
reconciling the same objects is how you get a fight.

## Adopting a cluster that already exists

Terraform will not take over what it did not create — it tries to create it
again and fails on the name. Existing Helm releases import cleanly:

```bash
terraform import helm_release.ingress_nginx ingress-nginx/ingress-nginx
terraform import helm_release.cert_manager  cert-manager/cert-manager
terraform import 'helm_release.argocd[0]'   argocd/argocd
terraform import 'helm_release.kyverno[0]'  kyverno/kyverno
terraform import kubernetes_namespace_v1.app damga
terraform plan     # should report no changes
```

Two things came out of doing this on a live cluster. The releases installed
from a static manifest with `kubectl apply` could not be imported at all, because
there was no Helm release to import — a shell script and this configuration
were installing the same components by different means. That is why
`bootstrap.sh` now calls Terraform instead of duplicating it.

And `kubernetes_manifest` has no import: policies applied by hand had to be
deleted before Terraform could create them. Cluster-scoped and idempotent, so
the gap costs nothing here; on a cluster carrying traffic it would need
planning.

## State

Local state, deliberately — this configures a laptop cluster. Anything shared
needs a remote backend with locking, or two people apply at once and the second
one wins by accident:

```hcl
terraform {
  backend "s3" {
    bucket       = "…"
    key          = "damga/platform.tfstate"
    region       = "…"
    use_lockfile = true
  }
}
```

## OpenTofu

The HCL is identical. `tofu init` and `tofu apply` work unchanged. Terraform
moved to the BUSL licence in 2023; OpenTofu is the MPL fork under the Linux
Foundation. This uses Terraform because that is still the name in job adverts,
not because the code needs it.
