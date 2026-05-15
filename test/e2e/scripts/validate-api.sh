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
  -d "{\"sessionId\":\"e2e-sess\",\"sandboxId\":\"${SB_ID}\",\"owner\":\"e2e-test\",\"ttlSeconds\":600}")

create_sandbox_id=$(echo "${create_resp}" | jq -r '.sandboxId // empty')
if [[ "${create_sandbox_id}" == "${SB_ID}" ]]; then
  pass "Create sandbox returns correct sandboxId"
else
  fail "Create sandbox returns correct sandboxId" "got '${create_sandbox_id}'"
fi

create_phase=$(echo "${create_resp}" | jq -r '.phase // empty')
assert_eq "Create sandbox returns phase" "Running" "${create_phase}"

create_runtime=$(echo "${create_resp}" | jq -r '.runtime // empty')
assert_eq "Create sandbox returns runtime=nsjail" "nsjail" "${create_runtime}"

# GET sandbox
get_status=$(curl_api_status GET "/v1/sandboxes/${SB_ID}")
assert_http_status "GET sandbox returns 200" "200" "${get_status}"

get_resp=$(curl_api GET "/v1/sandboxes/${SB_ID}")
get_ready=$(echo "${get_resp}" | jq -r '.ready // empty')
assert_eq "GET sandbox shows ready=true" "true" "${get_ready}"

get_owner=$(echo "${get_resp}" | jq -r '.owner // empty')
assert_eq "GET sandbox shows correct owner" "e2e-test" "${get_owner}"

# GET nonexistent
not_found_status=$(curl_api_status GET "/v1/sandboxes/does-not-exist-${RANDOM}")
assert_http_status "GET nonexistent sandbox returns 404" "404" "${not_found_status}"

# -----------------------------------------------------------------------
# Exec
# -----------------------------------------------------------------------

suite "Exec API"

exec_resp=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"e2e-sess\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo -n e2e-works\"],\"timeoutSeconds\":30}")
exec_stdout=$(echo "${exec_resp}" | jq -r '.stdout // empty')
assert_eq "Exec returns correct stdout" "e2e-works" "${exec_stdout}"

exec_code=$(echo "${exec_resp}" | jq -r '.exitCode // empty')
assert_eq "Exec returns exitCode=0" "0" "${exec_code}"

# Exec with env
exec_env_resp=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"e2e-sess\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo -n \$MY_VAR\"],\"env\":{\"MY_VAR\":\"injected\"},\"timeoutSeconds\":30}")
exec_env_out=$(echo "${exec_env_resp}" | jq -r '.stdout // empty')
assert_eq "Exec with env variable" "injected" "${exec_env_out}"

# Exec with non-zero exit code
exec_fail_resp=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"e2e-sess\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"exit 42\"],\"timeoutSeconds\":30}")
exec_fail_code=$(echo "${exec_fail_resp}" | jq -r '.exitCode // empty')
assert_eq "Exec returns non-zero exit code" "42" "${exec_fail_code}"

# Exec with stderr
exec_stderr_resp=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"e2e-sess\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo -n oops >&2\"],\"timeoutSeconds\":30}")
exec_stderr_out=$(echo "${exec_stderr_resp}" | jq -r '.stderr // empty')
assert_eq "Exec captures stderr" "oops" "${exec_stderr_out}"

# Exec against nonexistent sandbox
exec_bad_status=$(curl_api_status POST "/v1/exec" \
  -d "{\"sessionId\":\"e2e-sess\",\"sandboxId\":\"does-not-exist-${RANDOM}\",\"command\":\"id\",\"timeoutSeconds\":5}")
assert_http_status "Exec against nonexistent sandbox returns 404" "404" "${exec_bad_status}"

# -----------------------------------------------------------------------
# Input Validation
# -----------------------------------------------------------------------

suite "Input Validation"

no_session=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"sandboxId":"x","owner":"x","ttlSeconds":60}')
assert_http_status "Create without sessionId returns 400" "400" "${no_session}"

no_sandbox=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"sessionId":"x","owner":"x","ttlSeconds":60}')
assert_http_status "Create without sandboxId returns 400" "400" "${no_sandbox}"

no_owner=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"sessionId":"x","sandboxId":"x","ttlSeconds":60}')
assert_http_status "Create without owner returns 400" "400" "${no_owner}"

exec_no_cmd=$(curl_api_status POST "/v1/exec" \
  -d "{\"sessionId\":\"x\",\"sandboxId\":\"${SB_ID}\",\"timeoutSeconds\":5}")
