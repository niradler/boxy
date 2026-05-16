#!/usr/bin/env bash
# Validate ControllerPool CRD status accounting.
#
# Covers:
#   - ControllerPool CR exists and has correct spec.
#   - activeSandboxCount increments when sessions are created.
#   - activeSandboxCount decrements when sessions are deleted.
#   - Each Session CR has status.controllerPool set.
#   - readyReplicas reflects the StatefulSet.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

if [[ -z "${BASE_URL}" || -z "${ROUTER_TOKEN}" ]]; then
  echo "BASE_URL and ROUTER_TOKEN are required"
  exit 1
fi

: "${RELEASE_NAME:=boxy}"

POOL_NAME="${RELEASE_NAME}-ctrl"

# -----------------------------------------------------------------------
# ControllerPool existence and spec
# -----------------------------------------------------------------------

suite "ControllerPool Existence"

pool_json=$(kctl get controllerpool "${POOL_NAME}" -o json 2>/dev/null || echo "{}")

pool_kind=$(echo "${pool_json}" | jq -r '.kind // empty')
assert_eq "ControllerPool CR exists" "ControllerPool" "${pool_kind}"

pool_max=$(echo "${pool_json}" | jq -r '.spec.maxSandboxes // empty')
assert_gt "ControllerPool spec.maxSandboxes > 0" "${pool_max}" "0"

pool_max_r=$(echo "${pool_json}" | jq -r '.spec.maxReplicas // empty')
assert_gt "ControllerPool spec.maxReplicas > 0" "${pool_max_r}" "0"

pool_min_r=$(echo "${pool_json}" | jq -r '.spec.minReplicas // empty')
assert_gt "ControllerPool spec.minReplicas > 0" "${pool_min_r}" "0"

# -----------------------------------------------------------------------
# readyReplicas reflects the StatefulSet
# -----------------------------------------------------------------------

suite "ControllerPool readyReplicas"

sts_ready=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
# Give the reconciler a moment to sync if needed
sleep 3
pool_ready=$(kctl get controllerpool "${POOL_NAME}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "")
assert_eq "ControllerPool readyReplicas matches StatefulSet readyReplicas" "${sts_ready}" "${pool_ready}"

# -----------------------------------------------------------------------
# activeSandboxCount accounting
# -----------------------------------------------------------------------

suite "ControllerPool activeSandboxCount"

# Baseline count before test sandboxes
baseline=$(kctl get controllerpool "${POOL_NAME}" -o jsonpath='{.status.activeSandboxCount}' 2>/dev/null || echo "0")

SB_A=$(unique_id)
SB_B=$(unique_id)

curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_A}\",\"ttlSeconds\":300}" >/dev/null
curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_B}\",\"ttlSeconds\":300}" >/dev/null

SESS_A=$(curl_api POST "/v1/sessions" -d "{\"sandboxId\":\"${SB_A}\",\"owner\":\"e2e\"}" | jq -r '.sessionId // empty')
SESS_B=$(curl_api POST "/v1/sessions" -d "{\"sandboxId\":\"${SB_B}\",\"owner\":\"e2e\"}" | jq -r '.sessionId // empty')

if [[ -z "${SESS_A}" || -z "${SESS_B}" ]]; then
  fail "Created controllerpool test sessions" "SESS_A='${SESS_A}' SESS_B='${SESS_B}'"
else
  pass "Created controllerpool test sessions"
fi

wait_session_ready "${SESS_A}" 120
wait_session_ready "${SESS_B}" 120

# Allow reconciler to process the Session events
sleep 5
after_create=$(kctl get controllerpool "${POOL_NAME}" -o jsonpath='{.status.activeSandboxCount}' 2>/dev/null || echo "0")
expected_create=$((baseline + 2))
assert_eq "activeSandboxCount increments after creating 2 sessions" "${expected_create}" "${after_create}"

# Delete one session

curl_api DELETE "/v1/sessions/${SESS_A}" >/dev/null 2>&1 || true

# Wait for the session to terminate/disappear and reconciler to update
deadline=$((SECONDS + 60))
while [[ ${SECONDS} -lt ${deadline} ]]; do
  phase=$(kctl get session "${SESS_A}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  if [[ "${phase}" == "Terminated" || -z "${phase}" ]]; then
    break
  fi
  sleep 2
done
sleep 5  # let reconciler react

after_delete=$(kctl get controllerpool "${POOL_NAME}" -o jsonpath='{.status.activeSandboxCount}' 2>/dev/null || echo "0")
expected_delete=$((baseline + 1))
assert_eq "activeSandboxCount decrements after deleting 1 session" "${expected_delete}" "${after_delete}"

# -----------------------------------------------------------------------
# Session CR has controllerPool set
# -----------------------------------------------------------------------

suite "Session CR controllerPool Reference"

sess_b_pool=$(kctl get session "${SESS_B}" \
  -o jsonpath='{.status.controllerPool}' 2>/dev/null || echo "")
assert_eq "Session CR status.controllerPool is set" "${POOL_NAME}" "${sess_b_pool}"

# -----------------------------------------------------------------------
# ControllerPool Ready condition
# -----------------------------------------------------------------------

suite "ControllerPool Ready Condition"

ready_status=$(kctl get controllerpool "${POOL_NAME}" \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
assert_eq "ControllerPool Ready condition is True" "True" "${ready_status}"

# -----------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------

curl_api DELETE "/v1/sessions/${SESS_B}" >/dev/null 2>&1 || true
curl_api DELETE "/v1/sandboxes/${SB_A}" >/dev/null 2>&1 || true
curl_api DELETE "/v1/sandboxes/${SB_B}" >/dev/null 2>&1 || true

summary
