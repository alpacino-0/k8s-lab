# Generated. Do not edit.

These two files are what `internal/manifest.Fence("damga-gitops")` renders, byte
for byte, and `TestTheReferenceTenantIsUnderTheFenceTenantsGet` fails when they
are not. They carry no comments because a comment is a byte that differs.

The reference tenant is a tenant. It gets its namespace and its quota the way
every other tenant does — as manifests Argo CD applies, so `selfHeal` puts them
back when somebody takes them down. It used to get them through
`managedNamespaceMetadata`, which is the mechanism that does not: measured
against Argo CD v3.1.8, a pod-security label removed from a namespace managed
that way was never restored, and the Application reported Synced and Healthy
throughout.

To change the fence, change `internal/manifest/fence.go` and regenerate.
