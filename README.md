# boxy

`boxy` is a Kubernetes-native sandbox worker manager. A stateless HTTP **router** schedules and validates **sandbox worker pods**, then runs commands on an exact pod using either in-cluster HTTP (`api_exec`) or Kubernetes `pods/exec` (`pod_exec`). Each `POST /v1/exec` resolves the live pod by **`podRef` (namespace, name, uid)** or, if `podRef` is omitted or empty, by **`sandboxId` + `sessionId`** in `BOXY_SANDBOX_NAMESPACE` before validating labels, UID, and readiness. Services are not used for command delivery.

## Architecture (MVP)

| Layer | Responsibility |
|------|------------------|
| **boxy-router** | Authenticated HTTP API; creates/reads/deletes sandbox `Pod`s; resolves optional `podRef` or looks up pod by `sandboxId`/`sessionId`; validates UID + labels + phase; fans out exec (HTTP or SPDY exec); TTL/max-life reaper. |
| **boxy-worker** | Container image running `boxy-worker` (HTTP `/v1/exec`) plus common CLIs; same image works under `kubectl exec`. |

**Why this scales across replicas:** no in-memory session table. Every `POST /v1/exec` ends with a concrete `podRef`; the router verifies it against the API server (UID + `boxy.dev/*` labels + Ready) before contacting that pod’s IP or opening an SPDY exec stream.

**Labels and annotations**

- Labels: `boxy.dev/sandbox-id`, `boxy.dev/session-id`, `boxy.dev/owner`, `boxy.dev/managed-by=boxy`
- Annotations: `boxy.dev/created-at`, optional `boxy.dev/ttl-seconds`, `boxy.dev/expires-at`, `boxy.dev/max-lifetime-seconds`

```
Client --(Bearer)--> [boxy-router Deployment x N]
                         |  GET/LIST Pod (optional resolve) + validate UID+labels
                         |  api_exec -> http://podIP:workerPort/v1/exec (Bearer worker token)
                         v
                   [Sandbox Pod: boxy-worker]
```

## API contract

### `GET /healthz`

Returns `200` with body `ok`.

### `POST /v1/exec`

JSON body:

