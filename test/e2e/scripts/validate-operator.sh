#!/usr/bin/env bash
# Validate operator/CRD lifecycle: state machine, finalizer, TTL, bin-packing.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

if [[ -z "${BASE_URL}" || -z "${ROUTER_TOKEN}" ]]; then
  echo "BASE_URL and ROUTER_TOKEN are required"
  exit 1
fi

# -----------------------------------------------------------------------
# CRD Lifecycle
# -----------------------------------------------------------------------

suite "Sandbox CR Lifecycle"

SB_ID=$(unique_id)

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"op-sess\",\"sandboxId\":\"${SB_ID}\",\"owner\":\"e2e-op\",\"ttlSeconds\":600}" > /dev/null

sb_cr=$(kctl get sandbox "${SB_ID}" -o json 2>/dev/null || echo "{}")
if echo "${sb_cr}" | jq -e '.metadata.name' > /dev/null 2>&1; then
  pass "Sandbox CR created in cluster"
else
  fail "Sandbox CR created in cluster"
fi

# Spec fields
cr_sandbox_id=$(echo "${sb_cr}" | jq -r '.spec.sandboxId // empty')
assert_eq "CR spec.sandboxId matches" "${SB_ID}" "${cr_sandbox_id}"

cr_session=$(echo "${sb_cr}" | jq -r '.spec.sessionId // empty')
assert_eq "CR spec.sessionId set" "op-sess" "${cr_session}"

cr_owner=$(echo "${sb_cr}" | jq -r '.spec.owner // empty')
assert_eq "CR spec.owner set" "e2e-op" "${cr_owner}"

cr_ttl=$(echo "${sb_cr}" | jq -r '.spec.ttlSeconds // empty')
assert_eq "CR spec.ttlSeconds set" "600" "${cr_ttl}"

# Labels
cr_label=$(echo "${sb_cr}" | jq -r ".metadata.labels[\"boxy.dev/sandbox-id\"] // empty")
assert_eq "CR has sandbox-id label" "${SB_ID}" "${cr_label}"

cr_session_label=$(echo "${sb_cr}" | jq -r ".metadata.labels[\"boxy.dev/session-id\"] // empty")
assert_eq "CR has session-id label" "op-sess" "${cr_session_label}"

cr_owner_label=$(echo "${sb_cr}" | jq -r ".metadata.labels[\"boxy.dev/owner\"] // empty")
assert_eq "CR has owner label" "e2e-op" "${cr_owner_label}"

# Status fields
cr_phase=$(echo "${sb_cr}" | jq -r '.status.phase // empty')
assert_eq "CR status.phase=Running" "Running" "${cr_phase}"

cr_ctrl_pod=$(echo "${sb_cr}" | jq -r '.status.controllerPod // empty')
if [[ -n "${cr_ctrl_pod}" ]]; then
  pass "CR status.controllerPod assigned (${cr_ctrl_pod})"
else
  fail "CR status.controllerPod assigned"
fi

cr_ctrl_addr=$(echo "${sb_cr}" | jq -r '.status.controllerAddress // empty')
if [[ -n "${cr_ctrl_addr}" ]]; then
  pass "CR status.controllerAddress set"
else
  fail "CR status.controllerAddress set"
fi

cr_port=$(echo "${sb_cr}" | jq -r '.status.port // empty')
if [[ -n "${cr_port}" && "${cr_port}" != "0" ]]; then
  pass "CR status.port set (${cr_port})"
else
  fail "CR status.port set"
fi

cr_created=$(echo "${sb_cr}" | jq -r '.status.createdAt // empty')
if [[ -n "${cr_created}" ]]; then
  pass "CR status.createdAt timestamp set"
else
  fail "CR status.createdAt timestamp set"
fi

cr_expires=$(echo "${sb_cr}" | jq -r '.status.expiresAt // empty')
if [[ -n "${cr_expires}" ]]; then
  pass "CR status.expiresAt timestamp set (TTL-based)"
else
  fail "CR status.expiresAt timestamp set (TTL-based)"
fi

# -----------------------------------------------------------------------
# Finalizer
# -----------------------------------------------------------------------

suite "Finalizer"

cr_finalizers=$(echo "${sb_cr}" | jq -r '.metadata.finalizers[]? // empty')
assert_contains "Sandbox has cleanup finalizer" "${cr_finalizers}" "boxy.dev/sandbox-cleanup"

# -----------------------------------------------------------------------
# Sliding Window TTL (lastExecAt)
# -----------------------------------------------------------------------

suite "Sliding Window TTL"

exec_before=$(kctl get sandbox "${SB_ID}" -o jsonpath='{.status.lastExecAt}' 2>/dev/null || true)

curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"op-sess\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo touch\"],\"timeoutSeconds\":10}" > /dev/null 2>&1

sleep 3

