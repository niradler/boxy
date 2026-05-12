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
kind load docker-image "${IMAGE_REPO}/boxy-worker:${TAG}" --name "${CLUSTER_NAME}"

ROUTER_TOKEN="$(openssl rand -hex 16)"
WORKER_TOKEN="$(openssl rand -hex 16)"

helm upgrade --install boxy ./deploy/helm/boxy -n boxy --create-namespace \
  --set "imageRouter=${IMAGE_REPO}/boxy-router:${TAG}" \
  --set "imageWorker=${IMAGE_REPO}/boxy-worker:${TAG}" \
  --set routerReplicas=1 \
  --set "routerToken=${ROUTER_TOKEN}" \
  --set "workerToken=${WORKER_TOKEN}" \
  --set reaperIntervalSeconds=5

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
POD_UID="$(echo "${CREATE}" | jq -r .podRef.uid)"
POD_NAME="$(echo "${CREATE}" | jq -r .podRef.name)"
NS="$(echo "${CREATE}" | jq -r .podRef.namespace)"

kubectl --context "${CTX}" -n "${NS}" wait --for=condition=Ready "pod/${POD_NAME}" --timeout=300s

curl -fsS "${BASE}/v1/exec" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"sessionId\":\"s1\",\"sandboxId\":\"sb1\",\"podRef\":{\"namespace\":\"${NS}\",\"name\":\"${POD_NAME}\",\"uid\":\"${POD_UID}\"},\"command\":\"sh\",\"args\":[\"-c\",\"echo api_exec\"],\"timeoutSeconds\":60,\"mode\":\"api_exec\"}" | jq -e '.stdout | test("api_exec")'

curl -fsS "${BASE}/v1/exec" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"sessionId\":\"s1\",\"sandboxId\":\"sb1\",\"podRef\":{\"namespace\":\"${NS}\",\"name\":\"${POD_NAME}\",\"uid\":\"${POD_UID}\"},\"command\":\"sh\",\"args\":[\"-c\",\"echo pod_exec\"],\"timeoutSeconds\":60,\"mode\":\"pod_exec\"}" | jq -e '.stdout | test("pod_exec")'

curl -fsS "${BASE}/v1/exec" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"sessionId\":\"s1\",\"sandboxId\":\"sb1\",\"command\":\"sh\",\"args\":[\"-c\",\"echo no_podref\"],\"timeoutSeconds\":60,\"mode\":\"api_exec\"}" | jq -e '.stdout | test("no_podref")'

CODE="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/v1/exec" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"sessionId\":\"s1\",\"sandboxId\":\"sb1\",\"podRef\":{\"namespace\":\"${NS}\",\"name\":\"${POD_NAME}\",\"uid\":\"bad-uid\"},\"command\":\"sh\",\"args\":[\"-c\",\"echo x\"],\"timeoutSeconds\":60,\"mode\":\"pod_exec\"}" || true)"
test "${CODE}" = "403" || { echo "expected 403 for bad uid got ${CODE}"; exit 1; }

CODE="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/v1/exec" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"sessionId\":\"wrong\",\"sandboxId\":\"sb1\",\"podRef\":{\"namespace\":\"${NS}\",\"name\":\"${POD_NAME}\",\"uid\":\"${POD_UID}\"},\"command\":\"sh\",\"args\":[\"-c\",\"echo x\"],\"timeoutSeconds\":60,\"mode\":\"pod_exec\"}" || true)"
test "${CODE}" = "403" || { echo "expected 403 for label mismatch got ${CODE}"; exit 1; }

TTL_JSON="$(curl -fsS "${BASE}/v1/sandboxes" \
  -H "Authorization: Bearer ${ROUTER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"sessionId":"s2","sandboxId":"sb-ttl","owner":"bob","ttlSeconds":1}')"
TTL_POD="$(echo "${TTL_JSON}" | jq -r .podRef.name)"
kubectl --context "${CTX}" -n "${NS}" wait --for=condition=Ready "pod/${TTL_POD}" --timeout=300s

TTL_REAPED=0
for _ in $(seq 1 90); do
  if ! kubectl --context "${CTX}" -n "${NS}" get pod "${TTL_POD}" >/dev/null 2>&1; then
    TTL_REAPED=1
    break
  fi
  sleep 2
done
test "${TTL_REAPED:-0}" = "1" || { echo "ttl pod still present"; exit 1; }

export BOXY_E2E_BASE_URL="${BASE}"
export BOXY_E2E_ROUTER_TOKEN="${ROUTER_TOKEN}"
export BOXY_E2E_WORKER_IMAGE="${IMAGE_REPO}/boxy-worker:${TAG}"
go test -v -count=1 -tags=e2e ./test/e2e/... -timeout=15m

echo "e2e ok"
