# syntax=docker/dockerfile:1.7
#
# Multi-stage build for basic-kanban-board-mcp.
#
# Distroless final stage (NOT scratch): we need CA roots for outbound HTTPS
# (tokens, webhooks, future integrations) and there is no shell to install
# them manually. distroless/static-debian12:nonroot ships the CA bundle.
#
# ENTRYPOINT binds 0.0.0.0:8080 — a loopback bind inside a container is
# unreachable through `-p`, which is the bug we are avoiding.
#
# HEALTHCHECK calls `kanban healthcheck` because the image has no shell —
# `curl` does not exist.

ARG GO_VERSION=1.27.0

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src

# TARGETOS/TARGETARCH are populated by BuildKit for the target platform, so a
# single `docker buildx build --platform linux/amd64,linux/arm64` cross-compiles
# correctly. They default to linux/amd64 for a legacy (non-BuildKit) builder,
# which cannot cross-compile anyway.
ARG TARGETOS
ARG TARGETARCH

# Cache dependencies first.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

# CGO_ENABLED=0 is required because the runtime base is static. The
# pure-Go sqlite driver (modernc.org/sqlite) means we do not need cgo.
ARG VERSION=dev
ARG BUILD_DATE
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" \
        -o /out/kanban ./cmd/kanban

# A pre-owned data directory to copy into the final stage: distroless has no
# shell, so the volume mount point must arrive already owned by the runtime uid.
RUN mkdir -p /out/data

# ----------------------------------------------------------------------------
# Final stage
# ----------------------------------------------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/kanban /usr/local/bin/kanban

# distroless runs as uid 65532 by default; /data needs to be writable by it.
# We copy an already-owned directory from the build stage because distroless has
# no shell to run chown, and a bare VOLUME mount point would otherwise be
# root-owned — the container would then fail to open its SQLite file on a fresh
# named volume.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
VOLUME ["/data"]
WORKDIR /data

# The listener binds 0.0.0.0 (a loopback bind is unreachable through `-p`), which
# is a remote listener as far as the startup TLS check is concerned. Inside a
# container the operator is expected to terminate TLS at a reverse proxy (or is
# just evaluating locally), so acknowledge the plain-HTTP listener here. Without
# it, `docker run -p 8080:8080 …` refuses to start. Put real TLS in front for a
# public deployment — see docs/DEPLOY.md.
ENV KANBAN_INSECURE_HTTP=true

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/kanban", "serve", "--addr", "0.0.0.0:8080", "--data", "/data"]
HEALTHCHECK CMD ["/usr/local/bin/kanban", "healthcheck", "--addr", "127.0.0.1:8080", "--timeout", "2s"]