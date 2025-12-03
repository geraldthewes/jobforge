# Makefile for Nomad Build Service

# Build variables
BINARY_NAME=jobforge
IMAGE_NAME=nomad-build-service

# Versioning - automatically increment patch version
LATEST_TAG := $(shell git tag --list | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | sort -V | tail -1)
ifeq ($(LATEST_TAG),)
	# No version tags exist, start with v0.0.1
	VERSION := 0.0.1
else
	# Extract current version and increment patch
	CURRENT_VERSION := $(shell echo $(LATEST_TAG) | sed 's/^v//')
	MAJOR := $(shell echo $(CURRENT_VERSION) | cut -d. -f1)
	MINOR := $(shell echo $(CURRENT_VERSION) | cut -d. -f2)
	PATCH := $(shell echo $(CURRENT_VERSION) | cut -d. -f3)
	NEXT_PATCH := $(shell echo $$(($(PATCH) + 1)))
	VERSION := $(MAJOR).$(MINOR).$(NEXT_PATCH)
endif

BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
GIT_COMMIT=$(shell git rev-parse --short HEAD)
LDFLAGS=-ldflags "-X main.version=${VERSION} -X main.buildTime=${BUILD_TIME} -X main.gitCommit=${GIT_COMMIT}"

# Go variables
GO_VERSION=1.22
GOPATH=$(shell go env GOPATH)
GOOS=$(shell go env GOOS)
GOARCH=$(shell go env GOARCH)

# Docker variables
DOCKER_REGISTRY?=${REGISTRY_URL}
DOCKER_TAG=${DOCKER_REGISTRY}/${IMAGE_NAME}:${VERSION}

.PHONY: help build clean test lint docker-build docker-push deploy-dev deploy-prod

# Default target
help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-15s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Build targets
build: ## Build the application binary
	@echo "Building ${BINARY_NAME} server for ${GOOS}/${GOARCH}..."
	@CGO_ENABLED=0 go build ${LDFLAGS} -o bin/${BINARY_NAME} ./cmd/server
	@echo "Server build completed: bin/${BINARY_NAME}"
	@echo "Building jobforge CLI for ${GOOS}/${GOARCH}..."
	@CGO_ENABLED=0 go build ${LDFLAGS} -o bin/jobforge ./cmd/nomad-build
	@echo "CLI build completed: bin/jobforge"

build-linux: ## Build Linux binary
	@echo "Building ${BINARY_NAME} for linux/amd64..."
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o bin/${BINARY_NAME}-linux-amd64 ./cmd/server

build-all: ## Build binaries for all platforms
	@echo "Building server for multiple platforms..."
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o bin/${BINARY_NAME}-linux-amd64 ./cmd/server
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o bin/${BINARY_NAME}-linux-arm64 ./cmd/server
	@CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -o bin/${BINARY_NAME}-darwin-amd64 ./cmd/server
	@CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ${LDFLAGS} -o bin/${BINARY_NAME}-darwin-arm64 ./cmd/server
	@CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ${LDFLAGS} -o bin/${BINARY_NAME}-windows-amd64.exe ./cmd/server
	@echo "Building CLI for multiple platforms..."
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o bin/jobforge-linux-amd64 ./cmd/nomad-build
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o bin/jobforge-linux-arm64 ./cmd/nomad-build
	@CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -o bin/jobforge-darwin-amd64 ./cmd/nomad-build
	@CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ${LDFLAGS} -o bin/jobforge-darwin-arm64 ./cmd/nomad-build
	@CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ${LDFLAGS} -o bin/jobforge-windows-amd64.exe ./cmd/nomad-build
	@echo "Multi-platform build completed"

# Development targets
run: ## Run the application locally
	@echo "Running ${BINARY_NAME}..."
	@go run ./cmd/server

dev: ## Run in development mode with auto-reload
	@echo "Starting development server..."
	@which air > /dev/null || go install github.com/cosmtrek/air@latest
	@air -c .air.toml

# Testing targets
test: ## Run all tests
	@echo "Running tests..."
	@go test -v ./...

test-coverage: ## Run tests with coverage
	@echo "Running tests with coverage..."
	@go test -v -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

test-integration: ## Run integration tests
	@echo "Running integration tests..."
	@go test -v -tags=integration ./...

