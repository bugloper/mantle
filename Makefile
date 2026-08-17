# Mantle — build, test, and local development.

BINDIR      := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X github.com/mantle-sh/mantle/internal/server.Version=$(VERSION) \
               -X main.Version=$(VERSION)

# Tests that touch PostgreSQL skip themselves when this is unreachable, so
# `make test` works on a machine with no database while `make test-all` is the
# complete run.
TEST_DB_URL ?= postgres://$(USER)@localhost/postgres?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# Binaries are built to a temporary name and moved into place.
#
# This is not fussiness. `go build -o` truncates the output file, and on macOS
# overwriting a running executable invalidates its code signature — the kernel
# then kills the running process with SIGKILL ("Killed: 9"). Rebuilding while a
# daemon was running therefore killed it, and the failure looked like a network
# problem several minutes later. A rename replaces the directory entry and
# leaves the running process's inode alone, so a rebuild is always safe.
define build_binary
	@mkdir -p $(BINDIR)
	@go build -ldflags "$(LDFLAGS)" -o $(BINDIR)/.$(1).tmp ./cmd/$(1)
	@mv -f $(BINDIR)/.$(1).tmp $(BINDIR)/$(1)
endef

.PHONY: build
build: mantled mantle mantle-ui ## Build all three binaries into ./bin
	@echo "built $(BINDIR)/{mantled,mantle,mantle-ui} ($(VERSION))"

.PHONY: mantled mantle mantle-ui
mantled: ## Build just the daemon
	$(call build_binary,mantled)
mantle: ## Build just the CLI
	$(call build_binary,mantle)
mantle-ui: ## Build just the web interface
	$(call build_binary,mantle-ui)

.PHONY: test
test: ## Run tests that need no database
	go test ./internal/oci/... ./internal/auth/... ./internal/storage/... \
	        ./internal/distribution/... ./internal/config/... \
	        ./internal/observability/... ./cmd/mantle-ui/... ./test/architecture/...

.PHONY: test-all
test-all: ## Run every test, against a real PostgreSQL
	MANTLE_TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... -count=1

.PHONY: test-race
test-race: ## Run every test with the race detector
	MANTLE_TEST_DATABASE_URL="$(TEST_DB_URL)" go test -race ./... -count=1

.PHONY: lint
lint: ## gofmt, go vet, and the architecture dependency rule
	@unformatted=$$(gofmt -l . ); \
	 if [ -n "$$unformatted" ]; then \
	   echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	 fi
	go vet ./...
	go test ./test/architecture/ -count=1

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

# --- local development ---

DEV_ROOT := /tmp/mantle-dev
DEV_DB   := mantle_dev

.PHONY: dev-setup
dev-setup: mantled mantle ## Create a local database and bootstrap an admin
	@createdb $(DEV_DB) 2>/dev/null || true
	@mkdir -p $(DEV_ROOT)/storage
	@sed -e 's|__ROOT__|$(DEV_ROOT)|g' -e 's|__DB__|$(DEV_DB)|g' -e 's|__USER__|$(USER)|g' \
	     docs/mantle.dev.yaml > $(DEV_ROOT)/mantle.yaml
	@MANTLE_ADMIN_PASSWORD=devpassword $(BINDIR)/mantled --config $(DEV_ROOT)/mantle.yaml --bootstrap
	@echo
	@echo "  mantle login http://127.0.0.1:5100 --username admin --token devpassword"

.PHONY: dev
dev: mantled ## Run the registry in the foreground on :5100
	$(BINDIR)/mantled --config $(DEV_ROOT)/mantle.yaml

# Deliberately depends on mantle-ui alone. Depending on `build` here rebuilt
# mantled underneath the running registry and killed it.
.PHONY: dev-ui
dev-ui: mantle-ui ## Run the web interface in the foreground on :5180
	@echo "  open http://127.0.0.1:5180  (sign in as admin / devpassword)"
	$(BINDIR)/mantle-ui --registry http://127.0.0.1:5100 --listen 127.0.0.1:5180

# --- one command to start everything -------------------------------------

REGISTRY_URL := http://127.0.0.1:5100
UI_URL       := http://127.0.0.1:5180

