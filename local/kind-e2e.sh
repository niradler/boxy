#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-boxy-e2e}"
CTX="kind-${CLUSTER_NAME}"
TAG="${TAG:-e2e}"
IMAGE_REPO="${IMAGE_REPO:-boxydev}"

if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  kind create cluster --name "${CLUSTER_NAME}"
fi

kubectl cluster-info --context "${CTX}"

make docker-build "IMAGE_REPO=${IMAGE_REPO}" "TAG=${TAG}"

kind load docker-image "${IMAGE_REPO}/boxy-router:${TAG}" --name "${CLUSTER_NAME}"
kind load docker-image "${IMAGE_REPO}/boxy-controller:${TAG}" --name "${CLUSTER_NAME}"

ROUTER_TOKEN="$(openssl rand -hex 16)"

helm upgrade --install boxy ./deploy/helm/boxy -n boxy --create-namespace \
  --set "imageRouter=${IMAGE_REPO}/boxy-router:${TAG}" \
  --set "controllerImage=${IMAGE_REPO}/boxy-controller:${TAG}" \
  --set routerReplicas=1 \
  --set "routerToken=${ROUTER_TOKEN}" \
  --set reaperIntervalSeconds=5 \
  --set mtlsDisabled=true \
  --set kvmMode=hostpath

kubectl --context "${CTX}" rollout status deployment/boxy-router -n boxy --timeout=180s
kubectl --context "${CTX}" wait -n boxy --for=condition=Ready pods -l app=boxy-router --timeout=180s

kubectl --context "${CTX}" -n boxy port-forward svc/boxy-router 18080:8080 &
PF=$!
trap 'kill ${PF} 2>/dev/null || true' EXIT
sleep 2

BASE="http://127.0.0.1:18080"
curl -fsS "${BASE}/healthz" | grep ok

CREATE="$(curl -fsS "${BASE}/v1/sandboxes" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"sessionId":"s1","sandboxId":"sb1","owner":"alice","ttlSeconds":86400}')"
echo "${CREATE}"

curl -fsS "${BASE}/v1/exec" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"sessionId":"s1","sandboxId":"sb1","command":"sh","args":["-c","echo hello"],"timeoutSeconds":60}' | jq -e '.stdout | test("hello")'

curl -fsS "${BASE}/v1/exec" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"sessionId":"s1","sandboxId":"sb1","command":"sh","args":["-c","echo world"],"timeoutSeconds":60}' | jq -e '.stdout | test("world")'

curl -sS -X DELETE "${BASE}/v1/sandboxes/sb1" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" -o /dev/null -w '%{http_code}' | grep -q 204

export BOXY_E2E_BASE_URL="${BASE}"
export BOXY_E2E_ROUTER_TOKEN="${ROUTER_TOKEN}"
go test -v -count=1 -tags=e2e ./test/e2e/... -timeout=15m

echo "e2e ok"
