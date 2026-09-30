# Build CLIProxyAPI native plugins.
#
# Every plugin directory needs the generated C ABI glue in abi.go. The glue is
# identical for all plugins and carries the //export directives, which cgo only
# emits for the main package, so it cannot be shared as a library — it is copied
# in by sync-abi.
#
# Every plugin is one dynamic library with one provider key. Multi-product
# families are merged behind a single key instead, with one auth record per
# credential (see plugins/cpa-provider-tencent).

PLUGIN_DIRS := $(wildcard plugins/*)
PLUGIN_NAMES := $(notdir $(PLUGIN_DIRS))
DIST := dist

.PHONY: all build list sync-abi clean test test-live

all: build

build: sync-abi $(DIST)/cpa-provider-tencent.dylib $(DIST)/cpa-provider-qoder.dylib

list:
	@echo $(PLUGIN_NAMES)

sync-abi:
	@for dir in $(PLUGIN_DIRS); do cp abi/abi.go.tmpl $$dir/abi.go; done
	@echo "synced abi.go into $(words $(PLUGIN_DIRS)) plugin directories"

$(DIST)/cpa-provider-tencent.dylib: sync-abi
	@mkdir -p $(DIST)
	cd plugins/cpa-provider-tencent && CGO_ENABLED=1 go build -buildmode=c-shared -o ../../$@ .
	@rm -f $(DIST)/cpa-provider-tencent.h

$(DIST)/cpa-provider-qoder.dylib: sync-abi
	@mkdir -p $(DIST)
	cd plugins/cpa-provider-qoder && CGO_ENABLED=1 go build -buildmode=c-shared -o ../../$@ .
	@rm -f $(DIST)/cpa-provider-qoder.h

test:
	go test ./...

test-live:
	CPA_LIVE_TEST=1 go test ./plugins/... -run TestLive -v

clean:
	rm -rf $(DIST)
