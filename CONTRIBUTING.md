# Contributing

## Licence

This project is AGPL-3.0. Every contribution is published under it, and this
repository stays AGPL-3.0 in its entirety.

Running the software costs nothing and obliges nothing, commercial use
included. The licence asks for source from one group only: whoever offers this
software to other people as a service. That is a deliberate choice — it keeps
the project genuinely free for the people who self-host it, and stops it being
taken, closed, and sold back as a hosted product.

## Why there is a CLA

Contributions are accepted under a Contributor Licence Agreement — see
[CLA.md](CLA.md).

**The plan, told before you contribute rather than after.** This project intends
to fund itself with an open-core model: this repository stays fully AGPL-3.0,
and a separate, closed repository will hold enterprise features — SSO,
fine-grained RBAC, an audit archive, approval workflows — sold under a
commercial licence. None of that exists yet.

That enterprise build combines with the code in this repository. Combining AGPL
code with proprietary code is something only the copyright holder can permit,
which is why the right to relicense has to sit in one place — not the copyright
itself, which stays yours. Without a CLA, every past
contributor would have to be found and asked, and in practice that means it
never happens — so the model would not merely be harder, it would be
impossible.

You are being told this up front because finding out later is what makes open
core feel like a bait and switch. The line it will be drawn on is below.

The CLA does not take your rights away. You keep the copyright to what you
wrote and can use it anywhere else you like. What you grant is permission to
license it under other terms as well.

Signing is a one-off. It is not needed for typos, documentation fixes, or
anything that is not copyrightable.

## What will never be sold

On the record before there is anything to sell. When a paid tier exists, none
of these will be part of it, and no version of this project will meter them:

- the deploy flow itself
- the number of applications, servers and **users**
- the CLI and the API
- backups, and the rehearsal that proves a backup actually restores
- signature verification and policy enforcement
- the page that shows what is running and what was proved about it

What is meant to be sold is what only a large organisation needs: SSO, fine
grained roles, a long-lived tamper-evident audit archive, approval workflows,
multi-cluster, and support with an SLA.

## Getting set up

Requires Docker, [kind](https://kind.sigs.k8s.io/), kubectl, Helm, Terraform,
Node.js 20+ and Go. `make up` applies the platform layer with Terraform, and the
unit suite runs on Node.

```bash
make up          # cluster + ingress + build + deploy
make test        # unit and integration tests, no cluster needed
make lint        # ESLint + helm lint + terraform fmt/validate
make smoke       # end-to-end checks against the running deployment
make policy-test # prove each admission rule rejects what it should
```

The operator has its own suite:

```bash
make -C operator test   # unit + envtest, regenerates CRDs and deepcopy first
make -C operator lint
```

## What CI will check

A pull request has to pass the first five gates. The last row runs only on
`main`, after the merge:

| Job | What it does |
|---|---|
| Lint and test | ESLint and the unit suite |
| Validate manifests | `helm lint`, every values profile rendered, kubeconform against the Kubernetes and CRD schemas, terraform fmt and validate, hadolint |
| Build and scan image | builds each image, asserts it runs non-root under a read-only root filesystem, Trivy fails the build on CRITICAL/HIGH |
| Build and test the operator | the Go suite, plus a check that the committed generated code matches what the types produce |
| Deploy to a real cluster | a kind cluster, the policies, the chart, a zero-downtime upgrade, the smoke test, and a Workload reaching Ready through admission |
| Publish / Enforce signatures | images built natively per architecture, signed with keyless cosign, then proved against Kyverno in a fresh cluster |

If you change the operator's API types, run `make -C operator manifests generate`
and commit the result. CI fails when the generated files are stale.

## Conventions

**Comments explain why, not what.** The code says what it does. A comment earns
its place by recording the reason a decision was made, the alternative that was
rejected, or the failure that made it necessary. Several comments in this
repository exist because something broke in a measurable way; that measurement
belongs next to the fix.

**Numbers are measured, not estimated.** If a comment or a README claims a
duration, a count, or a rate, it came from a run. Say where it came from.

**A rule that is not tested is decoration.** Admission policies have
`scripts/policy-test.sh`, which proves each one rejects what it is meant to and
admits what it is meant to. New rules need both halves.

**Commit messages** are lowercase, describe the change, and say why in the body
when the why is not obvious. `git log` is the record of reasoning.

## Reporting security issues

Do not open a public issue. Email **damgahq@gmail.com** with what you found and
how to reproduce it. Scope, disclosure timing and what is already enforced are
in [SECURITY.md](SECURITY.md).
