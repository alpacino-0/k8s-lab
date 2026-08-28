# policies

Two files, and neither is an admission *rule* — both are the fence that keeps
one tenant from taking the cluster down for the others.

| File | What it is |
|---|---|
| `namespace.yaml` | The tenant namespace and its labels. The labels put it under **Pod Security Admission** at `restricted` — a namespace created with `kubectl create namespace` gets none of them and is silently unprotected |
| `tenant-quota.yaml` | A `ResourceQuota`. Without it a single tenant can request a node's entire memory and every other tenant's pods on that node stop scheduling |

Apply both together:

```bash
kubectl apply -f policies/namespace.yaml
kubectl apply -f policies/tenant-quota.yaml
```

## What used to be here, and why it is gone

Until 2026-08-29 this directory also held three `ValidatingAdmissionPolicy`
documents with their bindings, and a Kyverno `ClusterPolicy` verifying image
signatures. Together they enforced: resource requests and a memory limit, a
2Gi per-container ceiling, an image registry allowlist, no `:latest`, a
read-only root filesystem, no service-account token, readiness and liveness
probes, and a keyless cosign signature from this repository's pipeline.

They were removed because they made the product impossible rather than because
they were wrong. A one-click catalogue install — n8n, Ghost, Plausible — brings
a third-party image that is unsigned, often writes to its own filesystem, and
needs to reach the internet. Every one of those was a rejection.

Two measurements from that work are worth keeping, because both are silent and
both bite anyone who writes admission policies:

- **A `LimitRange` carrying only `max` injects that max as the default request
  and limit.** A pod arriving with no resources at all is silently given the
  ceiling instead of being rejected — and the rule demanding resources can never
  fire again. This is why the ceiling was a policy that *rejects* rather than a
  LimitRange that *defaults*.
- **`resources: ["pods"]` does not cover `pods/ephemeralcontainers`.**
  `kubectl debug --image=...` passes straight through a policy that looks like
  it covers pods, leaves no trace in git, and Kubernetes will not let you remove
  an ephemeral container once it is attached.

A third, about the quota that is still here: **a `ResourceQuota` on `requests.*`
answers before an admission rule asking for the same thing**, with its own
message. The fence and the explanation cannot live in the same namespace.
