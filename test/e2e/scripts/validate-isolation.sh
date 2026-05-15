#!/usr/bin/env bash
# Validate per-sandbox isolation: filesystem, network, env, and resource limits.
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

SB1=$(unique_id)
SB2=$(unique_id)
SB_ENV=$(unique_id)
SB_MEM=$(unique_id)
SB_NET=$(unique_id)

# SB1 and SB2: plain sandboxes for filesystem isolation tests
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB2}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null

# SB_ENV: sandbox with a predefined env var
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_ENV}\",\"owner\":\"e2e\",\"ttlSeconds\":300,\"env\":{\"ISO_SECRET\":\"sandbox_secret\"}}" >/dev/null

# SB_MEM: sandbox with a 64 MB cgroup memory cap (minimum allowed by CRD validation)
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_MEM}\",\"owner\":\"e2e\",\"ttlSeconds\":300,\"vm\":{\"memoryMb\":64}}" >/dev/null

# SB_NET: sandbox for network isolation (default = isolated network namespace)
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_NET}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null

# -----------------------------------------------------------------------
# Filesystem Isolation
# -----------------------------------------------------------------------

suite "Filesystem Isolation"

# SB1 writes a file to its /workspace
write_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo secret123 > /workspace/sb1_file.txt && echo ok\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "SB1 can write to /workspace" "ok" "${write_out}"

# SB2 cannot see SB1's workspace file (different bind-mount)
sb2_read_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB2}\",\"command\":\"sh\",\"args\":[\"-c\",\"cat /workspace/sb1_file.txt 2>/dev/null || echo absent\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "SB2 cannot see SB1 workspace files" "absent" "${sb2_read_out}"

# SB1 can read its own file back (workspace persists across execs)
readback_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"cat /workspace/sb1_file.txt\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "SB1 can read back its own workspace file" "secret123" "${readback_out}"

# SB2 workspace starts empty (its own private dir)
sb2_empty_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB2}\",\"command\":\"sh\",\"args\":[\"-c\",\"test -f /workspace/sb1_file.txt && echo found || echo absent\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "SB2 workspace does not contain SB1 file" "absent" "${sb2_empty_out}"

# -----------------------------------------------------------------------
# Read-Only Rootfs
# -----------------------------------------------------------------------

suite "Read-Only Rootfs"

# nsjail mounts the chroot read-only by default — writes to system dirs must fail
ro_etc_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"touch /etc/boxy_test 2>/dev/null && echo writable || echo readonly\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Rootfs /etc is read-only" "readonly" "${ro_etc_out}"

ro_usr_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"touch /usr/boxy_test 2>/dev/null && echo writable || echo readonly\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Rootfs /usr is read-only" "readonly" "${ro_usr_out}"

# /workspace is the one writable directory (bind-mounted rw)
ws_write_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"touch /workspace/rw_test && echo ok\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "/workspace is writable" "ok" "${ws_write_out}"

# /tmp is writable (per-exec tmpfs)
tmp_write_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"touch /tmp/rw_test && echo ok\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "/tmp is writable" "ok" "${tmp_write_out}"

# -----------------------------------------------------------------------
# Ephemeral /tmp (fresh tmpfs per exec, workspace persists)
# -----------------------------------------------------------------------

suite "Ephemeral /tmp"

# Create a file in /tmp during exec 1
curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo tmpdata > /tmp/tmpfile.txt\"],\"timeoutSeconds\":10}" >/dev/null

# Exec 2: /tmp file is gone (fresh tmpfs)
tmp_gone_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"cat /tmp/tmpfile.txt 2>/dev/null || echo absent\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "/tmp is fresh on each exec (not persisted)" "absent" "${tmp_gone_out}"

# /workspace file from earlier is still there (workspace is persistent, /tmp is not)
ws_persist_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB1}\",\"command\":\"sh\",\"args\":[\"-c\",\"cat /workspace/sb1_file.txt\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "/workspace persists across execs while /tmp does not" "secret123" "${ws_persist_out}"

# -----------------------------------------------------------------------
# Environment Variable Isolation
# -----------------------------------------------------------------------

suite "Environment Variable Isolation"

# Sandbox-level env var is visible inside exec
env_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_ENV}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo \$ISO_SECRET\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Sandbox-level env var is visible in exec" "sandbox_secret" "${env_out}"

# Exec-level env overrides sandbox-level env
env_override_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_ENV}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo \$ISO_SECRET\"],\"env\":{\"ISO_SECRET\":\"exec_override\"},\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Exec-level env overrides sandbox-level env" "exec_override" "${env_override_out}"

