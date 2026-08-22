# Common tasks. Run `make help` for the list.
SHELL       := /usr/bin/env bash
CLUSTER     ?= k8s-lab
NAMESPACE   ?= k8s-lab
RELEASE     ?= app
IMAGE       ?= k8s-lab-app
IMAGE_TAG   ?= 1.0.0

.DEFAULT_GOAL := help
.PHONY: help test lint build web up down deploy smoke policies policy-test tls logs monitoring port-forward clean

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

test: ## Run unit and integration tests
	cd app && npm ci --silent && npm test

lint: ## Lint JavaScript and the Helm chart
	cd app && npm run lint
	cd web && npm run lint
	helm lint chart
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
