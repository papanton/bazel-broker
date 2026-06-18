# Bazel Broker — build & verify entrypoints. See CLAUDE.md for per-component recipes.
SHELL          := /usr/bin/env bash
MODULE         := github.com/antoniospapantoniou/bazel-broker
BIN_DIR        := bin
BROKER_BIN     := $(BIN_DIR)/broker
BROKERCTL_BIN  := $(BIN_DIR)/brokerctl
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE           ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS        := -X $(MODULE)/internal/version.Version=$(VERSION) \
                  -X $(MODULE)/internal/version.Commit=$(COMMIT) \
                  -X $(MODULE)/internal/version.Date=$(DATE)

.DEFAULT_GOAL := build

.PHONY: build
build: $(BROKER_BIN) $(BROKERCTL_BIN)   ## build broker + brokerctl

$(BROKER_BIN): $(shell find cmd/broker internal -name '*.go' 2>/dev/null) go.mod
	@mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BROKER_BIN) ./cmd/broker

$(BROKERCTL_BIN): $(shell find cmd/brokerctl internal -name '*.go' 2>/dev/null) go.mod
	@mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BROKERCTL_BIN) ./cmd/brokerctl

.PHONY: run-broker
run-broker: $(BROKER_BIN)               ## run the daemon in the foreground (Ctrl-C to stop)
	$(BROKER_BIN)

.PHONY: fmt vet test tidy
fmt:  ; gofmt -w cmd internal
vet:  ; go vet ./...
test: ; go test ./...
tidy: ; go mod tidy

.PHONY: verify-fast
verify-fast: build                      ## headless ~3s sanity: build + unit tests + fake-bazel + daemon smoke
	go test ./...
	scripts/verify-fast.sh

.PHONY: verify-e2e
verify-e2e: build                       ## real bazel build in testdata/workspace (skips if no bazel)
	@if command -v bazel >/dev/null 2>&1 || command -v bazelisk >/dev/null 2>&1; then \
	  BAZEL=$$(command -v bazelisk || command -v bazel); \
	  echo ">> using $$BAZEL"; \
	  ( cd testdata/workspace && \
	    "$$BAZEL" build //:gen //:hello \
	      --build_event_json_file=/tmp/bb-e2e-bep.json \
	      --profile=/tmp/bb-e2e.profile.gz ); \
	  echo ">> BEP events:"; wc -l < /tmp/bb-e2e-bep.json; \
	  echo ">> profile written: /tmp/bb-e2e.profile.gz (open in https://ui.perfetto.dev)"; \
	else \
	  echo "SKIP verify-e2e: no bazel/bazelisk on PATH"; \
	fi

.PHONY: install uninstall
install: $(BROKER_BIN)                  ## install the launchd LaunchAgent (per-user)
	deploy/install.sh install "$(abspath $(BROKER_BIN))"
uninstall:                              ## remove the launchd LaunchAgent
	deploy/install.sh uninstall

.PHONY: smoke
smoke: $(BROKER_BIN)                    ## start broker, run the register->ls->deregister curl flow, assert states
	scripts/smoke.sh

.PHONY: clean
clean: ; rm -rf $(BIN_DIR)
