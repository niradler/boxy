#!/usr/bin/env bash
# Validate Kubernetes infrastructure: deployments, statefulsets, services, CRD, PDB.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

suite "CRD Installation"

crd_out=$(kubectl ${_CTX_FLAG} get crd sandboxes.boxy.dev -o name 2>/dev/null || true)
if [[ -n "${crd_out}" ]]; then
  pass "Sandbox CRD installed"
else
  fail "Sandbox CRD installed" "sandboxes.boxy.dev CRD not found"
fi

crd_cols=$(kubectl ${_CTX_FLAG} get crd sandboxes.boxy.dev -o jsonpath='{.spec.versions[0].additionalPrinterColumns[*].name}' 2>/dev/null || true)
assert_contains "CRD has Phase printer column" "${crd_cols}" "Phase"
assert_contains "CRD has SandboxID printer column" "${crd_cols}" "SandboxID"
assert_contains "CRD has Controller printer column" "${crd_cols}" "Controller"

crd_scope=$(kubectl ${_CTX_FLAG} get crd sandboxes.boxy.dev -o jsonpath='{.spec.scope}' 2>/dev/null || true)
assert_eq "CRD is Namespaced" "Namespaced" "${crd_scope}"

crd_shortnames=$(kubectl ${_CTX_FLAG} get crd sandboxes.boxy.dev -o jsonpath='{.spec.names.shortNames[0]}' 2>/dev/null || true)
assert_eq "CRD has sbx shortName" "sbx" "${crd_shortnames}"

crd_status=$(kubectl ${_CTX_FLAG} get crd sandboxes.boxy.dev -o jsonpath='{.spec.versions[0].subresources.status}' 2>/dev/null || true)
if [[ -n "${crd_status}" ]]; then
  pass "CRD has status subresource"
else
  fail "CRD has status subresource"
fi

# --- Router ---

suite "Router Deployment"

router_dep=$(kctl get deployment "${RELEASE_NAME}-router" -o jsonpath='{.metadata.name}' 2>/dev/null || true)
if [[ -n "${router_dep}" ]]; then
  pass "Router Deployment exists"
else
  fail "Router Deployment exists"
fi

router_ready=$(kctl get deployment "${RELEASE_NAME}-router" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
assert_gt "Router has ready replicas" "${router_ready:-0}" 0

router_sa=$(kctl get deployment "${RELEASE_NAME}-router" -o jsonpath='{.spec.template.spec.serviceAccountName}' 2>/dev/null || true)
assert_eq "Router uses correct ServiceAccount" "boxy-router" "${router_sa}"

# --- Router Service ---

router_svc=$(kctl get service "${RELEASE_NAME}-router" -o jsonpath='{.spec.ports[0].port}' 2>/dev/null || true)
if [[ -n "${router_svc}" ]]; then
  pass "Router Service exists"
else
  fail "Router Service exists"
fi

# --- Operator ---

suite "Operator Deployment"

op_dep=$(kctl get deployment "${RELEASE_NAME}-operator" -o jsonpath='{.metadata.name}' 2>/dev/null || true)
if [[ -n "${op_dep}" ]]; then
  pass "Operator Deployment exists"
else
  fail "Operator Deployment exists"
fi

op_ready=$(kctl get deployment "${RELEASE_NAME}-operator" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
assert_gt "Operator has ready replicas" "${op_ready:-0}" 0

op_replicas=$(kctl get deployment "${RELEASE_NAME}-operator" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)
assert_eq "Operator runs single replica" "1" "${op_replicas}"

op_sa=$(kctl get deployment "${RELEASE_NAME}-operator" -o jsonpath='{.spec.template.spec.serviceAccountName}' 2>/dev/null || true)
assert_eq "Operator uses correct ServiceAccount" "boxy-operator" "${op_sa}"

# --- Controller StatefulSet ---

suite "Controller StatefulSet"

sts=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o jsonpath='{.metadata.name}' 2>/dev/null || true)
if [[ -n "${sts}" ]]; then
  pass "Controller StatefulSet exists"
else
  fail "Controller StatefulSet exists"
fi

sts_svc=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o jsonpath='{.spec.serviceName}' 2>/dev/null || true)
assert_eq "StatefulSet uses headless service" "${RELEASE_NAME}-ctrl-headless" "${sts_svc}"

sts_policy=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o jsonpath='{.spec.podManagementPolicy}' 2>/dev/null || true)
assert_eq "StatefulSet uses Parallel pod management" "Parallel" "${sts_policy}"

sts_ready=$(kctl get statefulset "${RELEASE_NAME}-ctrl" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
assert_gt "Controller has ready replicas" "${sts_ready:-0}" 0

# --- Headless Service ---

suite "Controller Headless Service"

hl_svc=$(kctl get service "${RELEASE_NAME}-ctrl-headless" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
assert_eq "Headless Service has clusterIP=None" "None" "${hl_svc}"

hl_publish=$(kctl get service "${RELEASE_NAME}-ctrl-headless" -o jsonpath='{.spec.publishNotReadyAddresses}' 2>/dev/null || true)
assert_eq "Headless Service publishes not-ready addresses" "true" "${hl_publish}"

# --- PodDisruptionBudget ---

suite "PodDisruptionBudget"

pdb=$(kctl get pdb "${RELEASE_NAME}-ctrl" -o jsonpath='{.spec.maxUnavailable}' 2>/dev/null || true)
assert_eq "PDB maxUnavailable=1" "1" "${pdb}"

pdb_selector=$(kctl get pdb "${RELEASE_NAME}-ctrl" -o jsonpath='{.spec.selector.matchLabels.app}' 2>/dev/null || true)
assert_eq "PDB targets boxy-controller" "boxy-controller" "${pdb_selector}"

# --- ServiceAccounts ---

suite "ServiceAccounts"

for sa in boxy-router boxy-controller boxy-operator; do
  sa_out=$(kctl get serviceaccount "${sa}" -o name 2>/dev/null || true)
  if [[ -n "${sa_out}" ]]; then
    pass "ServiceAccount ${sa} exists"
  else
    fail "ServiceAccount ${sa} exists"
  fi
done

ctrl_automount=$(kctl get serviceaccount boxy-controller -o jsonpath='{.automountServiceAccountToken}' 2>/dev/null || true)
assert_eq "Controller SA automountServiceAccountToken=false" "false" "${ctrl_automount}"

# --- Secrets ---

suite "Secrets"

token_secret=$(kctl get secret "${RELEASE_NAME}-tokens" -o name 2>/dev/null || true)
if [[ -n "${token_secret}" ]]; then
  pass "Router token secret exists"
else
  fail "Router token secret exists"
fi

summary
