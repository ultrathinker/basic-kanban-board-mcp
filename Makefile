# Makefile for basic-kanban-board-mcp.
#
# Targets:
#   build          compile the binary into ./bin/kanban
#   test           run `go test ./... -race` (Linux/macOS only — race needs cgo)
#   lint           run go vet + gofmt check
#   css            regenerate web/static/app.css using the Tailwind standalone CLI
#   docker         build the multi-arch image
#   run            build and run `kanban serve`
#   demo           build and run `kanban demo --help` to confirm wiring seams
#   sync-embed     copy web/static -> internal/web/static and web/templates -> internal/web/templates
#   check-embed    fail when the two trees differ (CI runs this on every PR)
#   clean          remove ./bin and ./.tools
#
# The Tailwind CSS regeneration is OPTIONAL: a developer who never changes
# the templates can ignore `make css` entirely. The committed app.css must
# stay in sync, so CI checks it (see .github/workflows/ci.yml).
#
# The embed-mirror step (sync-embed / check-embed) exists because Go's
# //go:embed cannot traverse "..", so internal/web/{static,templates} are
# physical copies of the canonical trees under web/. Without these targets a
# developer who edits app.css in web/ and forgets to copy it into
# internal/web/static/ ships a stale embedded copy — see E-review-2.

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

# Embed-mirror pairs (canonical -> embedded). `make sync-embed` overwrites
# the destination; `make check-embed` asserts they are byte-identical.
EMBED_STATIC_SRC  := web/static
EMBED_STATIC_DST  := internal/web/static
EMBED_TPL_SRC     := web/templates
EMBED_TPL_DST     := internal/web/templates

# Files that legitimately live ONLY in the embedded tree (the Go embed
# directive itself, package doc, build glue). The check ignores them.
EMBED_EXTRA := internal/web/templates/templates.go

.PHONY: build test lint css docker run demo sync-embed check-embed clean help

help:
	@echo "Targets:"
	@echo "  build        - compile ./$(BIN)"
	@echo "  test         - run go test ./... -race"
	@echo "  lint         - go vet + gofmt check"
	@echo "  css          - regenerate $(CSS_OUT) via the Tailwind standalone binary"
	@echo "  sync-embed   - mirror web/{static,templates} into internal/web/{static,templates}"
	@echo "  check-embed  - fail when the two trees differ (CI)"
	@echo "  docker       - build the multi-arch image (buildx)"
	@echo "  run          - build and run 'kanban serve'"
	@echo "  demo         - print the demo seed wiring status"
	@echo "  clean        - remove ./$(BIN_DIR) and ./$(TOOLS_DIR)"

build: sync-embed
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

# sync-embed mirrors web/{static,templates} into internal/web/{static,templates}.
# Files that exist only in the embedded tree (the embed-directive Go file
# itself) are preserved — the destination directory is NOT wiped before the
# copy, so package-internal sources are untouched.
sync-embed:
	@echo "syncing embed mirrors"
	@mkdir -p $(EMBED_STATIC_DST) $(EMBED_TPL_DST)
	@# rsync is not portable to Windows; cp -R is. We use cp -R for the
	@# directory copy and then prune anything in the destination that is not
	@# in the source (a deleted file in web/ must NOT linger in the mirror).
	@for pair in "$(EMBED_STATIC_SRC) $(EMBED_STATIC_DST)" "$(EMBED_TPL_SRC) $(EMBED_TPL_DST)"; do \
	    set -- $$pair; \
	    src=$$1; dst=$$2; \
	    cp -R "$$src/." "$$dst/"; \
	done
	@# Prune files that exist in the destination but no longer in the source.
	@# For each non-extra file in dst, check that a same-named file exists in src.
	@for dst in $(EMBED_STATIC_DST) $(EMBED_TPL_DST); do \
	    case $$dst in \
	        $(EMBED_TPL_DST)) src=$(EMBED_TPL_SRC); extras="templates.go";; \
	        *) src=$(EMBED_STATIC_SRC); extras="";; \
	    esac; \
	    find $$dst -type f | while read f; do \
	        rel=$${f#$$dst/}; \
	        skip=0; \
	        for x in $$extras; do \
	            if [ "$$rel" = "$$x" ]; then skip=1; break; fi; \
	        done; \
	        if [ $$skip -eq 1 ]; then continue; fi; \
	        if [ ! -f "$$src/$$rel" ]; then \
	            echo "  pruning stale $$f"; \
	            rm -f "$$f"; \
	        fi; \
	    done; \
	done
	@echo "embed mirrors in sync"

# check-embed asserts that internal/web/{static,templates} is byte-identical
# to web/{static,templates}. CI runs this on every PR — a stale mirror
# fails the build rather than silently serving last week's CSS.
check-embed:
	@echo "checking embed mirrors"
	@status=0; \
	for pair in "$(EMBED_STATIC_SRC) $(EMBED_STATIC_DST)" "$(EMBED_TPL_SRC) $(EMBED_TPL_DST)"; do \
	    set -- $$pair; \
	    src=$$1; dst=$$2; \
	    if [ ! -d "$$src" ]; then echo "FAIL: $$src missing"; status=1; continue; fi; \
	    if [ ! -d "$$dst" ]; then echo "FAIL: $$dst missing"; status=1; continue; fi; \
	    out=$$(diff -r --brief "$$src" "$$dst" 2>&1 | grep -v '^Only in '"$$dst"': \(.*templates.go\)$' || true); \
	    if [ -n "$$out" ]; then \
	        echo "FAIL: $$dst does not match $$src:"; \
	        echo "$$out"; \
	        status=1; \
	    fi; \
	done; \
	if [ $$status -ne 0 ]; then \
	    echo; \
	    echo "embed mirrors are out of sync."; \
	    echo "Run 'make sync-embed' and commit the result."; \
	    exit 1; \
	fi; \
	echo "embed mirrors match"

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