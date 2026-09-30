#!/usr/bin/env bash
# End-to-end verification of every built plugin against a throwaway CPA instance.
#
# This never touches the user's running proxy: it uses its own port, auth
# directory, and plugin directory, so a red result here cannot disturb a live
# CPA installation.
set -uo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAB_DIR="${CPA_LAB_DIR:-$REPO_DIR/../cpa-lab}"
CPA_BIN="${CPA_BIN:-$HOME/Library/Application Support/com.cpa.gui/cpa-core/cli-proxy-api}"
PORT="${CPA_LAB_PORT:-8318}"
MGMT_KEY="${CPA_LAB_MGMT_KEY:-lab-test-key-0001}"
API_KEY="${CPA_LAB_API_KEY:-sk-lab}"
BASE="http://127.0.0.1:$PORT"

pass=0
fail=0
check() {
  local label="$1" actual="$2" expected="$3"
  if [[ "$actual" == "$expected" ]]; then
    printf '  ok   %-46s %s\n' "$label" "$actual"
    pass=$((pass + 1))
  else
    printf '  FAIL %-46s got=%s want=%s\n' "$label" "$actual" "$expected"
    fail=$((fail + 1))
  fi
}

if [[ ! -x "$CPA_BIN" ]]; then
  echo "error: CPA binary not found at $CPA_BIN (set CPA_BIN)" >&2
  exit 1
fi

echo "== staging plugins into $LAB_DIR/plugins"
mkdir -p "$LAB_DIR/plugins" "$LAB_DIR/auths"
cp -f "$REPO_DIR"/dist/*.dylib "$LAB_DIR/plugins/"

# One credential stub per enabled provider.
  printf '{"type":"%s"}\n' "$provider" > "$LAB_DIR/auths/$provider.json"
done

echo "== starting CPA on port $PORT"
pkill -f "config $LAB_DIR/config.yaml" 2>/dev/null
sleep 1
(cd "$LAB_DIR" && "$CPA_BIN" -config "$LAB_DIR/config.yaml" > "$LAB_DIR/core.log" 2>&1) &
for _ in $(seq 1 40); do
  if curl -sf -o /dev/null -H "Authorization: Bearer $API_KEY" "$BASE/v1/models"; then break; fi
  sleep 0.5
done

echo
echo "== plugin registration"
registered="$(curl -s -H "Authorization: Bearer $MGMT_KEY" "$BASE/v0/management/plugins" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(sorted(p["id"] for p in d["plugins"] if p.get("registered"))))')"
echo "  registered: $registered"

echo
echo "== quota providers"
curl -s -H "Authorization: Bearer $MGMT_KEY" "$BASE/v0/management/quota/providers" \
  | python3 -c 'import json,sys; [print("  %-28s -> %s" % (p["plugin_id"], p["provider"])) for p in json.load(sys.stdin)["providers"]]'

echo
echo "== per-provider status routes"
for id in $registered; do
  code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $MGMT_KEY" "$BASE/v0/management/plugins/$id/status")"
  check "$id status" "$code" "200"
done

echo
echo "== model inventory"
curl -s -H "Authorization: Bearer $API_KEY" "$BASE/v1/models" \
  | python3 -c '
import json,sys
from collections import Counter
data = json.load(sys.stdin)["data"]
for owner, count in sorted(Counter(m.get("owned_by") for m in data).items()):
    print("  %-20s %d" % (owner, count))
'

echo
echo "== quota fetch per credential"
  index="$(curl -s -H "Authorization: Bearer $MGMT_KEY" "$BASE/v0/management/auth-files" \
    | python3 -c "import json,sys; print(next((f['auth_index'] for f in json.load(sys.stdin)['files'] if f['provider']=='$provider'), ''))")"
  [[ -z "$index" ]] && continue
  body="$(curl -s -H "Authorization: Bearer $MGMT_KEY" -H 'Content-Type: application/json' \
    -X POST "$BASE/v0/management/quota/fetch" -d "{\"auth_index\":\"$index\"}")"
  summary="$(printf '%s' "$body" | python3 -c '
import json,sys
d = json.load(sys.stdin)
if "error" in d:
    print("error: " + d["error"][:70])
else:
    plan = (d.get("subscription") or {}).get("plan", "-")
    buckets = sum(len(g.get("buckets") or []) for g in (d.get("groups") or []))
    metrics = ", ".join("%s=%g" % (m["label"], m["value"]) for m in (d.get("summary") or [])[:2])
    print("plan=%s buckets=%d %s" % (plan, buckets, metrics))
' 2>/dev/null || echo "unparsable")"
  printf '  %-14s %s\n' "$provider" "$summary"
done

echo
echo "== result: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
