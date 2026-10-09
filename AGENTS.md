# AGENTS.md

> **This file is for AI assistants (Navi, future agents).** It is the canonical
> project context document. After any significant change, update this file first.
> Update README.md only when the change is user-facing (new feature, new command,
> changed behavior that a human visitor to the repo should know).

---

## Project Identity

| Field | Value |
|---|---|
| **Name** | Mini-Ticketmaster |
| **Type** | High-concurrency ticket booking / reservation system |
| **Go module** | `github.com/emmitt-k/ticket-deal` |
| **Repo** | https://github.com/emmitt-k/ticket-deal |
| **Owner** | Emmitt K. (`emmitt.karn@gmail.com`) |
| **Portfolio** | This IS the portfolio — all commits show `Emmitt K. <emmitt.karn@gmail.com>` |

---

## Tech Stack

| Layer | Technology |
|---|---|
| Language | Go 1.27.1 |
| HTTP API | `net/http` + chi router |
| Auth | JWT (RS256, keys on disk) |
| Cache / Rate-limit | Redis 7 (Lua scripts) |
| Primary DB | Postgres 16 |
| Queue | ElasticMQ (SQS-compatible, local) |
| Load testing | k6 (Go + JavaScript) |
| Service scripts | Bash + curl |
| Container runtime | Docker Compose |
| CI | GitHub Actions |
| Dependabot | Go modules + GitHub Actions (weekly, Mondays 09:00 BKK) |

---

## Architecture (high-level)

```
Client → API (:8080) → Redis (rate-limit + waiting-room queue)
                   → Postgres (reservations, inventory)
                   → ElasticMQ (async reservation publishing)
                            → Worker (:8081) consumes SQS → writes to Postgres
                            → Expiration-Watcher (keyspace notifications) → auto-expires holds
```

**Key correctness invariants:**
- 100 tickets per event (hardcoded seed in `scripts/start-bg.sh`)
- 200ms HTTP timeout on /reserve
- Redis atomic Lua scripts for seat locking (no oversell)
- Waiting room: VUs queued in Redis, released in FIFO order
- Postgres: idempotent writes via reservation UUID

---

## All Makefile Targets

Run `make` or `make help` to see all targets with descriptions.

### Development

```bash
make all-services     # Start API + worker + expiration-watcher + dashboard-server in background
make api             # Run API in foreground (ctrl-c to stop)
make api-bg          # Start API in background (logs/logs/api.log)
make worker-bg       # Start queue worker in background
make watcher-bg      # Start expiration-watcher in background
make dashboard-bg    # Start live dashboard (http://localhost:8082/)
make down            # Stop all docker compose services
make status          # Show running processes, container state, DB/Redis counts
```

### Load Testing

```bash
make loadtest-state        # Reset Redis + Postgres to clean state (no events, no holds)
make loadtest-jwts        # Mint 1000 JWTs for load test users
make loadtest-burst        # 1000 VUs × 1s burst (correctness test: 100 winners, 0 oversell)
make loadtest-ramp         # 7-stage ramp 0→50→200→1000 VUs over 90s (stress test)
make stop-loadtest         # Kill any running k6 process (burst or ramp)
```

**Load test logs** (never overwritten, timestamped):
- `logs/burst/TS-burst.log`
- `logs/ramp/TS-ramp.log`

### Code Quality

```bash
make build        # Build all 6 binaries into bin/
make vet          # go vet ./...
make test         # go test ./...
make clean        # Remove bin/
make purge        # clean + docker compose down (keeps volumes)
```

---

## Key Files & Directories