benchmark: ## Run benchmarks
	@echo "Running benchmarks..."
	@go test -bench=. -benchmem ./...

# Code quality targets
lint: ## Run linter
	@echo "Running linter..."
	@which golangci-lint > /dev/null || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(shell go env GOPATH)/bin v1.54.2
	@golangci-lint run

fmt: ## Format code
	@echo "Formatting code..."
	@go fmt ./...
	@goimports -w .

vet: ## Run go vet
	@echo "Running go vet..."
	@go vet ./...

security: ## Run security scan
	@echo "Running security scan..."
	@which gosec > /dev/null || go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest
	@gosec ./...

# Dependency management
deps: ## Download dependencies
	@echo "Downloading dependencies..."
	@go mod download
	@go mod verify

deps-update: ## Update dependencies
	@echo "Updating dependencies..."
	@go get -u ./...
	@go mod tidy

deps-check: ## Check for unused dependencies
	@echo "Checking for unused dependencies..."
	@which modcheck > /dev/null || go install github.com/tomarrell/modcheck@latest
	@modcheck ./...

# Docker targets
docker-build: ## Build Docker image
	@echo "Building Docker image: ${IMAGE_NAME}:${VERSION}"
	@docker build -t ${IMAGE_NAME}:${VERSION} .
	@docker tag ${IMAGE_NAME}:${VERSION} ${IMAGE_NAME}:latest
	@echo "Docker image built successfully"

docker-build-multi: ## Build multi-platform Docker image
	@echo "Building multi-platform Docker image..."
	@docker buildx build --platform linux/amd64,linux/arm64 -t ${DOCKER_TAG} --push .

docker-push: docker-build version-tag ## Push Docker image to registry and create version tag
	@echo "Pushing Docker image to registry..."
	@docker tag ${IMAGE_NAME}:${VERSION} ${DOCKER_TAG}
	@docker push ${DOCKER_TAG}
	@echo "Docker image pushed: ${DOCKER_TAG}"
	@docker tag ${IMAGE_NAME}:${VERSION} ${DOCKER_REGISTRY}/${IMAGE_NAME}:latest
	@docker push ${DOCKER_REGISTRY}/${IMAGE_NAME}:latest
	@echo "Docker image also pushed as: ${DOCKER_REGISTRY}/${IMAGE_NAME}:latest"
	@echo "Version v${VERSION} tagged locally"

docker-run: ## Run Docker container locally
	@echo "Running Docker container..."
	@docker run -p 8080:8080 -p 9090:9090 -p 8081:8081 \
		-e NOMAD_ADDR=http://host.docker.internal:4646 \
		-e CONSUL_HTTP_ADDR=host.docker.internal:8500 \
		-e VAULT_ADDR=http://host.docker.internal:8200 \
		--name ${BINARY_NAME} ${IMAGE_NAME}:${VERSION}

docker-stop: ## Stop Docker container
	@docker stop ${BINARY_NAME} || true
	@docker rm ${BINARY_NAME} || true

nomad-restart: ## Restart the Nomad service to pull latest image
	@echo "Restarting Nomad service..."
	@nomad job stop -yes nomad-build-service
	@nomad job start  nomad-build-service
	@echo "Nomad service restarted successfully"

# Database/Setup targets
setup-dev: ## Set up development environment
	@echo "Setting up development environment..."
	@docker-compose -f docker-compose.dev.yml up -d
	@sleep 10
	@./scripts/setup-dev-data.sh
	@echo "Development environment ready"

teardown-dev: ## Tear down development environment
	@echo "Tearing down development environment..."
	@docker-compose -f docker-compose.dev.yml down -v

# Deployment targets
deploy-dev: docker-build ## Deploy to development environment
	@echo "Deploying to development environment..."
	@nomad job run deployments/dev.nomad

deploy-staging: docker-build docker-push ## Deploy to staging environment
	@echo "Deploying to staging environment..."
	@nomad job run deployments/staging.nomad

deploy-prod: docker-build docker-push ## Deploy to production environment
	@echo "Deploying to production environment..."
	@nomad job run deployments/production.nomad

# Monitoring targets
logs: ## Show application logs
	@echo "Showing application logs..."
	@nomad logs -f nomad-build-service

logs-tail: ## Tail application logs
	@nomad logs -tail -f nomad-build-service

