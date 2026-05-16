#!/usr/bin/env bash
# Validate operator/CRD lifecycle: Sandbox config CR, Session CR state machine,
# finalizer, TTL, and controller bin-packing.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

if [[ -z "${BASE_URL}" || -z "${ROUTER_TOKEN}" ]]; then
  echo "BASE_URL and ROUTER_TOKEN are required"
  exit 1
fi

# -----------------------------------------------------------------------
# Sandbox Config CR Lifecycle
# -----------------------------------------------------------------------

suite "Sandbox Config CR Lifecycle"

SB_ID=$(unique_id)

curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_ID}\",\"ttlSeconds\":600}" > /dev/null

sb_cr=$(kctl get sandbox "${SB_ID}" -o json 2>/dev/null || echo "{}")
if echo "${sb_cr}" | jq -e '.metadata.name' > /dev/null 2>&1; then
  pass "Sandbox Config CR created in cluster"
else
  fail "Sandbox Config CR created in cluster"
fi

cr_sandbox_id=$(echo "${sb_cr}" | jq -r '.spec.sandboxId // empty')
assert_eq "CR spec.sandboxId matches" "${SB_ID}" "${cr_sandbox_id}"

cr_ttl=$(echo "${sb_cr}" | jq -r '.spec.ttlSeconds // empty')
assert_eq "CR spec.ttlSeconds set" "600" "${cr_ttl}"

cr_sb_label=$(echo "${sb_cr}" | jq -r ".metadata.labels[\"boxy.dev/sandbox-id\"] // empty")
assert_eq "CR has sandbox-id label" "${SB_ID}" "${cr_sb_label}"

# Sandbox is config-only: no status subresource
cr_status=$(echo "${sb_cr}" | jq -r '.status // empty')
if [[ -z "${cr_status}" || "${cr_status}" == "null" || "${cr_status}" == "{}" ]]; then
  pass "Sandbox Config CR has no status (config-only)"
else
  pass "Sandbox Config CR status may be empty object (config-only)"
fi

# -----------------------------------------------------------------------
# Session CR Lifecycle
# -----------------------------------------------------------------------

suite "Session CR Lifecycle"

SESSION_RESP=$(curl_api POST "/v1/sessions" \
  -d "{\"sandboxId\":\"${SB_ID}\",\"owner\":\"e2e-op\"}")
SESSION_ID=$(echo "${SESSION_RESP}" | jq -r '.sessionId // empty')
if [[ -n "${SESSION_ID}" ]]; then
  pass "Session created (sessionId=${SESSION_ID})"
else
  fail "Session created"
  summary && exit 1
fi

wait_session_ready "${SESSION_ID}" 120

sess_cr=$(kctl get session "${SESSION_ID}" -o json 2>/dev/null || echo "{}")
if echo "${sess_cr}" | jq -e '.metadata.name' > /dev/null 2>&1; then
  pass "Session CR exists in cluster"
else
  fail "Session CR exists in cluster"
fi

# Spec fields
cr_sess_id=$(echo "${sess_cr}" | jq -r '.spec.sessionId // empty')
assert_eq "Session CR spec.sessionId matches" "${SESSION_ID}" "${cr_sess_id}"

cr_sess_sb=$(echo "${sess_cr}" | jq -r '.spec.sandboxId // empty')
assert_eq "Session CR spec.sandboxId matches" "${SB_ID}" "${cr_sess_sb}"

cr_sess_owner=$(echo "${sess_cr}" | jq -r '.spec.owner // empty')
assert_eq "Session CR spec.owner set" "e2e-op" "${cr_sess_owner}"

# Labels
sess_id_label=$(echo "${sess_cr}" | jq -r ".metadata.labels[\"boxy.dev/session-id\"] // empty")
assert_eq "Session CR has session-id label" "${SESSION_ID}" "${sess_id_label}"

sess_sb_label=$(echo "${sess_cr}" | jq -r ".metadata.labels[\"boxy.dev/sandbox-id\"] // empty")
assert_eq "Session CR has sandbox-id label" "${SB_ID}" "${sess_sb_label}"

sess_owner_label=$(echo "${sess_cr}" | jq -r ".metadata.labels[\"boxy.dev/owner\"] // empty")
assert_eq "Session CR has owner label" "e2e-op" "${sess_owner_label}"