```
/
├── cmd/
│   ├── api/                    # HTTP API server (:8080)
│   ├── worker/                 # SQS consumer → Postgres writer
│   ├── expiration-watcher/     # Redis keyspace notifications → expire holds
│   ├── dashboard-server/       # Live k6 dashboard web server (:8082)
│   ├── mintjwt/                # JWT minting tool (used by load tests)
│   └── seed-inventory/         # Seeds Postgres with event + 100 seats
├── internal/
│   ├── api/                    # HTTP handlers, reserve_handler
│   ├── auth/                   # JWT middleware, JWT minting
│   ├── config/                 # Env var config
│   ├── db/                     # Postgres pool, reservation writes
│   ├── expire/                 # Expiration watcher, sweep, compensate
│   ├── iplimit/                # Redis IP rate-limiter (token bucket)
│   ├── queue/                  # SQS publisher + consumer
│   ├── redis/                  # Redis client, Lua scripts, seat locking
│   ├── waitingroom/            # Waiting room queue + SSE stream
│   └── apiutil/                # HTTP response helpers
├── loadtest/
│   ├── burst.js               # 1000-VU burst test script
│   ├── ramp.js                # 7-stage ramp test script
│   ├── dashboard.html         # Live dashboard (open in browser)
│   ├── reset-state.sh         # Reset Redis + Postgres to clean state
│   ├── mint-jwts.sh           # Mint N JWTs to /tmp/k6_jwts.txt
│   ├── run-burst.sh           # Run burst with timestamped log → logs/burst/
│   └── run-ramp.sh            # Run ramp with timestamped log → logs/ramp/
├── scripts/
│   ├── start-bg.sh            # Generic background process starter (pid + log)
│   ├── stop-bg.sh             # Generic background process stopper (pid-based)
│   ├── start-all.sh           # Start all services (used by make all-services)
│   └── stop-all.sh            # Stop all services
├── docs/
│   ├── architecture.md        # Full architecture deep-dive
│   ├── comprehensive-go.md     # Go language tour
│   └── comprehensive-redis.md # Redis deep-dive
├── logs/                      # Runtime logs + k6 output
│   ├── api.log
│   ├── worker.log
│   ├── burst/                 # Timestamped burst logs
│   └── ramp/                  # Timestamped ramp logs
├── migrations/
│   ├── 001_init.sql           # Reservations + seats schema
│   ├── 002_seed.sql           # Seed data
│   └── 003_add_seats.sql      # Adds seats column
├── docker-compose.yml         # Postgres + Redis + ElasticMQ
├── go.mod / go.sum
├── Makefile                   # All make targets
└── README.md                  # Human-facing project overview
```

---

## GitHub Setup (for this machine)

**SSH key:** `~/.ssh/id_ed25519_github` (comment: `emmitt.karn@gmail.com`)
- Added to GitHub: https://github.com/settings/keys
- Routed via `~/.ssh/config` with `IdentitiesOnly yes` (cannot accidentally use for other hosts)

**SSH config (`~/.ssh/config`):**
```
Host github.com
    IdentityFile ~/.ssh/id_ed25519_github
    IdentitiesOnly yes
```

**Git identity for this repo (local, not global):**
```
user.name  = Emmitt K.
user.email = emmitt.karn@gmail.com
```
Global identity (for other repos): `Emmitt Kaewkarn <emmitt.kaewkarn@krungsri.com>`

---

## CI/CD

### GitHub Actions (`.github/workflows/ci.yml`)
- Runs on: push to `main` + all PRs
- Job: `go vet` → `go test -race -coverprofile` → `go build`
- Artifact: coverage report (14-day retention)

### Dependabot (`.github/dependabot.yml`)
- Go modules: weekly Monday 09:00 BKK
- GitHub Actions: weekly
- Labels: `dependencies`, `go`, `ci`

### Branch Strategy
- `main`: stable, protected
- Feature work: **always create a new feature branch** (`git checkout -b feature/<name>`) off `main` before making changes; **always push commits to the remote feature branch** (`git push origin feature/<name>`) when done — never commit directly to `main`

---

## Important Conventions

1. **New feature = new feature branch** — before doing anything non-trivial (new file, new behavior, refactor), ask Master: *"Should I make a new feature branch for this?"* Branch name: `feature/<short-name>`
2. **Always push to remote when done** — after every commit, push to the remote feature branch immediately. Never leave commits unpushed.
3. **Load test before committing any reservation/Redis logic** — use `make loadtest-burst` to smoke-test correctness
4. **Reset state between load test runs** — `make loadtest-state && make loadtest-jwts`
5. **No external services in unit tests** — all tests use in-memory fakes; `go test ./...` passes without any docker services running
6. **JWT TTL** — default 600s (10 min); set via `JWT_TTL_SECONDS` env var in `loadtest/mint-jwts.sh`
7. **Ports**: API :8080, Dashboard :8082, Redis :6379, Postgres :5432, ElasticMQ :4566
8. **Corporate proxy** — `http.proxy` is set globally in `~/.gitconfig`; SSH bypasses it, HTTPS does not. SSH is used for github.com pushes.
9. **Commit style** — Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `ci:`)

---

## Common Task Sequences

### Full local dev loop
```bash
make all-services   # Start everything
make loadtest-state && make loadtest-jwts   # Reset + seed JWTs
make loadtest-burst  # Smoke test
make loadtest-ramp   # Full stress test
# ...iterate code...
make stop-loadtest && make all-services  # Restart services
```

### After pulling new code
```bash
go mod download
make build
make test
```

### Quick correctness check (1 second)
```bash
make loadtest-state && make loadtest-jwts && make loadtest-burst
```
