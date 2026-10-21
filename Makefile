# ──────────────────────────────────────────────────────────────────────
# Mini-Ticketmaster — Makefile
#
# Phases 0-8 are landed; this Makefile is Phase 9 (polish). Every target
# is documented via `##` after the colon; `make help` lists them.
#
# Common overrides (any target):
#   make loadtest-burst VUS=500
#   make seed-inventory EVENT_ID=2
#   make mintjwt USER=alice EVENT=42
# ──────────────────────────────────────────────────────────────────────

# ─── Variables ────────────────────────────────────────────────────────
BIN_DIR         := bin
LOG_DIR         := logs
PID_DIR         := $(LOG_DIR)
JWT_FILE        ?= /tmp/k6_jwts.txt
EVENT_ID        ?= 1
VUS             ?= 1000
K6_REST_ADDR    ?= localhost:6565
DASHBOARD_ADDR  ?= :8082

GO              := go
DOCKER          := docker
PSQL            := $(DOCKER) exec -i ticket-postgres psql -U tickets -d tickets
REDIS           := $(DOCKER) exec -i ticket-redis redis-cli

# All 6 binaries. Order doesn't matter for `build`; keep alphabetical.
CMDS := api dashboard-server expiration-watcher mintjwt seed-inventory worker

# ─── Phony + default ──────────────────────────────────────────────────
.PHONY: help
.DEFAULT_GOAL := help

help: ## Show this help message (default target)
	@printf '\033[1m%-22s %s\033[0m\n' TARGET DESCRIPTION
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@printf '\nCommon overrides: make <target> VUS=500 EVENT_ID=2 USER=alice\n'

# ─── Setup (run once on a fresh clone) ───────────────────────────────
.PHONY: env up down restart migrate seed doctor
env: ## Create .env from .env.example (if missing)
	@test -f .env || { cp .env.example .env && echo "→ created .env (edit JWT_SECRET if you want a custom value)"; }
	@test -f .env && echo "✓ .env present"

up: ## Start Redis + Postgres + ElasticMQ via docker compose
	$(DOCKER) compose up -d
	@echo "Waiting for Postgres to be healthy..."
	@until $(DOCKER) exec ticket-postgres pg_isready -U tickets >/dev/null 2>&1; do sleep 1; done
	@echo "Waiting for Redis..."
	@until $(DOCKER) exec ticket-redis redis-cli PING >/dev/null 2>&1; do sleep 1; done
	@echo "✓ All services up"

down: ## Stop docker compose services (keeps volumes)
	$(DOCKER) compose down

restart: down up ## Restart docker compose

migrate: ## Apply all SQL migrations in order
	@for f in migrations/0*.sql; do \
		echo "→ $$f"; \
		$(PSQL) < $$f || exit 1; \
	done
	@echo "✓ migrations applied"

seed: ## Seed Redis inventory from Postgres (cmd/seed-inventory)
	$(GO) run ./cmd/seed-inventory

doctor: ## Check prereqs (go, docker, k6, curl)
	@printf '\033[1mChecking prereqs...\033[0m\n'
	@command -v $(GO)      >/dev/null 2>&1 && echo "  ✓ go        $$(go version | awk '{print $$3}')"     || echo "  ✗ go        (brew install go)"
	@command -v $(DOCKER)  >/dev/null 2>&1 && echo "  ✓ docker    $$(docker --version | awk '{print $$3}')" || echo "  ✗ docker    (install Docker Desktop)"
	@command -v k6         >/dev/null 2>&1 && echo "  ✓ k6        $$(k6 version 2>/dev/null | awk '{print $$2}')" || echo "  ✗ k6        (brew install k6)"
	@command -v curl       >/dev/null 2>&1 && echo "  ✓ curl      $$(curl --version | head -1 | awk '{print $$2}')" || echo "  ✗ curl"
	@command -v redis-cli  >/dev/null 2>&1 && echo "  ✓ redis-cli"     || echo "  ⚠ redis-cli  (optional; for debugging)"

