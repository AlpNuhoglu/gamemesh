.PHONY: help build test test-integration lint cover up down k8s-build k8s-apply k8s-delete k6-leaderboard k6-matchmaking k6-websocket

SERVICES := gateway player matchmaking leaderboard websocket

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

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

k8s-build: ## Build images into Minikube's Docker daemon
	@eval $$(minikube docker-env) && \
	for svc in $(SERVICES); do \
		echo "building gamemesh/$$svc:latest"; \
		docker build -f deployments/docker/Dockerfile.$$svc -t gamemesh/$$svc:latest .; \
	done

k8s-apply: ## Deploy everything to the cluster
	kubectl apply -f deployments/k8s/

k8s-delete: ## Tear down the K8s deployment
	kubectl delete -f deployments/k8s/ --ignore-not-found

k6-leaderboard: ## 10k leaderboard updates
	@mkdir -p scripts/k6/reports
	k6 run --summary-export=scripts/k6/reports/leaderboard.json scripts/k6/leaderboard.js

k6-matchmaking: ## 5k concurrent matchmaking requests
	@mkdir -p scripts/k6/reports
	k6 run --summary-export=scripts/k6/reports/matchmaking.json scripts/k6/matchmaking.js

k6-websocket: ## 1k concurrent WebSocket clients
	@mkdir -p scripts/k6/reports
	k6 run --summary-export=scripts/k6/reports/websocket.json scripts/k6/websocket.js