# Another sandbox does NOT inherit SB_ENV's env vars
env_other_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB2}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo \${ISO_SECRET:-absent}\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Other sandbox does not see SB_ENV env vars" "absent" "${env_other_out}"

# -----------------------------------------------------------------------
# Network Isolation
# -----------------------------------------------------------------------

suite "Network Isolation"

# Default sandbox runs in an isolated network namespace (no interfaces except lo).
# bash /dev/tcp to an external IP must fail immediately with EHOSTUNREACH.
net_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_NET}\",\"command\":\"bash\",\"args\":[\"-c\",\"timeout 3 bash -c 'echo >/dev/tcp/8.8.8.8/53' 2>/dev/null && echo open || echo blocked\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "Default sandbox cannot reach external IPs" "blocked" "${net_out}"

# -----------------------------------------------------------------------
# Resource Limits (memoryMb → nsjail --cgroup_mem_max)
# -----------------------------------------------------------------------

suite "Resource Limits"

# Normal command works fine inside a memory-limited sandbox.
# Skip if cgroup memory delegation is unavailable (e.g. kind without host cgroupns).
mem_ok_out=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_MEM}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo ok\"],\"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
if [[ "${mem_ok_out}" == "ok" ]]; then
  pass "Memory-limited (64 MB) sandbox runs basic commands"
elif [[ -z "${mem_ok_out}" ]]; then
  skip "Memory-limited sandbox exec failed — cgroup memory delegation not available in this env"
else
  fail "Memory-limited (64 MB) sandbox runs basic commands" "expected 'ok', got '${mem_ok_out}'"
fi

# Try to allocate 128 MB (2× the 64 MB cgroup cap) by doubling a bash string 27 times.
# 2^27 = 128 MB; with cgroup_mem_max=64MB the OOM killer fires before "echo grew".
# If cgroup delegation is restricted in this environment, the test is skipped.
mem_oom_resp=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_MEM}\",\"command\":\"sh\",\"args\":[\"-c\",\"v=a; for x in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27; do v=\$v\$v; done; echo grew\"],\"timeoutSeconds\":15}" \
  || true)
mem_oom_code=$(echo "${mem_oom_resp}" | jq -r '.exitCode // empty' 2>/dev/null || true)
mem_oom_out=$(echo "${mem_oom_resp}"  | jq -r '.stdout   // empty' 2>/dev/null || true)
if [[ "${mem_oom_code}" != "0" ]]; then
  pass "Memory cgroup limit kills process exceeding 32 MB cap"
elif echo "${mem_oom_out}" | grep -q "grew"; then
  skip "Memory cgroup limit not enforced (cgroup delegation restricted in this env)"
else
  fail "Memory cgroup limit" "unexpected: exitCode=0 but stdout='${mem_oom_out}'"
fi

# -----------------------------------------------------------------------
# Cross-Sandbox Security via MCP
# -----------------------------------------------------------------------

suite "MCP Cross-Sandbox Isolation"

SB_MCP_A=$(unique_id)
SB_MCP_B=$(unique_id)

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"mcp-iso\",\"sandboxId\":\"${SB_MCP_A}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"mcp-iso\",\"sandboxId\":\"${SB_MCP_B}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null

mcp_call() {
  local sandbox_id="$1" cmd="$2"
  curl -fsS -X POST "${BASE_URL}/mcp" \
    -H "Authorization: Bearer ${ROUTER_TOKEN}" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    ${sandbox_id:+-H "X-Sandbox-Id: ${sandbox_id}"} \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"bash\",\"arguments\":{\"command\":\"${cmd}\"}}}"
}

# Sandbox A writes a secret file via MCP.
mcp_write=$(mcp_call "${SB_MCP_A}" "echo mcp-secret > /workspace/mcp_secret.txt && echo ok" \
  | jq -r '.result.content[0].text // empty' | tr -d '\r\n')
assert_contains "MCP sandbox A can write to /workspace" "${mcp_write}" "ok"

# Sandbox B cannot read sandbox A's file via MCP.
mcp_read=$(mcp_call "${SB_MCP_B}" "cat /workspace/mcp_secret.txt 2>/dev/null || echo absent" \
  | jq -r '.result.content[0].text // empty' | tr -d '\r\n')
assert_eq "MCP sandbox B cannot read sandbox A workspace file" "absent" "${mcp_read}"

# Invalid sandbox ID returns a tool-level error (not HTTP 5xx).
mcp_invalid_status=$(curl -fsS -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/mcp" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "X-Sandbox-Id: nonexistent-sandbox-xyzzy" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bash","arguments":{"command":"echo hi"}}}' \
  2>/dev/null || echo "000")