exec_after=$(kctl get sandbox "${SB_ID}" -o jsonpath='{.status.lastExecAt}' 2>/dev/null || true)
if [[ -n "${exec_after}" ]]; then
  pass "lastExecAt updated after exec"
  if [[ "${exec_after}" != "${exec_before}" ]]; then
    pass "lastExecAt changed from previous value"
  else
    if [[ -z "${exec_before}" ]]; then
      pass "lastExecAt was empty, now set"
    else
      fail "lastExecAt changed from previous value"
    fi
  fi
else
  fail "lastExecAt updated after exec"
fi

# -----------------------------------------------------------------------
# Controller Assignment (bin-packing)
# -----------------------------------------------------------------------

suite "Controller Assignment"

ctrl_pods=$(kctl get pods -l app=boxy-controller --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
if [[ -n "${ctrl_pods}" ]]; then
  pass "Controller pods are running"
else
  fail "Controller pods are running"
fi

sandbox_ctrl=$(kctl get sandbox "${SB_ID}" -o jsonpath='{.status.controllerPod}' 2>/dev/null || true)
if echo "${ctrl_pods}" | grep -qF "${sandbox_ctrl}"; then
  pass "Sandbox assigned to a running controller pod (${sandbox_ctrl})"
else
  fail "Sandbox assigned to a running controller pod" "pod '${sandbox_ctrl}' not in running pods"
fi

# Create a second sandbox and verify it gets assigned
SB_ID2=$(unique_id)
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"op-sess\",\"sandboxId\":\"${SB_ID2}\",\"owner\":\"e2e-op\",\"ttlSeconds\":600}" > /dev/null 2>&1

sb2_ctrl=$(kctl get sandbox "${SB_ID2}" -o jsonpath='{.status.controllerPod}' 2>/dev/null || true)
if [[ -n "${sb2_ctrl}" ]]; then
  pass "Second sandbox assigned to controller (${sb2_ctrl})"
else
  fail "Second sandbox assigned to controller"
fi

# -----------------------------------------------------------------------
# Deletion via API triggers CR cleanup
# -----------------------------------------------------------------------

suite "Sandbox Deletion Lifecycle"

del_status=$(curl_api_status DELETE "/v1/sandboxes/${SB_ID}")
assert_http_status "DELETE sandbox via API returns 204" "204" "${del_status}"

sleep 5

sb_deleted=$(kctl get sandbox "${SB_ID}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)
if [[ -z "${sb_deleted}" ]]; then
  pass "Sandbox CR deleted from cluster after API delete"
else
  sb_del_phase=$(kctl get sandbox "${SB_ID}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "${sb_del_phase}" == "Terminated" || "${sb_del_phase}" == "Deleting" ]]; then
    pass "Sandbox CR transitioning to ${sb_del_phase} (finalizer processing)"
  else
    fail "Sandbox CR deleted from cluster" "still exists with phase=${sb_del_phase}"
  fi
fi

# -----------------------------------------------------------------------
# Short TTL sandbox expires
# -----------------------------------------------------------------------

suite "TTL Expiry"

SHORT_SB=$(unique_id)
curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"ttl-sess\",\"sandboxId\":\"${SHORT_SB}\",\"owner\":\"e2e-ttl\",\"ttlSeconds\":10}" > /dev/null 2>&1

short_phase=$(kctl get sandbox "${SHORT_SB}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
if [[ "${short_phase}" == "Running" ]]; then
  pass "Short-TTL sandbox starts Running"
else
  fail "Short-TTL sandbox starts Running" "phase=${short_phase}"
fi

echo "  Waiting up to 60s for TTL expiry..."
deadline=$((SECONDS + 60))
expired=false
while [[ ${SECONDS} -lt ${deadline} ]]; do
  p=$(kctl get sandbox "${SHORT_SB}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "${p}" == "Terminated" || "${p}" == "Deleting" || -z "${p}" ]]; then
    expired=true
    break
  fi
  sleep 3
done

if [[ "${expired}" == "true" ]]; then
  pass "Short-TTL sandbox expired (phase=${p:-deleted})"
else
  current_phase=$(kctl get sandbox "${SHORT_SB}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  fail "Short-TTL sandbox expired" "still phase=${current_phase} after 60s"
fi

# -----------------------------------------------------------------------
# kubectl get sbx (shortname)
# -----------------------------------------------------------------------

suite "CRD Usability"

sbx_out=$(kctl get sbx 2>&1 || true)
if ! echo "${sbx_out}" | grep -qi "error"; then
  pass "kubectl get sbx works (shortName)"
else
  fail "kubectl get sbx works (shortName)"
fi

# -----------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------

curl_api DELETE "/v1/sandboxes/${SB_ID2}" > /dev/null 2>&1 || true
curl_api DELETE "/v1/sandboxes/${SHORT_SB}" > /dev/null 2>&1 || true

summary
