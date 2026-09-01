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

**Said before you contribute rather than after.** This project used to plan an
open-core model: this repository fully AGPL-3.0, and a separate closed
repository holding enterprise features — SSO, fine-grained RBAC, an audit
archive, approval workflows — sold under a commercial licence.

**That plan was dropped on 2026-08-29.** There is no enterprise repository, no
paid tier, and nothing held back for one. The product is one piece and all of it
is in this repository.

The CLA predates that decision, and the purpose it was written for went with it.
It stays for now for one narrower reason: it keeps open the option of changing
the project's licence later without having to find and ask every past
contributor. Whether that option is worth the friction it costs is an open
question — and if the answer turns out to be no, the CLA will be removed rather
than quietly kept.

The CLA does not take your rights away. You keep the copyright to what you
wrote and can use it anywhere else you like. What you grant is permission to
license it under other terms as well.

Signing is a one-off. It is not needed for typos, documentation fixes, or
anything that is not copyrightable.

## Nothing is metered

There is no paid tier, so there is no line to draw. Everything the product does
is here, under AGPL-3.0:

- the deploy flow itself
- unlimited applications, servers and **users**
- the CLI and the API
- backups, and the rehearsal that proves a backup actually restores
- signature verification and policy enforcement
- the page that shows what is running and what was proved about it

If that ever changes, it will be announced as a change — not discovered.

## Getting set up

Requires Docker, [kind](https://kind.sigs.k8s.io/), kubectl, Helm, Terraform,
jq, Node.js 20+ and Go. `make up` applies the platform layer with Terraform and
uses jq to approve the kubelet serving certificates; the unit suite runs on Node.

```bash
make up          # cluster + ingress + build + deploy
make test        # unit and integration tests, no cluster needed
make lint        # ESLint + helm lint + terraform fmt/validate
make smoke       # end-to-end checks against the running deployment
make alert-test  # break the service and prove the alert reaches Alertmanager
```

Some of the design notes this repository is written against — the plan and its
scope boundary, the decisions that are closed, what works today and what is
owed — are deliberately not committed. If you are working in a linked worktree
of a checkout that has them, `scripts/link-docs.sh` puts them where you can read
them; `docs/WORKING-NOTES.md` is committed and explains what they are. If you
have cloned this repository from GitHub you will not have them at all, and
nothing here depends on them.

The operator has its own suite:

```bash
make -f Makefile.operator test   # unit + envtest, regenerates CRDs and deepcopy first
make -f Makefile.operator lint
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
| Publish | images built natively per architecture, signed with keyless cosign, then verified |

If you change the operator's API types, run `make -f Makefile.operator manifests generate`
and commit the result. CI fails when the generated files are stale.

## Conventions

**Comments explain why, not what.** The code says what it does. A comment earns
its place by recording the reason a decision was made, the alternative that was
rejected, or the failure that made it necessary. Several comments in this
repository exist because something broke in a measurable way; that measurement
belongs next to the fix.

**Numbers are measured, not estimated.** If a comment or a README claims a
duration, a count, or a rate, it came from a run. Say where it came from.

**A rule that is not tested is decoration.** Every fix is bound to a test that
fails with its own message when the fix is reverted — a test satisfied for the
wrong reason keeps passing after the guard it was written for is gone.

**Commit messages** are lowercase, describe the change, and say why in the body
when the why is not obvious. `git log` is the record of reasoning.

## Reporting security issues

Do not open a public issue. Email **damgahq@gmail.com** with what you found and
how to reproduce it. Scope, disclosure timing and what is already enforced are
in [SECURITY.md](SECURITY.md).
