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
	"context"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

func database(engine platformv1alpha1.DatabaseEngine, image string) *platformv1alpha1.Database {
	db := &platformv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "team-a"},
		Spec: platformv1alpha1.DatabaseSpec{
			Engine: engine, Image: image, Storage: resource.MustParse("1Gi"),
		},
	}
	normaliseDatabase(db)
	return db
}

func redisDB() *platformv1alpha1.Database {
	return database(platformv1alpha1.EngineRedis, "redis:8.2.4-alpine")
}

// The chart's Redis evicts and this one must not, and that is the one line of
// chart/templates/redis.yaml that was absorbed reversed rather than copied.
//
// The chart says why it is safe there: everything it holds is "derivable and
// short-lived. Losing it costs a cache miss and a looser rate limit window, not
// data". This Redis is handed to a tenant through envFrom and the catalogue
// applications keep queues and sessions in it, so allkeys-lru is a platform
// deleting a tenant's data quietly when memory runs out.
func TestATenantsRedisRefusesWritesRatherThanDeletingTheirData(t *testing.T) {
	args := strings.Join(redisStatefulSet(redisDB()).Spec.Template.Spec.Containers[0].Command, " ")

	if strings.Contains(args, "allkeys-lru") || strings.Contains(args, "volatile-") {
		t.Errorf("the server starts with %q: an eviction policy deletes a tenant's keys when "+
			"the memory limit is reached, and the application sees a key that was there "+
			"simply not be there", args)
	}
	if !strings.Contains(args, "--maxmemory-policy noeviction") {
		t.Errorf("the server starts with %q and no noeviction policy; a tenant who is out of "+
			"memory has to find out from an error, not from missing data", args)
	}
	if !strings.Contains(args, "--appendonly yes") {
		t.Errorf("the server starts with %q: the chart turns persistence off because losing "+
			"its counters costs nothing, and losing a tenant's queue is not that", args)
	}
	if !strings.Contains(args, "--requirepass") {
		t.Error("the server starts with no password, so every pod in the namespace that can " +
			"reach the port is authenticated")
	}
}

// The data volume is the point of the CRD requiring a size, and a Redis that
// dropped it would be taking a field the tenant filled in and ignoring it.
func TestRedisKeepsWhatIsWrittenToIt(t *testing.T) {
	set := redisStatefulSet(redisDB())
	if len(set.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("the server has %d claims; the tenant asked for storage and got none",
			len(set.Spec.VolumeClaimTemplates))
	}
	if got := set.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String(); got != "1Gi" {
		t.Errorf("the claim asks for %s, and the tenant asked for 1Gi", got)
	}
	var mounted bool
	for _, m := range set.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == dataVolume && m.MountPath == "/data" {
			mounted = true
		}
	}
	if !mounted {
		t.Error("the claim exists and is not mounted at /data, which is the directory " +
			"--dir names; the append-only file would be written to the read-only root instead")
	}
}

// The reconcile reads the password back before it writes one. Reading under the
// wrong key finds nothing, and finding nothing means minting — on every pass,
// against a server whose storage still holds the first one.
func TestEachEnginesPasswordIsFoundWhereItWasPut(t *testing.T) {
	for _, tc := range []struct {
		db  *platformv1alpha1.Database
		key string
	}{
		{redisDB(), redisPasswordKey},
		{database(platformv1alpha1.EnginePostgres, "postgres:17.2-alpine3.21"), postgresPasswordKey},
	} {
		first, err := desiredDatabaseSecret(tc.db, nil)
		if err != nil {
			t.Fatal(err)
		}
		if first.StringData[tc.key] == "" {
			t.Fatalf("%s: no password was generated under %s", tc.db.Spec.Engine, tc.key)
		}

		// What the cluster would hand back on the next pass.
		live := &corev1.Secret{Data: map[string][]byte{tc.key: []byte(first.StringData[tc.key])}}
		again, err := desiredDatabaseSecret(tc.db, live)
		if err != nil {
			t.Fatal(err)
		}
		if again.StringData[tc.key] != first.StringData[tc.key] {
			t.Errorf("%s: the password changed on the second pass; the server still accepts "+
				"the first one and the application is locked out of its own data",
				tc.db.Spec.Engine)
		}
	}
}

