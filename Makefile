# Common tasks. Run `make help` for the list.
SHELL       := /usr/bin/env bash
CLUSTER     ?= k8s-lab
NAMESPACE   ?= k8s-lab
RELEASE     ?= app
IMAGE       ?= k8s-lab-app
IMAGE_TAG   ?= 1.0.0

.DEFAULT_GOAL := help
.PHONY: help test lint build web up down deploy smoke policies policy-test platform platform-plan tls logs logging gitops monitoring port-forward clean

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

test: ## Run unit and integration tests
	cd app && npm ci --silent && npm test

lint: ## Lint JavaScript, the Helm chart and the Terraform
	cd app && npm run lint
	cd web && npm run lint
	helm lint chart
	terraform -chdir=terraform fmt -check -diff
	terraform -chdir=terraform init -backend=false -input=false >/dev/null
	terraform -chdir=terraform validate
	helm template ci chart -f chart/values-prod.yaml > /dev/null
	helm template ci chart -f chart/values-dev.yaml  > /dev/null

build: ## Build both container images
	docker build -t $(IMAGE):$(IMAGE_TAG) ./app
	docker build -t $(IMAGE)-web:$(IMAGE_TAG) ./web

web: ## Run the interface locally against a port-forwarded backend
	@echo "run this in another terminal first:"
	@echo "  kubectl -n $(NAMESPACE) port-forward svc/$(RELEASE)-k8s-lab-app 18080:80"
	cd web && npm run dev

up: ## Create the cluster, install ingress, build and deploy
	./scripts/bootstrap.sh

deploy: build ## Rebuild the images and upgrade the release
	kind load docker-image $(IMAGE):$(IMAGE_TAG) $(IMAGE)-web:$(IMAGE_TAG) --name $(CLUSTER)
	helm upgrade --install $(RELEASE) ./chart \
	  --namespace $(NAMESPACE) --create-namespace \
	  --set image.tag=$(IMAGE_TAG) \
	  --set web.image.tag=$(IMAGE_TAG) \
	  --set postgres.auth.password=$${PGPASSWORD:-local-dev-password} \
	  -f chart/values-prod.yaml --timeout 10m
	kubectl -n $(NAMESPACE) rollout status statefulset/$(RELEASE)-postgres --timeout=300s
	kubectl -n $(NAMESPACE) rollout status deployment/$(RELEASE)-k8s-lab-app --timeout=300s

smoke: ## Run the end-to-end smoke test against the deployed release
	NAMESPACE=$(NAMESPACE) RELEASE=$(RELEASE) ./scripts/smoke-test.sh

policies: ## Apply Pod Security Admission labels and the admission policies
	kubectl apply -f policies/namespace.yaml
	kubectl apply -f policies/admission-policies.yaml -f policies/admission-bindings.yaml

policy-test: ## Prove each policy rejects what it is supposed to
	NAMESPACE=$(NAMESPACE) ./scripts/policy-test.sh

logs: ## Tail application logs
	kubectl -n $(NAMESPACE) logs -l app.kubernetes.io/name=k8s-lab-app -f --tail=50

monitoring: ## Install Prometheus + Grafana
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
	helm repo update
	helm upgrade --install monitoring prometheus-community/kube-prometheus-stack \
	  -n monitoring --create-namespace --set grafana.adminPassword=admin \
	  --set alertmanager.enabled=false --wait --timeout 15m

port-forward: ## Open Grafana on http://localhost:3000 (admin/admin)
	kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80

operator-test: ## Run the operator's unit and envtest suites
	$(MAKE) -C operator test

operator-install: ## Install the Workload CRD into the current cluster
	$(MAKE) -C operator install

operator-deploy: ## Build the operator image, load it into kind and deploy it
	docker build -t k8s-lab-operator:1.0.0 ./operator
	kind load docker-image k8s-lab-operator:1.0.0 --name k8s-lab
	$(MAKE) -C operator install
	$(MAKE) -C operator deploy-local
	kubectl -n k8s-lab-platform-system rollout status \
	  deployment/k8s-lab-platform-controller-manager --timeout=180s

down: ## Delete the kind cluster
	./scripts/teardown.sh

clean: ## Remove local build artefacts
	rm -rf app/node_modules app/coverage

tls: ## Install cert-manager, issue from a local CA, serve HTTPS on :8443
	kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
	kubectl wait -n cert-manager --for=condition=Available deployment --all --timeout=300s
	kubectl apply -f cluster/issuers.yaml
	helm upgrade --install $(RELEASE) ./chart -n $(NAMESPACE) \
	  -f chart/values-prod.yaml --set image.tag=$(IMAGE_TAG) --set web.image.tag=$(IMAGE_TAG) \
	  --set postgres.auth.password=$${PGPASSWORD:-local-dev-password} \
	  --set 'ingress.extraHosts[0]=localhost' \
	  --set ingress.tls.enabled=true --set ingress.tls.clusterIssuer=selfsigned-ca \
	  --set config.SECURE_COOKIE=true --timeout 10m
	@echo ""
	@echo "  https://localhost:8443 — the browser warns because the CA is local."
	@echo "  Everything else is real: Certificate, secret, TLS listener, 308 redirect, Secure cookie."

logging: ## Install Loki and Alloy, and register Loki with Grafana
	helm repo add grafana https://grafana.github.io/helm-charts
	helm repo update
	helm upgrade --install loki grafana/loki -n monitoring -f cluster/loki-values.yaml --wait --timeout 12m
	helm upgrade --install alloy grafana/alloy -n monitoring -f cluster/alloy-values.yaml --wait --timeout 10m
	kubectl apply -f cluster/loki-datasource.yaml
	@echo ""
	@echo "  Grafana: make port-forward, then Explore -> Loki"
	@echo '  Try:  {namespace="k8s-lab", app="k8s-lab-app"} | json | level=`warn`'

gitops: ## Install Argo CD and let it reconcile the release from git
	helm repo add argo https://argoproj.github.io/argo-helm
	helm repo update
	helm upgrade --install argocd argo/argo-cd -n argocd --create-namespace \
	  -f cluster/argocd-values.yaml --wait --timeout 12m
	kubectl create namespace k8s-lab-gitops --dry-run=client -o yaml | kubectl apply -f -
	kubectl -n k8s-lab-gitops create secret generic db-credentials \
	  --from-literal=POSTGRES_USER=labuser --from-literal=POSTGRES_DB=labdb \
	  --from-literal=POSTGRES_PASSWORD="$$(openssl rand -base64 24)" \
	  --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f gitops/application.yaml
	kubectl apply -f gitops/operator.yaml
	@echo ""
	@echo "  Watch it converge:  kubectl -n argocd get application -w"
	@echo "  Then break it:      kubectl -n k8s-lab-gitops scale deploy/k8s-lab-k8s-lab-app --replicas=5"
	@echo "  UI password:        kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"

platform: ## Apply the platform layer with Terraform (ingress, cert-manager, Argo CD, policies)
	terraform -chdir=terraform init -input=false
	terraform -chdir=terraform apply -input=false -var kube_context=kind-$(CLUSTER)
	kubectl apply -f cluster/issuers.yaml

platform-plan: ## Show what Terraform would change, without changing it
	terraform -chdir=terraform init -input=false -backend=false
	terraform -chdir=terraform plan -input=false -var kube_context=kind-$(CLUSTER)