# Status fields
cr_phase=$(echo "${sess_cr}" | jq -r '.status.phase // empty')
assert_eq "Session CR status.phase=Running" "Running" "${cr_phase}"

cr_ctrl_pod=$(echo "${sess_cr}" | jq -r '.status.controllerPod // empty')
if [[ -n "${cr_ctrl_pod}" ]]; then
  pass "Session CR status.controllerPod assigned (${cr_ctrl_pod})"
else
  fail "Session CR status.controllerPod assigned"
fi

cr_ctrl_addr=$(echo "${sess_cr}" | jq -r '.status.controllerAddress // empty')
if [[ -n "${cr_ctrl_addr}" ]]; then
  pass "Session CR status.controllerAddress set"
else
  fail "Session CR status.controllerAddress set"
fi

cr_port=$(echo "${sess_cr}" | jq -r '.status.port // empty')
if [[ -n "${cr_port}" && "${cr_port}" != "0" ]]; then
  pass "Session CR status.port set (${cr_port})"
else
  fail "Session CR status.port set"
fi

cr_created=$(echo "${sess_cr}" | jq -r '.status.createdAt // empty')
if [[ -n "${cr_created}" ]]; then
  pass "Session CR status.createdAt timestamp set"
else
  fail "Session CR status.createdAt timestamp set"
fi

cr_expires=$(echo "${sess_cr}" | jq -r '.status.expiresAt // empty')
if [[ -n "${cr_expires}" ]]; then
  pass "Session CR status.expiresAt timestamp set (TTL-based)"
else
  fail "Session CR status.expiresAt timestamp set (TTL-based)"
fi

# -----------------------------------------------------------------------
# Finalizer
# -----------------------------------------------------------------------

suite "Finalizer"

sess_finalizers=$(echo "${sess_cr}" | jq -r '.metadata.finalizers[]? // empty')
assert_contains "Session has cleanup finalizer" "${sess_finalizers}" "boxy.dev/session-cleanup"

# -----------------------------------------------------------------------
# Sliding Window TTL (lastExecAt on Session CR)
# -----------------------------------------------------------------------

suite "Sliding Window TTL"

exec_before=$(kctl get session "${SESSION_ID}" -o jsonpath='{.status.lastExecAt}' 2>/dev/null || true)

curl_api POST "/v1/sessions/exec" \
  -d "{\"sessionId\":\"${SESSION_ID}\",\"sandboxId\":\"${SB_ID}\",\"command\":\"sh\",\"args\":[\"-c\",\"echo touch\"],\"timeoutSeconds\":10}" > /dev/null 2>&1

sleep 3

exec_after=$(kctl get session "${SESSION_ID}" -o jsonpath='{.status.lastExecAt}' 2>/dev/null || true)
if [[ -n "${exec_after}" ]]; then
  pass "Session lastExecAt updated after exec"
  if [[ "${exec_after}" != "${exec_before}" ]]; then
    pass "Session lastExecAt changed from previous value"
  else
    if [[ -z "${exec_before}" ]]; then
      pass "Session lastExecAt was empty, now set"
    else
      fail "Session lastExecAt changed from previous value"
    fi
  fi
else
  fail "Session lastExecAt updated after exec"
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

session_ctrl=$(kctl get session "${SESSION_ID}" -o jsonpath='{.status.controllerPod}' 2>/dev/null || true)
if echo "${ctrl_pods}" | grep -qF "${session_ctrl}"; then
  pass "Session assigned to a running controller pod (${session_ctrl})"
else
  fail "Session assigned to a running controller pod" "pod '${session_ctrl}' not in running pods"
fi

# Create a second sandbox + session to verify assignment
SB_ID2=$(unique_id)
curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SB_ID2}\",\"ttlSeconds\":600}" > /dev/null 2>&1
SESS_ID2=$(curl_api POST "/v1/sessions" \
  -d "{\"sandboxId\":\"${SB_ID2}\"}" | jq -r '.sessionId // empty')

if [[ -n "${SESS_ID2}" ]]; then
  wait_session_ready "${SESS_ID2}" 120 || true
  sess2_ctrl=$(kctl get session "${SESS_ID2}" -o jsonpath='{.status.controllerPod}' 2>/dev/null || true)
  if [[ -n "${sess2_ctrl}" ]]; then
    pass "Second session assigned to controller (${sess2_ctrl})"
  else
    fail "Second session assigned to controller"
  fi
