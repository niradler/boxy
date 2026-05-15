#!/usr/bin/env bash
# Validate ControllerPool CRD status accounting.
#
# Covers:
#   - ControllerPool CR exists and has correct spec.
#   - activeSandboxCount increments when sandboxes are created.
#   - activeSandboxCount decrements when sandboxes are deleted.
#   - Each Sandbox CR has status.controllerPool set.
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
  -d "{\"sessionId\":\"cp\",\"sandboxId\":\"${SB_A}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"cp\",\"sandboxId\":\"${SB_B}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null

wait_sandbox_ready "${SB_A}" 120
wait_sandbox_ready "${SB_B}" 120

# Allow reconciler to process the Sandbox events
sleep 5
after_create=$(kctl get controllerpool "${POOL_NAME}" -o jsonpath='{.status.activeSandboxCount}' 2>/dev/null || echo "0")
expected_create=$((baseline + 2))
assert_eq "activeSandboxCount increments after creating 2 sandboxes" "${expected_create}" "${after_create}"

# Delete one sandbox
curl_api DELETE "/v1/sandboxes/${SB_A}" >/dev/null 2>&1 || true

# Wait for the sandbox to reach Terminated and reconciler to update
deadline=$((SECONDS + 60))
while [[ ${SECONDS} -lt ${deadline} ]]; do
  phase=$(kctl get sandbox -l "boxy.dev/sandbox-id=${SB_A}" -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "")
  if [[ "${phase}" == "Terminated" || -z "${phase}" ]]; then
    break
  fi
  sleep 2
done
sleep 5  # let reconciler react

after_delete=$(kctl get controllerpool "${POOL_NAME}" -o jsonpath='{.status.activeSandboxCount}' 2>/dev/null || echo "0")
expected_delete=$((baseline + 1))
assert_eq "activeSandboxCount decrements after deleting 1 sandbox" "${expected_delete}" "${after_delete}"

# -----------------------------------------------------------------------
# Sandbox CR has controllerPool set
# -----------------------------------------------------------------------

suite "Sandbox CR controllerPool Reference"

sb_b_pool=$(kctl get sandbox -l "boxy.dev/sandbox-id=${SB_B}" \
  -o jsonpath='{.items[0].status.controllerPool}' 2>/dev/null || echo "")
assert_eq "Sandbox CR status.controllerPool is set" "${POOL_NAME}" "${sb_b_pool}"

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

curl_api DELETE "/v1/sandboxes/${SB_B}" >/dev/null 2>&1 || true

summary
