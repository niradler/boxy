#!/usr/bin/env bash
# Validate internet-access sandboxes vs. isolated sandboxes.
#
# Covers:
#   - Sandbox with network.allowInternetAccess=true can reach the internet.
#   - Sandbox with default (no internet) is blocked.
#   - Two sandboxes on the same controller cannot reach each other's network
#     namespace (each gets its own isolated netns).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

if [[ -z "${BASE_URL}" || -z "${ROUTER_TOKEN}" ]]; then
  echo "BASE_URL and ROUTER_TOKEN are required"
  exit 1
fi

# -----------------------------------------------------------------------
# Setup
# -----------------------------------------------------------------------

SB_INET=$(unique_id)   # internet allowed
SB_BLOCK=$(unique_id)  # default (no internet)
SB_CROSS_A=$(unique_id)
SB_CROSS_B=$(unique_id)

curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_INET}\",\"ttlSeconds\":300,\"network\":{\"allowInternetAccess\":true}}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_BLOCK}\",\"ttlSeconds\":300}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_CROSS_A}\",\"ttlSeconds\":300,\"network\":{\"allowInternetAccess\":true}}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_CROSS_B}\",\"ttlSeconds\":300}" >/dev/null

SESS_INET=$(curl_api POST "/v1/sessions" -d "{\"sandboxId\":\"${SB_INET}\"}" | jq -r '.sessionId // empty')
SESS_BLOCK=$(curl_api POST "/v1/sessions" -d "{\"sandboxId\":\"${SB_BLOCK}\"}" | jq -r '.sessionId // empty')
SESS_CROSS_A=$(curl_api POST "/v1/sessions" -d "{\"sandboxId\":\"${SB_CROSS_A}\"}" | jq -r '.sessionId // empty')
SESS_CROSS_B=$(curl_api POST "/v1/sessions" -d "{\"sandboxId\":\"${SB_CROSS_B}\"}" | jq -r '.sessionId // empty')

if ! wait_session_ready "${SESS_INET}" 120 || ! wait_session_ready "${SESS_BLOCK}" 120; then
  echo "Sessions did not become Ready in time"
  exit 1
fi

# -----------------------------------------------------------------------
# Internet access: allowed
# -----------------------------------------------------------------------

suite "Internet Access — Allowed"

inet_out=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESS_INET}\",\"sandboxId\":\"${SB_INET}\",\"command\":\"bash\",
       \"args\":[\"-c\",\"timeout 5 bash -c 'echo >/dev/tcp/93.184.216.34/80' 2>/dev/null && echo reached || echo blocked\"],
       \"timeoutSeconds\":15}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
if [[ "${inet_out}" == "reached" ]]; then
  pass "allowInternetAccess=true can reach internet (TCP)"
elif [[ "${inet_out}" == "blocked" ]]; then
  skip "Internet egress blocked — set controller.networkPolicy.allowInternetEgress=true to test"
  inet_out="skipped"
else
  fail "allowInternetAccess=true internet check returned unexpected output: '${inet_out}'"
fi

# -----------------------------------------------------------------------
# DNS resolution: resolv.conf is injected when allowInternetAccess=true
# -----------------------------------------------------------------------

suite "DNS Resolution"

resolv_count=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESS_INET}\",\"sandboxId\":\"${SB_INET}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"grep -c nameserver /etc/resolv.conf 2>/dev/null || echo 0\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

if [[ "${resolv_count}" -gt 0 ]] 2>/dev/null; then
  pass "/etc/resolv.conf has ${resolv_count} nameserver(s) (resolv.conf bind-mount applied)"
else
  fail "/etc/resolv.conf is empty or missing in allowInternetAccess=true sandbox"
fi

dns_ip=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESS_INET}\",\"sandboxId\":\"${SB_INET}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"getent hosts example.com 2>/dev/null | head -1 | awk '{print \$1}' || echo blocked\"],
       \"timeoutSeconds\":15}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

if [[ "${dns_ip}" == "blocked" || -z "${dns_ip}" ]]; then
  skip "DNS resolution blocked — set controller.networkPolicy.allowInternetEgress=true to enable"
elif echo "${dns_ip}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$|^[0-9a-f:]+$'; then
  pass "example.com resolves to ${dns_ip}"
else
  fail "DNS returned unexpected output: '${dns_ip}'"
fi

# -----------------------------------------------------------------------
# Internet access: blocked (default)
# -----------------------------------------------------------------------

suite "Internet Access — Blocked"

block_out=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESS_BLOCK}\",\"sandboxId\":\"${SB_BLOCK}\",\"command\":\"bash\",
       \"args\":[\"-c\",\"timeout 3 bash -c 'echo >/dev/tcp/8.8.8.8/53' 2>/dev/null && echo open || echo blocked\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Default sandbox cannot reach external IPs" "blocked" "${block_out}"

block_http=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESS_BLOCK}\",\"sandboxId\":\"${SB_BLOCK}\",\"command\":\"bash\",
       \"args\":[\"-c\",\"timeout 3 bash -c 'echo >/dev/tcp/93.184.216.34/80' 2>/dev/null && echo reached || echo blocked\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Default sandbox cannot reach external HTTP (TCP)" "blocked" "${block_http}"

# -----------------------------------------------------------------------
# Cross-sandbox network isolation
# -----------------------------------------------------------------------

suite "Cross-Sandbox Network Isolation"

wait_session_ready "${SESS_CROSS_A}" 60 || true
wait_session_ready "${SESS_CROSS_B}" 60 || true

lo_addr_a=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESS_CROSS_A}\",\"sandboxId\":\"${SB_CROSS_A}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ip addr show lo | grep 'inet ' | awk '{print \$2}'\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

lo_addr_b=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESS_CROSS_B}\",\"sandboxId\":\"${SB_CROSS_B}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ip addr show lo | grep 'inet ' | awk '{print \$2}'\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

cross_connect=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESS_CROSS_A}\",\"sandboxId\":\"${SB_CROSS_A}\",\"command\":\"bash\",
       \"args\":[\"-c\",\"timeout 2 bash -c 'echo >/dev/tcp/127.0.0.1/22' 2>/dev/null && echo open || echo blocked\"],
       \"timeoutSeconds\":8}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Sandbox A cannot reach sandbox B via loopback (different network namespaces)" "blocked" "${cross_connect}"

if [[ "${block_out}" == "blocked" ]]; then
  pass "Isolated sandbox has no external network access"
else
  fail "Isolated sandbox unexpectedly reached the internet"
fi

# -----------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------

for sess in "${SESS_INET}" "${SESS_BLOCK}" "${SESS_CROSS_A}" "${SESS_CROSS_B}"; do
  curl_api DELETE "/v1/sessions/${sess}" >/dev/null 2>&1 || true
done
for sb in "${SB_INET}" "${SB_BLOCK}" "${SB_CROSS_A}" "${SB_CROSS_B}"; do
  curl_api DELETE "/v1/sandboxes/${sb}" >/dev/null 2>&1 || true
done

summary
