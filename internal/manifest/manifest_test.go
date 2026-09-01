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

package manifest_test

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/internal/manifest"
)

const (
	appName   = "api"
	namespace = "acme-prod"
)

func workload() platformv1alpha1.Workload {
	return platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: namespace},
		Spec: platformv1alpha1.WorkloadSpec{
			Image:    "ghcr.io/acme/api:1.4.2",
			Port:     8080,
			Replicas: ptr.To(int32(3)),
			Domain:   "api.acme.example",
			Env:      []platformv1alpha1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
		},
	}
}

// The round trip is what makes a deploy that changes one field safe. If
// anything is lost here, changing the image silently removes it.
func TestEverySettingSurvivesTheRoundTrip(t *testing.T) {
	body, err := manifest.Render(workload(), "rollout-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	back, err := manifest.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := workload()
	switch {
	case back.Spec.Image != want.Spec.Image:
		t.Errorf("Image = %q", back.Spec.Image)
	case back.Spec.Port != want.Spec.Port:
		t.Errorf("Port = %d", back.Spec.Port)
	case back.Spec.Replicas == nil || *back.Spec.Replicas != *want.Spec.Replicas:
		t.Errorf("Replicas = %v", back.Spec.Replicas)
	case back.Spec.Domain != want.Spec.Domain:
		t.Errorf("Domain = %q", back.Spec.Domain)
	case len(back.Spec.Env) != 1 || back.Spec.Env[0].Value != "info":
		t.Errorf("Env = %v", back.Spec.Env)
	case back.Name != want.Name || back.Namespace != want.Namespace:
		t.Errorf("identity = %s/%s", back.Namespace, back.Name)
	}
}

// The chain that lets a record opened at commit time be closed by what
// happened in the cluster: the id is on the Workload, the operator carries it
// to the Deployment, the observer reads it there.
func TestTheRolloutIDIsInTheFile(t *testing.T) {
	body, err := manifest.Render(workload(), "t_alpha-api-prod-41")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(body), "damga.co/rollout: t_alpha-api-prod-41") {
		t.Errorf("the rollout id is not in the file:\n%s", body)
	}
	if !strings.Contains(string(body), "apiVersion: platform.damga.co/v1alpha1") {
		t.Errorf("the file has no apiVersion:\n%s", body)
	}
	if !strings.Contains(string(body), "kind: Workload") {
		t.Errorf("the file has no kind:\n%s", body)
	}
}

// A committed status is a claim about the world made by whoever last edited a
// file. The cluster answers that question.
func TestStatusIsNeverCommitted(t *testing.T) {
	w := workload()
	w.Status = platformv1alpha1.WorkloadStatus{
		Conditions: []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue,
			Reason: "Invented", LastTransitionTime: metav1.Now(),
		}},
	}
	body, err := manifest.Render(w, "rollout-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(body), "Invented") {
		t.Errorf("a status reached the committed file:\n%s", body)
	}
}

// A field this build does not know must be an error, not a silent drop. The
// alternative is a deploy that changes the image and removes a setting added
// by a newer damga — data loss nobody would attribute to the deploy.
func TestAnUnknownFieldIsRefusedRatherThanDropped(t *testing.T) {
	body := []byte(`apiVersion: platform.damga.co/v1alpha1
kind: Workload
metadata:
  name: api
  namespace: acme-prod
spec:
  image: ghcr.io/acme/api:1.4.2
  somethingFromANewerDamga: true
`)
	if _, err := manifest.Parse(body); err == nil {
		t.Error("a field this build does not know was silently dropped")
	}
}

func TestWhatIsNotAWorkloadIsRefused(t *testing.T) {
	body := []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n")
	if _, err := manifest.Parse(body); err == nil {
		t.Error("a Deployment parsed as a Workload")
	}
	for _, w := range []platformv1alpha1.Workload{
		{ObjectMeta: metav1.ObjectMeta{Namespace: namespace}, Spec: platformv1alpha1.WorkloadSpec{Image: "x"}},
		{ObjectMeta: metav1.ObjectMeta{Name: appName}, Spec: platformv1alpha1.WorkloadSpec{Image: "x"}},
		{ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: namespace}},
	} {
		if _, err := manifest.Render(w, ""); err == nil {
			t.Errorf("Render accepted %+v", w.ObjectMeta)
		}
	}
}