# ─── Build ────────────────────────────────────────────────────────────
.PHONY: build vet test test-race
build: ## Build all 6 binaries into bin/
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "→ building $$cmd"; \
		$(GO) build -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit 1; \
	done
	@echo "✓ built $$(ls -1 $(BIN_DIR) | wc -l | tr -d ' ') binaries into $(BIN_DIR)/"

vet: ## go vet ./...
	$(GO) vet ./...

test: ## go test ./...
	$(GO) test ./...

test-race: ## go test -race ./...
	$(GO) test -race ./...

# ─── Run (foreground — for debugging) ────────────────────────────────
.PHONY: api worker watcher
api: $(BIN_DIR)/api ## Run API in foreground (Ctrl-C to stop)
	./$(BIN_DIR)/api

worker: $(BIN_DIR)/worker ## Run worker in foreground
	./$(BIN_DIR)/worker

watcher: $(BIN_DIR)/expiration-watcher ## Run expiration watcher in foreground
	./$(BIN_DIR)/expiration-watcher

# ─── Run (background — for dev & load tests) ─────────────────────────
.PHONY: api-bg worker-bg watcher-bg dashboard-bg all-services stop
api-bg: $(BIN_DIR)/api ## Run API in background, log → logs/api.log
	@./scripts/start-bg.sh api $(BIN_DIR)/api

worker-bg: $(BIN_DIR)/worker ## Run worker in background
	@./scripts/start-bg.sh worker $(BIN_DIR)/worker

watcher-bg: $(BIN_DIR)/expiration-watcher ## Run watcher in background
	@./scripts/start-bg.sh expiration-watcher $(BIN_DIR)/expiration-watcher

dashboard-bg: $(BIN_DIR)/dashboard-server ## Run dashboard server in background
	@./scripts/start-bg.sh dashboard-server $(BIN_DIR)/dashboard-server
	@echo "→ open http://localhost:8082/ in your browser"

all-services: api-bg worker-bg watcher-bg ## Start all 3 services (api + worker + watcher)

stop: ## Stop all background services (api/worker/watcher/dashboard)
	@for svc in api worker expiration-watcher dashboard-server; do \
		./scripts/stop-bg.sh $$svc; \
	done

# ─── Tools ────────────────────────────────────────────────────────────
.PHONY: mintjwt dashboard
mintjwt: $(BIN_DIR)/mintjwt ## Mint a JWT (USER=alice EVENT=1 TTL=120)
	@USER_ID=$${USER:-alice} EVENT_ID=$${EVENT:-1} TTL_SECONDS=$${TTL:-120} ./$(BIN_DIR)/mintjwt

dashboard: $(BIN_DIR)/dashboard-server ## Run dashboard server in foreground
	./$(BIN_DIR)/dashboard-server

# ─── Load test ───────────────────────────────────────────────────────
.PHONY: loadtest-state loadtest-jwts loadtest-burst loadtest-ramp stop-loadtest
loadtest-state: ## Reset DB+Redis to known clean state (inv=100, 0 holds)
	./loadtest/reset-state.sh

loadtest-jwts: ## Pre-mint N unique JWTs (VUS=1000)
	./loadtest/mint-jwts.sh $(VUS)

# k6 is run in the BACKGROUND (with --linger) so the Make target returns
# immediately and the dashboard at http://localhost:8082/ stays populated
# with the final state for inspection. Use `make stop-loadtest` to free
# the port before re-running, or `tail -f logs/burst/TS-burst.log` to follow.

loadtest-burst: ## Run 1000-VU burst in background (~1s, dashboard stays populated)
	@if ! curl -sf -m 2 http://localhost:8080/healthz >/dev/null 2>&1; then \
		echo ""; \
		echo "✗ API not running on :8080"; \
		echo "  → start it with: make all-services"; \
		echo "  → or for foreground debugging: make api"; \
		echo ""; \
		exit 1; \
	fi
	@./loadtest/run-burst.sh $(VUS)
	@echo ""
	@echo "→ burst running; dashboard at http://localhost:8082/"
	@echo "→ to re-run: make stop-loadtest && make loadtest-burst"

