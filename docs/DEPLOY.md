# Deploying to a public address

Runbook for putting the demo on a server people can reach. Written for k3s on a
single small VPS, which is the cheapest way to run this without giving up
anything the project demonstrates.

Everything here has been prepared and validated except the parts that need a
real server and a real domain — those are marked. Nothing is published yet.

---

## What you need

| | |
|---|---|
| Server | 1 vCPU / 2 GB RAM is comfortable; 1 GB works. Hetzner CX22 or similar, ~€4-5/month |
| Domain | A record pointing at the server's IP. A subdomain is fine |
| Local tools | `kubectl`, `helm` |
| Images | Already published: `ghcr.io/alpacino-0/k8s-lab` and `-web`, public, amd64 + arm64 |

---

## 1. Install k3s

k3s is a certified Kubernetes distribution packaged as a single binary. Same
API, same `kubectl`, same charts — it simply leaves out what a small cluster
does not need and bundles what it does.

Traefik is disabled because this chart depends on ingress-nginx annotations
(`rewrite-target`, `limit-rps`) that Traefik does not implement.

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

Create the issuer — replace the email, it receives expiry warnings:

```bash
kubectl apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: you@example.com
    privateKeySecretRef:
      name: letsencrypt-prod-account
    solvers:
      - http01:
          ingress:
            ingressClassName: nginx
EOF
```

> Let's Encrypt rate-limits failed attempts hard. Point the DNS record at the
> server and confirm it resolves *before* creating the issuer, and use
> `https://acme-staging-v02.api.letsencrypt.org/directory` while testing.

## 4. Database credentials

The password never goes into a values file or a shell history:

```bash
kubectl create namespace k8s-lab
kubectl -n k8s-lab create secret generic db-credentials \
  --from-literal=POSTGRES_USER=labuser \
  --from-literal=POSTGRES_DB=labdb \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -base64 24)"
```

`chart/values-public.yaml` refers to it by name via
`postgres.auth.existingSecret`, so the chart renders no secret of its own.

## 5. Deploy

```bash
helm upgrade --install app ./chart \
  --namespace k8s-lab \
  -f chart/values-public.yaml \
  --set ingress.host=demo.your-domain.com \
  --timeout 10m

kubectl -n k8s-lab rollout status statefulset/app-postgres --timeout=300s
kubectl -n k8s-lab rollout status deployment/app-k8s-lab-app --timeout=300s
kubectl -n k8s-lab rollout status deployment/app-k8s-lab-app-web --timeout=300s
```

`--wait` is deliberately not used: the backup volume uses a
`WaitForFirstConsumer` storage class and stays `Pending` until the first backup
job mounts it, which would block the release forever.

## 6. Verify

```bash
curl -sI https://demo.your-domain.com | head -3          # 200, valid certificate
curl -s  https://demo.your-domain.com/api/stats | jq .   # which pod answered
curl -s -o /dev/null -w '%{http_code}\n' \
     https://demo.your-domain.com/api/metrics            # 404 — scrape port is not public
```

Then run the full check against the live release:

```bash
NAMESPACE=k8s-lab RELEASE=app ./scripts/smoke-test.sh
```

Certificate issuance takes a minute or two on the first request:

```bash
kubectl -n k8s-lab get certificate
kubectl -n k8s-lab describe certificate k8s-lab-tls | tail -20
```

---

## What a single node costs you

Three replicas all land on the same machine. The pod ledger still fills with
three lanes, so the demo works — but "pods spread across nodes" and the
zero-downtime drain both need a second machine. Adding one is a single command:

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
helm uninstall app -n k8s-lab
kubectl delete namespace k8s-lab
# on the server
/usr/local/bin/k3s-uninstall.sh
```

---

## Why not Coolify

Coolify runs Docker, not Kubernetes. The application itself would deploy there
happily — and TLS and the deploy pipeline would be easier. What would not
survive: multiple replicas behind one Service, the pod ledger that makes the
demo worth opening, autoscaling, network policies, disruption budgets and the
Prometheus integration. Roughly everything this project exists to show.

Docker Swarm mode with `deploy.replicas: 3` would restore the ledger, at the
cost of everything else on that list.