| Field | Type | Notes |
|------|------|--------|
| `sessionId` | string | Must match pod label `boxy.dev/session-id`. |
| `sandboxId` | string | Must match pod label `boxy.dev/sandbox-id`. |
| `podRef` | object | Optional. If `namespace`, `name`, and `uid` are all set, the router uses them directly. If all three are empty or omitted, the router lists pods with label `boxy.dev/sandbox-id` equal to `sandboxId` and picks the pod whose `boxy.dev/session-id` matches `sessionId` (then applies the same UID and label checks as explicit `podRef`). Partial `podRef` (only one or two fields) is rejected. |
| `command` | string | Shell command segment (worker/router wrap for pod exec). |
| `args` | string[] | |
| `env` | map[string,string> | Injected safely for `pod_exec` via `/bin/sh -lc`; worker uses same wrapping. |
| `timeoutSeconds` | int | Hard cap from `BOXY_MAX_TIMEOUT_SECONDS`. |
| `mode` | `api_exec` \| `pod_exec` | `api_exec` hits worker HTTP; `pod_exec` uses Kubernetes exec. |
| `workerContainer` | string | Optional; default `worker`. |

Response: `{ "exitCode", "stdout", "stderr" }`.

### `POST /v1/sandboxes`

Creates **or reuses** a sandbox pod keyed by `sandboxId` label (and checks `sessionId` / `owner` align on reuse).

JSON (required): `sessionId`, `sandboxId`, `owner`.

JSON (optional provisioning): `ttlSeconds`, `maxLifetimeSeconds`, `image`, `workerPort`, `imagePullPolicy`, `imagePullSecretName`, `serviceAccountName`, `resources` (`cpuRequest`, `cpuLimit`, `memoryRequest`, `memoryLimit`), `env`, `labels`, `annotations`. Keys under `kubernetes.io/`, `k8s.io/`, or `boxy.dev/` are reserved. Env keys must not start with `BOXY_` (worker reserved). Values are capped by router env (see below).

Response: `sandboxId`, `sessionId`, `owner`, `runtime`, `execApiPath`, `image`, `workerPort`, `podRef`, `phase`, `ready`.

### `GET /v1/sandboxes/{sandboxId}`

Status for the sandbox pod (first match in the sandbox namespace).

### `DELETE /v1/sandboxes/{sandboxId}`

Deletes sandbox pod(s) with that `sandboxId` label.

## Security model

- **Router auth:** `Authorization: Bearer <BOXY_ROUTER_TOKEN>` on every mutating route (and exec/sandbox routes).
- **Worker auth:** router calls worker with `Authorization: Bearer <BOXY_WORKER_TOKEN>`; worker rejects missing/invalid tokens.
- **No trust in IDs:** `sessionId` / `sandboxId` / `podRef.name` alone are not sufficient; the router loads the live pod and checks **UID** and labels before exec. Omitted `podRef` still ends in a full live lookup and the same checks.
- **Limits:** request body max (`BOXY_MAX_BODY_BYTES`), output max (`BOXY_MAX_OUTPUT_BYTES`), per-request timeout max, concurrency semaphore (`BOXY_MAX_CONCURRENCY`), arg/env cardinality caps.
- **Pod hardening (worker):** non-root (65532), `allowPrivilegeEscalation: false`, `capabilities.drop: ALL`, `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem` with `emptyDir` `/tmp`, worker `ServiceAccount` defaults to `automountServiceAccountToken: false` (enable only if you intentionally need in-cluster Kubernetes API access from the sandbox).
- **RBAC:** router receives only `pods` + `pods/exec` + `pods/log` verbs within the sandbox namespace (Helm chart + `deploy/manifests/rbac.yaml` example).

Secrets belong in Kubernetes `Secret` objects or external secret managers—never in images.

## Configuration (router env)

| Variable | Meaning |
|---------|---------|
| `BOXY_ROUTER_TOKEN` | Required bearer token for clients. |
| `BOXY_WORKER_TOKEN` | Shared secret router→worker (`api_exec`). Injected into created sandbox pods. |
| `BOXY_SANDBOX_NAMESPACE` | Namespace for sandbox pods. |
| `BOXY_WORKER_IMAGE` | Image for new sandbox pods. |
| `BOXY_WORKER_SERVICE_ACCOUNT` | SA name mounted by worker pods. |
| `BOXY_WORKER_PORT` | Worker HTTP port (default `8080`). |
| `BOXY_WORKER_CPU` / `BOXY_WORKER_MEMORY` | Optional requests/limits on sandbox pods. |
| `BOXY_IMAGE_PULL_SECRET` | Optional default `imagePullSecrets` name on created sandboxes (and allowlist default). |
| `BOXY_ALLOWED_SERVICE_ACCOUNTS` | Comma-separated extra service account names allowed in `serviceAccountName` (always includes `BOXY_WORKER_SERVICE_ACCOUNT`). |
| `BOXY_ALLOWED_PULL_SECRETS` | Comma-separated extra pull secret names allowed in `imagePullSecretName` (includes `BOXY_IMAGE_PULL_SECRET` when set). |
| `BOXY_MAX_SANDBOX_ENV_KEYS` / `BOXY_MAX_SANDBOX_LABELS` / `BOXY_MAX_SANDBOX_ANNOTATIONS` | Caps on per-sandbox maps (defaults `32` / `16` / `32`). |
| `BOXY_MAX_IMAGE_REF_BYTES` | Max length of custom `image` string (default `512`). |
| `BOXY_MIN_WORKER_PORT` / `BOXY_MAX_WORKER_PORT` | Allowed range for optional `workerPort` (defaults `1`–`65535`). |
| `BOXY_MAX_SANDBOX_CPU` / `BOXY_MAX_SANDBOX_MEMORY` | Optional upper bounds for sandbox container resources (Kubernetes quantity strings). |
| `BOXY_MAX_SANDBOX_TTL_SECONDS` / `BOXY_MAX_SANDBOX_LIFETIME_SECONDS` | Upper bounds for `ttlSeconds` and `maxLifetimeSeconds`. |
| `BOXY_LISTEN_ADDR` | Default `:8080`. |
| `BOXY_MAX_BODY_BYTES` | Default `1048576`. |
| `BOXY_MAX_OUTPUT_BYTES` | Default `2097152`. |
| `BOXY_MAX_TIMEOUT_SECONDS` | Default `3600`. |
| `BOXY_MAX_CONCURRENCY` | Default `100` concurrent execs per replica. |
| `BOXY_MAX_ARGS` / `BOXY_MAX_ENV_KEYS` | Validation caps on exec requests. |
| `BOXY_REAPER_INTERVAL_SECONDS` | Default `30`. |

## Local development

```bash
make test
make lint
```

Requires Go 1.22+.

## Build images

```bash
make docker-build IMAGE_REPO=your.registry/boxy TAG=dev
```

## Helm install (recommended)

```bash
helm upgrade --install boxy ./deploy/helm/boxy -n boxy --create-namespace \
  --set imageRouter=your.registry/boxy/boxy-router:dev \
  --set imageWorker=your.registry/boxy/boxy-worker:dev \
  --set routerToken="$(openssl rand -hex 16)" \
  --set workerToken="$(openssl rand -hex 32)"
```

Set `sandboxNamespace` when sandboxes should live outside the release namespace.

## kind end-to-end plan

Requires: Docker, kind, kubectl, Helm, jq.

```bash
make e2e
```

This runs `hack/kind-e2e.sh`, which loads locally built images, installs the chart, port-forwards the router Service, exercises `api_exec` + `pod_exec`, negative UID/label cases, and TTL deletion via the reaper.

## curl examples

Replace `TOKEN` and host with live values. You can pass `podRef` from `POST /v1/sandboxes` or omit it and rely on `sandboxId` + `sessionId` lookup.

```bash
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sessionId":"demo","sandboxId":"demo-1","owner":"you","ttlSeconds":3600}' \
  http://127.0.0.1:8080/v1/sandboxes | jq .

curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d @- http://127.0.0.1:8080/v1/exec <<'JSON' | jq .
{
  "sessionId": "demo",
  "sandboxId": "demo-1",
  "podRef": { "namespace": "boxy", "name": "boxy-demo-1", "uid": "<uid-from-apiserver>" },
  "command": "sh",
  "args": ["-c", "echo hello"],
  "env": {},
  "timeoutSeconds": 60,
  "mode": "api_exec"
}
JSON

curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sessionId":"demo","sandboxId":"demo-1","command":"sh","args":["-c","echo hello"],"env":{},"timeoutSeconds":60,"mode":"api_exec"}' \
  http://127.0.0.1:8080/v1/exec | jq .
```

## Worker image contents

The `Dockerfile.worker` installs common networking/database CLIs plus `kubectl`, `helm`, `kustomize`, `yq`, `kn`, `argocd`, `terraform`, `gh`, `stern`, AWS CLI v2, Google Cloud CLI, and Azure CLI (`pip`). Datadog CLI is not bundled (hook your own lightweight install if required).

## API status codes (common)

| Code | Meaning |
|------|---------|
| `401` | Missing/invalid router bearer token. |
| `403` | Pod validation failed (UID/labels/phase/managed-by). |
| `404` | Unknown sandbox (`GET`), or exec lookup found no pod for `sandboxId`/`sessionId` when `podRef` was omitted. |
| `409` | Sandbox owner/session conflict on create. |
| `413` | Output cap exceeded. |
| `429` | Concurrency limit. |

## Testing

- Unit tests: request validation, pod validation (fake clientset), reaper decisions, shell/exit-code helpers.
- Integration style: `k8s.io/client-go/kubernetes/fake`.
- E2E: see `hack/kind-e2e.sh`.

## Project layout

```
cmd/boxy-router   # control plane HTTP server
cmd/boxy-worker   # sandbox-side HTTP exec
internal/api      # types + validation
internal/kube     # clients, pod lifecycle, validation helpers
internal/exec     # api_exec client + pod exec + shell wrapper
internal/session  # TTL / reaper logic
internal/router   # HTTP router / handlers
deploy/helm/boxy  # Helm chart + RBAC
deploy/manifests  # standalone RBAC example
```

## License

[MIT](LICENSE).
