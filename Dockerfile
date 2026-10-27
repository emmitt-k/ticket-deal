# syntax=docker/dockerfile:1.7
#
# Shared multi-stage Dockerfile for all 5 long-running Go services:
#   api, worker, stripe-worker, expiration-watcher, dashboard-server
#
# Build with:
#   docker build --build-arg SERVICE=api -t ticket-deal-api .
#   docker build --build-arg SERVICE=worker -t ticket-deal-worker .
#
# GO_VERSION must stay in step with the `go` directive in go.mod — and
# with a PATCHED release. go1.27.1 carried 12 standard-library CVEs
# (net/http, net/http2, html/template, crypto/tls, crypto/x509, …) that
# go1.27.3 fixes. The govulncheck step in CI scans the host toolchain;
# this ARG is what the IMAGES run on, so it needs the same treatment.
#
# Why one shared file:
#   The 4 binaries differ only in which cmd/<name>/ subdir they live in.
#   A single parameterized file is ~40 LOC instead of ~200 LOC of 4
#   near-identical files. The SERVICE arg is the only knob.
#
# Why distroless/static-debian12:nonroot:
#   - ~2 MB final image, no shell, no package manager (smaller attack surface)
#   - Runs as UID 65532 (nonroot) for least privilege
#   - Has ca-certificates baked in (required by OTLP/HTTPS and the AWS SDK)
#   - Scratch would need us to COPY ca-certificates ourselves
#
# Why GOOS=linux only (no GOARCH override):
#   - Docker buildkit injects TARGETARCH automatically when you build with
#     --platform linux/amd64 on Apple Silicon (or any cross-arch build).
#   - Local `docker compose build` on any host just uses the host arch.
#   - CGO_ENABLED=0 keeps the binary fully static so it runs on any libc.

ARG GO_VERSION=1.27.3

# ─── Build stage ─────────────────────────────────
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Copy module manifests first so `go mod download` is cached separately
# from source. Any code-only edit skips the dependency layer entirely.
COPY go.mod go.sum ./
RUN go mod download -x

# Now copy the rest. .dockerignore should exclude .git, bin/, logs/, etc.
COPY . .

ARG SERVICE

# Build static, stripped, reproducible:
#   -trimpath   strip local filesystem paths from the binary
#   -ldflags    drop symbol table (-s -w) and the build-id hash
#   -buildvcs=false is implicit — .dockerignore excludes .git so Go can't
#               embed VCS info. Explicitly setting it is harmless.
RUN CGO_ENABLED=0 GOOS=linux \
    go build \
        -trimpath \
        -ldflags="-s -w -buildid=" \
        -o /out/${SERVICE} \
        ./cmd/${SERVICE}

# ─── Runtime stage ──────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

ARG SERVICE

# Copy just the one binary we built. We always rename it to /app/server so
# ENTRYPOINT is a constant path (you can't expand ARG into an exec-form
# ENTRYPOINT — /app/${SERVICE} ends up literal in the image manifest).
COPY --from=build /out/${SERVICE} /app/server

WORKDIR /app

# distroless ships UID 65532 as `nonroot`. Run as that user.
USER nonroot:nonroot

# The compose file injects all env vars (REDIS_ADDR, DATABASE_URL, etc.) and
# decides whether to expose ports. Any CLI flags come from `command:` in
# docker-compose.yml. Compose sets `command:` per service to match the binary's
# expected flags (e.g. dashboard-server reads --addr, --k6, --html).
ENTRYPOINT ["/app/server"]
