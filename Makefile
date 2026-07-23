# Cortext CLIProxyAPI plugin
#
# Default build uses the in-process stub engine (no libcortext).
# Production build:
#   CORTEXT_ROOT=../cortext make build-native
#
# Install into a local CLIProxyAPI plugins dir:
#   make install PLUGINS_DIR=../cortext-proxy/plugins

PLUGIN_DIR := plugin
OUT_DIR    := bin
NAME       := cortext

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),darwin)
  EXT := dylib
else ifeq ($(GOOS),windows)
  EXT := dll
else
  EXT := so
endif

OUT := $(OUT_DIR)/$(NAME).$(EXT)

CORTEXT_ROOT ?= $(abspath ../cortext)
CPA_ROOT     ?= $(abspath ../cortext-proxy)
PLUGINS_DIR  ?= $(CPA_ROOT)/plugins

.PHONY: all test build build-native install tidy clean

all: test build

tidy:
	cd $(PLUGIN_DIR) && go mod tidy

test:
	cd $(PLUGIN_DIR) && go test ./...

# Stub engine (no cgo link to libcortext). Good for protocol tests and CI.
build:
	mkdir -p $(OUT_DIR)
	cd $(PLUGIN_DIR) && CGO_ENABLED=1 go build -buildmode=c-shared -o ../$(OUT) .

# Real Cortext engine via the Go cgo binding + local cortext checkout.
build-native:
	@test -d "$(CORTEXT_ROOT)/bindings/go" || (echo "CORTEXT_ROOT=$(CORTEXT_ROOT) missing bindings/go"; exit 1)
	mkdir -p $(OUT_DIR)
	cd $(PLUGIN_DIR) && \
	  go mod edit -replace=github.com/gabrielwillen/cortext/bindings/go=$(CORTEXT_ROOT)/bindings/go && \
	  go get github.com/gabrielwillen/cortext/bindings/go@v0.0.0 && \
	  CGO_ENABLED=1 go build -tags cortext_native -buildmode=c-shared -o ../$(OUT) .
	@echo "built $(OUT) with cortext_native (CORTEXT_ROOT=$(CORTEXT_ROOT))"

install: build
	mkdir -p "$(PLUGINS_DIR)"
	cp "$(OUT)" "$(PLUGINS_DIR)/"
	@echo "installed $(OUT) -> $(PLUGINS_DIR)/"

install-native: build-native
	mkdir -p "$(PLUGINS_DIR)"
	cp "$(OUT)" "$(PLUGINS_DIR)/"
	@echo "installed native $(OUT) -> $(PLUGINS_DIR)/"

clean:
	rm -rf $(OUT_DIR) $(PLUGIN_DIR)/$(NAME).h
