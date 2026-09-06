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
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" \
        -o /out/kanban ./cmd/kanban

# ----------------------------------------------------------------------------
# Final stage
# ----------------------------------------------------------------------------

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/kanban /usr/local/bin/kanban

# distroless runs as uid 65532 by default; /data needs to be writable.
USER 65532:65532
VOLUME ["/data"]
WORKDIR /data

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/kanban", "serve", "--addr", "0.0.0.0:8080", "--data", "/data"]
HEALTHCHECK CMD ["/usr/local/bin/kanban", "healthcheck", "--addr", "127.0.0.1:8080", "--timeout", "2s"]