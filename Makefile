# Cortext CLIProxyAPI plugin
#
# Production (default install path):
#   make build-native && make install-native
#
# Stub engine (CI / protocol wiring only — NOT production Cortext quality):
#   make build-stub && make test
#
# Install into a local CLIProxyAPI plugins dir:
#   make install-native PLUGINS_DIR=../cortext-proxy/plugins

PLUGIN_DIR := plugin
OUT_DIR    := bin
NAME       := cortext
STUB_NAME  := cortext-stub

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),darwin)
  EXT := dylib
else ifeq ($(GOOS),windows)
  EXT := dll
else
  EXT := so
endif

# Production artifact basename is "cortext" (matches plugins.configs.cortext).
OUT        := $(OUT_DIR)/$(NAME).$(EXT)
# Stub artifact is explicitly named so it cannot be mistaken for production.
OUT_STUB   := $(OUT_DIR)/$(STUB_NAME).$(EXT)

CPA_ROOT     ?= $(abspath ../cortext-proxy)
PLUGINS_DIR  ?= $(CPA_ROOT)/plugins
# Optional offline assets for github.com/augmem/cortext.go (skips download).
# CORTEXT_ASSETS_DIR ?=
# CORTEXT_LIBRARY_PATH ?=

.PHONY: all test vet test-race build build-stub build-native test-native \
	install install-native install-stub e2e-blackbox tidy clean

# Default "all" is protocol-safe: unit tests + stub library for CI.
all: test build-stub

tidy:
	cd $(PLUGIN_DIR) && go mod tidy

test:
	cd $(PLUGIN_DIR) && go test ./... -count=1

vet:
	cd $(PLUGIN_DIR) && go vet ./...

test-race:
	cd $(PLUGIN_DIR) && go test -race ./... -count=1

# Alias: "build" means stub for historical CI; production is build-native.
build: build-stub

# Stub engine (no native libcortext). CI / protocol only.
# Emits bin/cortext-stub.* — never overwrite the production basename.
build-stub:
	mkdir -p $(OUT_DIR)
	cd $(PLUGIN_DIR) && CGO_ENABLED=1 go build -buildmode=c-shared -o ../$(OUT_STUB) .
	@echo "built $(OUT_STUB) engine=stub (protocol/CI only — not production Cortext)"

# Real Cortext engine via github.com/augmem/cortext.go (purego; no CGO for the
# binding). CGO is still required for -buildmode=c-shared (CPA plugin ABI).
# On first open, cortext.go fetches release natives+AIST unless CORTEXT_ASSETS_DIR
# or CORTEXT_LIBRARY_PATH is set.
build-native:
	mkdir -p $(OUT_DIR)
	cd $(PLUGIN_DIR) && \
	  CGO_ENABLED=1 go build -tags cortext_native -buildmode=c-shared -o ../$(OUT) .
	@echo "built $(OUT) engine=native (github.com/augmem/cortext.go) — production artifact"

# All native-tagged unit tests (open/process/flush + helpers).
# Downloads assets on first run unless CORTEXT_ASSETS_DIR / CORTEXT_LIBRARY_PATH is set.
test-native:
	cd $(PLUGIN_DIR) && CGO_ENABLED=1 go test -tags cortext_native ./... -count=1

# Production install: native library as plugins/cortext.*
install-native: build-native
	mkdir -p "$(PLUGINS_DIR)"
	cp "$(OUT)" "$(PLUGINS_DIR)/"
	@echo "installed native $(OUT) -> $(PLUGINS_DIR)/ (engine=native)"

# Explicit stub install (never the default production path).
install-stub: build-stub
	mkdir -p "$(PLUGINS_DIR)"
	cp "$(OUT_STUB)" "$(PLUGINS_DIR)/"
	@echo "installed stub $(OUT_STUB) -> $(PLUGINS_DIR)/ (engine=stub, NOT production quality)"

# "install" without a suffix refuses to ship the stub under the production name.
install:
	@echo "error: use 'make install-native' for production (plugins/cortext.*)."
	@echo "       use 'make install-stub' to install the protocol-only stub as cortext-stub.*"
	@exit 1

# Blackbox e2e against a *running* CPA that already has cortext (native) loaded.
# Example (brew CPA on :8317 with kimi):
#   make build-native && make install-native PLUGINS_DIR=$$HOME/.cli-proxy-api/plugins
#   brew services restart cliproxyapi
#   make e2e-blackbox
CPA_BASE  ?= http://127.0.0.1:8317
CPA_MODEL ?= kimi-k2.7-code
E2E_OUT   ?= ./bench/e2e_blackbox/out

e2e-blackbox:
	python3 bench/e2e_blackbox/run.py --base "$(CPA_BASE)" --model "$(CPA_MODEL)" --driver http --out "$(E2E_OUT)"

clean:
	rm -rf $(OUT_DIR) $(PLUGIN_DIR)/$(NAME).h $(PLUGIN_DIR)/$(STUB_NAME).h
