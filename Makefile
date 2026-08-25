PLUGIN_NAME := cursor
BIN_DIR := $(CURDIR)/bin
VERSION ?= 0.2.0
VERSION_LDFLAG := -X github.com/router-for-me/cliproxy-cursor-plugin/internal/dispatch.version=$(VERSION)

UNAME_S := $(shell uname -s)

ifeq ($(OS),Windows_NT)
PLUGIN_EXT := dll
else ifeq ($(UNAME_S),Darwin)
PLUGIN_EXT := dylib
else
PLUGIN_EXT := so
endif

.PHONY: build list package clean generate

# Mirrors CLIProxyAPI's own examples/plugin/Makefile convention:
# a cgo c-shared build producing a platform-appropriate .so/.dylib/.dll
# under bin/, loadable via plugins.configs in config.yaml.
build: $(BIN_DIR)/$(PLUGIN_NAME).$(PLUGIN_EXT)

package:
	$(MAKE) clean
	$(MAKE) build VERSION="$(VERSION)"
	./scripts/package-release.sh "$(VERSION)"

list:
	@echo $(PLUGIN_NAME)

clean:
	rm -rf $(BIN_DIR)

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

$(BIN_DIR)/$(PLUGIN_NAME).$(PLUGIN_EXT): cmd/plugin/main.go go.mod | $(BIN_DIR)
	go build -buildvcs=false -trimpath -ldflags "$(VERSION_LDFLAG)" -buildmode=c-shared -o $(abspath $@) ./cmd/plugin
	rm -f $(BIN_DIR)/$(PLUGIN_NAME).h

# Regenerates internal/cursorpb/proto/agent.protoset from the sibling
# ../gajae-code repo's generated Cursor protobuf descriptor, then
# regenerates the Go + Connect-RPC client types from it. Requires the
# sibling ../gajae-code checkout, bun, protoc, protoc-gen-go, and
# protoc-gen-connect-go on PATH. See scripts/extract-cursor-proto/.
generate:
	bun run scripts/extract-cursor-proto/extract.ts
	rm -rf internal/cursorpb/gen
	mkdir -p internal/cursorpb/gen
	protoc --descriptor_set_in=internal/cursorpb/proto/agent.protoset \
		--go_out=internal/cursorpb/gen --go_opt=paths=source_relative \
		--go_opt=Magent.proto=github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen \
		agent.proto
	protoc --descriptor_set_in=internal/cursorpb/proto/agent.protoset \
		--connect-go_out=internal/cursorpb/gen --connect-go_opt=paths=source_relative \
		--connect-go_opt=Magent.proto=github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen \
		agent.proto
	go build ./internal/cursorpb/...
