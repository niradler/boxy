#!/usr/bin/env bash
# Run all e2e validation suites. Requires a running boxy cluster.
#
# Required env vars:
#   NAMESPACE          - K8s namespace (default: boxy)
#   RELEASE_NAME       - Helm release name (default: boxy)
#   BASE_URL           - Router base URL (e.g. http://127.0.0.1:18080)
#   ROUTER_TOKEN       - Auth token for the router
#
# Optional:
#   KUBECTL_CTX        - kubectl context (default: current context)
#   CHART_DIR          - Path to Helm chart (default: deploy/helm/boxy)
#   SKIP_SUITES        - Comma-separated list of suites to skip (infra,security,config,api,operator)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

: "${NAMESPACE:=boxy}"
: "${RELEASE_NAME:=boxy}"
: "${SKIP_SUITES:=}"

export NAMESPACE RELEASE_NAME

_BOLD='\033[1m'
_CYAN='\033[0;36m'
_RED='\033[0;31m'
_GREEN='\033[0;32m'
_RESET='\033[0m'

SUITES_RUN=0
FAILED_SUITES=()

run_suite() {
  local name="$1" script="$2"
  if echo ",${SKIP_SUITES}," | grep -qi ",${name},"; then
    echo -e "\n${_CYAN}>>> Skipping ${name}${_RESET}"
    return 0
  fi

  echo -e "\n${_BOLD}${_CYAN}>>> Running: ${name}${_RESET}"
  echo "================================================================"

  local rc=0
  bash "${script}" || rc=$?

  SUITES_RUN=$((SUITES_RUN + 1))
  if [[ ${rc} -ne 0 ]]; then
    FAILED_SUITES+=("${name}")
  fi
  return 0
}

echo -e "${_BOLD}Boxy E2E Validation${_RESET}"
echo "================================================================"
echo "  Namespace:    ${NAMESPACE}"
echo "  Release:      ${RELEASE_NAME}"
echo "  Base URL:     ${BASE_URL:-<not set>}"
echo "  Context:      ${KUBECTL_CTX:-<current>}"
echo "================================================================"

run_suite "infra"    "${SCRIPT_DIR}/validate-infra.sh"
run_suite "security" "${SCRIPT_DIR}/validate-security.sh"
run_suite "config"   "${SCRIPT_DIR}/validate-config.sh"

if [[ -n "${BASE_URL:-}" && -n "${ROUTER_TOKEN:-}" ]]; then
  run_suite "api"              "${SCRIPT_DIR}/validate-api.sh"
  run_suite "isolation"        "${SCRIPT_DIR}/validate-isolation.sh"
  run_suite "operator"         "${SCRIPT_DIR}/validate-operator.sh"
  run_suite "network-internet" "${SCRIPT_DIR}/validate-network-internet.sh"
  run_suite "controllerpool"   "${SCRIPT_DIR}/validate-controllerpool.sh"
  run_suite "allowed-binaries-dev" "${SCRIPT_DIR}/validate-allowed-binaries-dev.sh"
else
  echo -e "\n${_CYAN}>>> Skipping api/isolation/operator/network/controllerpool/binaries suites (BASE_URL/ROUTER_TOKEN not set)${_RESET}"
fi

echo ""
echo "================================================================"
echo -e "${_BOLD}Final Summary${_RESET}"
echo "================================================================"
echo "  Suites run: ${SUITES_RUN}"

if [[ ${#FAILED_SUITES[@]} -gt 0 ]]; then
  echo -e "  ${_RED}Failed suites: ${FAILED_SUITES[*]}${_RESET}"
  echo ""
  echo -e "${_RED}${_BOLD}E2E FAILED${_RESET}"
  exit 1
else
  echo -e "  ${_GREEN}All suites passed${_RESET}"
  echo ""
  echo -e "${_GREEN}${_BOLD}E2E PASSED${_RESET}"
fi
