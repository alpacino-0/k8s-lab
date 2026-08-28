/*
Copyright 2026 Orhan Yavuz.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package forge

import (
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// PolicyFile is what the rendered policy is called inside a placement's
// directory, beside the workload manifest.
//
// Fixed rather than derived, for the reason manifest.File is: the directory
// already names the app, and a second copy of the name in the filename is a
// second thing to keep in step.
const PolicyFile = "signature-policy.yaml"

// The two things a rendered policy can do, and the transition between them is
// the only one this package has: recording until a signature carrying this
// identity has been seen, rejecting afterwards.
const (
	actionAudit   = "Audit"
	actionEnforce = "Enforce"
)

// Policy renders the admission rule that accepts this connection's identity and
// no other.
//
// Namespace-scoped on purpose, and that is what makes per-tenant identity
// expressible at all: a cluster-wide rule would have to name every tenant's
// workflow in one attestor list, and any tenant's identity would then be
// accepted for any tenant's images. `kyverno.io/v1 Policy` with scope
// Namespaced was measured working for this on the installed 1.15.2 — no
// upgrade, and none of the newer CEL policy types help, because
// NamespacedImageValidatingPolicy does not exist and ImageValidatingPolicy is
// cluster-scoped.
func (c Connection) Policy(namespace string) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// The namespace is an argument and not a field on the connection, because
	// one connection produces one of these per environment the app runs in.
	if strings.TrimSpace(namespace) == "" {
		return nil, fmt.Errorf("forge: a policy needs a namespace to be scoped to")
	}

	action, reason := actionAudit, "no signature carrying this identity has been seen yet"
	if c.Verified() {
		action = actionEnforce
		reason = "first signature seen at " + c.FirstSignatureAt.UTC().Format(time.RFC3339)
	}

	p := map[string]any{
		"apiVersion": "kyverno.io/v1",
		"kind":       "Policy",
		"metadata": map[string]any{
			"name":      "damga-image-signature",
			"namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "damga-platform",
				"damga.co/tenant":              c.TenantID,
			},
			"annotations": map[string]any{
				// The subject is repeated here in prose because the person
				// reading `kubectl get policy -o yaml` during an incident is
				// asking "why was my image rejected", and the answer is a
				// string comparison they can make by eye against the signature.
				"damga.co/identity": c.Identity(),
				// Why this policy is or is not rejecting anything, in the
				// place somebody reads during an incident.
				"damga.co/enforcement": reason,
			},
		},
		"spec": map[string]any{
			// Audit until this identity has produced a signature, Enforce
			// afterwards.
			//
			// Audit forever would be the failure — a rule that records the
			// thing it was installed to prevent. Audit until the chain is
			// proven once is a rollout, and the difference is that this one
			// ends. Applying it the other way round refuses the tenant's next
			// deploy for a workflow that has never run, which is connecting a
			// repository and having deploys stop.
			"validationFailureAction": action,
			// Existing workloads are not re-admitted when this lands, because a
			// tenant who connects a repository today would otherwise have
			// everything already running evicted by a rule about images built
			// before the rule existed.
			"background": false,
			"rules": []any{map[string]any{
				"name": "verify-tenant-signature",
				"match": map[string]any{
					"any": []any{map[string]any{
						"resources": map[string]any{
							"kinds": []any{"Pod"},
						},
					}},
				},
				"verifyImages": []any{map[string]any{
					// Only this tenant's images. Without the scope the rule
					// would demand a signature from this tenant's workflow on
					// every image in the namespace, including sidecars and
					// anything the platform itself puts there.
					"imageReferences": []any{c.ImageRepository + "*"},
					// Rewrite the verified tag to the digest that was actually
					// checked. Without it a signed digest is verified and then a
					// mutable tag is pulled, which is a different image by the
					// time it runs.
					"mutateDigest": true,
					"required":     true,
					"attestors": []any{map[string]any{
						"count": 1,
						"entries": []any{map[string]any{
							"keyless": map[string]any{
								// One string, from one function, shared with the
								// workflow that will produce it. See Identity.
								"subject": c.Identity(),
								"issuer":  OIDCIssuer,
								// The trigger is in the certificate, so it is
								// checked. Without it, a workflow_dispatch run
								// of the same file on the same branch is an
								// accepted identity that no push ever reviewed.
								"additionalExtensions": map[string]any{
									"githubWorkflowTrigger": WorkflowTrigger,
								},
								"rekor": map[string]any{"url": RekorURL},
							},
						}},
					}},
				}},
			}},
		},
	}

	out, err := yaml.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("forge: rendering policy: %w", err)
	}
	return out, nil
}
