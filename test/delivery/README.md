# A directory for Argo CD to apply

Two objects with no purpose other than being applied by something else. CI
points an Argo CD `Application` at this path, waits for the namespace to appear,
removes one of its labels by hand and waits for it to come back.

It is a fixture rather than the platform's own fence — `internal/manifest`'s
`Fence` renders that one, per tenant, and a Go test binds its contents to
`policies/`. What is under test here is not what the fence says; it is whether
Argo CD puts a label back after somebody takes it off.

That question has an answer this project measured and was surprised by. A
namespace created through `CreateNamespace=true` and labelled through
`managedNamespaceMetadata` keeps its labels only until somebody removes one: the
label was never restored — not in four minutes, not after a forced sync, not
after a hard refresh — and the `Application` called itself Synced and Healthy
the whole time. The same objects committed as ordinary manifests, which is what
this directory holds, came back in about five seconds.

The design turns on that difference, so it is measured on every run rather than
trusted to stay true across an Argo CD upgrade.
