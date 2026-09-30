#!/usr/bin/env bash
# Install the built plugins into the local EasyCLIProxyAPI (CPA) instance.
#
# The script is deliberately conservative: it copies libraries, prints the exact
# config and auth-file changes the operator still has to make, and never edits
# config.yaml or restarts the core on its own. Both of those change a running
# proxy the user is actively using, so they stay explicit decisions.
set -euo pipefail

CORE_DIR="${CPA_CORE_DIR:-$HOME/Library/Application Support/com.cpa.gui/cpa-core}"
PLUGIN_DIR="${CPA_PLUGIN_DIR:-$CORE_DIR/plugins}"
AUTH_DIR="${CPA_AUTH_DIR:-$HOME/.cli-proxy-api}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$REPO_DIR/dist"

if [[ ! -d "$CORE_DIR" ]]; then
  echo "error: CPA core directory not found: $CORE_DIR" >&2
  echo "set CPA_CORE_DIR if your install lives elsewhere" >&2
  exit 1
fi

shopt -s nullglob
libraries=("$DIST_DIR"/*.dylib)
if (( ${#libraries[@]} == 0 )); then
  echo "error: no built plugins in $DIST_DIR; run 'make build' first" >&2
  exit 1
fi

# The host prefers <root>/<goos>/<goarch>/ over <root>/ for plugin lookup.
target="$PLUGIN_DIR/$(go env GOOS)/$(go env GOARCH)"
mkdir -p "$target"
echo "installing into $target"

for library in "${libraries[@]}"; do
  name="$(basename "$library")"
  cp -f "$library" "$target/$name"
  echo "  copied $name"
done

# Auth files are small JSON stubs that bind a provider key to this plugin.
mkdir -p "$AUTH_DIR"

echo
echo "Next steps (deliberately not automated):"
echo
echo "1. Enable the plugins in $CORE_DIR/config.yaml:"
echo
echo "   plugins:"
echo "     enabled: true"
echo "     dir: "plugins""
echo "     configs:"
for library in "${libraries[@]}"; do
  id="$(basename "$library" .dylib)"
  echo "       $id:"
  echo "         enabled: true"
  echo "         priority: 10"
done
echo
echo "2. Create the credential stub for each provider you enabled, in $AUTH_DIR:"
echo "     workbuddy     -> workbuddy.json      {"type":"workbuddy"}"
echo "     codebuddy     -> codebuddy.json      {"type":"codebuddy"}"
echo "     qoder         -> qoder.json         {"type":"qoder"}"
echo "     trae          -> trae.json           {"type":"trae"}"
echo
echo "3. Restart the CPA core. Plugin libraries and enabled state are read at startup;"
echo "   config changes alone do not reload them."
echo
echo "Verify afterwards:"
echo "   curl -s -H 'Authorization: Bearer <management-key>' http://127.0.0.1:8317/v0/management/plugins"
