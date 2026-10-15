#!/usr/bin/env bash
# Install the built plugins into a local CLIProxyAPI (CPA) instance.
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

# Optional: pass a version to install each library under a versioned name. The
# host selects a versioned file over an unversioned one, which is the only way to
# replace a library that is already loaded.
VERSION="${1:-}"

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
  if [[ -n "$VERSION" ]]; then
    stem="${name%.dylib}"
    name="${stem}-v${VERSION}.dylib"
  fi
  cp -f "$library" "$target/$name"
  echo "  copied $name"
done

# Auth records are small JSON files that bind a provider key to this plugin.
mkdir -p "$AUTH_DIR"

echo
echo "Next steps (deliberately not automated):"
echo
echo "1. Enable the plugins in $CORE_DIR/config.yaml:"
echo
echo "   plugins:"
echo "     enabled: true"
echo "     configs:"
for library in "${libraries[@]}"; do
  id="$(basename "$library" .dylib)"
  echo "       $id:"
  echo "         enabled: true"
  echo "         priority: 10"
done
echo
echo "2. Give each provider the auth record it needs, in $AUTH_DIR:"
echo
echo "     opencode -> opencode-free.json         {\"type\":\"opencode\",\"tier\":\"free\"}"
echo "                 the plugin writes this record itself; add an"
echo "                 api_key in the config block for the paid Go tier"
echo "     qoder    -> qoder.json                 {\"type\":\"qoder\",\"pat\":\"<token>\",\"region\":\"china\"}"
echo "     tencent  -> tencent-workbuddy-ai.json  {\"type\":\"tencent\",\"kind\":\"workbuddy-ai\"}"
echo
echo "3. Apply the config. A content change reloads the core in place, so no restart"
echo "   is needed. A plugin library is replaced only when the selected file path"
echo "   changes; rerun this script with a version argument to do that:"
echo
echo "     scripts/install.sh 0.4.0"
echo
echo "Verify afterwards:"
echo "   curl -s -H 'Authorization: Bearer <management-key>' http://127.0.0.1:8317/v0/management/plugins"
