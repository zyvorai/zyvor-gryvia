# Gryvia Makefile
# Automates building, testing, and deploying all components

.PHONY: all build test clean docker-build docker-push deploy install help

# Variables
REGISTRY ?= ghcr.io/zyvorai
VERSION ?= $(shell git describe --tags --always --dirty)
OPERATORS := gpu-operator ai-operator storage-operator network-operator quota-operator

# Colors for output
GREEN  := $(shell tput -Txterm setaf 2)
YELLOW := $(shell tput -Txterm setaf 3)
RESET  := $(shell tput -Txterm sgr0)

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  ${GREEN}%-20s${RESET} %s\n", $$1, $$2}' $(MAKEFILE_LIST)

all: build test ## Build and test all components

## Build targets
build: build-operators build-cli build-web-ui build-api-gateway ## Build all components

build-operators: ## Build all operators
	@echo "${GREEN}Building operators...${RESET}"
	@for op in $(OPERATORS); do \
		echo "${YELLOW}Building $$op...${RESET}"; \
		(cd operators/$$op && go build -o bin/manager main.go); \
	done

build-cli: ## Build CLI tool
	@echo "${GREEN}Building CLI...${RESET}"
	cd cli && cargo build --release

build-web-ui: ## Build Web UI
	@echo "${GREEN}Building Web UI...${RESET}"
	cd web-ui && npm ci && npm run build

build-api-gateway: ## Build API Gateway
	@echo "${GREEN}Building API Gateway...${RESET}"
	cd services/api-gateway && pip install -r requirements.txt

## Test targets
test: test-operators test-cli test-api-gateway ## Run all tests

test-operators: ## Test all operators
	@echo "${GREEN}Testing operators...${RESET}"
	@for op in $(OPERATORS); do \
		echo "${YELLOW}Testing $$op...${RESET}"; \
		(cd operators/$$op && go test -v ./...); \
	done

test-cli: ## Test CLI
	@echo "${GREEN}Testing CLI...${RESET}"
	cd cli && cargo test

test-api-gateway: ## Test API Gateway
	@echo "${GREEN}Testing API Gateway...${RESET}"
	cd services/api-gateway && pytest

## Docker targets
docker-build: docker-build-operators docker-build-cli docker-build-web-ui docker-build-api-gateway ## Build all Docker images

docker-build-operators: ## Build operator Docker images
	@echo "${GREEN}Building operator images...${RESET}"
	@for op in $(OPERATORS); do \
		echo "${YELLOW}Building $$op image...${RESET}"; \
		docker build -t $(REGISTRY)/gryvia-$$op:$(VERSION) operators/$$op; \
		docker tag $(REGISTRY)/gryvia-$$op:$(VERSION) $(REGISTRY)/gryvia-$$op:latest; \
	done

docker-build-cli: ## Build CLI Docker image
	@echo "${GREEN}Building CLI image...${RESET}"
	docker build -t $(REGISTRY)/gryvia-cli:$(VERSION) cli
	docker tag $(REGISTRY)/gryvia-cli:$(VERSION) $(REGISTRY)/gryvia-cli:latest

docker-build-web-ui: ## Build Web UI Docker image
	@echo "${GREEN}Building Web UI image...${RESET}"
	docker build -t $(REGISTRY)/gryvia-ui:$(VERSION) -f docker/Dockerfile.ui .
	docker tag $(REGISTRY)/gryvia-ui:$(VERSION) $(REGISTRY)/gryvia-ui:latest

docker-build-api-gateway: ## Build API Gateway Docker image
	@echo "${GREEN}Building API Gateway image...${RESET}"
	docker build -t $(REGISTRY)/gryvia-api-gateway:$(VERSION) services/api-gateway
	docker tag $(REGISTRY)/gryvia-api-gateway:$(VERSION) $(REGISTRY)/gryvia-api-gateway:latest

docker-push: ## Push all Docker images
	@echo "${GREEN}Pushing Docker images...${RESET}"
	@for op in $(OPERATORS); do \
		docker push $(REGISTRY)/gryvia-$$op:$(VERSION); \
		docker push $(REGISTRY)/gryvia-$$op:latest; \
	done
	docker push $(REGISTRY)/gryvia-cli:$(VERSION)
	docker push $(REGISTRY)/gryvia-cli:latest
	docker push $(REGISTRY)/gryvia-ui:$(VERSION)
	docker push $(REGISTRY)/gryvia-ui:latest
	docker push $(REGISTRY)/gryvia-api-gateway:$(VERSION)
	docker push $(REGISTRY)/gryvia-api-gateway:latest

## Deployment targets
deploy: deploy-crds deploy-operators deploy-web-ui deploy-api-gateway ## Deploy all components to Kubernetes

