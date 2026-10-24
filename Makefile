.PHONY: help build test test-integration lint cover up down k8s-build k8s-secrets k8s-context k8s-apply k8s-delete k6-leaderboard k6-matchmaking k6-websocket

# Every directory under cmd/, as CI's image matrix: the hand-written list had
# lost outbox-relay, so k8s-build skipped an image the cluster needs.
SERVICES := $(notdir $(patsubst %/,%,$(wildcard cmd/*/)))
# The local k3d cluster the k8s-* targets act on (see docs/deployment.md).
K3D_CLUSTER := gamemesh
K3D_CONTEXT := k3d-$(K3D_CLUSTER)

help: ## Show available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## Build all service binaries into ./bin
	@mkdir -p bin
	@for svc in $(SERVICES); do \
		echo "building $$svc"; \
		CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/$$svc ./cmd/$$svc; \
	done

test: ## Run unit tests with race detector
	go test -race -coverprofile=coverage.out -covermode=atomic ./cmd/... ./internal/... ./pkg/...

cover: test ## Show coverage summary
	go tool cover -func=coverage.out | tail -1

test-integration: ## Run Testcontainers integration tests (needs Docker)
	go test -tags integration -timeout 10m ./tests/integration/...

lint: ## Run golangci-lint
	golangci-lint run

up: ## Start the full local stack
	docker compose up --build -d

down: ## Stop the local stack
	docker compose down

k8s-build: ## Build the images and load them into the local k3d cluster
	@for svc in $(SERVICES); do \
		echo "building gamemesh/$$svc:dev"; \
		docker build -f deployments/docker/Dockerfile --build-arg SERVICE=$$svc -t gamemesh/$$svc:dev . || exit 1; \
	done
	k3d image import $(addprefix gamemesh/,$(addsuffix :dev,$(SERVICES))) -c $(K3D_CLUSTER)

# Refuses unless kubectl points at the local cluster: these targets must never
# reach production, which is deployed only from CI (overlays/prod).
k8s-context:
	@test "$$(kubectl config current-context)" = "$(K3D_CONTEXT)" || \
		{ echo "kubectl context is '$$(kubectl config current-context)', not $(K3D_CONTEXT); refusing" >&2; exit 1; }

k8s-secrets: ## Generate the local overlay's secrets (.env.local, gitignored)
	@scripts/k8s/local-secrets.sh

# Monitoring the way production has it: the namespace and Prometheus's
# rights (cluster-level there, written by the admin), then what CI applies.
# The app first: it creates gamemesh, where Prometheus's Role lives.
k8s-apply: k8s-context k8s-secrets ## Deploy the local overlay and monitoring to the k3d cluster
	kubectl apply -k deployments/k8s/overlays/local
	kubectl apply -f deployments/k8s/namespace/monitoring.yaml -f deployments/k8s/cluster/prometheus-rbac.yaml
	@scripts/k8s/local-grafana-secret.sh
	kubectl apply -k deployments/k8s/monitoring

k8s-delete: k8s-context ## Tear down the local overlay and monitoring
	kubectl delete -k deployments/k8s/monitoring --ignore-not-found
	kubectl -n monitoring delete secret grafana-admin --ignore-not-found
	kubectl delete -f deployments/k8s/cluster/prometheus-rbac.yaml -f deployments/k8s/namespace/monitoring.yaml --ignore-not-found
	kubectl delete -k deployments/k8s/overlays/local --ignore-not-found

k6-leaderboard: ## 10k leaderboard updates
	@mkdir -p scripts/k6/reports
	k6 run --summary-export=scripts/k6/reports/leaderboard.json scripts/k6/leaderboard.js

k6-matchmaking: ## 5k concurrent matchmaking requests
	@mkdir -p scripts/k6/reports
	k6 run --summary-export=scripts/k6/reports/matchmaking.json scripts/k6/matchmaking.js

k6-websocket: ## 1k concurrent WebSocket clients
	@mkdir -p scripts/k6/reports
	k6 run --summary-export=scripts/k6/reports/websocket.json scripts/k6/websocket.js
