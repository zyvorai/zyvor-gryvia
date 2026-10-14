# Gryvia Apple — build, test, run and deploy to the Mac mini.   `make help`
SHELL := /bin/bash
PYTHON ?= $(firstword $(wildcard /opt/homebrew/bin/python3.14) python3)
MINI ?= mini
REMOTE_DIR ?= gryvia
G := $(shell tput setaf 2 2>/dev/null)
Y := $(shell tput setaf 3 2>/dev/null)
R := $(shell tput sgr0 2>/dev/null)
.DEFAULT_GOAL := help
.PHONY: setup-runtimes mini-setup icon brand headers demo help verify test swift-test build smoke web-test package run-server run-agent doctor deploy deploy-quick deploy-verify deploy-dry-run mini-logs mini-status

help: ## Show this help
	@awk 'BEGIN{FS=":.*?## "} /^[a-zA-Z_-]+:.*?## /{printf "  $(G)%-16s$(R) %s\n",$$1,$$2}' $(MAKEFILE_LIST)

icon: ## Regenerate the app icon (.icns, web icon) from brand/gryvia-icon.svg
	bash scripts/make-icon.sh
brand: ## Regenerate web tokens.css and Brand.swift from brand/tokens.json
	$(PYTHON) scripts/gen-brand.py
headers: ## Add missing SPDX/copyright headers
	$(PYTHON) scripts/check-headers.py --fix
demo: ## Local server + agent + seeded jobs, then open the dashboard
	@[ -d local ] || $(PYTHON) scripts/configure.py --output local
	@set -a; . local/credentials.env; \
	($(PYTHON) -m server.app --config local/config.json --db local/gryvia.db >/dev/null 2>&1 &) ; sleep 1; \
	(cd native && swift build -c release --product gryvia-agent >/dev/null) ; \
	(native/.build/release/gryvia-agent local/agent.json >/dev/null 2>&1 &) ; sleep 1; \
	for i in 1 2 3; do $(PYTHON) scripts/gryvia.py --json submit examples/demo-job.json >/dev/null; done; \
	echo "Dashboard: http://127.0.0.1:8443  (token in local/credentials.env)"; open http://127.0.0.1:8443

setup-runtimes: ## Install MLX + mlx-lm into ~/gryvia/venv on this Mac
	bash scripts/setup-runtimes.sh
mini-setup: ## Install MLX on the mini and enable detected runtimes in its agent config
	ssh $(MINI) 'export PATH=/opt/homebrew/bin:$$PATH; cd ~/$(REMOTE_DIR)/app && bash scripts/setup-runtimes.sh && python3.14 scripts/update-runtimes.py ~/$(REMOTE_DIR)/state/agent.json'
	@echo "Restart the agent: make deploy-quick"

verify: ## Full local verification (Python, JS syntax, Swift test + release build)
	@REQUIRE_SWIFT=$$([ "$$(uname)" = Darwin ] && echo 1 || echo 0) PYTHON=$(PYTHON) bash scripts/verify.sh
test: ## Python unit tests
	$(PYTHON) -m unittest discover -s tests
swift-test: ## Swift unit tests
	cd native && swift test
build: ## Release build of agent + desktop app
	cd native && swift build -c release
smoke: ## End-to-end native smoke (agent -> API -> runtime -> logs)
	bash scripts/mac-smoke.sh
web-test: ## Browser tests for the dashboard (needs Playwright)
	bash scripts/browser-test.sh
package: ## Build the ad hoc signed macOS .app
	bash scripts/package-macos.sh
run-server: ## Run a local server with generated config (./local)
	@[ -d local ] || $(PYTHON) scripts/configure.py --output local
	$(PYTHON) -m server.app --config local/config.json --db local/gryvia.db
run-agent: build ## Run the native agent against the local server
	@set -a; . local/credentials.env; GRYVIA_AGENT_TOKEN=$$GRYVIA_AGENT_TOKEN native/.build/release/gryvia-agent local/agent.json
doctor: ## Check server reachability and token (uses local/credentials.env)
	@set -a; . local/credentials.env; $(PYTHON) scripts/gryvia.py doctor

deploy: ## Deploy to the Mac mini (rsync, build, test, launchd)
	bash scripts/deploy-remote.sh $(MINI) --dir $(REMOTE_DIR)
deploy-quick: ## Deploy without running tests
	bash scripts/deploy-remote.sh $(MINI) --dir $(REMOTE_DIR) --quick
deploy-verify: ## Verify the existing mini deployment only
	bash scripts/deploy-remote.sh $(MINI) --dir $(REMOTE_DIR) --verify-only
deploy-dry-run: ## Show what deploy would do
	bash scripts/deploy-remote.sh $(MINI) --dir $(REMOTE_DIR) --dry-run
mini-status: ## launchd + health on the mini
	ssh $(MINI) 'launchctl list | grep gryvia; curl -fsS http://127.0.0.1:8443/healthz; echo'
mini-logs: ## Tail the mini server/agent logs
	ssh $(MINI) 'tail -n 40 -f ~/Library/Logs/Gryvia/*.log'
