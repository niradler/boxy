#!/usr/bin/env bash
# Validate Helm configuration: env vars, values propagation, resource limits.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

# -----------------------------------------------------------------------
# Router environment variables
# -----------------------------------------------------------------------

suite "Router Environment Variables"

router_env=$(kctl get deployment "${RELEASE_NAME}-router" -o json 2>/dev/null | \
  jq -c '[.spec.template.spec.containers[0].env[].name]' || echo "[]")

for expected in BOXY_LISTEN_ADDR BOXY_ROUTER_TOKEN BOXY_SANDBOX_NAMESPACE BOXY_CONTROLLER_PORT \
  BOXY_MTLS_DISABLED BOXY_TLS_CA_PATH BOXY_TLS_CLIENT_CERT_PATH BOXY_TLS_CLIENT_KEY_PATH \
  BOXY_DEFAULT_SANDBOX_ENABLED; do
  if echo "${router_env}" | jq -e "index(\"${expected}\")" > /dev/null 2>&1; then
    pass "Router has env ${expected}"
  else
    fail "Router has env ${expected}"
  fi
done

for removed in BOXY_REAPER_INTERVAL_SECONDS BOXY_CONTROLLER_IMAGE BOXY_CONTROLLER_TTL_SECONDS \
  BOXY_MAX_SANDBOXES_PER_CONTROLLER BOXY_CONTROLLER_SERVICE_ACCOUNT BOXY_VM_LOG_LEVEL \
  BOXY_VM_METRICS_INTERVAL_MS BOXY_VM_PULL_POLICY BOXY_LIBKRUNFW_PATH \
  BOXY_MTLS_CONTROLLER_SECRET; do
  if echo "${router_env}" | jq -e "index(\"${removed}\")" > /dev/null 2>&1; then
    fail "Router should NOT have env ${removed} (moved to operator/controller)"
  else
    pass "Router does not have legacy env ${removed}"
  fi
done

router_token_ref=$(kctl get deployment "${RELEASE_NAME}-router" -o json 2>/dev/null | \
  jq -r '[.spec.template.spec.containers[0].env[] | select(.name == "BOXY_ROUTER_TOKEN")] | .[0].valueFrom.secretKeyRef.name // empty')
if [[ -n "${router_token_ref}" ]]; then
  pass "Router token comes from Secret ref (not plaintext)"
else
  fail "Router token comes from Secret ref (not plaintext)"
fi

# -----------------------------------------------------------------------
# Operator environment variables
# -----------------------------------------------------------------------

suite "Operator Environment Variables"

op_env=$(kctl get deployment "${RELEASE_NAME}-operator" -o json 2>/dev/null | \
  jq -c '[.spec.template.spec.containers[0].env[].name]' || echo "[]")

for expected in BOXY_NAMESPACE BOXY_CONTROLLER_STATEFULSET_NAME BOXY_CONTROLLER_HEADLESS_SERVICE \
  BOXY_CONTROLLER_PORT BOXY_MAX_SANDBOXES_PER_CONTROLLER BOXY_MAX_CONTROLLER_REPLICAS \
  BOXY_MIN_CONTROLLER_REPLICAS BOXY_TERMINATED_RETENTION_SECONDS BOXY_MTLS_DISABLED; do
  if echo "${op_env}" | jq -e "index(\"${expected}\")" > /dev/null 2>&1; then
    pass "Operator has env ${expected}"
  else
    fail "Operator has env ${expected}"
  fi
done

op_sts_name=$(kctl get deployment "${RELEASE_NAME}-operator" -o json 2>/dev/null | \
  jq -r '[.spec.template.spec.containers[0].env[] | select(.name == "BOXY_CONTROLLER_STATEFULSET_NAME")] | .[0].value // empty')
assert_eq "Operator STS name matches actual StatefulSet" "${RELEASE_NAME}-ctrl" "${op_sts_name}"

op_headless=$(kctl get deployment "${RELEASE_NAME}-operator" -o json 2>/dev/null | \
  jq -r '[.spec.template.spec.containers[0].env[] | select(.name == "BOXY_CONTROLLER_HEADLESS_SERVICE")] | .[0].value // empty')
assert_eq "Operator headless svc matches actual Service" "${RELEASE_NAME}-ctrl-headless" "${op_headless}"

# -----------------------------------------------------------------------
# Controller environment variables
# -----------------------------------------------------------------------

suite "Controller Environment Variables"

ctrl_env=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o json 2>/dev/null | \
  jq -c '[.spec.template.spec.containers[0].env[].name]' || echo "[]")

for expected in BOXY_CONTROLLER_PORT BOXY_MAX_SANDBOXES BOXY_MTLS_DISABLED; do
  if echo "${ctrl_env}" | jq -e "index(\"${expected}\")" > /dev/null 2>&1; then
    pass "Controller has env ${expected}"
  else
    fail "Controller has env ${expected}"
  fi
done

# -----------------------------------------------------------------------
# Resource limits
# -----------------------------------------------------------------------

