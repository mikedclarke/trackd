BIN := bin/trackd
PREFIX ?= $(HOME)/.local
# The version is the single const in main.go, bumped by hand at release time
# (see the release runbook). No git-describe, so `trackd version` is always a
# clean SemVer, never a commit-dirtied string.
VERSION = $(shell sed -n 's/^const version = "\(.*\)"/\1/p' main.go)

.PHONY: all build test lint install clean version dist

all: build

build:
	@mkdir -p $(dir $(BIN))
	go build -o $(BIN) .
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

# Release archives for every supported platform, plus a checksum file, in
# dist/. A release must be traceable to a commit, so the tree must be clean
# (ALLOW_DIRTY=1 is the local escape hatch).
dist:
	@if [ -n "$$(git status --porcelain)" ] && [ "$(ALLOW_DIRTY)" != "1" ]; then \
	  echo "refusing to build a release from a dirty tree; commit first or set ALLOW_DIRTY=1" >&2; exit 1; \
	fi
	@v="$(VERSION)"; \
	rm -rf dist && mkdir -p dist; \
	for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  name="trackd_$${v}_$${os}_$${arch}"; ext=""; \
	  [ "$$os" = windows ] && ext=".exe"; \
	  mkdir -p "dist/$$name"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w" -o "dist/$$name/trackd$$ext" . || exit 1; \
	  cp README.md LICENSE "dist/$$name/"; \
	  if [ "$$os" = windows ]; then (cd dist && zip -qr "$$name.zip" "$$name"); else tar -C dist -czf "dist/$$name.tar.gz" "$$name"; fi; \
	  rm -rf "dist/$$name"; \
	done; \
	(cd dist && shasum -a 256 *.tar.gz *.zip > checksums.txt); \
	ls -1 dist

clean:
	rm -rf bin
