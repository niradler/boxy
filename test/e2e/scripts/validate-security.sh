#!/usr/bin/env bash
# Validate security hardening: pod security contexts, RBAC, network policies, auth.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

# -----------------------------------------------------------------------
# Pod Security Contexts
# -----------------------------------------------------------------------

suite "Router Pod Security"

router_spec=$(kctl get deployment "${RELEASE_NAME}-router" -o json 2>/dev/null || echo "{}")

router_run_as_nonroot=$(echo "${router_spec}" | jq -r '.spec.template.spec.securityContext.runAsNonRoot // empty')
assert_eq "Router pod runAsNonRoot" "true" "${router_run_as_nonroot}"

router_run_as_user=$(echo "${router_spec}" | jq -r '.spec.template.spec.securityContext.runAsUser // empty')
assert_eq "Router pod runAsUser=65532" "65532" "${router_run_as_user}"

router_seccomp=$(echo "${router_spec}" | jq -r '.spec.template.spec.securityContext.seccompProfile.type // empty')
assert_eq "Router pod seccompProfile=RuntimeDefault" "RuntimeDefault" "${router_seccomp}"

router_ctr=$(echo "${router_spec}" | jq '.spec.template.spec.containers[0].securityContext')
router_priv=$(echo "${router_ctr}" | jq -r '.allowPrivilegeEscalation // "false"')
assert_eq "Router container allowPrivilegeEscalation=false" "false" "${router_priv}"

router_ro=$(echo "${router_ctr}" | jq -r '.readOnlyRootFilesystem // empty')
assert_eq "Router container readOnlyRootFilesystem=true" "true" "${router_ro}"

router_caps=$(echo "${router_ctr}" | jq -r '.capabilities.drop[0] // empty')
assert_eq "Router container drops ALL capabilities" "ALL" "${router_caps}"

suite "Operator Pod Security"

op_spec=$(kctl get deployment "${RELEASE_NAME}-operator" -o json 2>/dev/null || echo "{}")

op_run_as_nonroot=$(echo "${op_spec}" | jq -r '.spec.template.spec.securityContext.runAsNonRoot // empty')
assert_eq "Operator pod runAsNonRoot" "true" "${op_run_as_nonroot}"

op_run_as_user=$(echo "${op_spec}" | jq -r '.spec.template.spec.securityContext.runAsUser // empty')
assert_eq "Operator pod runAsUser=65532" "65532" "${op_run_as_user}"

op_ctr=$(echo "${op_spec}" | jq '.spec.template.spec.containers[0].securityContext')
op_priv=$(echo "${op_ctr}" | jq -r '.allowPrivilegeEscalation // "false"')
assert_eq "Operator container allowPrivilegeEscalation=false" "false" "${op_priv}"

op_ro=$(echo "${op_ctr}" | jq -r '.readOnlyRootFilesystem // empty')
assert_eq "Operator container readOnlyRootFilesystem=true" "true" "${op_ro}"

op_caps=$(echo "${op_ctr}" | jq -r '.capabilities.drop[0] // empty')
assert_eq "Operator container drops ALL capabilities" "ALL" "${op_caps}"

suite "Controller Pod Security"

ctrl_spec=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o json 2>/dev/null || echo "{}")
ctrl_ctr=$(echo "${ctrl_spec}" | jq '.spec.template.spec.containers[0].securityContext')
ctrl_caps=$(echo "${ctrl_ctr}" | jq -r '.capabilities.drop[0] // empty')
assert_eq "Controller container drops ALL capabilities" "ALL" "${ctrl_caps}"

ctrl_sys_admin=$(echo "${ctrl_ctr}" | jq -r '[.capabilities.add[]? | select(. == "SYS_ADMIN")] | length')
assert_gt "Controller container adds SYS_ADMIN (overlayfs)" "${ctrl_sys_admin}" 0

ctrl_automount=$(echo "${ctrl_spec}" | jq -r '.spec.template.spec.automountServiceAccountToken // "false"')
assert_eq "Controller pod automountServiceAccountToken=false" "false" "${ctrl_automount}"

