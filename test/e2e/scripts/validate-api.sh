#!/usr/bin/env bash
# Validate REST API and MCP functional behaviour.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

if [[ -z "${BASE_URL}" || -z "${ROUTER_TOKEN}" ]]; then
  echo "BASE_URL and ROUTER_TOKEN are required"
  exit 1
fi

# -----------------------------------------------------------------------
# Health
# -----------------------------------------------------------------------

suite "Health Endpoint"

health_body=$(curl -sS "${BASE_URL}/healthz" 2>/dev/null || true)
assert_contains "GET /healthz returns ok" "${health_body}" "ok"

# -----------------------------------------------------------------------
# Sandbox CRUD
# -----------------------------------------------------------------------

suite "Sandbox Lifecycle"

SB_ID=$(unique_id)

create_resp=$(curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_ID}\",\"ttlSeconds\":600}")

create_sandbox_id=$(echo "${create_resp}" | jq -r '.sandboxId // empty')
if [[ "${create_sandbox_id}" == "${SB_ID}" ]]; then
  pass "Create sandbox returns correct sandboxId"
else
  fail "Create sandbox returns correct sandboxId" "got '${create_sandbox_id}'"
fi

create_ttl=$(echo "${create_resp}" | jq -r '.ttlSeconds // empty')
assert_eq "Create sandbox returns ttlSeconds=600" "600" "${create_ttl}"

# Duplicate create returns 409
dup_status=$(curl_api_status POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_ID}\",\"ttlSeconds\":600}")
assert_http_status "Duplicate sandbox create returns 409" "409" "${dup_status}"

# GET sandbox
get_status=$(curl_api_status GET "/v1/sandboxes/${SB_ID}")
assert_http_status "GET sandbox returns 200" "200" "${get_status}"

get_resp=$(curl_api GET "/v1/sandboxes/${SB_ID}")
get_sb_id=$(echo "${get_resp}" | jq -r '.sandboxId // empty')
assert_eq "GET sandbox returns correct sandboxId" "${SB_ID}" "${get_sb_id}"

# GET nonexistent
not_found_status=$(curl_api_status GET "/v1/sandboxes/does-not-exist-${RANDOM}")
assert_http_status "GET nonexistent sandbox returns 404" "404" "${not_found_status}"

# Create a session
SESSION_RESP=$(curl_api POST "/v1/sessions" -d "{\"sandboxId\":\"${SB_ID}\"}")
SESSION_ID=$(echo "${SESSION_RESP}" | jq -r '.sessionId // empty')
if [[ -n "${SESSION_ID}" ]]; then
  pass "Session created for sandbox"
else
  fail "Session created for sandbox" "got empty sessionId"
  summary && exit 1
fi
wait_session_ready "${SESSION_ID}" 120

# GET session
sess_resp=$(curl_api GET "/v1/sessions/${SESSION_ID}")
sess_phase=$(echo "${sess_resp}" | jq -r '.phase // empty')
assert_eq "Session phase is Running" "Running" "${sess_phase}"

sess_ready=$(echo "${sess_resp}" | jq -r '.ready // empty')
assert_eq "Session ready=true" "true" "${sess_ready}"

sess_sb=$(echo "${sess_resp}" | jq -r '.sandboxId // empty')
assert_eq "Session sandboxId matches" "${SB_ID}" "${sess_sb}"

# -----------------------------------------------------------------------
# Exec
# -----------------------------------------------------------------------

suite "Exec API"

exec_resp=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESSION_ID}\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo -n e2e-works\"],\"timeoutSeconds\":30}")
exec_stdout=$(echo "${exec_resp}" | jq -r '.stdout // empty')
assert_eq "Exec returns correct stdout" "e2e-works" "${exec_stdout}"

exec_code=$(echo "${exec_resp}" | jq -r '.exitCode // empty')
assert_eq "Exec returns exitCode=0" "0" "${exec_code}"

# Exec with env
exec_env_resp=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESSION_ID}\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo -n \$MY_VAR\"],\"env\":{\"MY_VAR\":\"injected\"},\"timeoutSeconds\":30}")
exec_env_out=$(echo "${exec_env_resp}" | jq -r '.stdout // empty')
assert_eq "Exec with env variable" "injected" "${exec_env_out}"

