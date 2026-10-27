BINARY    := ilo100vnc
PKG       := ./cmd/ilo100vnc
BIN_DIR   := bin

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOBUILD   := CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)'

# noVNC is downloaded at build time and not committed (MPL-2.0, license
# files are copied along).
NOVNC_VERSION := v1.7.0
NOVNC_SHA256  := b1003a11b6e6e8d8f7f5e5586daae7f8ca651d8aee0aa155ff9ac841c48f52c6
NOVNC_URL     := https://github.com/novnc/noVNC/archive/refs/tags/$(NOVNC_VERSION).tar.gz
NOVNC_DIR     := internal/web/novnc
NOVNC_FILES   := app core vendor vnc.html vnc_lite.html defaults.json mandatory.json package.json LICENSE.txt AUTHORS

GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0

.PHONY: all build novnc test vet lint clean clean-novnc

all: build

## build: static binary in bin/ (cross-compile with GOOS/GOARCH)
build: novnc
	$(GOBUILD) -o $(BIN_DIR)/$(BINARY) $(PKG)

## novnc: download and vendor noVNC into internal/web/novnc (not committed)
novnc: $(NOVNC_DIR)/vnc.html

$(NOVNC_DIR)/vnc.html:
	@set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	echo "downloading noVNC $(NOVNC_VERSION)"; \
	curl -fsSL -o "$$tmp/novnc.tar.gz" "$(NOVNC_URL)"; \
	echo "$(NOVNC_SHA256)  $$tmp/novnc.tar.gz" | sha256sum -c --quiet -; \
	mkdir -p "$$tmp/src"; \
	tar -xzf "$$tmp/novnc.tar.gz" -C "$$tmp/src" --strip-components=1; \
	mkdir -p $(NOVNC_DIR)/docs; \
	for f in $(NOVNC_FILES); do cp -R "$$tmp/src/$$f" $(NOVNC_DIR)/; done; \
	cp "$$tmp"/src/docs/LICENSE.* $(NOVNC_DIR)/docs/; \
	echo "$(NOVNC_VERSION)" > $(NOVNC_DIR)/VERSION

## test: run unit tests with the race detector
test:
	go test -race ./...

## vet: go vet
vet:
	go vet ./...

## lint: golangci-lint (incl. gofumpt), see .golangci.yml
lint:
	go run $(GOLANGCI_LINT) run ./...

## clean: remove build output
clean:
	rm -rf $(BIN_DIR)

## clean-novnc: remove the downloaded noVNC
clean-novnc:
	find $(NOVNC_DIR) -mindepth 1 ! -name .gitkeep -exec rm -rf {} +
