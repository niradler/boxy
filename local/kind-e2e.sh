#!/usr/bin/env bash
# Spin up a kind cluster, deploy boxy via Helm, and run the full e2e validation suite.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-boxy-e2e}"
CTX="kind-${CLUSTER_NAME}"
TAG="${TAG:-e2e}"
IMAGE_REPO="${IMAGE_REPO:-boxydev}"
NAMESPACE="${NAMESPACE:-boxy}"
RELEASE_NAME="${RELEASE_NAME:-boxy}"

# -----------------------------------------------------------------------
# 1. Kind cluster
# -----------------------------------------------------------------------

if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  echo ">>> Creating kind cluster: ${CLUSTER_NAME}"
  kind create cluster --name "${CLUSTER_NAME}"
fi

kubectl cluster-info --context "${CTX}"

# -----------------------------------------------------------------------
# 2. KVM detection (probe inside kind node, not host)
#
# microsandbox requires hardware-assisted virtualization:
#   - Linux: /dev/kvm  (KVM module)
#   - macOS Apple Silicon: Apple Hypervisor Framework (Docker Desktop exposes
#     /dev/kvm inside its Linux VM, so the same check applies here)
#
# If /dev/kvm is not present inside the kind node:
#   - the api and operator suites are skipped automatically
#   - api and operator suites are skipped automatically
#   - infra, security, and config suites still run
# -----------------------------------------------------------------------

KVM_AVAILABLE=false
KIND_NODE="$(kind get nodes --name "${CLUSTER_NAME}" 2>/dev/null | head -1 || true)"
if [[ -n "${KIND_NODE}" ]] && docker exec "${KIND_NODE}" test -c /dev/kvm 2>/dev/null; then
  KVM_AVAILABLE=true
fi

if [[ "${KVM_AVAILABLE}" == "true" ]]; then
  echo ">>> /dev/kvm found inside kind node '${KIND_NODE}' — all suites will run"
else
  SKIP_SUITES="${SKIP_SUITES:+${SKIP_SUITES},}api,operator"
  echo ""
  echo ">>> WARNING: /dev/kvm not available inside kind node"
  echo ">>>   microsandbox requires KVM (Linux) or Apple Hypervisor Framework (macOS Apple Silicon)"
  echo ">>>   — no process-isolation fallback exists in microsandbox v0.4"
  echo ">>>   Skipping api and operator suites; infra/security/config will still run."
  echo ">>>   To run all suites, use a Linux host with KVM enabled, or macOS Apple Silicon"
  echo ">>>   with Docker Desktop (which exposes /dev/kvm inside containers)."
  echo ""
fi

export SKIP_SUITES

# -----------------------------------------------------------------------
# 3. Build and load images
# -----------------------------------------------------------------------

echo ">>> Building images"
make docker-build "IMAGE_REPO=${IMAGE_REPO}" "TAG=${TAG}"

echo ">>> Loading images into kind"
kind load docker-image "${IMAGE_REPO}/boxy-router:${TAG}" --name "${CLUSTER_NAME}"
kind load docker-image "${IMAGE_REPO}/boxy-controller:${TAG}" --name "${CLUSTER_NAME}"
kind load docker-image "${IMAGE_REPO}/boxy-operator:${TAG}" --name "${CLUSTER_NAME}"

# -----------------------------------------------------------------------
# 4. Install CRD + Helm chart
# -----------------------------------------------------------------------

ROUTER_TOKEN="$(openssl rand -hex 16)"

echo ">>> Installing CRD"
kubectl --context "${CTX}" apply -f deploy/helm/boxy/crds/sandbox-crd.yaml

echo ">>> Installing Helm chart"
helm upgrade --install "${RELEASE_NAME}" ./deploy/helm/boxy \
  -n "${NAMESPACE}" --create-namespace \
  --set "imageRouter=${IMAGE_REPO}/boxy-router:${TAG}" \
  --set "controllerImage=${IMAGE_REPO}/boxy-controller:${TAG}" \
  --set "imageOperator=${IMAGE_REPO}/boxy-operator:${TAG}" \
  --set routerReplicas=1 \
  --set controllerReplicas=1 \
  --set "routerToken=${ROUTER_TOKEN}" \
  --set mtlsDisabled=true \
  --set defaultSandbox.enabled=false \
  --kube-context "${CTX}"

# -----------------------------------------------------------------------
# 5. Wait for rollout
# -----------------------------------------------------------------------

echo ">>> Waiting for deployments"
kubectl --context "${CTX}" -n "${NAMESPACE}" rollout status deployment/${RELEASE_NAME}-router --timeout=180s
kubectl --context "${CTX}" -n "${NAMESPACE}" rollout status deployment/${RELEASE_NAME}-operator --timeout=180s
kubectl --context "${CTX}" -n "${NAMESPACE}" rollout status statefulset/${RELEASE_NAME}-ctrl --timeout=180s

echo ">>> Waiting for all pods ready"
kubectl --context "${CTX}" -n "${NAMESPACE}" wait --for=condition=Ready pods --all --timeout=180s

# -----------------------------------------------------------------------
# 6. Port-forward
# -----------------------------------------------------------------------

kubectl --context "${CTX}" -n "${NAMESPACE}" port-forward svc/${RELEASE_NAME}-router 18080:8080 &
PF=$!
trap 'kill ${PF} 2>/dev/null || true' EXIT

BASE="http://127.0.0.1:18080"
echo ">>> Waiting for router health..."
for i in $(seq 1 30); do
  curl -fsS "${BASE}/healthz" 2>/dev/null | grep -q ok && break
  sleep 2
done
curl -fsS "${BASE}/healthz" | grep ok

# -----------------------------------------------------------------------
# 7. Run e2e validation scripts
# -----------------------------------------------------------------------

export NAMESPACE RELEASE_NAME ROUTER_TOKEN
export BASE_URL="${BASE}"
export KUBECTL_CTX="${CTX}"
export CHART_DIR="deploy/helm/boxy"
export BOXY_NO_KVM
BOXY_NO_KVM="$([[ "${KVM_AVAILABLE}" == "true" ]] && echo false || echo true)"

echo ""
echo ">>> Running e2e validation suite"
bash test/e2e/scripts/run-all.sh

# -----------------------------------------------------------------------
# 8. Run Go e2e tests
# -----------------------------------------------------------------------

if [[ "${KVM_AVAILABLE}" == "true" ]]; then
  echo ""
  echo ">>> Running Go e2e tests"
  export BOXY_E2E_BASE_URL="${BASE}"
  export BOXY_E2E_ROUTER_TOKEN="${ROUTER_TOKEN}"
  go test -v -count=1 -tags=e2e ./test/e2e/... -timeout=15m
else
  echo ""
  echo ">>> Skipping Go e2e tests (no /dev/kvm — sandbox creation requires KVM)"
fi

echo ""
echo ">>> e2e run complete"
