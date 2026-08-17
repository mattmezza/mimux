.DEFAULT_GOAL := help

VERSION ?= dev
BINARY  := mimux
IMAGE   := ghcr.io/mattmezza/mimux

##@ Development
.PHONY: dev
dev: ## Run with hot reload (requires air)
	$(shell go env GOPATH)/bin/air

.PHONY: run
run: ## Build and run locally
	go run ./cmd/mimux

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

.PHONY: test-pro
test-pro: ## Run tests including the pro layer
	go test -race -cover -tags pro ./...

.PHONY: check
check: lint test verify-licence verify-free verify-boundary ## Run all checks

.PHONY: diagnose
diagnose: ## Sanitised environment dump to paste into a bug report
	@sh scripts/diagnose.sh

##@ Build
# The free build passes no build tags, so every file in pro/ (each carrying
# //go:build pro) is excluded from the build graph entirely. See LICENSING.md.
.PHONY: build
build: css-build ## Build the free binary (AGPL-3.0 only)
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/mimux

.PHONY: build-pro
build-pro: css-build ## Build the commercial binary (AGPL client + ELv2 pro)
	CGO_ENABLED=0 go build -tags pro -ldflags="-s -w -X main.version=$(VERSION)-pro" -o bin/$(BINARY)-pro ./cmd/mimux

.PHONY: docker
docker: ## Build Docker image locally
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

##@ Licence
# Both targets are wired into `make check` and CI, so the split in LICENSING.md
# is verified rather than merely claimed.
.PHONY: verify-free
verify-free: ## Prove the free binary links zero ELv2 code
	@if go list -deps ./cmd/mimux | grep -q '/mimux/pro'; then \
		echo "FAIL: the free build depends on pro/ (ELv2). Check the //go:build pro tags."; \
		go list -deps ./cmd/mimux | grep '/mimux/pro'; exit 1; \
	fi
	@go list -deps -tags pro ./cmd/mimux | grep -q '/mimux/pro' \
		|| { echo "FAIL: the pro build does NOT link pro/ — the build tag is broken."; exit 1; }
	@echo "OK: free build excludes pro/; pro build includes it."

.PHONY: verify-boundary
verify-boundary: ## Prove pro/ binds via internal/ext and never reaches into internal/server
	@if go list -deps -tags pro ./pro | grep -q '/mimux/internal/server'; then \
		echo "FAIL: pro/ imports internal/server."; \
		echo "  Bind through internal/ext instead. If you need something that only"; \
		echo "  exists as a private method on *server.Server, move it down into"; \
		echo "  internal/mail or internal/store and call it from both sides."; \
		exit 1; \
	fi
	@echo "OK: pro/ does not reach into internal/server."

.PHONY: verify-licence
verify-licence: ## Every .go file has an SPDX header, and ELv2 only under pro/
	@missing=$$(grep -rL 'SPDX-License-Identifier' --include='*.go' cmd internal pro web 2>/dev/null); \
	if [ -n "$$missing" ]; then echo "FAIL: missing SPDX header:"; echo "$$missing"; exit 1; fi
	@stray=$$(grep -rl 'LicenseRef-Elastic-2.0' --include='*.go' cmd internal web 2>/dev/null); \
	if [ -n "$$stray" ]; then echo "FAIL: ELv2 header outside pro/:"; echo "$$stray"; exit 1; fi
	@agpl=$$(grep -rl 'AGPL-3.0-only' --include='*.go' pro 2>/dev/null); \
	if [ -n "$$agpl" ]; then echo "FAIL: AGPL header inside pro/:"; echo "$$agpl"; exit 1; fi
	@echo "OK: SPDX headers present and on the right side of the line."

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
	@echo "Done. Run 'make dev' — mimux boots with zero config; add accounts from Settings → Accounts."

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\n\033[1mmimux\033[0m\n\nUsage: make \033[36m<target>\033[0m\n"} \
		/^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} \
		/^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