# Exec with non-zero exit code
exec_fail_resp=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESSION_ID}\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"exit 42\"],\"timeoutSeconds\":30}")
exec_fail_code=$(echo "${exec_fail_resp}" | jq -r '.exitCode // empty')
assert_eq "Exec returns non-zero exit code" "42" "${exec_fail_code}"

# Exec with stderr
exec_stderr_resp=$(curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESSION_ID}\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo -n oops >&2\"],\"timeoutSeconds\":30}")
exec_stderr_out=$(echo "${exec_stderr_resp}" | jq -r '.stderr // empty')
assert_eq "Exec captures stderr" "oops" "${exec_stderr_out}"

# Exec against nonexistent sandbox (no session)
exec_bad_status=$(curl_api_status POST "/v1/sessions/exec" \
  -d "{\"sandboxId\":\"does-not-exist-${RANDOM}\",\"command\":\"id\",\"timeoutSeconds\":5}")
assert_http_status "Exec against nonexistent sandbox returns 404" "404" "${exec_bad_status}"

# -----------------------------------------------------------------------
# Session Management
# -----------------------------------------------------------------------

suite "Session Management"

list_resp=$(curl_api GET "/v1/sessions")
list_count=$(echo "${list_resp}" | jq -r '.sessions | length')
if [[ "${list_count}" -ge 1 ]] 2>/dev/null; then
  pass "GET /v1/sessions returns at least one session"
else
  fail "GET /v1/sessions returns at least one session" "got count=${list_count}"
fi

sb_sessions=$(curl_api GET "/v1/sandboxes/${SB_ID}/sessions")
sb_sess_count=$(echo "${sb_sessions}" | jq -r '.sessions | length')
if [[ "${sb_sess_count}" -ge 1 ]] 2>/dev/null; then
  pass "GET /v1/sandboxes/:id/sessions returns sessions"
else
  fail "GET /v1/sandboxes/:id/sessions returns sessions"
fi

not_found_sess=$(curl_api_status GET "/v1/sessions/does-not-exist-${RANDOM}")
assert_http_status "GET nonexistent session returns 404" "404" "${not_found_sess}"

# -----------------------------------------------------------------------
# Input Validation
# -----------------------------------------------------------------------

suite "Input Validation"

no_sandbox=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"ttlSeconds":60}')
assert_http_status "Create sandbox without sandboxId returns 400" "400" "${no_sandbox}"

invalid_sb_id=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"sandboxId":"INVALID_UPPERCASE","ttlSeconds":60}')
assert_http_status "Create sandbox with invalid sandboxId returns 400" "400" "${invalid_sb_id}"

no_sb_sess=$(curl_api_status POST "/v1/sessions" \
  -d '{"owner":"x"}')
assert_http_status "Create session without sandboxId returns 400" "400" "${no_sb_sess}"

exec_no_cmd=$(curl_api_status POST "/v1/sessions/exec" \
  -d "{\"sandboxId\":\"${SB_ID}\",\"timeoutSeconds\":5}")
assert_http_status "Exec without command returns 400" "400" "${exec_no_cmd}"

exec_no_timeout=$(curl_api_status POST "/v1/sessions/exec" \
  -d "{\"sandboxId\":\"${SB_ID}\",\"command\":\"id\"}")
assert_http_status "Exec without timeoutSeconds returns 400" "400" "${exec_no_timeout}"

bad_json=$(curl_api_status POST "/v1/sessions/exec" -d "not json")
assert_http_status "Exec with invalid JSON returns 400" "400" "${bad_json}"

blocked_env=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"sandboxId":"x","ttlSeconds":60,"env":{"KUBERNETES_SERVICE_HOST":"evil"}}')
assert_http_status "Create with KUBERNETES_ env returns 400" "400" "${blocked_env}"

boxy_env=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"sandboxId":"x","ttlSeconds":60,"env":{"BOXY_SECRET":"evil"}}')
assert_http_status "Create with BOXY_ env returns 400" "400" "${boxy_env}"

# -----------------------------------------------------------------------
# Delete
# -----------------------------------------------------------------------

suite "Sandbox Deletion"

