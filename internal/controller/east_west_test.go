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

package controller

import (
	"os"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	testEntry = "n8n"

	// One of the entry's workloads. Which one it is does not matter to any
	// case here: the policy is rendered per workload and the rule it gains
	// names the entry, not the object.
	testEntryWorkload = "worker"
)

func fromCatalogue() *platformv1alpha1.Workload {
	return &platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name: testEntryWorkload, Namespace: "acme-prod",
			Labels: map[string]string{composeGroupLabel: testEntry},
		},
		Spec: platformv1alpha1.WorkloadSpec{Image: "example.test/app:1", Port: 8080},
	}
}

// The gap this closes, as the shape of a policy: a catalogue entry with a front
// end and a worker installs cleanly and cannot talk to itself.
//
// Measured on a real cluster before the rule existed — a probe carrying a
// workload's own labels reached https://example.com with a 200 and a sibling
// Service with 000. Egress was open the whole time; the destination's ingress
// admitted the ingress controller and nobody else.
func TestAnEntrysWorkloadsMayReachEachOther(t *testing.T) {
	policy := desiredNetworkPolicy(fromCatalogue())

	if len(policy.Spec.Ingress) != 2 {
		t.Fatalf("the policy has %d ingress rule(s); one admits the ingress controller and the "+
			"second is the one that lets an entry's own workloads reach each other",
			len(policy.Spec.Ingress))
	}
	sibling := policy.Spec.Ingress[1]
	if len(sibling.From) != 1 || sibling.From[0].PodSelector == nil {
		t.Fatalf("the second rule does not admit pods: %+v", sibling.From)
	}
	if got := sibling.From[0].PodSelector.MatchLabels[composeGroupLabel]; got != testEntry {
		t.Errorf("the rule admits entry %q, want %q — a rule keyed on anything else admits "+
			"either nothing or everything", got, testEntry)
	}
}

// The half that keeps this a door rather than a demolition.
//
// A bare podSelector means "in this policy's own namespace". Adding a
// namespaceSelector beside it would join every tenant that installed the same
// entry, because they all carry the same value — two customers who both
// installed n8n would be reachable from each other, and nothing in the diff
// would say so.
func TestTheRuleCannotReachIntoAnotherNamespace(t *testing.T) {
	policy := desiredNetworkPolicy(fromCatalogue())
	peer := policy.Spec.Ingress[1].From[0]

	if peer.NamespaceSelector != nil {
		t.Fatalf("the sibling rule carries a namespaceSelector (%+v). Every tenant that "+
			"installs this entry writes the same label value, so this admits their pods into "+
			"this one's application", peer.NamespaceSelector)
	}
	if peer.IPBlock != nil {
		t.Fatalf("the sibling rule carries an ipBlock: %+v", peer.IPBlock)
	}
}

// A workload nobody installed from the catalogue keeps the policy it had.
func TestAWorkloadWithNoEntryIsUnchanged(t *testing.T) {
	plain := &platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "acme-prod"},
		Spec:       platformv1alpha1.WorkloadSpec{Image: "example.test/app:1", Port: 8080},
	}
	policy := desiredNetworkPolicy(plain)

	if len(policy.Spec.Ingress) != 1 {
		t.Fatalf("a workload with no entry label got %d ingress rules; the second rule is for "+
			"siblings and it has none", len(policy.Spec.Ingress))
	}
	if _, leaked := labelsFor(plain)[composeGroupLabel]; leaked {
		t.Error("a workload with no entry label was given one")
	}
}

// The rule selects pods, so the label has to be on the pod — and it must not be
// on the Deployment's selector, which is immutable: adding it there would make
// every Deployment that already exists refuse its next update.
func TestTheEntryLabelReachesThePodAndNotTheSelector(t *testing.T) {
	app := fromCatalogue()
	deploy := desiredDeployment(app)

	if got := deploy.Spec.Template.Labels[composeGroupLabel]; got != testEntry {
		t.Errorf("the pod carries entry %q, want %q — a NetworkPolicy selects pods, and a "+
			"label that stops at the Workload selects nothing", got, testEntry)
	}
	if _, inSelector := deploy.Spec.Selector.MatchLabels[composeGroupLabel]; inSelector {
		t.Fatal("the entry label is in .spec.selector, which is immutable: every Deployment " +
			"that already exists would refuse its next update with a field-is-immutable error")
	}
}

// A pod template field is hashed by the deployment controller, so an unstable
// value here is a rollout on every reconcile rather than one rollout ever.
func TestTheEntryLabelIsStableAcrossRenders(t *testing.T) {
	app := fromCatalogue()
	first, second := desiredDeployment(app), desiredDeployment(app)

	if !reflect.DeepEqual(first.Spec.Template.Labels, second.Spec.Template.Labels) {
		t.Fatalf("two renders of one workload produced different pod labels:\n%v\n%v",
			first.Spec.Template.Labels, second.Spec.Template.Labels)
	}
}

// The label's name is a contract with the converter, which keeps its own copy
// unexported — so this is the only thing holding the two together. A mismatch
// is silent in the worst way: every entry installs, every policy is rendered,
// and no workload can reach its siblings because the rule selects a label
// nothing writes.
func TestTheConverterWritesTheLabelThisRuleSelects(t *testing.T) {
	const converter = "../../compose/convert.go"

	body, err := os.ReadFile(converter)
	if err != nil {
		t.Fatalf("the converter is unreadable: %v", err)
	}
	if !strings.Contains(string(body), `"`+composeGroupLabel+`"`) {
		t.Fatalf("%s does not write %q. This package's east-west rule selects that label, so "+
			"a converter writing another one produces entries whose workloads cannot reach "+
			"each other — and nothing fails until somebody tries", converter, composeGroupLabel)
	}
}