// Ownership is decided by what is in the file, because the alternative is a
// filename convention and the first rename makes a convention wrong in the
// expensive direction: a file that is ours and does not look it stops being
// maintained, and a file that looks ours and is not gets deleted.
func TestOwnsRecognisesThisPlatformsFilesAndNothingElse(t *testing.T) {
	rendered, err := manifest.Render(platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: namespace},
		Spec:       platformv1alpha1.WorkloadSpec{Image: "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64)},
	}, "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Owns(rendered) {
		t.Fatal("a file this package rendered is not recognised as this platform's, so a " +
			"render that stops producing it could never remove it")
	}

	for name, body := range map[string]string{
		"a kustomization":     "resources:\n  - workload.yaml\n",
		"somebody's Secret":   "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n",
		"another operator's":  "apiVersion: kyverno.io/v1\nkind: ClusterPolicy\n",
		"a README":            "# state\n\nthis directory is managed by damga\n",
		"not YAML at all":     "\x00\x01binary\n",
		"YAML that is a list": "- one\n- two\n",
		"empty":               "",
	} {
		if manifest.Owns([]byte(body)) {
			t.Errorf("%s was claimed as this platform's; claiming somebody else's file "+
				"removes work from a repository they cannot push to", name)
		}
	}
}

// The primary keeps its fixed name and everything beside it is named for what
// it is, so a directory holding six manifests is readable by somebody running
// ls — and so nothing can take the name every deploy reads and rewrites.
func TestFileForNamesAnObjectWithoutTakingThePrimarysName(t *testing.T) {
	for _, tc := range []struct{ kind, name, want string }{
		{"Workload", "worker", "workload-worker.yaml"},
		{"Database", "db", "database-db.yaml"},
	} {
		if got := manifest.FileFor(tc.kind, tc.name); got != tc.want {
			t.Errorf("FileFor(%q, %q) = %q, want %q", tc.kind, tc.name, got, tc.want)
		}
	}
	if got := manifest.FileFor("Workload", "worker"); got == manifest.File {
		t.Fatalf("a second object took the primary manifest's name (%q), which is the one "+
			"file every deploy reads and rewrites", got)
	}
}

// A database is committed like everything else here, and recognised like
// everything else here — that second half is the one that matters. The claim in
// RenderDatabase's comment is that keeping the renderer beside Owns is what
// stops the two drifting into a file the platform writes and does not know is
// its own, and a file it does not know is its own is a file it can never remove.
func TestARenderedDatabaseIsRecognisedAsThisPlatformsOwn(t *testing.T) {
	body, err := manifest.RenderDatabase(platformv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: namespace},
		Spec:       platformv1alpha1.DatabaseSpec{Engine: platformv1alpha1.EnginePostgres},
		Status:     platformv1alpha1.DatabaseStatus{SecretName: "db-credentials"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Owns(body) {
		t.Fatalf("a database this package rendered is not recognised as this platform's:\n%s", body)
	}
	if strings.Contains(string(body), "db-credentials") {
		t.Errorf("the committed database carries a status, which is a claim about the world "+
			"made by whoever last edited a file:\n%s", body)
	}
	// No rollout id, ever. The observer closes a record by finding one live
	// object that claims it, and a second claimant closes it on whichever it
	// saw first.
	if strings.Contains(string(body), "rollout") {
		t.Errorf("the database carries a rollout annotation:\n%s", body)
	}

	if _, err := manifest.RenderDatabase(platformv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "db"},
	}); err == nil {
		t.Error("a database with no namespace was rendered; the namespace is where it lands " +
			"and nothing downstream would supply one")
	}
}