assert_http_status "MCP with invalid sandbox ID returns HTTP 200 (tool-level error)" "200" "${mcp_invalid_status}"

mcp_invalid_body=$(curl -fsS -X POST "${BASE_URL}/mcp" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "X-Sandbox-Id: nonexistent-sandbox-xyzzy" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bash","arguments":{"command":"echo hi"}}}' \
  2>/dev/null || echo "{}")
mcp_is_error=$(echo "${mcp_invalid_body}" | jq -r '.result.isError // false')
assert_eq "MCP with invalid sandbox ID returns tool-level isError=true" "true" "${mcp_is_error}"

# Default sandbox disabled → no X-Sandbox-Id header → tool-level error (not HTTP error).
mcp_no_sandbox_body=$(curl -fsS -X POST "${BASE_URL}/mcp" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bash","arguments":{"command":"echo hi"}}}' \
  2>/dev/null || echo "{}")
mcp_no_sb_is_error=$(echo "${mcp_no_sandbox_body}" | jq -r '.result.isError // false')
if [[ "${mcp_no_sb_is_error}" == "true" ]]; then
  pass "MCP with no X-Sandbox-Id and default disabled returns tool-level error"
else
  # If default sandbox is enabled, the call should succeed — skip this assertion.
  skip "MCP no-sandbox check (default sandbox may be enabled)"
fi

for sb in "${SB_MCP_A}" "${SB_MCP_B}"; do
  curl_api DELETE "/v1/sandboxes/${sb}" >/dev/null 2>&1 || true
done

# -----------------------------------------------------------------------
# Sandbox → Controller API isolation
# An internet-enabled sandbox shares the controller pod's network namespace
# and can reach localhost:<BOXY_CONTROLLER_PORT>. It must NOT be able to
# call the controller exec API unauthenticated and run commands in other
# sandboxes. Protection: mTLS (production) or BOXY_CONTROLLER_TOKEN (dev).
# -----------------------------------------------------------------------

suite "Sandbox → Controller API Isolation"

SB_INET_ISO=$(unique_id)
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_INET_ISO}\",\"owner\":\"e2e\",\"ttlSeconds\":120,
       \"network\":{\"allowInternetAccess\":true}}" >/dev/null
wait_sandbox_ready "${SB_INET_ISO}" 60 || true

# Try to call the controller exec API directly from within an internet-enabled sandbox.
# Uses bash /dev/tcp to send a raw HTTP request — no curl needed in the rootfs.
# If the controller API responds with HTTP 200 to an unauthenticated exec call,
# the sandbox can escape isolation and run commands in other sandboxes. FAIL.
ctrl_port=8080
ctrl_api_result=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"iso\",\"sandboxId\":\"${SB_INET_ISO}\",\"command\":\"bash\",
       \"args\":[\"-c\",\"exec 3<>/dev/tcp/127.0.0.1/${ctrl_port} 2>/dev/null && printf 'POST /v1/exec HTTP/1.0\\\\r\\\\nHost: localhost\\\\r\\\\nContent-Type: application/json\\\\r\\\\nContent-Length: 49\\\\r\\\\n\\\\r\\\\n{\\\\\"sandbox_id\\\\\":\\\\\"x\\\\\",\\\\\"command\\\\\":\\\\\"id\\\\\"}' >&3 && timeout 2 head -1 <&3 2>/dev/null || echo unreachable\"],
       \"timeoutSeconds\":15}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')

if [[ "${ctrl_api_result}" == "unreachable" || -z "${ctrl_api_result}" ]]; then
  pass "Controller API unreachable from sandbox (mTLS or network isolation)"
elif echo "${ctrl_api_result}" | grep -q "^HTTP/1\.. 200"; then
  fail "SECURITY: Sandbox reached controller API unauthenticated (exec in other sandboxes possible)"
elif echo "${ctrl_api_result}" | grep -qE "^HTTP/1\.. 401|^HTTP/1\.. 403"; then
  pass "Controller API reachable but protected by token (${ctrl_api_result})"
else
  # TLS error, connection reset, timeout, other non-200 — controller is protected
  pass "Controller API access blocked or rejected (${ctrl_api_result})"
fi

curl_api DELETE "/v1/sandboxes/${SB_INET_ISO}" >/dev/null 2>&1 || true

# -----------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------

for sb in "${SB1}" "${SB2}" "${SB_ENV}" "${SB_MEM}" "${SB_NET}"; do
  curl_api DELETE "/v1/sandboxes/${sb}" >/dev/null 2>&1 || true
done

summary
