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
| Local tools | `kubectl`, `helm` |
| Images | Already published: `ghcr.io/damgahq/damga`, public, amd64 + arm64. If you fork this and publish your own, note that a new organisation's GHCR packages start private and the visibility has to be flipped once by hand |

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

## 5. Database credentials

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

## 6. Deploy

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

## 7. Verify

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

---

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
| Backups | Written to a volume **on the same cluster** — fine for a demo, not a backup strategy. Copy them off the machine if the data ever matters |
| Updates | Dependabot opens the pull requests; CI proves them; you merge |
| Certificate | cert-manager renews automatically. It emails you if renewal fails |

## Teardown

```bash
helm uninstall app -n damga
kubectl delete namespace damga
# on the server
/usr/local/bin/k3s-uninstall.sh
```

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
