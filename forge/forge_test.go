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
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The app, the repository and the last segment of the image are the same word
// in the common case, and that is worth spelling once: a fixture that gives
// them three different values would pass while hiding which of the three the
// identity is actually built from.
const (
	appName = "shop"

	// One connection renders one of these per environment the app runs in,
	// which is why the namespace is an argument rather than a field.
	testNamespace = "acme-prod"
)

func conn(mutate ...func(*Connection)) Connection {
	c := Connection{
		TenantID:        "t_acme",
		App:             appName,
		Host:            "github.com",
		Owner:           "acme",
		Repo:            appName,
		Branch:          "main",
		WorkflowPath:    ".github/workflows/damga-sign.yml",
		ImageRepository: "ghcr.io/acme/" + appName,
	}
	for _, m := range mutate {
		m(&c)
	}
	return c
}

// The one that matters.
//
// Two files are rendered from one connection and neither Kubernetes, Sigstore
// nor git checks that they agree. If the policy pins an identity the workflow
// will not present, every deploy is rejected and looks like an outage; if it
// pins a broader one, nothing fails and the guarantee is gone — which is the
// direction that does not announce itself.
//
// So this rebuilds the identity from what the workflow file actually says —
// where it lives, which branch it triggers on, which event — and compares it to
// what the policy actually pins. Reading the constant from both sides would
// prove nothing; the point is that the rendered artefacts agree.
func TestIdentityIsOne(t *testing.T) {
	c := conn()

	workflow, err := c.Workflow()
	if err != nil {
		t.Fatalf("rendering workflow: %v", err)
	}
	policy, err := c.Policy(testNamespace)
	if err != nil {
		t.Fatalf("rendering policy: %v", err)
	}

	// What the workflow itself says it runs on.
	branches := pushBranches(t, workflow)
	if len(branches) != 1 {
		t.Fatalf("the workflow triggers on %v, and an identity names one branch — "+
			"a second branch is a second identity the policy does not pin", branches)
	}

	// Rebuilt from the file, not from the connection.
	fromWorkflow := "https://" + c.Host + "/" + c.Owner + "/" + c.Repo + "/" +
		c.WorkflowPath + "@refs/heads/" + branches[0]

	fromPolicy := keylessSubject(t, policy)

	if fromWorkflow != fromPolicy {
		t.Errorf("the workflow will sign as\n  %s\nand the policy accepts\n  %s\n"+
			"nothing in kubernetes, sigstore or git compares these two, so this "+
			"ships as either every deploy rejected or a signature rule that is "+
			"not checking what it claims", fromWorkflow, fromPolicy)
	}
}

// The trigger is half of the pin and is easy to drop, because the workflow
// expresses it as a yaml key and the policy as a certificate extension. They do
// not look alike, so nothing about editing one suggests editing the other.
func TestTriggerIsPinnedOnBothSides(t *testing.T) {
	c := conn()

	workflow, err := c.Workflow()
	if err != nil {
		t.Fatalf("rendering workflow: %v", err)
	}
	on := triggers(t, workflow)
	if len(on) != 1 {
		t.Errorf("the workflow triggers on %v; every trigger beyond %q presents a "+
			"different ref for the same file, so the branch pin stops constraining it",
			keys(on), WorkflowTrigger)
	}
	if _, ok := on[WorkflowTrigger]; !ok {
		t.Errorf("the workflow does not trigger on %q, which is what the policy checks "+
			"for in the certificate", WorkflowTrigger)
	}

	policy, err := c.Policy(testNamespace)
	if err != nil {
		t.Fatalf("rendering policy: %v", err)
	}
	if got := extension(t, policy, "githubWorkflowTrigger"); got != WorkflowTrigger {
		t.Errorf("policy pins trigger %q, workflow runs on %q", got, WorkflowTrigger)
	}
}

