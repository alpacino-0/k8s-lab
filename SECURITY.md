# Security policy

## Reporting a vulnerability

Email **damgahq@gmail.com**. Do not open a public issue, and do not describe the
issue in a pull request.

Include what you found, how to reproduce it, and what an attacker gets out of
it. A commit SHA or an image digest is more useful than a version number: this
project is pre-1.0 and only `main` is supported.

You will get an acknowledgement within 72 hours. This is a small project with
one maintainer — that number is what can actually be honoured, not a contractual
SLA.

Please allow 90 days before public disclosure. If you have evidence the issue is
being exploited, say so and that window shrinks to whatever is needed.

## Scope

In scope:

- **`operator/`** — the Workload CRD, its controller, and the resources it
  generates. A field that lets a user weaken a hardened default is a
  vulnerability here, not a feature request.
- **`policies/`** — a rule that admits what it is meant to reject is a
  vulnerability, not a bug.
- **`chart/`** — generated manifests, RBAC, secret handling.
- **`.github/workflows/`** — anything that lets an unsigned or unintended image
  reach a cluster.

Out of scope:

- **`app/`** — the sample service exists to be deployed, not to withstand an
  attacker who already has a shell in it.
- Findings that require cluster-admin. Cluster-admin already wins.
- Missing hardening on the local `kind` cluster, which is a development target.
- Scanner output with no demonstrated path to impact.

## What is already enforced

So you can tell a gap from a deliberate design decision:

- Images are signed **keyless** through GitHub Actions OIDC. There is no private
  key to steal. Verify one:

  ```bash
  cosign verify \
    --certificate-identity-regexp '^https://github.com/damgahq/damga/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    ghcr.io/damgahq/damga@sha256:<digest>
  ```

- Signing is what this project does to its own releases. **The cluster does not
  verify them.** Image signature verification and three
  ValidatingAdmissionPolicies were removed on 2026-08-29: they made a one-click
  install of a third-party application impossible, since a catalogue image is
  unsigned, may need to write to its filesystem, and needs to reach the
  internet. Treat any claim that this platform enforces image provenance as out
  of date.
- What still runs at the API server: **Pod Security Admission** at `restricted`
  and a per-tenant **ResourceQuota**. Both guard neighbours rather than
  constrain the workload's owner.

A report showing that one of these can be bypassed is exactly what this project
wants to receive.
