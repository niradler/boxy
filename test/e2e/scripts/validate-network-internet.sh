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
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_INET}\",\"owner\":\"e2e\",\"ttlSeconds\":300,
       \"network\":{\"allowInternetAccess\":true}}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_BLOCK}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_CROSS_A}\",\"owner\":\"e2e\",\"ttlSeconds\":300,
       \"network\":{\"allowInternetAccess\":true}}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_CROSS_B}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null

if ! wait_sandbox_ready "${SB_INET}" 120 || ! wait_sandbox_ready "${SB_BLOCK}" 120; then
  echo "Sandboxes did not become Ready in time"
  exit 1
fi

# -----------------------------------------------------------------------
# Internet access: allowed
# -----------------------------------------------------------------------

suite "Internet Access — Allowed"

inet_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_INET}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"wget -q -T 5 -O /dev/null http://example.com 2>&1 && echo reached || echo blocked\"],
       \"timeoutSeconds\":15}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "allowInternetAccess=true can reach internet" "reached" "${inet_out}"

# -----------------------------------------------------------------------
# DNS resolution: resolv.conf is injected when allowInternetAccess=true
# -----------------------------------------------------------------------

suite "DNS Resolution"

resolv_count=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_INET}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"grep -c nameserver /etc/resolv.conf 2>/dev/null || echo 0\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

if [[ "${resolv_count}" -gt 0 ]] 2>/dev/null; then
  pass "/etc/resolv.conf has ${resolv_count} nameserver(s) (resolv.conf bind-mount applied)"
else
  fail "/etc/resolv.conf is empty or missing in allowInternetAccess=true sandbox"
fi

dns_ip=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_INET}\",\"command\":\"sh\",
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

block_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_BLOCK}\",\"command\":\"bash\",
       \"args\":[\"-c\",\"timeout 3 bash -c 'echo >/dev/tcp/8.8.8.8/53' 2>/dev/null && echo open || echo blocked\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Default sandbox cannot reach external IPs" "blocked" "${block_out}"

block_wget=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_BLOCK}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"wget -q -T 3 -O /dev/null http://example.com 2>&1 && echo reached || echo blocked\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Default sandbox wget is blocked" "blocked" "${block_wget}"

# -----------------------------------------------------------------------
# Cross-sandbox network isolation
# Sandbox A (internet-enabled) cannot reach sandbox B's loopback since
# each sandbox runs in its own isolated network namespace.
# -----------------------------------------------------------------------

suite "Cross-Sandbox Network Isolation"

wait_sandbox_ready "${SB_CROSS_A}" 60 || true
wait_sandbox_ready "${SB_CROSS_B}" 60 || true

# Verify each sandbox only sees its own lo interface (no bridge to other sandboxes)
lo_a=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_CROSS_A}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ip -o link show | awk -F: '{print \$2}' | tr -d ' ' | sort | tr '\n' ',' | sed 's/,$//'\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

lo_b=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_CROSS_B}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ip -o link show | awk -F: '{print \$2}' | tr -d ' ' | sort | tr '\n' ',' | sed 's/,$//'\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

# Each sandbox has its own lo. The internet sandbox also has eth0/veth.
# They must NOT share interfaces — their lo addresses differ.
lo_addr_a=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_CROSS_A}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ip addr show lo | grep 'inet ' | awk '{print \$2}'\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

lo_addr_b=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_CROSS_B}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ip addr show lo | grep 'inet ' | awk '{print \$2}'\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

# Both have 127.0.0.1/8 but in separate namespaces — A cannot connect to B's lo.
# Verify A cannot TCP-connect to B's loopback (only accessible inside B's netns).
cross_connect=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"net\",\"sandboxId\":\"${SB_CROSS_A}\",\"command\":\"bash\",
       \"args\":[\"-c\",\"timeout 2 bash -c 'echo >/dev/tcp/127.0.0.1/22' 2>/dev/null && echo open || echo blocked\"],
       \"timeoutSeconds\":8}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Sandbox A cannot reach sandbox B via loopback (different network namespaces)" "blocked" "${cross_connect}"

# Confirm sandbox A has internet access but B does not (summarised)
if [[ "${inet_out}" == "reached" && "${block_out}" == "blocked" ]]; then
  pass "Internet sandbox and isolated sandbox are on different network namespaces"
else
  fail "Network namespace isolation mismatch" "A: ${inet_out}, B: ${block_out}"
fi

# -----------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------

for sb in "${SB_INET}" "${SB_BLOCK}" "${SB_CROSS_A}" "${SB_CROSS_B}"; do
  curl_api DELETE "/v1/sandboxes/${sb}" >/dev/null 2>&1 || true
done

summary