loadtest-ramp: ## Run 7-stage ramp in background (~90s, dashboard tracks VU curve)
	@if ! curl -sf -m 2 http://localhost:8080/healthz >/dev/null 2>&1; then \
		echo ""; \
		echo "✗ API not running on :8080"; \
		echo "  → start it with: make all-services"; \
		echo "  → or for foreground debugging: make api"; \
		echo ""; \
		exit 1; \
	fi
	@./loadtest/run-ramp.sh $(VUS)
	@echo ""
	@echo "→ ramp running (~90s); dashboard at http://localhost:8082/"
	@echo "→ to free the port: make stop-loadtest"

stop-loadtest: ## Stop any background k6 (burst or ramp)
	@./scripts/stop-bg.sh k6-burst || true
	@./scripts/stop-bg.sh k6-ramp || true

# ─── Cleanup ─────────────────────────────────────────────────────────
.PHONY: clean purge
clean: ## Remove built binaries
	rm -rf $(BIN_DIR)
	@echo "✓ bin/ removed"

purge: clean down ## Clean + stop docker compose (keeps volumes)
	@echo "✓ purged (volumes preserved; for full reset use 'docker compose down -v')"

# ─── Status & logs ───────────────────────────────────────────────────
.PHONY: status logs clean-logs clean-logs-truncate
status: ## Show running processes, container state, and DB/Redis counts
	@printf '\033[1m=== Containers ===\033[0m\n'
	@$(DOCKER) compose ps
	@printf '\n\033[1m=== Background services ===\033[0m\n'
	@for svc in api worker expiration-watcher dashboard-server; do \
		pidfile=$(PID_DIR)/$$svc.pid; \
		if [ -f $$pidfile ]; then \
			pid=$$(cat $$pidfile); \
			if kill -0 $$pid 2>/dev/null; then \
				logfile=$(LOG_DIR)/$$svc.log; \
				size=$$([ -f $$logfile ] && wc -c < $$logfile | tr -d ' ' || echo 0); \
				printf "  \033[32m●\033[0m %-22s PID %-6s log: %s (%s bytes)\n" $$svc $$pid $$logfile $$size; \
			else \
				printf "  \033[31m○\033[0m %-22s stale PID file (%s not running)\n" $$svc $$pid; \
			fi; \
		else \
			printf "  · %-22s (not started; try 'make %s-bg')\n" $$svc $$svc; \
		fi; \
	done
	@printf '\n\033[1m=== System state (event $(EVENT_ID)) ===\033[0m\n'
	@printf "  inventory:event:$(EVENT_ID)  = "; $(REDIS) GET inventory:event:$(EVENT_ID) 2>/dev/null || echo "(not set)"
	@printf "  reservations                = "; echo "SELECT COUNT(*) FROM reservations WHERE event_id=$(EVENT_ID);" | $(PSQL) -tA 2>/dev/null
	@printf "  hold keys                   = "; $(REDIS) EVAL "return #redis.call('KEYS', 'hold:event:$(EVENT_ID):user:*')" 0 2>/dev/null

logs: ## Tail all background service logs (Ctrl-C to exit)
	@ls -1 $(LOG_DIR)/*.log 2>/dev/null | xargs -I{} sh -c 'echo ""; echo "==> {}"; tail -f {}' || echo "(no log files in $(LOG_DIR)/)"

clean-logs: ## Delete all log files (api/worker logs + k6 burst/ramp results)
	@./scripts/clear-logs.sh

clean-logs-truncate: ## Truncate (don't delete) all log files; safe in-flight
	@./scripts/clear-logs.sh --truncate
