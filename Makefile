# Build CLIProxyAPI native plugins.
#
# Every plugin directory needs the generated C ABI glue in abi.go. The glue is
# identical for all plugins and carries the //export directives, which cgo only
# emits for the main package, so it cannot be shared as a library — it is copied
# in by sync-abi from abi/main.go.tmpl.
#
# Every plugin is one dynamic library with one provider key. Multi-product
# families are merged behind a single key instead, with one auth record per
# credential (see plugins/cpa-provider-tencent).

PLUGIN_DIRS := $(wildcard plugins/*)
PLUGIN_NAMES := $(notdir $(PLUGIN_DIRS))
DIST := dist

.PHONY: all build build-linux list sync-abi clean test test-live

all: build

# Native build of every plugin for the host platform.
build: sync-abi
	@mkdir -p $(DIST)
	@for dir in $(PLUGIN_DIRS); do \
		name=$$(basename $$dir); \
		(cd $$dir && CGO_ENABLED=1 go build -buildmode=c-shared -o ../../$(DIST)/$$name.dylib .) \
		&& echo "built $(DIST)/$$name.dylib"; \
	done

# Cross-build for the Linux deployment target. The server build script runs this
# inside a golang container so cgo sees a Linux toolchain.
build-linux: sync-abi
	@mkdir -p $(DIST)
	@for dir in $(PLUGIN_DIRS); do \
		name=$$(basename $$dir); \
		(cd $$dir && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o ../../$(DIST)/$$name.so .) \
		&& echo "built $(DIST)/$$name.so"; \
	done

list:
	@echo $(PLUGIN_NAMES)

sync-abi:
	@for dir in $(PLUGIN_DIRS); do cp abi/main.go.tmpl $$dir/abi.go; done
	@echo "synced abi.go into $(words $(PLUGIN_DIRS)) plugin directories"

test:
	go test ./...

test-live:
	CPA_LIVE_TEST=1 go test ./plugins/... -run TestLive -v

clean:
	rm -rf $(DIST)
