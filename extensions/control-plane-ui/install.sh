#!/usr/bin/env bash
# Installs and activates the control-plane UI through the extension
# registry itself (daemon/api's /v1/extensions routes) — proving the UI is
# a real, removable extension, not code baked into the daemon. Requires an
# operator token; PORT/AMH_API_BASE_URL are passed through to the UI
# process's own environment when the daemon launches it (see server.js).
#
# Usage:
#   AMH_API_URL=http://127.0.0.1:8090 AMH_OPERATOR_TOKEN=... ./install.sh
set -euo pipefail

AMH_API_URL="${AMH_API_URL:-http://127.0.0.1:8090}"
: "${AMH_OPERATOR_TOKEN:?set AMH_OPERATOR_TOKEN to the daemon operator token}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVER_JS="$SCRIPT_DIR/server.js"

MANIFEST="$(python3 - "$SCRIPT_DIR" "$SERVER_JS" <<'PYEOF'
import json, sys
script_dir, server_js = sys.argv[1], sys.argv[2]
with open(script_dir + '/extension.json') as f:
    manifest = json.load(f)
manifest['spec']['entrypoint'] = server_js
print(json.dumps(manifest))
PYEOF
)"

echo "Discovering amh.control-plane/ui@1.0.0 ..."
curl -sf -X POST "$AMH_API_URL/v1/extensions" \
  -H "Authorization: Bearer $AMH_OPERATOR_TOKEN" \
  -H "Content-Type: application/json" \
  -d "$MANIFEST" | python3 -m json.tool

echo "Activating amh.control-plane/ui@1.0.0 ..."
curl -sf -X POST "$AMH_API_URL/v1/extensions/activate" \
  -H "Authorization: Bearer $AMH_OPERATOR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"id":"amh.control-plane/ui","version":"1.0.0"}' | python3 -m json.tool

echo "Installed. The UI process is now running under the daemon's extension host, listening on \${PORT:-8091}."
