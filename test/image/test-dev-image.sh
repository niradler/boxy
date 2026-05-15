#!/usr/bin/env bash
# Validate the dev controller image structure without a running cluster.
#
# Runs a one-shot container and asserts:
#   + /usr/local/bin/jq  exists and is executable  (bind-mount source)
#   + /usr/local/bin/yq  exists and is executable  (bind-mount source)
#   - /rootfs/ubuntu-24.04/usr/bin/jq  does NOT exist  (rootfs is clean)
#   - /rootfs/ubuntu-24.04/usr/bin/yq  does NOT exist  (rootfs is clean)
#
# These assertions encode the two-path rule: dev binaries live ONLY on the
# controller's /usr/local/bin, never inside the sandbox rootfs. A rootfs
# that contains the binary would bypass the allowedBinaries whitelist.
#
# Usage:
#   ./test/image/test-dev-image.sh [IMAGE]
#   IMAGE defaults to boxydev/boxy-controller-dev:e2e
set -euo pipefail

IMAGE="${1:-boxydev/boxy-controller-dev:e2e}"

_PASS=0
_FAIL=0
_RED='\033[0;31m'
_GREEN='\033[0;32m'
_RESET='\033[0m'

pass() { _PASS=$((_PASS+1)); echo -e "  ${_GREEN}PASS${_RESET} $1"; }
fail() { _FAIL=$((_FAIL+1)); echo -e "  ${_RED}FAIL${_RESET} $1"; }

run() {
  docker run --rm --entrypoint sh "${IMAGE}" -c "$1" 2>/dev/null
}

check_true() {
  local label="$1" cmd="$2"
  if run "${cmd}" >/dev/null 2>&1; then pass "${label}"; else fail "${label}"; fi
}

check_false() {
  local label="$1" cmd="$2"
  if ! run "${cmd}" >/dev/null 2>&1; then pass "${label}"; else fail "${label}"; fi
}

echo "Testing image: ${IMAGE}"
echo "================================================================"

echo ""
echo "=== Controller bind-mount sources (must be present + executable) ==="
check_true "/usr/local/bin/jq is executable"           "test -x /usr/local/bin/jq"
check_true "/usr/local/bin/yq is executable"           "test -x /usr/local/bin/yq"
check_true "/usr/local/bin/boxy-controller is present" "test -x /usr/local/bin/boxy-controller"

echo ""
echo "=== Sandbox rootfs hygiene (must NOT contain dev tool executables) ==="
check_false "jq absent from rootfs /usr/bin"          "test -e /rootfs/ubuntu-24.04/usr/bin/jq"
check_false "yq absent from rootfs /usr/bin"          "test -e /rootfs/ubuntu-24.04/usr/bin/yq"
check_false "curl absent from rootfs /usr/bin"        "test -e /rootfs/ubuntu-24.04/usr/bin/curl"
check_false "git absent from rootfs /usr/bin"         "test -e /rootfs/ubuntu-24.04/usr/bin/git"
check_false "python3 absent from rootfs /usr/bin"     "test -e /rootfs/ubuntu-24.04/usr/bin/python3"
check_false "node absent from rootfs /usr/bin"        "test -e /rootfs/ubuntu-24.04/usr/bin/node"
check_false "jq absent from rootfs /usr/local/bin"    "test -e /rootfs/ubuntu-24.04/usr/local/bin/jq"
check_false "yq absent from rootfs /usr/local/bin"    "test -e /rootfs/ubuntu-24.04/usr/local/bin/yq"

echo ""
echo "================================================================"
echo "  Passed: ${_PASS}  Failed: ${_FAIL}"
if [[ ${_FAIL} -gt 0 ]]; then
  echo -e "${_RED}FAILED${_RESET}"
  exit 1
fi
echo -e "${_GREEN}ALL PASSED${_RESET}"