// What an application is handed. A Redis given PostgreSQL's variable names is a
// Redis nothing can connect to.
func TestRedisPublishesWhatARedisClientAsksFor(t *testing.T) {
	secret, err := desiredDatabaseSecret(redisDB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	data := secret.StringData

	for _, absent := range []string{"POSTGRES_USER", "POSTGRES_DB", postgresPasswordKey} {
		if _, present := data[absent]; present {
			t.Errorf("a redis Database publishes %s; there is no such thing here and an "+
				"application reading it would be configured for a server that does not exist",
				absent)
		}
	}
	if data["DB_PORT"] != "6379" {
		t.Errorf("DB_PORT = %q, and redis answers on 6379", data["DB_PORT"])
	}
	url := data["REDIS_URL"]
	if !strings.HasPrefix(url, "redis://:") || !strings.Contains(url, ":6379") {
		t.Errorf("REDIS_URL = %q; the catalogue applications this exists for ask for one by "+
			"that name and every client spells the separate variables differently", url)
	}
	if !strings.Contains(url, data[redisPasswordKey]) {
		t.Error("REDIS_URL carries no credential, so it is a hostname the application still " +
			"cannot use")
	}
}

// The probes have to authenticate or the pod never goes ready, and the password
// must not reach the command line, where it would be in `kubectl describe pod`
// and in every event about a failing probe.
func TestTheProbesAuthenticateWithoutPuttingThePasswordInAnArgument(t *testing.T) {
	c := redisStatefulSet(redisDB()).Spec.Template.Spec.Containers[0]

	var auth *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "REDISCLI_AUTH" {
			auth = &c.Env[i]
		}
	}
	if auth == nil {
		t.Fatal("redis-cli is given no credential, so both probes answer NOAUTH and the pod " +
			"never becomes ready")
	}
	if auth.Value != "" || auth.ValueFrom == nil || auth.ValueFrom.SecretKeyRef == nil {
		t.Errorf("REDISCLI_AUTH is a literal %q rather than a reference; the password would be "+
			"in the pod spec anybody who can read pods can read", auth.Value)
	}
	for _, p := range []*corev1.Probe{c.ReadinessProbe, c.LivenessProbe} {
		if p == nil || p.Exec == nil {
			t.Fatal("a probe is missing; a redis that is loading its append-only file would be " +
				"sent traffic")
		}
		if strings.Contains(strings.Join(p.Exec.Command, " "), "-a ") {
			t.Errorf("the probe runs %v, which puts the password in the process list and in "+
				"every event about a failed probe", p.Exec.Command)
		}
	}
}

// The two engines write different things into one volume, so the name that ends
// up inside an immutable selector has to differ — and postgres's must not move,
// or every Database that already exists becomes unreconcilable.
func TestTheEnginesAreToldApartByANameThatCannotChangeLater(t *testing.T) {
	pg := database(platformv1alpha1.EnginePostgres, "postgres:17.2-alpine3.21")
	if got := databaseSelector(pg)[nameLabel]; got != postgresPortName {
		t.Errorf("a postgres Database now selects on %s=%q; a StatefulSet's selector is "+
			"immutable, so every database that already exists stops reconciling", nameLabel, got)
	}
	if got := databaseSelector(redisDB())[nameLabel]; got != redisPortName {
		t.Errorf("a redis Database selects on %s=%q, which is what a postgres one selects on; "+
			"two servers in one namespace would answer for each other's Service", nameLabel, got)
	}

	// An object built in Go never passes through the API server's defaulting,
	// and the two answers have to agree or the same Database is labelled one
	// way through kubectl and another through the controller.
	bare := &platformv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"}}
	normaliseDatabase(bare)
	if bare.Spec.Engine != platformv1alpha1.EnginePostgres {
		t.Errorf("an engine nobody set normalised to %q rather than to the CRD's default",
			bare.Spec.Engine)
	}
}

// The rules the API server has to enforce, against a real API server — which is
// the only place that can say whether a CEL expression this repository wrote is
// valid. A rule with a typo compiles to nothing and admits everything, and no
// unit test on the Go struct would ever see it.
var _ = Describe("Redis databases", func() {
	ctx := context.Background()
	const namespace = "default"

	redis := func(name string, mutate ...func(*platformv1alpha1.Database)) *platformv1alpha1.Database {
		d := &platformv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: platformv1alpha1.DatabaseSpec{
				Engine:  platformv1alpha1.EngineRedis,
				Image:   "redis:8.2.4-alpine",
				Storage: resource.MustParse("1Gi"),
			},
		}
		for _, m := range mutate {
			m(d)
		}
		return d
	}

	AfterEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &platformv1alpha1.Database{},
			client.InNamespace(namespace))).To(Succeed())
	})

	It("accepts one, and defaults the engine of one that names none", func() {
		Expect(k8sClient.Create(ctx, redis("cache"))).To(Succeed())

		plain := redis("plain")
		plain.Spec.Engine = ""
		plain.Spec.Image = testPostgresImage
		Expect(k8sClient.Create(ctx, plain)).To(Succeed())

		got := &platformv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "plain", Namespace: namespace}, got)).To(Succeed())
		Expect(got.Spec.Engine).To(Equal(platformv1alpha1.EnginePostgres),
			"a Database that names no engine changed server, and every one that already "+
				"exists was created before the field did")
	})

	// The rehearsal dumps with pg_dump, restores with psql and counts rows in
	// tables, and none of those four words mean anything here. Accepting the
	// field would be the same absence with a nightly failing Job attached.
	It("refuses a backup schedule rather than promising one it cannot keep", func() {
		err := k8sClient.Create(ctx, redis("with-backup", func(d *platformv1alpha1.Database) {
			d.Spec.Backup = &platformv1alpha1.DatabaseBackup{Storage: resource.MustParse("1Gi")}
		}))
		Expect(err).To(HaveOccurred(),
			"a redis Database was given a backup schedule; what renders is a CronJob running "+
				"pg_dump against a server that has never heard of it, failing every night")
		Expect(err.Error()).To(ContainSubstring("pg_dump"))
	})

	// The two write different things into the same volume.
	It("refuses to change engine under a volume that already exists", func() {
		Expect(k8sClient.Create(ctx, redis("switcher"))).To(Succeed())

		got := &platformv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "switcher", Namespace: namespace}, got)).To(Succeed())
		got.Spec.Engine = platformv1alpha1.EnginePostgres
		got.Spec.Image = testPostgresImage
		err := k8sClient.Update(ctx, got)
		Expect(err).To(HaveOccurred(),
			"the engine was changed on a live Database, so the next pod starts a server "+
				"against a data directory the other one wrote")
		Expect(err.Error()).To(ContainSubstring("immutable"))
	})
})
