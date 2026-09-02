# Deploying to a public address

Runbook for putting the demo on a server people can reach. Written for k3s on a
single small VPS, which is the cheapest way to run this without giving up
anything the project demonstrates.

Everything here has been prepared and validated except the parts that need a
real server and a real domain — those are marked. Nothing is deployed at a
public address yet.

---

## What you need

| | |
|---|---|
| Server | 1 vCPU / 2 GB RAM is comfortable; 1 GB works. Hetzner CX22 or similar, ~€4-5/month |
| Domain | A record pointing at the server's IP. A subdomain is fine |
| Tools | `kubectl`, `helm`, `openssl`, `sed`. On the server, if you use `scripts/install.sh`; on your own machine if you follow the sections by hand |
| This repository | checked out **on the server**. Every manifest and the chart are applied from it, and `scripts/install.sh` lives in it |
| Images | Published: `ghcr.io/damgahq/damga` (the reference tenant), `ghcr.io/damgahq/damga-operator` and `ghcr.io/damgahq/damga-control-plane`, public, amd64 + arm64, each signed with keyless cosign. If you fork this and publish your own, note that a new organisation's GHCR packages start private and the visibility has to be flipped once by hand |

---

## The one command

```bash
sudo ./scripts/install.sh --domain demo.your-domain.com \
    --email you@example.com --tenant acme
```

That is sections 1 to 10 below, in order, on one machine. Re-running it is safe:
every step re-applies what is already there, and the database password is
created once and then left alone — replacing it would leave a PostgreSQL whose
password no longer matches the one the app presents, which fails as a refused
connection with nothing anywhere saying the credential moved.

Before it touches anything:

```bash
DRY_RUN=1 ./scripts/install.sh --domain demo.your-domain.com \
    --email you@example.com --tenant acme
```

prints every command it would run, in order, and changes nothing. The plan comes
out of the same wrapper every mutation goes through rather than a second list
kept beside the script, so it cannot describe a sequence the script does not
perform. `scripts/install_test.go` reads that plan, and is where the ordering
rule in section 5 is enforced — reversing those two steps fails `go test ./...`
with a message saying which way round they go.

| Flag | When |
|---|---|
| `--issuer letsencrypt-prod` | the default is `letsencrypt-staging`, deliberately: rehearse against the server that does not rate-limit failures |
| `--skip-k3s` | k3s is already there, or the kubeconfig points elsewhere. The install then asks the cluster what already holds ports 80 and 443, and refuses rather than installing an ingress controller that could never get an address — on stock k3s that is Traefik, and the refusal names it and says how to remove it |
| `--skip-node-config` | do not write the containerd redirect or restart k3s |
| `--control-plane-image <ref>` | run a control plane you built yourself instead of the published one |
| `--skip-control-plane` | the platform and the reference tenant only |

> **What was measured.** The dry run, the order it emits, that it never prints a
> generated credential, and that every path in it exists in this repository —
> all four run in `go test ./...`. **Not measured:** a real run against a real
> k3s server. Every step in the script is a step from this page, so it inherits
> this page's marks and adds no confidence of its own.

---

## 1. Install k3s

k3s is a certified Kubernetes distribution packaged as a single binary. Same
API, same `kubectl`, same charts — it simply leaves out what a small cluster
does not need and bundles what it does.

Traefik is disabled because this chart depends on ingress-nginx annotations
(`limit-rps`, `limit-burst-multiplier`, `limit-connections`) that Traefik does
not implement.

```bash
# on the server
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--disable traefik" sh -
```

Take the kubeconfig back to your machine:

```bash
# on the server
sudo cat /etc/rancher/k3s/k3s.yaml

# on your machine: save it, then point it at the server instead of localhost
sed -i '' "s/127.0.0.1/<SERVER_IP>/" ~/.kube/k3s-demo.yaml
export KUBECONFIG=~/.kube/k3s-demo.yaml
kubectl get nodes
```

Restrict the API port to your own address in the provider firewall — k3s
listens on 6443 and the kubeconfig is the whole cluster.

## 2. Ingress controller