deploy-crds: ## Deploy CRDs
	@echo "${GREEN}Deploying CRDs...${RESET}"
	kubectl apply -f crds/

deploy-operators: ## Deploy operators
	@echo "${GREEN}Deploying operators...${RESET}"
	@for op in $(OPERATORS); do \
		kubectl apply -f operators/$$op/config/deployment.yaml; \
	done

deploy-web-ui: ## Deploy Web UI
	@echo "${GREEN}Deploying Web UI...${RESET}"
	kubectl apply -f manifests/deploy/ui-deployment.yaml

deploy-api-gateway: ## Deploy API Gateway
	@echo "${GREEN}Deploying API Gateway...${RESET}"
	kubectl apply -f manifests/deploy/api-gateway-deployment.yaml

deploy-monitoring: ## Deploy monitoring stack
	@echo "${GREEN}Deploying monitoring...${RESET}"
	kubectl apply -f monitoring/

install: ## Install using Helm
	@echo "${GREEN}Installing Gryvia with Helm...${RESET}"
	helm repo add nvidia https://helm.ngc.nvidia.com/nvidia --force-update
	helm dependency build helm/gryvia
	helm install gryvia helm/gryvia -n gryvia-system --create-namespace

uninstall: ## Uninstall using Helm
	@echo "${GREEN}Uninstalling Gryvia...${RESET}"
	helm uninstall gryvia -n gryvia-system

sovereign-aios-deps: ## Copy the Gryvia, Zyntra and Netra charts into helm/sovereign-aios/charts (ZYNTRA_DIR, NETRA_DIR)
	scripts/sovereign-aios-deps.sh

sovereign-aios-test: sovereign-aios-deps ## Render tests for the Sovereign AI OS chart
	scripts/tests/sovereign-aios-chart.test.sh

sovereign-aios-agent-test: ## A GryviaAgent's zyntra tool against a real Zyntra built from ZYNTRA_DIR (no cluster)
	scripts/tests/sovereign-aios-agent-zyntra.sh

sovereign-aios-airgap-test: sovereign-aios-deps ## Offline checks of the air-gap artifacts (deploy/airgap, deploy/argocd)
	scripts/tests/sovereign-aios-airgap.test.sh

sovereign-aios-images: sovereign-aios-deps ## Regenerate deploy/airgap/images.txt (pulls the add-on charts)
	scripts/sovereign-aios-images.sh --addons > deploy/airgap/images.txt

sovereign-aios-install: sovereign-aios-deps ## Install Gryvia, Zyntra and Netra as one release (docs/sovereign-aios.md)
	helm upgrade --install sovereign-aios helm/sovereign-aios -n gryvia-system --create-namespace

## Development targets
dev-setup: ## Set up development environment
	@echo "${GREEN}Setting up development environment...${RESET}"
	@echo "Installing Go dependencies..."
	@for op in $(OPERATORS); do \
		(cd operators/$$op && go mod download); \
	done
	@echo "Installing Rust toolchain..."
	rustup update stable
	@echo "Installing Node.js dependencies..."
	cd web-ui && npm install
	@echo "Installing Python dependencies..."
	cd services/api-gateway && pip install -r requirements.txt
	@echo "${GREEN}Development environment ready!${RESET}"

fmt: ## Format all code
	@echo "${GREEN}Formatting code...${RESET}"
	@for op in $(OPERATORS); do \
		(cd operators/$$op && go fmt ./...); \
	done
	cd cli && cargo fmt
	cd web-ui && npm run format || true
	cd services/api-gateway && black main.py

lint: ## Lint all code
	@echo "${GREEN}Linting code...${RESET}"
	@for op in $(OPERATORS); do \
		(cd operators/$$op && golangci-lint run); \
	done
	cd cli && cargo clippy
	cd web-ui && npm run lint || true
	cd services/api-gateway && flake8 main.py

clean: ## Clean build artifacts
	@echo "${GREEN}Cleaning build artifacts...${RESET}"
	@for op in $(OPERATORS); do \
		rm -rf operators/$$op/bin; \
	done
	cd cli && cargo clean
	rm -rf web-ui/dist web-ui/node_modules
	find . -type d -name __pycache__ -exec rm -rf {} +

## Documentation targets
docs: ## Generate documentation
	@echo "${GREEN}Generating documentation...${RESET}"
	@for op in $(OPERATORS); do \
		(cd operators/$$op && go doc -all > REFERENCE.md); \
	done

## Release targets
release: test docker-build docker-push ## Create a release (test, build, push)
	@echo "${GREEN}Release $(VERSION) complete!${RESET}"

version: ## Show current version
	@echo "Version: $(VERSION)"

.DEFAULT_GOAL := help
