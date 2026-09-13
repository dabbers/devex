GO      ?= go
BIN     ?= bin
PKGS    := ./...
# Binary targets depend on every source file, embedded assets included: the web
# UI is compiled into the binary, so a CSS or JS change is a rebuild. Without
# this make would treat an existing binary as up to date and ship a stale build.
SOURCES := $(shell find . -type f \( -name '*.go' -o -path './internal/web/static/*' -o -name '*.sql' \) -not -path './bin/*') go.mod go.sum
LDFLAGS := -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all
all: build

.PHONY: build
build: $(BIN)/dabberzd $(BIN)/dabberzctl

$(BIN)/dabberzd: $(SOURCES)
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/dabberzd

$(BIN)/dabberzctl: $(SOURCES)
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/dabberzctl

.PHONY: test
test:
	$(GO) test -race -count=1 $(PKGS)

.PHONY: cover
cover:
	$(GO) test -covermode=atomic -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt:
	$(GO) fmt $(PKGS)

.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed; skipping"; exit 0; }
	golangci-lint run

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: check
check: fmt vet test

.PHONY: run
run: build
	$(BIN)/dabberzd --config configs/dabberz.yaml

# The web UI is compiled into the binary, so editing CSS or JS has no effect
# until the binary is rebuilt and the daemon restarted. This does both.
.PHONY: dev
dev:
	$(MAKE) build
	@echo "restart dabberzd to pick up the rebuilt UI"

.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out