```bash
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.13.0/deploy/static/provider/baremetal/deploy.yaml
kubectl wait -n ingress-nginx --for=condition=Ready pod \
  -l app.kubernetes.io/component=controller --timeout=300s
```

On a single-node k3s, expose it on the host's ports 80 and 443:

```bash
kubectl patch svc ingress-nginx-controller -n ingress-nginx --type=json -p='[
  {"op":"replace","path":"/spec/type","value":"LoadBalancer"}
]'
```

k3s ships a service load balancer (`klipper-lb`) that binds the node's ports,
so a `LoadBalancer` Service works without a cloud provider.

## 3. TLS

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
kubectl wait -n cert-manager --for=condition=Available deployment --all --timeout=300s
```

Create the issuers from the template — set the email, it receives expiry
warnings:

```bash
cp cluster/issuers-letsencrypt.yaml.example cluster/issuers-letsencrypt.yaml
# edit the email address, then
kubectl apply -f cluster/issuers-letsencrypt.yaml
```

`scripts/install.sh` substitutes the address into the template and pipes it
straight to `kubectl`, writing no second file — one template, and no copy beside
it to edit and then forget.

Rehearse against staging first (`--set ingress.tls.clusterIssuer=letsencrypt-staging`),
confirm a certificate is issued, then switch to `letsencrypt-prod`.

> Let's Encrypt rate-limits failed attempts hard. Point the DNS record at the
> server and confirm it resolves *before* creating the issuer, and use
> `https://acme-staging-v02.api.letsencrypt.org/directory` while testing.

## 4. Namespace and the tenant fence

Create the namespace from the manifest, not with `kubectl create namespace`. The
labels are the whole point of the file:

```bash
kubectl apply -f policies/namespace.yaml
kubectl apply -f policies/tenant-quota.yaml
```

The labels put the namespace under Pod Security Admission at `restricted`. The
quota is the neighbour guard: without it one tenant can take a node's whole
memory and bring down everybody else's apps on it. A namespace created without
either is not a namespace with weaker rules — it is one with **no** rules, and
nothing says so.

Confirm the labels landed:

```bash
kubectl get namespace damga -o jsonpath='{.metadata.labels}' | tr ',' '\n'
```

> **What is deliberately not here.** Until 2026-08-29 this step also installed
> three ValidatingAdmissionPolicies and Kyverno for image signature
> verification. Both were removed: they made it impossible to install a
> third-party application — a catalogue image is unsigned, may need to write to
> its filesystem, and needs to reach the internet. Nothing verifies an image
> now. That is a deliberate trade, recorded rather than quietly dropped.

## 5. The types the platform reconciles

```bash
kubectl apply -f config/crd/bases
```

`Workload`, `Database` and `Build`. They go in **before** the next section, and
that ordering is the one thing on this page that is not a matter of taste.

`cluster/build-namespace.yaml` puts a quota on
`count/builds.platform.damga.co`. A ResourceQuota that counts a type the API
server has never heard of cannot compute a usage for it, and until it can every
create in that namespace is refused with `status unknown for quota` — a message
that names the quota and not the missing type. Measured: applying the quota
first made the first build of every CI run fail on the platform's own guard
rail.

`make -f Makefile.operator install` installs the same three types through
kustomize, and wants Go, make and a download on a machine whose only job is to
run one server. Measured equivalent: `kustomize build config/crd` (v5.8.1) and
the three files in `config/crd/bases` parse to the same three objects, field for
field — `config/crd/kustomization.yaml` carries no active patches.