status: ## Show deployment status
	@echo "Deployment status:"
	@nomad job status nomad-build-service

# Maintenance targets
clean: ## Clean build artifacts
	@echo "Cleaning build artifacts..."
	@rm -rf bin/
	@rm -f coverage.out coverage.html
	@go clean -cache
	@docker system prune -f

backup: ## Backup configuration
	@echo "Backing up configuration..."
	@mkdir -p backups
	@consul kv export nomad-build-service/ > backups/consul-config-$(shell date +%Y%m%d_%H%M%S).json
	@echo "Configuration backed up"

restore: ## Restore configuration (requires BACKUP_FILE variable)
	@echo "Restoring configuration from ${BACKUP_FILE}..."
	@consul kv import @${BACKUP_FILE}
	@echo "Configuration restored"

health-check: ## Check service health
	@echo "Checking service health..."
	@curl -f http://localhost:8081/health || echo "Health check failed"
	@curl -f http://localhost:8081/ready || echo "Readiness check failed"

metrics: ## Show metrics
	@echo "Service metrics:"
	@curl -s http://localhost:9090/metrics | grep -E "^(build_|test_|publish_|job_|concurrent_|total_|resource_|health_)"

# Versioning targets
version-info: ## Show current version information
	@echo "Current version: ${VERSION}"
	@echo "Latest git tag: ${LATEST_TAG}"
	@echo "Build time: ${BUILD_TIME}"
	@echo "Git commit: ${GIT_COMMIT}"

version-tag: ## Create local version tag
	@echo "Creating local version tag: v${VERSION}"
	@git tag -a v${VERSION} -m "Release version ${VERSION}"
	@echo "Version v${VERSION} tagged locally"

version-major: ## Set major version (use: make version-major MAJOR=1)
	@if [ -z "$(MAJOR)" ]; then echo "Usage: make version-major MAJOR=X"; exit 1; fi
	@echo "Setting major version to $(MAJOR).0.0"
	@git tag -a v$(MAJOR).0.0 -m "Major release version $(MAJOR).0.0"
	@echo "Major version v$(MAJOR).0.0 tagged locally"

version-minor: ## Set minor version (use: make version-minor MINOR=1)
	@if [ -z "$(MINOR_VER)" ]; then echo "Usage: make version-minor MINOR_VER=X"; exit 1; fi
	@echo "Setting minor version to $(MAJOR).$(MINOR_VER).0"
	@git tag -a v$(MAJOR).$(MINOR_VER).0 -m "Minor release version $(MAJOR).$(MINOR_VER).0"
	@echo "Minor version v$(MAJOR).$(MINOR_VER).0 tagged locally"

# Installation targets
install: build ## Install binary to system
	@echo "Installing ${BINARY_NAME} to /usr/local/bin..."
	@sudo cp bin/${BINARY_NAME} /usr/local/bin/
	@sudo chmod +x /usr/local/bin/${BINARY_NAME}
	@echo "Installation completed"

uninstall: ## Uninstall binary from system
	@echo "Uninstalling ${BINARY_NAME}..."
	@sudo rm -f /usr/local/bin/${BINARY_NAME}
	@echo "Uninstallation completed"

# Release targets
tag: ## Create local git tag (requires VERSION variable)
	@echo "Creating local git tag: v${VERSION}"
	@git tag -a v${VERSION} -m "Release version ${VERSION}"
	@echo "Git tag v${VERSION} created locally"

release: clean test lint build-all docker-build docker-push ## Create a full release
	@echo "Creating release ${VERSION}..."
	@mkdir -p release
	@cp bin/* release/
	@tar -czf release/${BINARY_NAME}-${VERSION}.tar.gz -C release .
	@echo "Release ${VERSION} created in release/"

# Documentation targets
docs: ## Generate documentation
	@echo "Generating documentation..."
	@which godoc > /dev/null || go install golang.org/x/tools/cmd/godoc@latest
	@echo "Documentation server starting at http://localhost:6060"
	@godoc -http=:6060

docs-generate: ## Generate static documentation
	@echo "Generating static documentation..."
	@mkdir -p docs/api
	@go doc -all ./... > docs/api/godoc.txt

# Default development workflow
dev-setup: deps setup-dev ## Complete development setup
dev-test: lint test test-coverage ## Run all code quality checks
dev-build: clean build docker-build ## Clean build for development
