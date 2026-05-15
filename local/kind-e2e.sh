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
  kind create cluster --name "${CLUSTER_NAME}" --config test/e2e/kind-config.yaml
fi

kubectl cluster-info --context "${CTX}"

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

echo ">>> Installing CRD"
kubectl --context "${CTX}" apply -f deploy/helm/boxy/crds/sandbox-crd.yaml

echo ">>> Installing Helm chart"
helm upgrade --install "${RELEASE_NAME}" ./deploy/helm/boxy \
  -n "${NAMESPACE}" --create-namespace \
  --set "router.image.repository=${IMAGE_REPO}/boxy-router" \
  --set "router.image.tag=${TAG}" \
  --set "controller.image.repository=${IMAGE_REPO}/boxy-controller" \
  --set "controller.image.tag=${TAG}" \
  --set "operator.image.repository=${IMAGE_REPO}/boxy-operator" \
  --set "operator.image.tag=${TAG}" \
  --set router.replicas=1 \
  --set controller.replicas=1 \
  --set mtls.disabled=true \
  --set router.defaultSandbox.enabled=false \
  --kube-context "${CTX}"

# Force a restart so pods pick up the freshly loaded images even when the
# tag is unchanged (Kubernetes does not restart pods when only the image
# content behind an existing tag is replaced).
echo ">>> Restarting deployments to pick up new images"
kubectl --context "${CTX}" -n "${NAMESPACE}" rollout restart \
  deployment/${RELEASE_NAME}-router \
  deployment/${RELEASE_NAME}-operator \
  statefulset/${RELEASE_NAME}-ctrl 2>/dev/null || true

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
# 6. Obtain a K8s SA token for authenticating to the router
# -----------------------------------------------------------------------
# Create a dedicated e2e ServiceAccount and issue a short-lived bound token.
# The router validates this via the K8s TokenReview API — no static secret needed.

echo ">>> Creating e2e ServiceAccount"
kubectl --context "${CTX}" -n "${NAMESPACE}" create serviceaccount boxy-e2e-client \
  --dry-run=client -o yaml | kubectl --context "${CTX}" apply -f -

echo ">>> Fetching e2e SA token"
ROUTER_TOKEN="$(kubectl --context "${CTX}" -n "${NAMESPACE}" \
  create token boxy-e2e-client --duration=3600s)"

# -----------------------------------------------------------------------
# 7. Port-forward
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

echo ""
echo ">>> Running e2e validation suite"
bash test/e2e/scripts/run-all.sh

# -----------------------------------------------------------------------
# 8. Run Go e2e tests
# -----------------------------------------------------------------------

echo ""
echo ">>> Running Go e2e tests"
export BOXY_E2E_BASE_URL="${BASE}"
export BOXY_E2E_ROUTER_TOKEN="${ROUTER_TOKEN}"
go test -v -count=1 -tags=e2e ./test/e2e/... -timeout=15m

echo ""
echo ">>> e2e run complete"
