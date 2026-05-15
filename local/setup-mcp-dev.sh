#!/usr/bin/env bash
# Set up boxy for real-world Claude Code MCP usage.
#
# What this does:
#   1. Upgrades the helm chart with defaultSandbox enabled + a static dev token.
#   2. Starts port-forwarding in the background (kills any existing one first).
#   3. Creates a "custom" sandbox alongside the default one.
#   4. Writes .claude/settings.json with two MCP server entries so Claude Code
#      can reach boxy out of the box without additional config.
#
# Usage:
#   bash local/setup-mcp-dev.sh
#
# Prerequisites: kind cluster running (run `make e2e` first).
# After running, open a new Claude Code session inside this repo.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-boxy-e2e}"
CTX="kind-${CLUSTER_NAME}"
NAMESPACE="${NAMESPACE:-boxy}"
RELEASE_NAME="${RELEASE_NAME:-boxy}"
IMAGE_REPO="${IMAGE_REPO:-boxydev}"
TAG="${TAG:-e2e}"
ROUTER_PORT="${ROUTER_PORT:-18080}"
DEV_TOKEN="${BOXY_ROUTER_TOKEN:-boxy-dev-token}"

# -----------------------------------------------------------------------
# 1. Helm upgrade: enable default sandbox, set static dev token
# -----------------------------------------------------------------------

echo ">>> Upgrading helm release with defaultSandbox + staticToken"
helm upgrade "${RELEASE_NAME}" ./deploy/helm/boxy \
  -n "${NAMESPACE}" \
  --set "router.image.repository=${IMAGE_REPO}/boxy-router" \
  --set "router.image.tag=${TAG}" \
  --set "controller.image.repository=${IMAGE_REPO}/boxy-controller" \
  --set "controller.image.tag=${TAG}" \
  --set "operator.image.repository=${IMAGE_REPO}/boxy-operator" \
  --set "operator.image.tag=${TAG}" \
  --set router.replicas=1 \
  --set controller.replicas=1 \
  --set mtls.disabled=true \
  --set router.auth.staticToken="${DEV_TOKEN}" \
  --set router.defaultSandbox.enabled=true \
  --set router.defaultSandbox.sandboxId=default \
  --set router.defaultSandbox.ttlSeconds=86400 \
  --set "router.defaultSandbox.vm.memoryMb=0" \
  --kube-context "${CTX}"

echo ">>> Waiting for router rollout"
kubectl --context "${CTX}" -n "${NAMESPACE}" \
  rollout status deployment/"${RELEASE_NAME}"-router --timeout=120s

# -----------------------------------------------------------------------
# 2. Port-forward (background, replacing any existing one)
# -----------------------------------------------------------------------

echo ">>> Setting up port-forward on :${ROUTER_PORT}"
PF_PIDFILE="${TMPDIR:-/tmp}/boxy-pf.pid"

if [[ -f "${PF_PIDFILE}" ]]; then
  OLD_PID=$(cat "${PF_PIDFILE}")
  kill "${OLD_PID}" 2>/dev/null || true
  rm -f "${PF_PIDFILE}"
fi

kubectl --context "${CTX}" -n "${NAMESPACE}" \
  port-forward svc/"${RELEASE_NAME}"-router "${ROUTER_PORT}":8080 &
PF_PID=$!
echo "${PF_PID}" > "${PF_PIDFILE}"
echo "    port-forward PID: ${PF_PID} (saved to ${PF_PIDFILE})"

BASE="http://127.0.0.1:${ROUTER_PORT}"
echo ">>> Waiting for router health"
for i in $(seq 1 20); do
  curl -fsS "${BASE}/healthz" 2>/dev/null | grep -q ok && break
  sleep 2
done
curl -fsS "${BASE}/healthz" | grep ok
echo ""

# -----------------------------------------------------------------------
# 3. Create custom sandbox (for comparing against default)
# -----------------------------------------------------------------------

echo ">>> Creating custom sandbox (sandboxId=custom-sandbox)"
custom_resp=$(curl -sS -X POST "${BASE}/v1/sandboxes" \
  -H "Authorization: Bearer ${DEV_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "sandboxId": "custom-sandbox",
    "sessionId": "mcp-dev",
    "owner": "dev",
    "ttlSeconds": 86400,
    "env": {"SANDBOX_TYPE": "custom", "CUSTOM_SECRET": "only-in-custom"}
  }' 2>/dev/null || echo '{}')

custom_phase=$(echo "${custom_resp}" | jq -r '.phase // "error"')
echo "    custom-sandbox phase: ${custom_phase}"

# -----------------------------------------------------------------------
# 4. Write .claude/settings.json
# -----------------------------------------------------------------------

mkdir -p .claude

echo ">>> Writing .claude/settings.json"
cat > .claude/settings.json <<EOF
{
  "mcpServers": {
    "boxy-default": {
      "type": "http",
      "url": "${BASE}/mcp",
      "headers": {
        "Authorization": "Bearer ${DEV_TOKEN}"
      }
    },
    "boxy-custom": {
      "type": "http",
      "url": "${BASE}/mcp",
      "headers": {
        "Authorization": "Bearer ${DEV_TOKEN}",
        "X-Sandbox-Id": "custom-sandbox"
      }
    }
  }
}
EOF

echo ""
echo "================================================================"
echo "  boxy MCP dev environment ready"
echo "================================================================"
echo "  Router:          ${BASE}"
echo "  Dev token:       ${DEV_TOKEN}"
echo "  Default sandbox: auto-created on first MCP call"
echo "  Custom sandbox:  custom-sandbox (phase: ${custom_phase})"
echo ""
echo "  MCP servers configured in .claude/settings.json:"
echo "    boxy-default  → ${BASE}/mcp  (no X-Sandbox-Id, uses default)"
echo "    boxy-custom   → ${BASE}/mcp  (X-Sandbox-Id: custom-sandbox)"
echo ""
echo "  Reload Claude Code to pick up the new MCP config."
echo "================================================================"