Installing the types is not installing the controller. Nothing reconciles a
`Workload` until the operator is running, and this page does not install it; see
[What this page does not install](#what-this-page-does-not-install).

## 6. The image store

Needed as soon as the platform builds anything: a build pushes an image here and
the kubelet pulls it back out. Nothing else on this page depends on it.

```bash
kubectl apply -f cluster/registry.yaml -f cluster/build-namespace.yaml
kubectl -n damga-registry rollout status deployment/registry --timeout=300s
```

That is one half of it. The other half is on the node, and it is the half that
gets missed, because the build that produced the image succeeds either way and
the failure arrives later, as a pull.

A build pushes to `registry.damga-registry.svc:5000`, and the reference recorded
for the deploy is that exact string. containerd runs on the host rather than in
the cluster, does not use cluster DNS, and answers every pull of that name with
`no such host`. So the node is told once that this one name is served by the
registry's NodePort. kind gets that from `kind-config.yaml` and
`scripts/bootstrap.sh`. k3s takes a file, on **every** node:

```bash
# on the server, as root
cat > /etc/rancher/k3s/registries.yaml <<'YAML'
mirrors:
  "registry.damga-registry.svc:5000":
    endpoint:
      - "http://127.0.0.1:30500"
YAML
```

```bash
systemctl restart k3s      # on a node that is not the server: k3s-agent
```

**The restart is what applies it.** k3s reads this file at start-up and writes
containerd's own configuration from it; edited afterwards it changes nothing,
and nothing says so. Measured: a second mirror added to the file left
containerd's copy untouched until k3s restarted, and appeared immediately after.

Both halves, checked:

```bash
sudo cat "/var/lib/rancher/k3s/agent/etc/containerd/certs.d/registry.damga-registry.svc:5000/hosts.toml"
```

```bash
sudo crictl pull registry.damga-registry.svc:5000/<repository>:<tag>
```

A pull that succeeds is the whole thing proved: the node cannot resolve that
name at all, so the only way the bytes arrive is the redirect. Two notes on that
command:

- `sudo k3s crictl`, the form older guides use, is not a subcommand any more —
  on v1.34.1+k3s1 it answers `No help topic for 'crictl'`. Plain `crictl` is a
  symlink to the same binary and works.
- `127.0.0.1` rather than the node's address, because kube-proxy's iptables
  proxier deliberately makes NodePorts reachable on loopback: it sets
  `route_localnet=1` and logs that it has. A cluster that turns that off
  (`--iptables-localhost-nodeports=false`, or a `--nodeport-addresses` that
  filters loopback) wants the node's own address here instead.

### What keeps it off the disk

`cluster/registry.yaml` also installs a CronJob that sweeps the store at 03:17
each night: it keeps the ten newest builds of each application and collects the
rest, blobs included. Without it the 10Gi claim is a date rather than a size — a
registry removes nothing on its own.

Two consequences, better met here than in production:

- Rolling back further than ten builds of an application finds no image and says
  so as an `ImagePullBackOff`. The commit it was named after is still there, and
  building it again produces the same image.
- The bound is per application, not per cluster. Ten applications keep ten
  builds each, so raise the claim before adding applications rather than after.

`./scripts/registry-gc-test.sh` proves the sweep against a running cluster: it
pushes three images, collects with the retention set to one, and asserts that
the old image's bytes are gone, that the current one still downloads whole, and
that the store on the volume shrank.

> **What was measured here.** Everything in this section ran against k3s
> v1.34.1+k3s1 and, for the sweep, a three-node kind cluster as well: the two
> manifests, the generated `hosts.toml` and its path, the pull through the
> redirect, the restart requirement, and all six assertions of the test. Not
> run: `systemctl restart k3s` — that k3s was restarted whole rather than
> through its unit.

## 7. Database credentials

The password never goes into a values file or a shell history:

```bash
kubectl -n damga create secret generic db-credentials \
  --from-literal=POSTGRES_USER=labuser \
  --from-literal=POSTGRES_DB=labdb \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -base64 24)"
```

`chart/values-public.yaml` refers to it by name via
`postgres.auth.existingSecret`, so the chart renders no PostgreSQL secret of its
own — the credentials exist only in the cluster, never in this repository.

## 8. Deploy the reference tenant

```bash
helm upgrade --install app ./chart \
  --namespace damga \
  -f chart/values-public.yaml \
  --set ingress.host=demo.your-domain.com \
  --timeout 10m

kubectl -n damga rollout status statefulset/app-postgres --timeout=300s
kubectl -n damga rollout status deployment/app-redis --timeout=300s
kubectl -n damga rollout status deployment/app-damga-app --timeout=300s
```

Redis comes from the chart, so there is nothing extra to install. It holds the
rate-limit window and the note-count cache, and both degrade rather than fail
if it is unavailable — but the limit stops binding across replicas, which on a
public address is the difference between a limit and a suggestion.

`--wait` is deliberately not used: the backup volume uses a
`WaitForFirstConsumer` storage class and stays `Pending` until the first backup
job mounts it, which would block the release forever.

## 9. The control plane

```bash
kubectl apply -f cluster/control-plane.yaml
kubectl -n damga-system rollout status deployment/damga --timeout=300s
```

The panel, the API and the deploy write path — the product, as opposed to the
reference tenant the previous section deployed. The manifest carries a
ServiceAccount whose identity is deliberately lopsided: cluster-wide reads, one
namespace it may create a `Build` in, and no delete anywhere.

> **Read the image line before you rely on it.** For most of this repository's
> life `cluster/control-plane.yaml` named an image nothing published — first
> `:latest`, then `:unpublished` — and it survived because CI builds the control
> plane into a kind cluster and overrides the field, so no step here ever pulled
> the reference. The publish job builds it now, and the manifest names
> `ghcr.io/damgahq/damga-control-plane:1.0.0`.
>
> That tag is republished on every push to `main`, so it means "whatever is
> there today". Two consequences, both of which matter more here than they do
> for the Argo CD sources in `gitops/`, which at least show drift:
>
> - `imagePullPolicy` defaults to `IfNotPresent` for a tag other than `:latest`,
>   so a node that pulled `:1.0.0` last week keeps running last week's bytes
>   while this line says what it always said. `scripts/upgrade.sh` prints the
>   running `imageID`, which is the only field that can tell you.
> - `kubectl apply` of an unchanged pod template is a no-op. A new version under
>   the same tag produces no rollout, and the apply reports success.
>
> Pin a digest — `...@sha256:...` — in any install you intend to keep, and see
> the comment on that line for why the manifest does not carry one yet.
>
> To run one you built yourself, pass `--control-plane-image`, or by hand:
>
> ```bash
> kubectl -n damga-system set image deployment/damga \
>   damga=ghcr.io/you/damga-control-plane:1
> ```

## 10. The first owner

```bash
kubectl -n damga-system exec -it deploy/damga -- \
  damga bootstrap -evidence-dsn /data/damga.db \
  -email you@example.com -tenant acme
```

One account and one tenant. It prints a generated password once and keeps only
an argon2id hash, so copy it before the terminal scrolls.

`kubectl exec` streams over the CRI exec channel, which is not the container log
a collector tails — so the password is shown to whoever ran the command and to
nobody else. Printing the same string from the running server would put it in
Loki for the retention period. Running it a second time exits `3` and changes
nothing, which is why the installer calls it unconditionally.

There is no "create the first account" page, and no setup token printed at
startup. [docs/CONTROL-PLANE.md](CONTROL-PLANE.md) says what each of those
alternatives gives away.

## 11. Verify

```bash
curl -sI https://demo.your-domain.com | head -3          # 200, valid certificate
curl -s  https://demo.your-domain.com/stats | jq .       # which pod answered
curl -s -o /dev/null -w '%{http_code}\n' \
     https://demo.your-domain.com/metrics                # 404 — scrape port is not public
```

Then run the full check against the live release:

```bash
NAMESPACE=damga RELEASE=app ./scripts/smoke-test.sh
```

Certificate issuance takes a minute or two on the first request:

```bash
kubectl -n damga get certificate
kubectl -n damga describe certificate damga-tls | tail -20
```

### The control plane, from a browser and from a terminal

The control plane is not on the public address — nothing routes to it and no
Ingress in this repository claims a host for it. Reach it over a port-forward:

```bash
kubectl -n damga-system port-forward svc/damga 9000:80
```

Then <http://localhost:9000> for the panel, or the CLI against the same API:

```bash
go build -o damga-cli ./cmd/damga-cli

./damga-cli login --server http://localhost:9000 --email you@example.com
./damga-cli apps
./damga-cli verify <app> <env>
```

`damga-cli` calls the endpoints the panel calls and cannot call anything else:
its route table is walked by a test that starts a real control plane and asks it
for every row, so an endpoint the CLI believes in and the API does not have
fails in `go test ./...` rather than in somebody's terminal.

Two things worth knowing before the first run:

- **The session is bound to the host that issued it.** Signing in against
  `http://localhost:9000` and then passing `--server http://127.0.0.1:9000`
  gets a refusal, because the control plane binds a session to its host and
  answers a mismatch with the same "not signed in" it gives an expired one. The
  CLI knows both hosts and says which; the server deliberately does not.
- **`verify` exits non-zero when the chain does not hold** (`4`, and `3` for a
  session that is gone), so a cron job can ask without reading the output.

> **What was measured.** Every command above ran against a control plane started
> in-process by the CLI's own test suite: `login`, `whoami`, `apps`,
> `apps create`, `apps delete`, `status`, `history`, `verify` and `retention`
> answered the real server. **Not measured against a real server:** `build`,
> `deploy`, `backup` and `export` — `deploy` needs a git token this install does
> not have and `build` needs an operator it does not run, so those four are
> proved against a recorded stand-in only, and what is proved about them is the
> request they send, not the answer they get.

---

## 12. Upgrading

```bash
git pull
./scripts/upgrade.sh
```

That is the whole upgrade path, and it is deliberately much smaller than the
install. It re-applies the two things on this page that carry this repository's
own code — the types from section 5 and the control plane from section 9 — in
that order, waits for the rollout, and prints what moved. k3s, the ingress
controller, cert-manager and the image store are not touched: re-applying them
to move one Deployment forward is how an upgrade acquires failure modes it did
not need.

`DRY_RUN=1 ./scripts/upgrade.sh` prints the two commands and changes nothing.

The reference tenant is not upgraded here either, and that is the product
working rather than a gap: an application moves to a new image through the
deploy path — a commit, then Argo CD.

Three things the script does that a bare `kubectl apply` does not:

- **It says when nothing moved.** An apply of an unchanged pod template is a
  no-op and a rollout status against an unchanged Deployment succeeds
  immediately. Both report success. If the image and the running digest are the
  same afterwards as before, this says so and names the likely reason — a tag
  that has not changed.
- **It refuses if the data volume was replaced.** Accounts, sessions,
  placements and every deploy record are one SQLite file on `damga-data`. An
  upgrade that comes back on a new claim has installed a second, empty platform
  wearing the first one's name, and every symptom of that shows up later as
  "my apps are gone".
- **It substitutes `--image` into the manifest instead of patching after the
  apply.** With `strategy: Recreate` the old pod is gone before a replacement
  that cannot be pulled fails, so a correction applied afterwards arrives after
  an outage the upgrade did not require.

### What is, and is not, zero-downtime

The control plane runs **one replica with `strategy: Recreate`**, because the
default `-evidence-dsn` is SQLite on a `ReadWriteOnce` claim and two pods cannot
both hold it. So its own upgrade has a gap, by construction. An install that
points `-evidence-dsn` at PostgreSQL can raise the replica count and change the
strategy; nothing on this page assumes it has.

What does not have a gap is **everything already deployed**. The control plane
is not in the request path of a tenant application, and CI asserts it: while the
control plane is upgraded, the reference tenant is polled through the ingress
and every request must succeed. That assertion has content — the upgrade also
re-applies the CRDs, and a change to a type reaches every `Workload` through the
operator.

> **What was measured.** Two upgrades on a single-node kind cluster
> (Kubernetes v1.36.1), 2026-09-01, polling the control plane's ready endpoints
> as fast as `kubectl` answers — about 30 ms a sample:
>
> | | samples with no ready endpoint | ≈ |
> |---|---|---|
> | upgrade 1 | 25 of 400 over 14.7 s | 0.9 s |
> | upgrade 2 | 35 of 125 over 3.8 s | 1.1 s |
>
> Both gaps were one unbroken run, so this is a real outage of about a second
> and not sampling noise. **Not measured:** the same upgrade on a busier machine,
> on a slower volume, or with a database large enough for a migration to take
> time — all three make it longer, and none of them was tried.
>
> Measured in the same runs: an owner created before the upgrade was still there
> after it. `damga bootstrap` exits `3` for *"this install already has an
> owner"*, and it did — which is what says the volume came back rather than
> being replaced by an empty one.
>
> CI's *"Upgrading the control plane must not disturb what is deployed"* step
> re-measures all of it on every run, and asserts two of the three: zero failed
> tenant requests, and the owner still known. The control plane's own gap is
> **printed rather than asserted**, because an assertion that has to pass
> against a known gap is either always red or written loosely enough to pass for
> the wrong reason.

---

## What this page does not install

Named here rather than discovered on the machine.

- **The operator.** The types from section 5 exist and nothing reconciles them,
  so a `Workload` is a row in etcd and not a Deployment. It needs an image and
  `make -f Makefile.operator deploy`, which wants kustomize and Go on the
  server. `ghcr.io/damgahq/damga-operator` **is** published, so only the deploy
  step is missing here.
- **Argo CD.** A deploy writes a commit and nothing applies it. `make gitops`
  installs Argo CD against a local cluster; there is no equivalent on this page.
- **A git token.** `cluster/control-plane.yaml` passes no `-git-token-file`, so
  `POST .../deploys` refuses with a message naming that flag. Everything the
  panel reads works without it.

Each of these is a step, not a redesign. What they add up to is that this page
gets you a platform you can log into and look at, and not yet one that ships a
commit to a running pod.

## What a single node costs you

Three replicas all land on the same machine. Repeated requests to `/` still
come back from different pods and every response names the one that answered,
so the demo works — but "pods spread across nodes" and the zero-downtime drain
both need a second machine. Adding one is a single command:

```bash
# on the server: print the join token
sudo cat /var/lib/rancher/k3s/server/node-token

# on the second machine
curl -sfL https://get.k3s.io | K3S_URL=https://<SERVER_IP>:6443 \
  K3S_TOKEN=<TOKEN> sh -
```

The topology-spread constraint is already in the chart with
`whenUnsatisfiable: ScheduleAnyway`, so pods redistribute on their own once a
second node appears. No redeploy.

## Ongoing costs and duties

| | |
|---|---|
| Server | ~€4-5/month, ~€9-10 with a second node |
| Domain | ~€10-15/year |
| Image store | A 10Gi claim, swept nightly, ten builds of each application kept. Raise it before adding applications rather than after |
| Backups | Written to a volume **on the same cluster** — fine for a demo, not a backup strategy. Copy them off the machine if the data ever matters |
| Updates | Dependabot opens the pull requests; CI proves them; you merge. Moving this install onto a merged version is `git pull && ./scripts/upgrade.sh` — section 12, and about a second with the panel unavailable |
| Certificate | cert-manager renews automatically. It emails you if renewal fails |

## Teardown

```bash
helm uninstall app -n damga
kubectl delete namespace damga damga-system damga-registry damga-build
# The types last: deleting a CRD deletes every object of that type, so doing it
# before the namespaces above removes the objects the operator would otherwise
# get a chance to finalise.
kubectl delete -f config/crd/bases
# on the server
/usr/local/bin/k3s-uninstall.sh
rm -f /etc/rancher/k3s/registries.yaml
```

`k3s-uninstall.sh` removes the cluster whole, so the first three commands only
matter when the machine is being kept. **Not verified:** the order above is
reasoned from how finalisers work and has not been run against a live install.

---

## Why not Coolify

Coolify manages **servers**: it talks SSH to a box and runs Docker there. Damga
manages a **cluster**. For one application on one machine Coolify is easier, and
saying otherwise would be dishonest.

The difference shows up on the second machine. In Coolify a second server is a
second box you deploy to; in Damga a second node is just somewhere the same
application can go — nothing in the application's definition changes. Everything
in this guide that a single node cannot demonstrate (replicas spread across
nodes, autoscaling, disruption budgets, rescheduling on node loss) is waiting
for that second machine rather than needing to be rebuilt for it.

The other difference is backups. Every platform in this space takes them. This
one restores each backup into a scratch database and counts the rows, and puts
the result on the page — so "we have backups" and "the backup works" stop being
the same sentence.
