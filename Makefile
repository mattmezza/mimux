.DEFAULT_GOAL := help

VERSION ?= dev
BINARY  := sm
IMAGE   := ghcr.io/mattmezza/sm

##@ Development
.PHONY: dev
dev: ## Run with hot reload (requires air)
	air

.PHONY: run
run: ## Build and run locally
	go run ./cmd/sm

.PHONY: css
css: ## Build Tailwind CSS (watch mode)
	npx @tailwindcss/cli -i web/static/css/app.css -o web/static/css/dist.css --watch

.PHONY: css-build
css-build: ## Build Tailwind CSS (production)
	npx @tailwindcss/cli -i web/static/css/app.css -o web/static/css/dist.css --minify

##@ Testing
.PHONY: test
test: ## Run tests
	go test -race -cover ./...

.PHONY: lint
lint: ## Run linter
	golangci-lint run

.PHONY: check
check: lint test ## Run all checks

##@ Build
.PHONY: build
build: css-build ## Build binary
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/sm

.PHONY: docker
docker: ## Build Docker image locally
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

##@ Release
.PHONY: release
release: ## Create a GitHub release (usage: make release name=v0.1)
	@if [ -z "$(name)" ]; then echo "Usage: make release name=vX.Y"; exit 1; fi
	@echo "Creating release $(name)..."
	git tag -a $(name) -m "Release $(name)"
	git push origin $(name)
	gh release create $(name) --generate-notes --title "$(name)"
	@echo "Release $(name) created. GitHub Actions will build and push the Docker image."

##@ Setup
.PHONY: setup
setup: ## Install development dependencies
	go install github.com/air-verse/air@latest
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	npm install
	cp -n config.example.toml config.toml 2>/dev/null || true
	@echo "Done. Edit config.toml and run 'make dev'."

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\n\033[1mSM — Simple Mail\033[0m\n\nUsage: make \033[36m<target>\033[0m\n"} \
		/^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} \
		/^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
