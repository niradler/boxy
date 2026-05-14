#!/usr/bin/env bash
# Shared e2e test library. Source this from every test script.
set -euo pipefail

: "${NAMESPACE:=boxy}"
: "${RELEASE_NAME:=boxy}"
: "${ROUTER_TOKEN:=}"
: "${BASE_URL:=}"
: "${KUBECTL_CTX:=}"

_CTX_FLAG=""
if [[ -n "${KUBECTL_CTX}" ]]; then
  _CTX_FLAG="--context=${KUBECTL_CTX}"
fi

# --- Colours -----------------------------------------------------------
_RED='\033[0;31m'
_GREEN='\033[0;32m'
_YELLOW='\033[0;33m'
_CYAN='\033[0;36m'
_BOLD='\033[1m'
_RESET='\033[0m'

# --- Counters -----------------------------------------------------------
_PASS=0
_FAIL=0
_SKIP=0
_SUITE=""

suite() {
  _SUITE="$1"
  echo ""
  echo -e "${_BOLD}${_CYAN}=== SUITE: ${_SUITE} ===${_RESET}"
}

pass() {
  _PASS=$((_PASS + 1))
  echo -e "  ${_GREEN}PASS${_RESET} $1"
}

fail() {
  _FAIL=$((_FAIL + 1))
  echo -e "  ${_RED}FAIL${_RESET} $1"
  if [[ $# -gt 1 ]]; then
    echo -e "       ${_YELLOW}$2${_RESET}"
  fi
}

skip() {
  _SKIP=$((_SKIP + 1))
  echo -e "  ${_YELLOW}SKIP${_RESET} $1"
}

summary() {
  echo ""
  echo -e "${_BOLD}--- Results ---${_RESET}"
  echo -e "  ${_GREEN}Passed:${_RESET}  ${_PASS}"
  echo -e "  ${_RED}Failed:${_RESET}  ${_FAIL}"
  echo -e "  ${_YELLOW}Skipped:${_RESET} ${_SKIP}"
  if [[ ${_FAIL} -gt 0 ]]; then
    echo -e "${_RED}${_BOLD}FAILED${_RESET}"
    return 1
  fi
  echo -e "${_GREEN}${_BOLD}ALL PASSED${_RESET}"
}

# --- Assertions ---------------------------------------------------------

assert_eq() {
  local label="$1" expected="$2" actual="$3"
  if [[ "${expected}" == "${actual}" ]]; then
    pass "${label}"
  else
    fail "${label}" "expected '${expected}', got '${actual}'"
  fi
}

assert_contains() {
  local label="$1" haystack="$2" needle="$3"
  if echo "${haystack}" | grep -qF "${needle}"; then
    pass "${label}"
  else
    fail "${label}" "output does not contain '${needle}'"
  fi
}

assert_not_contains() {
  local label="$1" haystack="$2" needle="$3"
  if ! echo "${haystack}" | grep -qF "${needle}"; then
    pass "${label}"
  else
    fail "${label}" "output unexpectedly contains '${needle}'"
  fi
}

assert_matches() {
  local label="$1" haystack="$2" pattern="$3"
  if echo "${haystack}" | grep -qE "${pattern}"; then
    pass "${label}"
  else
    fail "${label}" "output does not match pattern '${pattern}'"
  fi
}

assert_gt() {
  local label="$1" actual="$2" threshold="$3"
  if [[ "${actual}" -gt "${threshold}" ]]; then
    pass "${label}"
  else
    fail "${label}" "expected >${threshold}, got ${actual}"
  fi
}

assert_http_status() {
  local label="$1" expected="$2" actual="$3"
  if [[ "${expected}" == "${actual}" ]]; then
    pass "${label}"
  else
    fail "${label}" "expected HTTP ${expected}, got ${actual}"
  fi
}

# --- Helpers ------------------------------------------------------------

kctl() {
  kubectl ${_CTX_FLAG} -n "${NAMESPACE}" "$@"
}

curl_api() {
  local method="$1" path="$2"
  shift 2
  curl -sS -X "${method}" "${BASE_URL}${path}" \
    -H "Authorization: Bearer ${ROUTER_TOKEN}" \
    -H 'Content-Type: application/json' \
    "$@"
}

curl_api_status() {
  local method="$1" path="$2"
  shift 2
  curl -sS -o /dev/null -w '%{http_code}' -X "${method}" "${BASE_URL}${path}" \
    -H "Authorization: Bearer ${ROUTER_TOKEN}" \
    -H 'Content-Type: application/json' \
    "$@"
}

curl_mcp() {
  local body="$1"
  local sandbox_header="${2:-}"
  local extra_args=()
  if [[ -n "${sandbox_header}" ]]; then
    extra_args+=(-H "X-Sandbox-Id: ${sandbox_header}")
  fi
  curl -sS -X POST "${BASE_URL}/mcp" \
    -H "Authorization: Bearer ${ROUTER_TOKEN}" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    "${extra_args[@]}" \
    -d "${body}"
}

wait_sandbox_ready() {
  local sandbox_id="$1"
  local timeout="${2:-120}"
  local deadline=$((SECONDS + timeout))
  while [[ ${SECONDS} -lt ${deadline} ]]; do
    local phase
    phase=$(curl_api GET "/v1/sandboxes/${sandbox_id}" 2>/dev/null | jq -r '.phase // empty' 2>/dev/null || true)
    if [[ "${phase}" == "Running" ]]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

unique_id() {
  echo "e2e-$(date +%s)-${RANDOM}"
}
