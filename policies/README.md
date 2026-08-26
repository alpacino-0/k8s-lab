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
| Unsigned local images need the namespace's consent | A `damga-*` image built on the machine carries no signature and no rule downstream can evaluate one. Admitting it everywhere made *never checked* and *verified* render identically — measured, three policy results against four for the same application one namespace over. A namespace now says `damga.co/unsigned-images: permitted` or the image is rejected |
| Read-only root filesystem | PSA `restricted` does not require it, and every workload here manages without a writable root — PostgreSQL included |
| `automountServiceAccountToken: false` | A token in a pod that never calls the API is a credential waiting to be stolen — unless the namespace and the pod both say otherwise, below |
| Readiness and liveness probes | Except for run-once jobs, which have nothing to probe |
| No memory limit above 2Gi | One container asking for more than a small node can give is how a namespace stops scheduling at all. A `LimitRange` is the usual way to say this and cannot be used here — see below |

## Why the built-in engine carries almost every rule

These started as Kyverno `ClusterPolicy` objects. Two things came out of that:
Kyverno had deprecated the API in favour of a CEL-based one, and — more to the
point — every rule turned out to be expressible in `ValidatingAdmissionPolicy`,
which has been GA since Kubernetes 1.30.

The comparison, measured on this cluster:

| | Kyverno | ValidatingAdmissionPolicy |
|---|---|---|
| Pods to run | 3 | 0 |
| Memory | 174 Mi | none — it runs in the API server |
| API stability | `ClusterPolicy` deprecated | `admissionregistration.k8s.io/v1`, GA |

Kyverno is installed on this cluster again anyway, and now costs three pods —
but it carries exactly one rule. Every rule listed at the top of this file
stayed in the API server.

The third pod is the reports controller, and it is here because of what the
move cost. A `ValidatingAdmissionPolicy` decides and forgets: its `.status`
holds `observedGeneration` and nothing about any resource it judged, and the
`Audit` action on the bindings writes an annotation into the API server's audit
log, which this cluster does not configure. So for as long as the rules lived
only in the API server, there was no way to ask which workloads satisfy them —
only to find out at the moment one was rejected. Kyverno's reports controller
reads native policies it does not own and writes their results as
`PolicyReport`s, which is the only in-cluster answer to that question:

```
kubectl get policyreports -A
kubectl get policyreports -A -o json | jq -r '.items[].results[]
  | select(.source=="ValidatingAdmissionPolicy") | [.policy,.result,.severity] | @tsv'
```

The `policies.kyverno.io/category` and `/severity` annotations on each policy
exist for that reader: Kyverno copies them into every row it writes, and
without them a finding cannot be ranked. Measured — stripping them turned all
24 rows for a policy null on the next scan.

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

**ResourceQuota** (`tenant-quota.yaml`) is the other half of the fence: a
ceiling on what the namespace may take in total, so one tenant cannot starve the
rest. There is deliberately no `LimitRange` beside it. Measured on this cluster,
a `LimitRange` carrying only `max` injects that max as the default request and
limit, so a pod that arrives with no resources at all is silently given 2Gi
instead of being rejected — and the first rule in the table above could never
fire again. The per-container ceiling is a rule instead, because a rule rejects
without injecting anything.

The quota has no `limits.cpu` for a related reason: with it, the API server
rejects every pod that does not set a CPU limit, and the operator sets none on
purpose. One line would have made every Workload it renders unschedulable.

A quota on `requests.*` does answer before the rule that requires them, with its
own message. That is fine for a tenant — both messages are clear — but it means
the proof of that rule has to run somewhere without a quota, and `policy-test.sh`
creates a namespace for exactly that.

The token rule has the one exception in this directory, and it takes two keys
to open. The namespace has to carry `damga.co/api-access: permitted` and the pod
— or the `spec.template` of whatever creates it — has to carry
`damga.co/api-access: required`. Either alone is refused: a pod cannot exempt
itself, and labelling a namespace is cluster-admin work. The exemption lifts
that one validation and nothing else, and everything holding it is one query
away: `kubectl get pods -A -l damga.co/api-access`. This project's own operator
is the only thing using it, because a controller that reconciles custom
resources genuinely does call the API.

The fifth file here, `kyverno-image-signatures.yaml`, is not in that first
command — `make platform` applies it. The `ClusterPolicy` CRD does not exist
until Terraform has installed Kyverno, so `make policies` cannot apply the rule
on a cluster that has no engine yet, the same reason the cert-manager issuers
sit outside Terraform.

`policy-test.sh` submits twelve cases unconditionally: two compliant manifests
that must be admitted, and ten broken ones that must each be rejected with a
specific message. A thirteenth runs wherever a `damga-tenant` ResourceQuota is
applied, and is skipped with a note where it is not. A rule that rejects everything is as broken as one that
rejects nothing, which is why the compliant cases are in there.

Two more run only when the signature policy is on the cluster — a signed image
that must be admitted, an unsigned one that must be rejected. With no
`verify-image-signatures` `ClusterPolicy` present the script skips that block
entirely, so a green run on a cluster that never had `make platform` proves
nothing about that rule. The unsigned half needs `UNSIGNED_IMAGE` set to an
image that exists, carries no signature, and still reaches this rule: it has to
clear the registry allowlist in `admission-policies.yaml` and match the
`ghcr.io/damgahq/damga*` references in `kyverno-image-signatures.yaml`. Miss
either and the rejection comes from a different rule, which proves nothing. CI
passes `ghcr.io/damgahq/damga:unsigned-fixture`, built and pushed by the publish
job after the signing step and deliberately never signed, so both halves run
there; locally the case is skipped unless you set the variable yourself.

## Rolling this out somewhere that already has workloads

Do not start with `Deny`. Bind with `validationActions: [Audit, Warn]`, read
what the existing workloads violate, fix them, then switch. The same applies to
PSA: set `warn` and `audit` first and leave `enforce` off until the warnings
stop.
