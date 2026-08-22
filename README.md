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

## Bölüm 6 — Ölçekleme, kalıcı veri, yetkilendirme

### metrics-server + HPA
```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch deployment metrics-server -n kube-system --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
kubectl apply -f k8s/hpa.yaml
```
> `--kubelet-insecure-tls` yalnızca kind içindir (self-signed kubelet sertifikası).
> HPA'nın çalışması için Deployment'ta **`resources.requests.cpu` zorunludur**.

Ölçülen davranış (`/yuk?ms=400` adresine 4 paralel istemci):

| Yön | Süre | Ayar |
|---|---|---|
| 3 → 10 replica | **45 saniye** | `scaleUp.stabilizationWindowSeconds: 0` |
| 10 → 2 replica | **~3 dakika** | `scaleDown.stabilizationWindowSeconds: 60` + metrik gecikmesi |

Asimetri bilinçlidir: büyümede gecikmenin bedelini kullanıcı öder, küçülmede acele
etmek zikzak (thrashing) yaratır. `maxReplicas` tavana çarptığında (`cpu: 140%/50%`,
replica 10'da sabit) sıradaki adım node eklemektir — **HPA pod ekler, Cluster Autoscaler node ekler.**

### PVC + StatefulSet (PostgreSQL)
`k8s/postgres.yaml`: headless Service (`clusterIP: None`) + StatefulSet + `volumeClaimTemplates`.

Pod silme testi:

| | Silmeden önce | Sildikten sonra |
|---|---|---|
| Pod adı | `pg-0` | `pg-0` — **aynı** |
| IP | `10.244.1.29` | `10.244.1.30` — değişti |
| PVC | `pvc-989aec9f…` | `pvc-989aec9f…` — **aynı disk** |
| Veri | 2 satır | **2 satır** |

Kalıcı DNS: `pg-0.pg.default.svc.cluster.local`. Deployment rastgele isim verir,
StatefulSet sıralı ve kalıcı isim + pod başına ayrı disk verir.

> `RECLAIM POLICY: Delete` — PVC silinince veri de silinir. Veritabanı için `Retain` kullan.

**kind tuzağı:** çok-mimarili imajlarda `kind load docker-image` şu hatayı verebilir:
`ctr: content digest ... not found`. Çözüm: node'ların imajı registry'den çekmesine izin ver.

### RBAC
`k8s/rbac.yaml`: ServiceAccount `okuyucu` + Role (`pods: get/list/watch`) + RoleBinding.

```bash
kubectl auth can-i list pods   --as=system:serviceaccount:default:okuyucu -n default   # yes
kubectl auth can-i delete pods --as=system:serviceaccount:default:okuyucu -n default   # no
kubectl auth can-i list pods   --as=system:serviceaccount:default:okuyucu -n staging   # no
```

Pod içinden gerçek API çağrısı (token `/var/run/secrets/kubernetes.io/serviceaccount/` altında
otomatik montelidir):
- `GET /api/v1/namespaces/default/pods` → **200**, PodList
- `GET /api/v1/namespaces/default/secrets` → **403 Forbidden**

Kurallar: Role tek namespace / ClusterRole cluster geneli · API'ye erişmeyen iş yüküne
`automountServiceAccountToken: false` · `default` ServiceAccount'a asla yetki verme.

### Helm — `chart/`
```bash
helm lint chart
helm template prod chart                       # kurmadan ciktiyi gor
helm upgrade --install dev  chart -f chart/values-dev.yaml  -n helm-dev  --create-namespace --wait
helm upgrade --install prod chart -f chart/values-prod.yaml -n helm-prod --create-namespace --wait
helm history dev -n helm-dev
helm rollback dev 1 -n helm-dev
```

Tek chart, iki ortam: `dev` → 1 replica / `ORTAM=dev` / HPA yok,
`prod` → HPA açık (2-8) / `ORTAM=production`. HPA şablonu `{{ if .Values.autoscaling.enabled }}`
ile koşullu üretilir.

`checksum/config` anotasyonu: ConfigMap değişince pod'ların yeniden başlaması için standart hile —
yoksa ConfigMap güncellenir ama pod'lar eski değeri kullanmaya devam eder.

**`kubectl rollout undo` vs `helm rollback`:** ilki sadece imajı, ikincisi tüm manifest setini
(replica, config, secret, HPA) atomik olarak geri alır — ölçülen süre **5.8 saniye**.

## Temizlik

```bash
helm uninstall dev -n helm-dev; helm uninstall prod -n helm-prod
kind delete cluster --name k8s-lab
```
