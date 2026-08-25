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
| Images | Already published: `ghcr.io/damgahq/damga`, public, amd64 + arm64. If you fork this and publish your own, note that a new organisation's GHCR packages start private and the visibility has to be flipped once by hand, or Kyverno cannot fetch the signature |

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

## 4. Namespace and admission policies

Create the namespace from the manifest, not with `kubectl create namespace`. The
labels are the whole point of the file:

```bash
kubectl apply -f policies/namespace.yaml
kubectl apply -f policies/admission-policies.yaml -f policies/admission-bindings.yaml
kubectl apply -f policies/tenant-quota.yaml
```

Four of those labels put the namespace under Pod Security Admission at
`restricted`; the fifth, `damga.co/policies: enforced`, is what the
ValidatingAdmissionPolicy bindings select on. A namespace created without them
is not a namespace with weaker rules — it is a namespace with **no** rules, and
nothing says so. The release installs, every pod starts, and the enforcement
layer this project is built around is silently absent.

Confirm it is on before going further:

```bash
kubectl get namespace damga -o jsonpath='{.metadata.labels}' | tr ',' '\n'
NAMESPACE=damga ./scripts/policy-test.sh     # each rule must reject what it should
```

Optionally, image signature verification. It is the one rule the built-in
admission engine cannot express, because verifying a signature means reaching a
registry and a transparency log. The same install also brings the reports
controller, which is what records the results of the policies above — a
ValidatingAdmissionPolicy keeps none of its own. Three pods, 174 Mi measured:

```bash
helm repo add kyverno https://kyverno.github.io/kyverno/
helm upgrade --install kyverno kyverno/kyverno -n kyverno --create-namespace \
  --version 3.5.2 --wait \
  --set admissionController.replicas=1 \
  --set backgroundController.replicas=1 \
  --set cleanupController.enabled=false \
  --set admissionController.container.resources.requests.memory=128Mi \
  --set admissionController.container.resources.limits.memory=384Mi \
  --set reportsController.enabled=true \
  --set reportsController.replicas=1 \
  --set reportsController.resources.requests.cpu=100m \
  --set reportsController.resources.requests.memory=64Mi \
  --set reportsController.resources.limits.memory=256Mi
kubectl apply -f policies/kyverno-image-signatures.yaml
```

If that last line returns `connection refused`, wait a few seconds and run it
again: `--wait` returns when the pods are Ready, which is slightly before
Kyverno's own webhook starts accepting the policy you are applying through it.

After this, only images signed by this repository's pipeline can run, and each
is rewritten to its digest on admission so a moved tag cannot change what runs.

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

Coolify runs Docker, not Kubernetes. The application itself would deploy there
happily — and TLS and the deploy pipeline would be easier. What would not
survive: multiple replicas behind one Service, autoscaling, network policies,
disruption budgets and the Prometheus integration. Roughly everything this
project exists to show.

Docker Swarm mode with `deploy.replicas: 3` would restore multi-replica
routing, at the cost of everything else on that list.