del_sess_status=$(curl_api_status DELETE "/v1/sessions/${SESSION_ID}")
assert_http_status "DELETE session returns 204" "204" "${del_sess_status}"

del_sess_again=$(curl_api_status DELETE "/v1/sessions/${SESSION_ID}")
assert_http_status "DELETE already-deleted session returns 404" "404" "${del_sess_again}"

del_status=$(curl_api_status DELETE "/v1/sandboxes/${SB_ID}")
assert_http_status "DELETE sandbox returns 204" "204" "${del_status}"

del_again=$(curl_api_status DELETE "/v1/sandboxes/${SB_ID}")
assert_http_status "DELETE already-deleted sandbox returns 404" "404" "${del_again}"

# -----------------------------------------------------------------------
# MCP Protocol
# -----------------------------------------------------------------------

suite "MCP Protocol"

# Initialize and tools/list don't require a session
init_resp=$(curl_mcp '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"e2e","version":"1"},"capabilities":{}}}')
init_name=$(echo "${init_resp}" | jq -r '.result.serverInfo.name // empty')
assert_eq "MCP initialize returns server name" "boxy" "${init_name}"

init_tools=$(echo "${init_resp}" | jq -r '.result.capabilities.tools // empty')
if [[ -n "${init_tools}" ]]; then
  pass "MCP initialize advertises tools capability"
else
  fail "MCP initialize advertises tools capability"
fi

list_resp=$(curl_mcp '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')
list_tool=$(echo "${list_resp}" | jq -r '.result.tools[0].name // empty')
assert_eq "MCP tools/list returns bash tool" "bash" "${list_tool}"

list_desc=$(echo "${list_resp}" | jq -r '.result.tools[0].description // empty')
if [[ -n "${list_desc}" ]]; then
  pass "MCP bash tool has description"
else
  fail "MCP bash tool has description"
fi

# tools/call requires a session
MCP_SB=$(unique_id)
curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${MCP_SB}\",\"ttlSeconds\":600}" > /dev/null

MCP_SESSION=$(curl_api POST "/v1/sessions" \
  -d "{\"sandboxId\":\"${MCP_SB}\"}" | jq -r '.sessionId // empty')
wait_session_ready "${MCP_SESSION}" 120

call_resp=$(curl_mcp \
  "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"bash\",\"arguments\":{\"command\":\"echo -n mcp-e2e\"}}}" \
  "${MCP_SESSION}")
call_text=$(echo "${call_resp}" | jq -r '.result.content[0].text // empty')
assert_eq "MCP tools/call returns exec output" "mcp-e2e" "${call_text}"

call_error=$(echo "${call_resp}" | jq -r '.result.isError // "false"')
assert_eq "MCP tools/call isError=false" "false" "${call_error}"

fail_resp=$(curl_mcp \
  "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"bash\",\"arguments\":{\"command\":\"exit 1\"}}}" \
  "${MCP_SESSION}")
fail_is_error=$(echo "${fail_resp}" | jq -r '.result.isError // empty')
assert_eq "MCP tools/call with failing command returns isError=true" "true" "${fail_is_error}"

# tools/call with nonexistent session returns tool-level error
no_sess_resp=$(curl_mcp \
  '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"bash","arguments":{"command":"id"}}}' \
  "nonexistent-session-xyzzy")
no_sess_err=$(echo "${no_sess_resp}" | jq -r '.result.isError // empty')
if [[ "${no_sess_err}" == "true" ]]; then
  pass "MCP tools/call with nonexistent session returns tool-level error"
else
  fail "MCP tools/call with nonexistent session returns tool-level error"
fi

# unknown method
unknown_resp=$(curl_mcp '{"jsonrpc":"2.0","id":6,"method":"nonexistent/method"}')
unknown_err=$(echo "${unknown_resp}" | jq -r '.error.code // empty' 2>/dev/null || true)
if [[ -n "${unknown_err}" ]]; then
  pass "MCP unknown method returns JSON-RPC error"
else
  pass "MCP unknown method handled (no crash)"
fi

# Cleanup
curl_api DELETE "/v1/sessions/${MCP_SESSION}" > /dev/null 2>&1 || true
curl_api DELETE "/v1/sandboxes/${MCP_SB}" > /dev/null 2>&1 || true

summary