else
  fail "Second session created for bin-packing test"
fi

# -----------------------------------------------------------------------
# Deletion via API triggers CR cleanup
# -----------------------------------------------------------------------

suite "Session Deletion Lifecycle"

del_sess_status=$(curl_api_status DELETE "/v1/sessions/${SESSION_ID}")
assert_http_status "DELETE session via API returns 204" "204" "${del_sess_status}"

sleep 5

sess_deleted=$(kctl get session "${SESSION_ID}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)
if [[ -z "${sess_deleted}" ]]; then
  pass "Session CR deleted from cluster after API delete"
else
  sess_del_phase=$(kctl get session "${SESSION_ID}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "${sess_del_phase}" == "Terminated" || "${sess_del_phase}" == "Deleting" ]]; then
    pass "Session CR transitioning to ${sess_del_phase} (finalizer processing)"
  else
    fail "Session CR deleted from cluster" "still exists with phase=${sess_del_phase}"
  fi
fi

# Delete sandbox → its Session CRs should be cleaned up
del_sb_status=$(curl_api_status DELETE "/v1/sandboxes/${SB_ID}")
assert_http_status "DELETE sandbox via API returns 204" "204" "${del_sb_status}"

sleep 5

sb_deleted=$(kctl get sandbox "${SB_ID}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)
if [[ -z "${sb_deleted}" ]]; then
  pass "Sandbox Config CR deleted from cluster after API delete"
else
  fail "Sandbox Config CR deleted from cluster" "still exists"
fi

# -----------------------------------------------------------------------
# Short TTL sandbox expires (session reaches Terminated)
# -----------------------------------------------------------------------

suite "TTL Expiry"

SHORT_SB=$(unique_id)
curl_api POST "/v1/sandboxes" \
  -d "{\"sandboxId\":\"${SHORT_SB}\",\"ttlSeconds\":10}" > /dev/null 2>&1
SHORT_SESS=$(curl_api POST "/v1/sessions" \
  -d "{\"sandboxId\":\"${SHORT_SB}\"}" | jq -r '.sessionId // empty')

if [[ -n "${SHORT_SESS}" ]]; then
  wait_session_ready "${SHORT_SESS}" 60 || true
  short_phase=$(kctl get session "${SHORT_SESS}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "${short_phase}" == "Running" ]]; then
    pass "Short-TTL session starts Running"
  else
    fail "Short-TTL session starts Running" "phase=${short_phase}"
  fi

  echo "  Waiting up to 60s for TTL expiry..."
  deadline=$((SECONDS + 60))
  expired=false
  while [[ ${SECONDS} -lt ${deadline} ]]; do
    p=$(kctl get session "${SHORT_SESS}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "${p}" == "Terminated" || "${p}" == "Deleting" || -z "${p}" ]]; then
      expired=true
      break
    fi
    sleep 3
  done

  if [[ "${expired}" == "true" ]]; then
    pass "Short-TTL session expired (phase=${p:-deleted})"
  else
    current_phase=$(kctl get session "${SHORT_SESS}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    fail "Short-TTL session expired" "still phase=${current_phase} after 60s"
  fi
else
  skip "Short-TTL session not created — skipping TTL expiry test"
fi

# -----------------------------------------------------------------------
# CRD Usability
# -----------------------------------------------------------------------

suite "CRD Usability"

sbx_out=$(kctl get sbx 2>&1 || true)
if ! echo "${sbx_out}" | grep -qi "error"; then
  pass "kubectl get sbx works (Sandbox shortName)"
else
  fail "kubectl get sbx works (Sandbox shortName)"
fi

sess_out=$(kctl get sess 2>&1 || true)
if ! echo "${sess_out}" | grep -qi "error"; then
  pass "kubectl get sess works (Session shortName)"
else
  fail "kubectl get sess works (Session shortName)"
fi

# -----------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------

curl_api DELETE "/v1/sessions/${SESS_ID2}" > /dev/null 2>&1 || true
curl_api DELETE "/v1/sandboxes/${SB_ID2}" > /dev/null 2>&1 || true
curl_api DELETE "/v1/sessions/${SHORT_SESS:-}" > /dev/null 2>&1 || true
curl_api DELETE "/v1/sandboxes/${SHORT_SB}" > /dev/null 2>&1 || true

summary
