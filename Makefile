# Bazel Broker — build & verify entrypoints. See CLAUDE.md for per-component recipes.
SHELL          := /usr/bin/env bash
MODULE         := github.com/papanton/bazel-broker
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

.PHONY: up
up:                                     ## build, run the daemon, and open the dashboard (one command; Ctrl-C stops)
	@scripts/up.sh

.PHONY: dist
dist:                                   ## build universal (arm64+x86_64) app + brokerctl into dist/ for Homebrew
	@scripts/dist.sh $(VERSION)

.PHONY: run-broker
run-broker: $(BROKER_BIN)               ## run the daemon in the foreground (Ctrl-C to stop)
	$(BROKER_BIN)

.PHONY: fmt vet test tidy
fmt:  ; gofmt -w cmd internal
vet:  ; go vet ./...
test: ; go test ./...
tidy: ; go mod tidy

# E4: regenerate Bazel BEP Go types from the vendored, pinned (Bazel 8.3.1) protos
# under third_party/bazel_protos/. The generated *.pb.go ARE committed, so this is
# only re-run on a Bazel version bump. Requires protoc + protoc-gen-go on PATH
# (go install google.golang.org/protobuf/cmd/protoc-gen-go).
PROTO_ROOT := third_party/bazel_protos
GENPROTO   := $(MODULE)/internal/genproto
BES_PROTO  := src/main/java/com/google/devtools/build/lib/buildeventstream/proto/build_event_stream.proto
.PHONY: protos
protos:                                 ## regenerate internal/genproto from vendored Bazel protos
	protoc -I "$(PROTO_ROOT)" \
	  --go_out=internal/genproto --go_opt=module="$(GENPROTO)" \
	  --go_opt=M$(BES_PROTO)="$(GENPROTO)/buildeventstream" \
	  --go_opt=Msrc/main/java/com/google/devtools/build/lib/packages/metrics/package_load_metrics.proto="$(GENPROTO)/packagemetrics" \
	  --go_opt=Msrc/main/protobuf/command_line.proto="$(GENPROTO)/commandline" \
	  --go_opt=Msrc/main/protobuf/option_filters.proto="$(GENPROTO)/optionfilters" \
	  --go_opt=Msrc/main/protobuf/invocation_policy.proto="$(GENPROTO)/invocationpolicy" \
	  --go_opt=Msrc/main/protobuf/strategy_policy.proto="$(GENPROTO)/strategypolicy" \
	  --go_opt=Msrc/main/protobuf/action_cache.proto="$(GENPROTO)/actioncache" \
	  --go_opt=Msrc/main/protobuf/failure_details.proto="$(GENPROTO)/failuredetails" \
	  $(BES_PROTO) \
	  src/main/java/com/google/devtools/build/lib/packages/metrics/package_load_metrics.proto \
	  src/main/protobuf/command_line.proto src/main/protobuf/option_filters.proto \
	  src/main/protobuf/invocation_policy.proto src/main/protobuf/strategy_policy.proto \
	  src/main/protobuf/action_cache.proto src/main/protobuf/failure_details.proto
	gofmt -w internal/genproto
	go build ./internal/genproto/...

.PHONY: verify-e4
verify-e4: build                        ## E4: real-fixture cache-hit + truncation + provider tests
	go test ./internal/bep/... ./internal/metrics/... ./internal/store/...

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

.PHONY: verify-brokerctl
verify-brokerctl:                       ## brokerctl unit tests (scoped; no broker needed)
	go test ./cmd/brokerctl/... ./internal/cli/... ./internal/apiclient/...

.PHONY: admission-verify
admission-verify:                       ## E5: admission engine + tools/bazel wrapper (race; no daemon needed)
	go test -race ./internal/admission/...

.PHONY: clean
clean: ; rm -rf $(BIN_DIR)