.PHONY: up
up: build dev-setup ## Start the registry and the web interface in the background
	@$(MAKE) --no-print-directory _stop_quiet
	@mkdir -p $(DEV_ROOT)
	@# nohup with stdin from /dev/null: a background job that keeps the
	@# terminal's stdin can be stopped with SIGTTIN, which leaves a process
	@# holding the port while answering nothing — a hang that looks like a bug
	@# in the registry rather than a job-control accident.
	@nohup $(BINDIR)/mantled --config $(DEV_ROOT)/mantle.yaml \
	    </dev/null > $(DEV_ROOT)/mantled.log 2>&1 & echo $$! > $(DEV_ROOT)/mantled.pid
	@$(MAKE) --no-print-directory _wait_for URL=$(REGISTRY_URL)/readyz WHAT=registry \
	    LOG=$(DEV_ROOT)/mantled.log
	@nohup $(BINDIR)/mantle-ui --registry $(REGISTRY_URL) --listen 127.0.0.1:5180 \
	    </dev/null > $(DEV_ROOT)/mantle-ui.log 2>&1 & echo $$! > $(DEV_ROOT)/mantle-ui.pid
	@$(MAKE) --no-print-directory _wait_for URL=$(UI_URL)/ WHAT="web interface" \
	    LOG=$(DEV_ROOT)/mantle-ui.log
	@echo
	@echo "  registry   $(REGISTRY_URL)"
	@echo "  interface  $(UI_URL)   (admin / devpassword)"
	@echo "  metrics    http://127.0.0.1:9190/metrics"
	@echo
	@echo "  make logs    follow both logs"
	@echo "  make status  check health"
	@echo "  make down    stop both"

.PHONY: down
down: ## Stop the registry and the web interface
	@$(MAKE) --no-print-directory _stop_quiet
	@echo "stopped"

# _stop_quiet stops by recorded pid, then sweeps anything left holding the
# ports. The sweep matters: a process left in the stopped state still owns its
# listening socket, and the next `make up` would bind-fail or, worse, appear to
# work while the old one answered nothing.
.PHONY: _stop_quiet
_stop_quiet:
	@for name in mantled mantle-ui; do \
	  if [ -f $(DEV_ROOT)/$$name.pid ]; then \
	    kill $$(cat $(DEV_ROOT)/$$name.pid) 2>/dev/null || true; \
	    rm -f $(DEV_ROOT)/$$name.pid; \
	  fi; \
	done
	@pkill -f '$(BINDIR)/mantled --config $(DEV_ROOT)' 2>/dev/null || true
	@pkill -f '$(BINDIR)/mantle-ui --registry $(REGISTRY_URL)' 2>/dev/null || true
	@sleep 1
	@pkill -9 -f '$(BINDIR)/mantled --config $(DEV_ROOT)' 2>/dev/null || true
	@pkill -9 -f '$(BINDIR)/mantle-ui --registry $(REGISTRY_URL)' 2>/dev/null || true

# _wait_for polls until a URL answers, and prints the log on failure rather
# than leaving the operator to find it.
.PHONY: _wait_for
_wait_for:
	@printf "  waiting for %s" "$(WHAT)"
	@for i in $$(seq 1 40); do \
	  if curl -sf -m 2 -o /dev/null "$(URL)"; then echo " ok"; exit 0; fi; \
	  printf "."; sleep 0.5; \
	done; \
	echo " FAILED"; \
	echo "--- $(LOG) ---"; tail -20 $(LOG); exit 1

.PHONY: dev-seed
dev-seed: mantle ## Push sample images and deployments into the local instance
	@contrib/dev-seed.sh $(REGISTRY_URL) admin devpassword

.PHONY: status
status: ## Show whether the registry and interface are healthy
	@printf "  registry   "; curl -sf -m 3 $(REGISTRY_URL)/readyz >/dev/null \
	  && echo "up    $(REGISTRY_URL)" || echo "DOWN  $(REGISTRY_URL)"
	@printf "  interface  "; curl -sf -m 3 $(UI_URL)/ >/dev/null \
	  && echo "up    $(UI_URL)" || echo "DOWN  $(UI_URL)"

.PHONY: logs
logs: ## Follow the registry and interface logs
	@tail -f $(DEV_ROOT)/mantled.log $(DEV_ROOT)/mantle-ui.log

.PHONY: dev-clean
dev-clean: down ## Stop everything and remove the local development instance
	@dropdb --if-exists $(DEV_DB)
	@rm -rf $(DEV_ROOT)
	@echo "removed $(DEV_DB) and $(DEV_ROOT)"

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BINDIR)