suite "Resource Limits"

router_mem_limit=$(kctl get deployment "${RELEASE_NAME}-router" -o json 2>/dev/null | \
  jq -r '.spec.template.spec.containers[0].resources.limits.memory // empty')
if [[ -n "${router_mem_limit}" ]]; then
  pass "Router has memory limit set (${router_mem_limit})"
else
  fail "Router has memory limit set"
fi

router_cpu_req=$(kctl get deployment "${RELEASE_NAME}-router" -o json 2>/dev/null | \
  jq -r '.spec.template.spec.containers[0].resources.requests.cpu // empty')
if [[ -n "${router_cpu_req}" ]]; then
  pass "Router has CPU request set (${router_cpu_req})"
else
  fail "Router has CPU request set"
fi

op_mem_limit=$(kctl get deployment "${RELEASE_NAME}-operator" -o json 2>/dev/null | \
  jq -r '.spec.template.spec.containers[0].resources.limits.memory // empty')
if [[ -n "${op_mem_limit}" ]]; then
  pass "Operator has memory limit set (${op_mem_limit})"
else
  fail "Operator has memory limit set"
fi

# -----------------------------------------------------------------------
# Probes
# -----------------------------------------------------------------------

suite "Health Probes"

op_liveness=$(kctl get deployment "${RELEASE_NAME}-operator" -o json 2>/dev/null | \
  jq -r '.spec.template.spec.containers[0].livenessProbe.httpGet.path // empty')
assert_eq "Operator liveness probe path" "/healthz" "${op_liveness}"

op_readiness=$(kctl get deployment "${RELEASE_NAME}-operator" -o json 2>/dev/null | \
  jq -r '.spec.template.spec.containers[0].readinessProbe.httpGet.path // empty')
assert_eq "Operator readiness probe path" "/readyz" "${op_readiness}"

ctrl_readiness_port=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o json 2>/dev/null | \
  jq -r '.spec.template.spec.containers[0].readinessProbe.httpGet.port // .spec.template.spec.containers[0].readinessProbe.tcpSocket.port // empty')
if [[ -n "${ctrl_readiness_port}" ]]; then
  pass "Controller has readiness probe configured"
else
  fail "Controller has readiness probe configured"
fi

ctrl_liveness_port=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o json 2>/dev/null | \
  jq -r '.spec.template.spec.containers[0].livenessProbe.httpGet.port // .spec.template.spec.containers[0].livenessProbe.tcpSocket.port // empty')
if [[ -n "${ctrl_liveness_port}" ]]; then
  pass "Controller has liveness probe configured"
else
  fail "Controller has liveness probe configured"
fi

# -----------------------------------------------------------------------
# Helm template render
# -----------------------------------------------------------------------

suite "Helm Template Validation"

CHART_DIR="${CHART_DIR:-deploy/helm/boxy}"
if [[ -d "${CHART_DIR}" ]] && command -v helm >/dev/null 2>&1; then
  render_out=$(helm template test "${CHART_DIR}" --set mtlsDisabled=true --set routerToken=test 2>&1)
  render_status=$?
  if [[ ${render_status} -eq 0 ]]; then
    pass "Helm template renders without errors"
  else
    fail "Helm template renders without errors" "${render_out}"
  fi

  render_with_defaults=$(helm template test "${CHART_DIR}" \
    --set mtlsDisabled=true \
    --set routerToken=test \
    --set defaultSandbox.enabled=true 2>&1)
  if [[ $? -eq 0 ]]; then
    pass "Helm template renders with default sandbox enabled"
  else
    fail "Helm template renders with default sandbox enabled"
  fi

  render_hpa=$(helm template test "${CHART_DIR}" \
    --set mtlsDisabled=true \
    --set routerToken=test \
    --set autoscaling.enabled=true 2>&1)
  if echo "${render_hpa}" | grep -q "HorizontalPodAutoscaler"; then
    pass "HPA rendered when autoscaling.enabled=true"
  else
    fail "HPA rendered when autoscaling.enabled=true"
  fi

  render_no_hpa=$(helm template test "${CHART_DIR}" \
    --set mtlsDisabled=true \
    --set routerToken=test \
    --set autoscaling.enabled=false 2>&1)
  if ! echo "${render_no_hpa}" | grep -q "HorizontalPodAutoscaler"; then
    pass "HPA not rendered when autoscaling.enabled=false"
  else
    fail "HPA not rendered when autoscaling.enabled=false"
  fi

  render_netpol=$(helm template test "${CHART_DIR}" \
    --set mtlsDisabled=true \
    --set routerToken=test \
    --set enableSandboxNetworkPolicy=true 2>&1)
  if echo "${render_netpol}" | grep -q "NetworkPolicy"; then
    pass "NetworkPolicy rendered when enabled"
  else
    fail "NetworkPolicy rendered when enabled"
  fi
else
  skip "Helm not available or chart dir not found at ${CHART_DIR}"
fi

summary
