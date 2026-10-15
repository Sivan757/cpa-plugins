# Build CLIProxyAPI native plugins.
#
# Every plugin directory needs the generated C ABI glue in abi.go. The glue is
# identical for all plugins and carries the //export directives, which cgo only
# emits for the main package, so it cannot be shared as a library. sync-abi
# copies it from abi/main.go.tmpl.
#
# Every plugin is one dynamic library with one provider key. A product family
# that must expose several credentials merges them behind one key, with one auth
# record per credential (see plugins/cpa-provider-tencent).

PLUGIN_DIRS := $(wildcard plugins/*)
PLUGIN_NAMES := $(notdir $(PLUGIN_DIRS))
DIST := dist

.PHONY: all build build-linux list sync-abi clean test

all: build

# Native build for the host platform.
build: sync-abi
	@mkdir -p $(DIST)
	@for dir in $(PLUGIN_DIRS); do 		name=$$(basename $$dir); 		(cd $$dir && CGO_ENABLED=1 go build -buildmode=c-shared -o ../../$(DIST)/$$name.dylib .) 		&& echo "built $(DIST)/$$name.dylib"; 	done

# Cross-build for Linux. On macOS this needs a Linux cgo toolchain, so the
# deployment path builds inside a golang container instead.
build-linux: sync-abi
	@mkdir -p $(DIST)
	@for dir in $(PLUGIN_DIRS); do 		name=$$(basename $$dir); 		(cd $$dir && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o ../../$(DIST)/$$name.so .) 		&& echo "built $(DIST)/$$name.so"; 	done

list:
	@echo $(PLUGIN_NAMES)

# The ABI glue is generated, so it is never edited in place.
sync-abi:
	@for dir in $(PLUGIN_DIRS); do cp abi/main.go.tmpl $$dir/abi.go; done
	@echo "synced abi.go into $(words $(PLUGIN_DIRS)) plugin directories"

test:
	go test ./...

clean:
	rm -rf $(DIST) dist-linux
