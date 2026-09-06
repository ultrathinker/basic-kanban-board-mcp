# Makefile for basic-kanban-board-mcp.
#
# Targets:
#   build   compile the binary into ./bin/kanban
#   test    run `go test ./... -race` (Linux/macOS only — race needs cgo)
#   lint    run go vet + gofmt check
#   css     regenerate web/static/app.css using the Tailwind standalone CLI
#   docker  build the multi-arch image
#   run     build and run `kanban serve`
#   demo    build and run `kanban demo --help` to confirm wiring seams
#
# The Tailwind CSS regeneration is OPTIONAL: a developer who never changes
# the templates can ignore `make css` entirely. The committed app.css must
# stay in sync, so CI checks it (see .github/workflows/ci.yml).

GO          ?= go
BIN_DIR      ?= bin
BIN         ?= $(BIN_DIR)/kanban
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

TOOLS_DIR   ?= .tools
TAILWIND    ?= $(TOOLS_DIR)/tailwindcss
TAILWIND_VERSION ?= v4.3.3

CSS_SRC      := web/templates
CSS_OUT      := web/static/app.css

.PHONY: build test lint css docker run demo clean help

help:
	@echo "Targets:"
	@echo "  build   - compile ./$(BIN)"
	@echo "  test    - run go test ./... -race"
	@echo "  lint    - go vet + gofmt check"
	@echo "  css     - regenerate $(CSS_OUT) via the Tailwind standalone binary"
	@echo "  docker  - build the multi-arch image (buildx)"
	@echo "  run     - build and run 'kanban serve'"
	@echo "  demo    - print the demo seed wiring status"
	@echo "  clean   - remove ./$(BIN_DIR) and ./$(TOOLS_DIR)"

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -trimpath \
	    -ldflags "-s -w -X main.version=$(VERSION) -X main.buildDate=$(BUILD_DATE)" \
	    -o $(BIN) ./cmd/kanban

# `test` uses the race detector. CGO_ENABLED=1 is required for race on the
# platforms Go supports it; on Windows the developer must have GCC
# available (e.g. via winget install BrechtSanders.WinLibs.POSIX.UCRT).
test:
	CGO_ENABLED=1 $(GO) test ./... -race -count=1

lint:
	$(GO) vet ./...
	@gofmt -l . | tee /tmp/gofmt-issues.txt && \
	    test ! -s /tmp/gofmt-issues.txt || (echo "gofmt diffs above" && exit 1)

# Regenerate web/static/app.css from the templates via the Tailwind v4.3.3
# standalone CLI. The binary is downloaded into .tools/ on first use. CI
# runs this and checks the committed file matches.
css: $(TAILWIND)
	$(TAILWIND) -i $(CSS_SRC)/**/*.html -o $(CSS_OUT) --minify

$(TAILWIND):
	mkdir -p $(TOOLS_DIR)
	@if [ "$$(uname -s)" = "Linux" ]; then \
	    arch=$$(uname -m); case $$arch in x86_64) arch=amd64;; aarch64) arch=arm64;; esac; \
	    curl -fsSL -o $(TAILWIND) \
	        https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-linux-$$arch; \
	elif [ "$$(uname -s)" = "Darwin" ]; then \
	    arch=$$(uname -m); case $$arch in x86_64) arch=x64;; aarch64) arch=arm64;; esac; \
	    curl -fsSL -o $(TAILWIND) \
	        https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-macos-$$arch; \
	else \
	    echo "OS unsupported by this Makefile; download Tailwindcss $(TAILWIND_VERSION) manually."; \
	    exit 1; \
	fi
	chmod +x $(TAILWIND)

docker:
	docker buildx build --platform linux/amd64,linux/arm64 \
	    --build-arg VERSION=$(VERSION) \
	    --build-arg BUILD_DATE=$(BUILD_DATE) \
	    -t ghcr.io/ultrathinker/basic-kanban-board-mcp:$(VERSION) \
	    --load .

run: build
	./$(BIN) serve

demo: build
	./$(BIN) demo --help || true

clean:
	rm -rf $(BIN_DIR) $(TOOLS_DIR)