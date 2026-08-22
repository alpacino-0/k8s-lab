# k8s-lab — Kubernetes Öğrenme Laboratuvarı

Node.js uygulamasını konteynerleyip 3 node'luk yerel bir Kubernetes cluster'ında
çalıştıran, self-healing / scaling / rolling update / rollback davranışlarını
gösteren uygulamalı lab.

## Mimari

```
curl localhost:8080
   ↓ kind port mapping (host 8080 → node 30080)
NodePort Service (k8s-lab-app)
   ↓ kube-proxy / iptables — selector: app=k8s-lab-app
Deployment (replicas: 3) → ReplicaSet → Pod → container (k8s-lab-app:1.0)
```

Cluster: 1 control-plane + 2 worker (kind, Kubernetes v1.36.1, containerd runtime)

## Dosyalar

| Dosya | Görev |
|---|---|
| `app/server.js` | Express uygulaması. `/` hostname döner, `/healthz` sağlık kontrolü |
| `app/Dockerfile` | Layer caching için bağımlılıklar koddan önce kopyalanır |
| `kind-config.yaml` | 3 node + NodePort 30080 → host 8080 eşlemesi |
| `k8s/deployment.yaml` | 3 replica, readiness + liveness probe, resource requests/limits |
| `k8s/service.yaml` | NodePort Service, etiketle pod seçimi |

## Kurulum

```bash
# 1) imajı üret
docker build -t k8s-lab-app:1.0 ./app

# 2) cluster'ı kur
kind create cluster --name k8s-lab --config kind-config.yaml

# 3) imajı node'lara yükle (registry kullanmıyoruz)
kind load docker-image k8s-lab-app:1.0 --name k8s-lab

# 4) deploy
kubectl apply -f k8s/

# 5) doğrula — her istek farklı pod'dan cevap gelir
for i in $(seq 1 10); do curl -s localhost:8080; done
```

## Doğrulanmış davranışlar

| Senaryo | Komut | Ölçülen sonuç |
|---|---|---|
| Load balancing | 30x `curl` | 3 pod'a 12/11/7 dağılım |
| Self-healing | `kubectl delete pod <ad>` | Yeni pod **2 saniyede** doğdu, kesinti yok |
| Ölçekleme | `kubectl scale --replicas=6` | 0.18s, node'lara 3+3 dengeli dağıldı |
| Rolling update | `kubectl set image ... :1.1` | 250 istek, **0 kesinti**, geçişte iki sürüm birlikte |
| Bozuk sürüm | eksik imaj → `ImagePullBackOff` | 200 istek, **0 kesinti** — eski pod'lar korundu |
| Rollback | `kubectl rollout undo` | **0.043s**, eski ReplicaSet 0→3 |

## Hata ayıklama sırası

```bash
kubectl get pods                 # kim sorunlu?
kubectl describe pod <ad>        # Events bölümünü oku — sebep burada
kubectl logs <ad> [--previous]   # uygulama ne diyor?
```

## Öğrenilen kritik ayrımlar

- **readinessProbe vs livenessProbe** — readiness başarısızsa pod trafik almaz ama yaşar;
  liveness başarısızsa konteyner öldürülüp yeniden kurulur.
- **Docker vs containerd** — Docker imaj build eder; cluster'da konteynerleri containerd
  çalıştırır (Kubernetes 1.24'te dockershim kaldırıldı).
- **Pod'lar geçicidir** — isim ve IP her doğuşta değişir. Bağlantı her zaman Service üzerinden.
- **Rolling update sırasında iki sürüm aynı anda çalışır** — DB şeması değişiklikleri
  geriye dönük uyumlu olmalı.

## Bölüm 5 — Üretim pratikleri

### ConfigMap & Secret
Ayarlar imaja gömülmez, ortam değişkeni olarak enjekte edilir (`envFrom`).
Aynı imaj farklı ConfigMap ile dev/staging/prod'da çalışır.

> **Secret şifreleme değildir** — sadece base64. `kubectl get secret -o jsonpath=... | base64 -d`
> ile herkes okur. Üretimde: etcd encryption-at-rest + RBAC + Vault/Sealed Secrets.
> Bu repoda `k8s/secret.yaml` **.gitignore'da**; şablonu `k8s/secret.yaml.ornek`.

### Graceful shutdown (PID 1 tuzağı)
Konteynerde PID 1 olan süreç, handler tanımlamadığı sinyalleri **yok sayar**.
SIGTERM işleyicisi olmadan her pod kapanışı `terminationGracePeriodSeconds`
(varsayılan 30s) dolana kadar bekler, sonra SIGKILL yer.

| Durum | Pod'un silinme süresi |
|---|---|
| SIGTERM işleyicisi yok | **30 saniye** |
| `process.on('SIGTERM', ...)` var | **0 saniye** |

### Liveness vs readiness — ölçülmüş davranış
`/kirilsin` çağrılıp `/healthz` 500 döndürüldüğünde:

| Süre | Olay | Sorumlu |
|---|---|---|
| +15s | `READY 1/1 → 0/1`, pod trafikten çıkarıldı | readiness (5s × 3) |
| +25s | Konteyner öldürülüp yeniden kuruldu | liveness (10s × 3) |
| +35s | Pod sağlıklı, tekrar trafikte | readiness |

Önce readiness devreye girer → hasta pod'a tek istek bile gitmez.

### Ingress
NodePort yerine tek giriş noktası, host + path tabanlı yönlendirme.

| İstek | Hedef servis |
|---|---|
| `app.local/` | `k8s-lab-app` |
| `app.local/api` | `k8s-lab-api` (`rewrite-target` ile `/api` öneki soyulur) |
| `baska.local/` | HTTP 404 |

```bash
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.13.0/deploy/static/provider/baremetal/deploy.yaml
kubectl patch svc ingress-nginx-controller -n ingress-nginx --type=json \
  -p='[{"op":"replace","path":"/spec/ports/0/nodePort","value":30080}]'
kubectl apply -f k8s/ingress.yaml
curl -H "Host: app.local" localhost:8080/api
```

### Servis keşfi (CoreDNS)
Pod içinden `http://k8s-lab-api` çalışır. Tam ad: `<servis>.<namespace>.svc.cluster.local`.
Mikroservisler birbirine IP ile değil, **servis adıyla** ulaşır.

### Namespace + ResourceQuota + LimitRange
`staging` namespace'i `default` ile aynı isimli kaynakları barındırır, kotayla sınırlıdır.
Kota aşımı `kubectl get pods` çıktısında **görünmez** — Deployment koşullarına bakılır:

```bash
kubectl -n staging get deployment k8s-lab-app -o jsonpath='{.status.conditions}'
# ReplicaFailure: FailedCreate — exceeded quota: staging-kota, used: pods=4, limited: pods=4
kubectl -n staging get events --sort-by=.lastTimestamp
```

### CI/CD — `.github/workflows/ci.yml`
| Job | Yaptığı |
|---|---|
| `build-test` | İmajı build eder, konteyneri ayağa kaldırıp `/healthz` ve `/` duman testi |
| `k8s-test` | Gerçek kind cluster kurar, deploy eder, cluster içinden doğrular; hata olursa `describe`+`logs` basar |
| `publish` | Sadece `main`'de: imajı GHCR'a push eder (`:sha` ve `:latest`), GHA build cache ile |

## Temizlik

```bash
kind delete cluster --name k8s-lab
```