// Changing the connection has to move both halves, or the agreement above holds
// only for the one connection somebody happened to write a test for.
func TestBothHalvesFollowTheConnection(t *testing.T) {
	for name, mutate := range map[string]func(*Connection){
		"a different branch": func(c *Connection) { c.Branch = "release" },
		"a different file":   func(c *Connection) { c.WorkflowPath = ".github/workflows/sign.yml" },
		"a different repo":   func(c *Connection) { c.Repo = "storefront" },
		"a different owner":  func(c *Connection) { c.Owner = "globex" },
	} {
		t.Run(name, func(t *testing.T) {
			base, other := conn(), conn(mutate)

			bp, err := base.Policy(testNamespace)
			if err != nil {
				t.Fatalf("rendering base policy: %v", err)
			}
			op, err := other.Policy(testNamespace)
			if err != nil {
				t.Fatalf("rendering policy: %v", err)
			}
			if keylessSubject(t, bp) == keylessSubject(t, op) {
				t.Error("the identity did not change, so two different repositories " +
					"are accepted under one subject")
			}

			ow, err := other.Workflow()
			if err != nil {
				t.Fatalf("rendering workflow: %v", err)
			}
			if !strings.Contains(keylessSubject(t, op), other.Branch) {
				t.Error("the policy subject does not name this connection's branch")
			}
			// And the workflow moved with it.
			if got := pushBranches(t, ow); len(got) != 1 || got[0] != other.Branch {
				t.Errorf("the workflow triggers on %v, want [%s]", got, other.Branch)
			}
			if !strings.Contains(string(ow), other.Branch) {
				t.Error("the workflow does not mention this connection's branch, so it " +
					"would not run where the policy expects the signature to come from")
			}
		})
	}
}

// Every one of these renders a subject that is either wider than the one
// workflow the tenant approved, or that matches nothing at all. The second kind
// is survivable and loud; the first is the whole reason this design exists.
func TestAnIdentityThatWouldBeTooWideIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*Connection){
		"a wildcard repo":        func(c *Connection) { c.Repo = "*" },
		"a wildcard owner":       func(c *Connection) { c.Owner = "acme*" },
		"a wildcard branch":      func(c *Connection) { c.Branch = "*" },
		"no branch":              func(c *Connection) { c.Branch = "" },
		"no workflow path":       func(c *Connection) { c.WorkflowPath = "" },
		"a path outside actions": func(c *Connection) { c.WorkflowPath = "sign.yml" },
		"a ref where a branch goes": func(c *Connection) {
			c.Branch = "refs/heads/main"
		},
		"owner and repo in one field": func(c *Connection) {
			c.Owner, c.Repo = "acme/shop", "shop"
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := conn(mutate)
			if err := c.Validate(); err == nil {
				t.Errorf("accepted, and would render the subject %q", c.Identity())
			}
			if _, err := c.Policy(testNamespace); err == nil {
				t.Error("a policy was rendered from a connection that cannot name one workflow")
			}
			if _, err := c.Workflow(); err == nil {
				t.Error("a workflow was rendered for an identity no policy can pin")
			}
		})
	}
}

// A forge public Fulcio does not accept has to be refused by name rather than
// quietly verified against something weaker. The tenant is on a different tier
// of evidence and the product's position is that the tier is shown, not hidden.
func TestAForgeThatCannotSignIsRefusedByName(t *testing.T) {
	c := conn(func(c *Connection) { c.Host = "git.internal.example" })
	err := c.Validate()
	if err == nil {
		t.Fatal("accepted a host whose keyless identity this build cannot express")
	}
	if !strings.Contains(err.Error(), "git.internal.example") {
		t.Errorf("the error does not name the host, so the operator cannot tell which "+
			"connection is on a weaker tier: %v", err)
	}
}

// The policy has to be namespace-scoped, because that is the only thing making
// per-tenant identity expressible: one cluster-wide rule would list every
// tenant's workflow as an accepted attestor for every tenant's images.
func TestThePolicyIsScopedToOneTenant(t *testing.T) {
	c := conn()
	policy, err := c.Policy(testNamespace)
	if err != nil {
		t.Fatalf("rendering policy: %v", err)
	}

	var p struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			ValidationFailureAction string `json:"validationFailureAction"`
			Rules                   []struct {
				VerifyImages []struct {
					ImageReferences []string `json:"imageReferences"`
					MutateDigest    bool     `json:"mutateDigest"`
					Required        bool     `json:"required"`
				} `json:"verifyImages"`
			} `json:"rules"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(policy, &p); err != nil {
		t.Fatalf("parsing policy: %v", err)
	}

	if p.Kind != "Policy" {
		t.Errorf("kind = %q; a ClusterPolicy would accept every tenant's identity for "+
			"every tenant's images", p.Kind)
	}
	if p.Metadata.Namespace != testNamespace {
		t.Errorf("namespace = %q, want %q", p.Metadata.Namespace, testNamespace)
	}
	if p.Spec.ValidationFailureAction != "Enforce" {
		t.Errorf("action = %q; an audit-only signature rule records the thing it was "+
			"installed to prevent", p.Spec.ValidationFailureAction)
	}

	vi := p.Spec.Rules[0].VerifyImages[0]
	if !strings.HasPrefix(vi.ImageReferences[0], c.ImageRepository) {
		t.Errorf("imageReferences = %v; unscoped, this demands this tenant's signature "+
			"on every image in the namespace, sidecars included", vi.ImageReferences)
	}
	if !vi.MutateDigest {
		t.Error("mutateDigest is off, so a signed digest is verified and a mutable tag " +
			"is pulled — a different image by the time it runs")
	}
	if !vi.Required {
		t.Error("required is off, so an image with no signature at all passes")
	}
}

// Helpers below. They fail the test rather than returning errors, because every
// caller above would do the same thing with the error.

func keylessSubject(t *testing.T, policy []byte) string {
	t.Helper()
	var p struct {
		Spec struct {
			Rules []struct {
				VerifyImages []struct {
					Attestors []struct {
						Entries []struct {
							Keyless struct {
								Subject string `json:"subject"`
							} `json:"keyless"`
						} `json:"entries"`
					} `json:"attestors"`
				} `json:"verifyImages"`
			} `json:"rules"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(policy, &p); err != nil {
		t.Fatalf("parsing policy: %v", err)
	}
	return p.Spec.Rules[0].VerifyImages[0].Attestors[0].Entries[0].Keyless.Subject
}

