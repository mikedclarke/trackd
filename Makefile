BIN := bin/trackd
PREFIX ?= $(HOME)/.local
VERSION = $(shell git describe --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test lint install clean version

all: build

# A release binary must be traceable to a commit, so a build refuses a tree
# with uncommitted changes. ALLOW_DIRTY=1 is the escape hatch for local work.
build:
	@v="$(VERSION)"; \
	case "$$v" in \
	  *-dirty) \
	    if [ "$(ALLOW_DIRTY)" != "1" ]; then \
	      echo "refusing to build from a dirty tree ($$v); commit first or set ALLOW_DIRTY=1" >&2; \
	      exit 1; \
	    fi; \
	    echo "building from a dirty tree ($$v)" >&2 ;; \
	esac; \
	mkdir -p $(dir $(BIN)); \
	go build -ldflags "-X main.version=$$v" -o $(BIN) .
	@echo "built $(BIN) $$($(BIN) version)"

test:
	go test -race ./...

lint:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  golangci-lint run; \
	else \
	  echo "golangci-lint not installed, skipping"; \
	fi

# Install builds to a temporary path and moves it into place. A move is atomic
# and replaces the directory entry, so a trackd already running from the target
# keeps its own inode; writing over the file in place would corrupt the running
# process on some systems.
install: build
	@mkdir -p $(PREFIX)/bin
	@tmp="$(PREFIX)/bin/.trackd.$$$$"; \
	cp $(BIN) "$$tmp" && chmod 0755 "$$tmp" && mv -f "$$tmp" "$(PREFIX)/bin/trackd"
	@echo "installed $(PREFIX)/bin/trackd"

version:
	@echo $(VERSION)

clean:
	rm -rf bin