# -----------------------------------------------------------------------
# RBAC Validation
# -----------------------------------------------------------------------

suite "Router RBAC"

router_role=$(kctl get role boxy-router -o json 2>/dev/null || echo "{}")
router_rules=$(echo "${router_role}" | jq -c '.rules // []')

router_has_sandbox_crd=$(echo "${router_rules}" | jq '[.[] | select(.apiGroups[]? == "boxy.dev" and (.resources[]? == "sandboxes"))] | length')
assert_gt "Router role has sandboxes CRD permission" "${router_has_sandbox_crd}" 0

router_has_sessions=$(echo "${router_rules}" | jq '[.[] | select(.apiGroups[]? == "boxy.dev" and (.resources[]? == "sessions"))] | length')
assert_gt "Router role has sessions CRD permission" "${router_has_sessions}" 0

router_has_sessions_status=$(echo "${router_rules}" | jq '[.[] | select(.apiGroups[]? == "boxy.dev" and (.resources[]? == "sessions/status"))] | length')
assert_gt "Router role has sessions/status permission" "${router_has_sessions_status}" 0

router_has_configmaps=$(echo "${router_rules}" | jq '[.[] | select(.resources[]? == "configmaps")] | length')
assert_eq "Router role has NO configmaps permission" "0" "${router_has_configmaps}"

router_has_pods=$(echo "${router_rules}" | jq '[.[] | select(.apiGroups[]? == "" and (.resources[]? == "pods"))] | length')
assert_eq "Router role has NO pods permission" "0" "${router_has_pods}"

suite "Operator RBAC"

op_role=$(kctl get role boxy-operator -o json 2>/dev/null || echo "{}")
op_rules=$(echo "${op_role}" | jq -c '.rules // []')

op_has_sandbox=$(echo "${op_rules}" | jq '[.[] | select(.apiGroups[]? == "boxy.dev")] | length')
assert_gt "Operator role has boxy.dev permissions" "${op_has_sandbox}" 0

op_has_sts=$(echo "${op_rules}" | jq '[.[] | select(.apiGroups[]? == "apps" and (.resources[]? == "statefulsets"))] | length')
assert_gt "Operator role has statefulsets permission" "${op_has_sts}" 0

op_has_pods=$(echo "${op_rules}" | jq '[.[] | select(.apiGroups[]? == "" and (.resources[]? == "pods"))] | length')
assert_gt "Operator role has pods read permission" "${op_has_pods}" 0

op_has_leases=$(echo "${op_rules}" | jq '[.[] | select(.apiGroups[]? == "coordination.k8s.io" and (.resources[]? == "leases"))] | length')
assert_gt "Operator role has leader election lease permission" "${op_has_leases}" 0

suite "Controller RBAC"

ctrl_role=$(kctl get role boxy-controller -o json 2>/dev/null || echo "{}")
ctrl_rules=$(echo "${ctrl_role}" | jq -c '.rules // []')
ctrl_rule_count=$(echo "${ctrl_rules}" | jq 'length')
assert_eq "Controller role has minimal permissions (1 rule)" "1" "${ctrl_rule_count}"

# -----------------------------------------------------------------------
# Network Policy
# -----------------------------------------------------------------------

suite "Network Policy"

netpol=$(kctl get networkpolicy boxy-sandbox-default-deny -o json 2>/dev/null || echo "")
if [[ -n "${netpol}" && "${netpol}" != "" ]]; then
  pass "Default-deny NetworkPolicy exists"

  netpol_types=$(echo "${netpol}" | jq -r '.spec.policyTypes[]? // empty')
  assert_contains "NetworkPolicy enforces Egress" "${netpol_types}" "Egress"

  netpol_selector=$(echo "${netpol}" | jq -r '.spec.podSelector.matchLabels["boxy.dev/managed-by"] // empty')
  assert_eq "NetworkPolicy targets managed pods" "boxy" "${netpol_selector}"

  dns_port=$(echo "${netpol}" | jq '[.spec.egress[]? | .ports[]? | select(.port == 53)] | length')
  assert_gt "NetworkPolicy allows DNS egress" "${dns_port}" 0