func extension(t *testing.T, policy []byte, name string) string {
	t.Helper()
	var p struct {
		Spec struct {
			Rules []struct {
				VerifyImages []struct {
					Attestors []struct {
						Entries []struct {
							Keyless struct {
								AdditionalExtensions map[string]string `json:"additionalExtensions"`
							} `json:"keyless"`
						} `json:"entries"`
					} `json:"attestors"`
				} `json:"verifyImages"`
			} `json:"rules"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(policy, &p); err != nil {
		t.Fatalf("parsing policy: %v", err)
	}
	return p.Spec.Rules[0].VerifyImages[0].Attestors[0].Entries[0].Keyless.AdditionalExtensions[name]
}

// triggers reads the workflow's `on:` block.
//
// The lookup is under the key "true", which is not a typo. `on` is a YAML 1.1
// boolean, so sigs.k8s.io/yaml — which converts to JSON on the way in — turns
// the bare key into "true". GitHub's parser does not do this and neither does
// the signature; only Go-side readers of the file see it. It is checked both
// ways here so that quoting the key in the template, which would be the obvious
// "fix", fails loudly instead of silently changing what the file looks like to
// the person being asked to merge it.
func triggers(t *testing.T, workflow []byte) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := yaml.Unmarshal(workflow, &raw); err != nil {
		t.Fatalf("the workflow is not parseable yaml, so GitHub would not run it: %v", err)
	}
	for _, key := range []string{"true", "on"} {
		if on, ok := raw[key].(map[string]any); ok {
			return on
		}
	}
	t.Fatalf("the workflow has no trigger block; keys are %v", keys(raw))
	return nil
}

func pushBranches(t *testing.T, workflow []byte) []string {
	t.Helper()
	push, ok := triggers(t, workflow)[WorkflowTrigger].(map[string]any)
	if !ok {
		t.Fatalf("the workflow does not trigger on %q", WorkflowTrigger)
	}
	raw, ok := push["branches"].([]any)
	if !ok {
		t.Fatal("the push trigger names no branches, so it runs on every branch and " +
			"the identity the policy pins is one of many the file can present")
	}
	out := make([]string, 0, len(raw))
	for _, b := range raw {
		out = append(out, b.(string))
	}
	return out
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// One connection, several environments. This is the shape the key correction
// exists for: the identity is a property of the source repository and does not
// move between environments, while the policy is namespace-scoped and there is
// one per environment the app runs in.
func TestOneConnectionPolicesEveryEnvironment(t *testing.T) {
	c := conn()

	subjects := map[string]string{}
	for _, ns := range []string{"acme-dev", "acme-staging", "acme-prod"} {
		policy, err := c.Policy(ns)
		if err != nil {
			t.Fatalf("rendering policy for %s: %v", ns, err)
		}
		var p struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(policy, &p); err != nil {
			t.Fatalf("parsing policy: %v", err)
		}
		if p.Metadata.Namespace != ns {
			t.Errorf("policy for %s landed in %s", ns, p.Metadata.Namespace)
		}
		subjects[ns] = keylessSubject(t, policy)
	}

	for ns, got := range subjects {
		if got != c.Identity() {
			t.Errorf("the identity changed with the environment: %s pins %q, want %q — "+
				"an app has one source repository and one signing identity, and "+
				"deploys to several environments out of it", ns, got, c.Identity())
		}
	}
}

func TestAPolicyNeedsSomewhereToLive(t *testing.T) {
	if _, err := conn().Policy(""); err == nil {
		t.Error("rendered a namespace-scoped policy with no namespace, which applies " +
			"nowhere while looking like a rule that is in force")
	}
}