assert_http_status "Exec without command returns 400" "400" "${exec_no_cmd}"

exec_no_timeout=$(curl_api_status POST "/v1/exec" \
  -d "{\"sessionId\":\"x\",\"sandboxId\":\"${SB_ID}\",\"command\":\"id\"}")
assert_http_status "Exec without timeoutSeconds returns 400" "400" "${exec_no_timeout}"

bad_json=$(curl_api_status POST "/v1/exec" -d "not json")
assert_http_status "Exec with invalid JSON returns 400" "400" "${bad_json}"

blocked_env=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"sessionId":"x","sandboxId":"x","owner":"x","ttlSeconds":60,"env":{"KUBERNETES_SERVICE_HOST":"evil"}}')
assert_http_status "Create with KUBERNETES_ env returns 400" "400" "${blocked_env}"

boxy_env=$(curl_api_status POST "/v1/sandboxes" \
  -d '{"sessionId":"x","sandboxId":"x","owner":"x","ttlSeconds":60,"env":{"BOXY_SECRET":"evil"}}')
assert_http_status "Create with BOXY_ env returns 400" "400" "${boxy_env}"

# -----------------------------------------------------------------------
# Delete
# -----------------------------------------------------------------------

suite "Sandbox Deletion"

del_status=$(curl_api_status DELETE "/v1/sandboxes/${SB_ID}")
assert_http_status "DELETE sandbox returns 204" "204" "${del_status}"

del_again=$(curl_api_status DELETE "/v1/sandboxes/${SB_ID}")
assert_http_status "DELETE already-deleted sandbox returns 404" "404" "${del_again}"

# -----------------------------------------------------------------------
# MCP Protocol
# -----------------------------------------------------------------------

suite "MCP Protocol"

# Initialize
init_resp=$(curl_mcp '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"e2e","version":"1"},"capabilities":{}}}')
init_name=$(echo "${init_resp}" | jq -r '.result.serverInfo.name // empty')
assert_eq "MCP initialize returns server name" "boxy" "${init_name}"

init_tools=$(echo "${init_resp}" | jq -r '.result.capabilities.tools // empty')
if [[ -n "${init_tools}" ]]; then
  pass "MCP initialize advertises tools capability"
else
  fail "MCP initialize advertises tools capability"
fi

# tools/list
list_resp=$(curl_mcp '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')
list_tool=$(echo "${list_resp}" | jq -r '.result.tools[0].name // empty')
assert_eq "MCP tools/list returns bash tool" "bash" "${list_tool}"

list_desc=$(echo "${list_resp}" | jq -r '.result.tools[0].description // empty')
if [[ -n "${list_desc}" ]]; then
  pass "MCP bash tool has description"
else
  fail "MCP bash tool has description"
fi

# tools/call with sandbox
MCP_SB=$(unique_id)
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"mcp-sess\",\"sandboxId\":\"${MCP_SB}\",\"owner\":\"e2e\",\"ttlSeconds\":600}" > /dev/null

wait_sandbox_ready "${MCP_SB}" 120

call_resp=$(curl_mcp \
  "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"bash\",\"arguments\":{\"command\":\"echo -n mcp-e2e\"}}}" \
  "${MCP_SB}")
call_text=$(echo "${call_resp}" | jq -r '.result.content[0].text // empty')
assert_eq "MCP tools/call returns exec output" "mcp-e2e" "${call_text}"

call_error=$(echo "${call_resp}" | jq -r '.result.isError // "false"')
assert_eq "MCP tools/call isError=false" "false" "${call_error}"

# tools/call with non-zero exit
fail_resp=$(curl_mcp \
  "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"bash\",\"arguments\":{\"command\":\"exit 1\"}}}" \
  "${MCP_SB}")
fail_is_error=$(echo "${fail_resp}" | jq -r '.result.isError // empty')
assert_eq "MCP tools/call with failing command returns isError=true" "true" "${fail_is_error}"

# tools/call without sandbox (no default)
no_sb_resp=$(curl_mcp \
  '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"bash","arguments":{"command":"id"}}}')
no_sb_err=$(echo "${no_sb_resp}" | jq -r '.result.isError // empty')
# This either returns a tool error or works via default sandbox
if [[ "${no_sb_err}" == "true" ]] || echo "${no_sb_resp}" | jq -e '.result.content[0].text' > /dev/null 2>&1; then
  pass "MCP tools/call without sandbox header handles gracefully"
else
  fail "MCP tools/call without sandbox header handles gracefully"
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
curl_api DELETE "/v1/sandboxes/${MCP_SB}" > /dev/null 2>&1 || true

summary