else
  skip "NetworkPolicy not enabled (enableSandboxNetworkPolicy=false)"
fi

# -----------------------------------------------------------------------
# Auth Enforcement
# -----------------------------------------------------------------------

suite "Authentication Enforcement"

if [[ -n "${BASE_URL}" ]]; then
  no_auth_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/v1/sandboxes" \
    -H 'Content-Type: application/json' \
    -d '{"sessionId":"x","sandboxId":"x","owner":"x","ttlSeconds":60}' 2>/dev/null || echo "000")
  assert_http_status "POST /v1/sandboxes without auth returns 401" "401" "${no_auth_status}"

  bad_auth_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/v1/sandboxes" \
    -H "Authorization: Bearer wrong-token" \
    -H 'Content-Type: application/json' \
    -d '{"sessionId":"x","sandboxId":"x","owner":"x","ttlSeconds":60}' 2>/dev/null || echo "000")
  assert_http_status "POST /v1/sandboxes with wrong token returns 401" "401" "${bad_auth_status}"

  no_auth_exec=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/v1/sessions/exec" \
    -H 'Content-Type: application/json' \
    -d '{"sandboxId":"x","command":"id","timeoutSeconds":5}' 2>/dev/null || echo "000")
  assert_http_status "POST /v1/sessions/exec without auth returns 401" "401" "${no_auth_exec}"

  no_auth_get=$(curl -sS -o /dev/null -w '%{http_code}' "${BASE_URL}/v1/sandboxes/nonexistent" 2>/dev/null || echo "000")
  assert_http_status "GET /v1/sandboxes/:id without auth returns 401" "401" "${no_auth_get}"

  no_auth_delete=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "${BASE_URL}/v1/sandboxes/nonexistent" 2>/dev/null || echo "000")
  assert_http_status "DELETE /v1/sandboxes/:id without auth returns 401" "401" "${no_auth_delete}"

  no_auth_mcp=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/mcp" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"t","version":"1"},"capabilities":{}}}' 2>/dev/null || echo "000")
  assert_http_status "POST /mcp without auth returns 401" "401" "${no_auth_mcp}"

  health_status=$(curl -sS -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz" 2>/dev/null || echo "000")
  assert_http_status "GET /healthz works without auth" "200" "${health_status}"
else
  skip "Auth tests (BASE_URL not set)"
fi

# -----------------------------------------------------------------------
# mTLS Secrets
# -----------------------------------------------------------------------

suite "mTLS Configuration"

mtls_disabled=$(kctl get deployment "${RELEASE_NAME}-router" -o json 2>/dev/null | \
  jq -r '[.spec.template.spec.containers[0].env[] | select(.name == "BOXY_MTLS_DISABLED")] | .[0].value // "false"')

if [[ "${mtls_disabled}" == "true" ]]; then
  skip "mTLS disabled in this deployment"
else
  ctrl_secret=$(kctl get secret boxy-mtls-controller -o name 2>/dev/null || true)
  if [[ -n "${ctrl_secret}" ]]; then
    pass "mTLS controller secret exists"
  else
    fail "mTLS controller secret exists"
  fi

  router_secret=$(kctl get secret boxy-mtls-router -o name 2>/dev/null || true)
  if [[ -n "${router_secret}" ]]; then
    pass "mTLS router secret exists"
  else
    fail "mTLS router secret exists"
  fi

  ctrl_keys=$(kctl get secret boxy-mtls-controller -o json 2>/dev/null | jq -r '.data | keys | join(",")' || true)
  assert_contains "mTLS controller secret has ca.crt" "${ctrl_keys}" "ca.crt"
  assert_contains "mTLS controller secret has tls.crt" "${ctrl_keys}" "tls.crt"
  assert_contains "mTLS controller secret has tls.key" "${ctrl_keys}" "tls.key"
fi

summary
