# Cluster policies

Platform configuration, not application configuration — which is why these live
outside the chart. A release should not be able to install the rules that
govern it.

## Two layers, both built into Kubernetes

**Pod Security Admission** (`namespace.yaml`) is a namespace label. The
`restricted` level covers root users, privilege escalation, capabilities, host
namespaces, hostPath volumes and seccomp. Everything this repository deploys
passes it, including PostgreSQL and every hook Job.

**ValidatingAdmissionPolicy** (`admission-policies.yaml`, `admission-bindings.yaml`)
covers what PSA has no opinion about, in CEL evaluated inside the API server:

| Rule | Why it exists |
|---|---|
| CPU + memory requests, memory limit | Without requests the scheduler is guessing and the HPA cannot work; without a memory limit one container can take down its node |
| No `:latest`, no bare repository name | A mutable tag means a rollback can restore something other than what was rolled back from |
| Known registries only | An unrestricted list means any pod can run anyone's code |
| Read-only root filesystem | PSA `restricted` does not require it, and every workload here manages without a writable root — PostgreSQL included |
| `automountServiceAccountToken: false` | A token in a pod that never calls the API is a credential waiting to be stolen |
| Readiness and liveness probes | Except for run-once jobs, which have nothing to probe |

## Why the built-in engine carries almost every rule

These started as Kyverno `ClusterPolicy` objects. Two things came out of that:
Kyverno had deprecated the API in favour of a CEL-based one, and — more to the
point — every rule turned out to be expressible in `ValidatingAdmissionPolicy`,
which has been GA since Kubernetes 1.30.

The comparison, measured on this cluster:

| | Kyverno | ValidatingAdmissionPolicy |
|---|---|---|
| Pods to run | 2 | 0 |
| Memory | 122 Mi | none — it runs in the API server |
| API stability | `ClusterPolicy` deprecated | `admissionregistration.k8s.io/v1`, GA |

Kyverno is installed on this cluster again anyway, and still costs those two
pods — but it now carries exactly one rule. Every rule listed at the top of
this file stayed in the API server.

A policy engine earns its keep when you need what the built-in one cannot do:
mutating defaults into submitted manifests, generating resources across
namespaces, or verifying image signatures against a signing identity. The first
two are still not needed here. The third is: the publish pipeline signs every
image with keyless cosign, and no policy evaluated inside the API server can
reach a registry, fetch a signature and check it against an identity. So the
engine is back for that one rule — `kyverno-image-signatures.yaml` in this
directory, with Kyverno itself installed by Terraform (`install_kyverno`, on by
default).

## Applying and testing

```bash
make policies      # label the namespace, apply the policies and bindings
make policy-test   # prove each rule rejects what it is supposed to
```

The fourth file here, `kyverno-image-signatures.yaml`, is not in that first
command — `make platform` applies it. The `ClusterPolicy` CRD does not exist
until Terraform has installed Kyverno, so `make policies` cannot apply the rule
on a cluster that has no engine yet, the same reason the cert-manager issuers
sit outside Terraform.

`policy-test.sh` submits eleven cases unconditionally: two compliant manifests
that must be admitted, and nine broken ones that must each be rejected with a
specific message. A rule that rejects everything is as broken as one that
rejects nothing, which is why the compliant cases are in there.

Two more run only when the signature policy is on the cluster — a signed image
that must be admitted, an unsigned one that must be rejected. With no
`verify-image-signatures` `ClusterPolicy` present the script skips that block
entirely, so a green run on a cluster that never had `make platform` proves
nothing about that rule. The unsigned half of it needs `UNSIGNED_IMAGE` set to
an image that exists but was published before signing.

## Rolling this out somewhere that already has workloads

Do not start with `Deny`. Bind with `validationActions: [Audit, Warn]`, read
what the existing workloads violate, fix them, then switch. The same applies to
PSA: set `warn` and `audit` first and leave `enforce` off until the warnings
stop.
