# boxy

`boxy` is a Kubernetes-native sandbox runtime that runs isolated workloads inside **nsjail process sandboxes**. A stateless Go **router** bin-packs sandboxes across dynamically scaled **controller pods** (Rust), each hosting multiple sandboxes via [nsjail](https://github.com/google/nsjail). Commands are executed inside sandboxes through `POST /v1/exec` or the built-in **MCP server** (`POST /mcp`).

## Architecture

| Layer          | Language | Responsibility                                                                                                                                                                       |
| -------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **boxy-router**     | Go       | Authenticated HTTP API + MCP server; routes requests to the correct controller pod; manages Sandbox CRs in Kubernetes; delegates scaling to the operator.                       |
| **boxy-operator**   | Go       | Watches Sandbox CRs; scales the controller StatefulSet up/down; assigns sandboxes to controller pods; tracks TTL and cleans up expired sandboxes.                               |
| **boxy-controller** | Rust     | Runs on a Kubernetes StatefulSet pod; manages nsjail sandbox lifecycle (create/exec/delete); exposes HTTP API over mTLS; each controller pod hosts up to `BOXY_MAX_SANDBOXES` sandboxes. |

**How it works:**

1. Client calls `POST /v1/sandboxes` on the router.
2. Router creates a Sandbox CR in Kubernetes.
3. Operator assigns the CR to an available controller pod (bin-packing), scaling the StatefulSet if needed.
4. Router dials the assigned controller over mTLS and creates the nsjail sandbox.
5. `POST /v1/exec` (or `POST /mcp` tools/call) dials the controller and executes inside the sandbox.

```
Client --(Bearer)--> [boxy-router Deployment x N]
                         |  Create / lookup Sandbox CR
                         |  mTLS -> https://ctrl-pod-dns:port/v1/...
                         v
                  [boxy-operator Deployment]
                         |  Scales StatefulSet, assigns pods
                         v
                  [boxy-controller StatefulSet pod]
                         |  nsjail --chroot /rootfs/ubuntu-24.04
                         v
                  [nsjail sandbox 1..N]
                     /workspace (per-sandbox bind-mount)
                     /tmp       (per-exec tmpfs)
                     /rootfs    (shared read-only Ubuntu 24.04)
```

## Sandbox isolation

Each sandbox gets:

- **Read-only base rootfs** — pre-baked Ubuntu 24.04 image, mounted read-only by nsjail. Sandboxes cannot modify system files or contaminate each other's rootfs.
- **Per-sandbox `/workspace`** — writable directory on the controller pod, bind-mounted read-write. Persists across multiple execs within the same sandbox lifetime.
- **Ephemeral `/tmp`** — fresh tmpfs per exec, discarded when the exec completes.
- **Network namespace isolation** — by default each sandbox runs in an isolated network namespace. `network.allowInternetAccess: true` opts out to the host network.
- **No hardware dependency** — nsjail uses Linux kernel namespaces and chroot. No KVM, no nested virtualization required.

## API contract

### `GET /healthz`

Returns `200` with body `ok`.

### `POST /v1/sandboxes`

Create a sandbox on a controller pod.

**Required:** `sessionId`, `sandboxId`, `owner`.

**Optional:**

| Field | Type | Notes |
|-------|------|-------|
| `ttlSeconds` | int | Sandbox TTL. Refreshed on each exec (sliding window). |
| `env` | map | Environment variables injected into the sandbox. Cannot use `KUBERNETES_*` or `BOXY_*` prefixes. |
| `allowedBinaries` | string[] | CLI names (e.g. `"curl"`, `"python3"`) available in the controller image, bind-mounted read-only into the sandbox. |
| `vm` | object | Sandbox resource/identity config (see below). |
| `network` | object | Network policy (see below). |
| `volumes` | array | Extra volume mounts inside the sandbox. |

**`vm` object:**

| Field | Type | Notes |
|-------|------|-------|
| `user` | string | Run as this user inside the sandbox. |
| `hostname` | string | Sandbox hostname. Default: sandbox ID. |
| `workdir` | string | Working directory inside the sandbox. |
| `memoryMb` | int | Memory cap via cgroup (nsjail `--cgroup_mem_max`). |
| `rlimits` | array | POSIX resource limits. Each item: `{ "resource": "nofile", "soft": 1024 }`. Supported: `as`, `core`, `cpu`, `fsize`, `nofile`, `nproc`, `stack`. |
| `image` | string | Override the rootfs path (must be an absolute path to a pre-baked rootfs on the controller node). Default: `/rootfs/ubuntu-24.04`. |

> `vcpus`, `shell`, `maxDurationSec`, `idleTimeoutSec`, `scripts` are accepted but ignored — they were microsandbox-specific and have no nsjail equivalent.

**`network` object:**

| Field | Type | Notes |
|-------|------|-------|
| `enabled` | bool | Default `true`. |
| `allowInternetAccess` | bool | When `true`, disables network namespace isolation (host network). |

Response: `{ "sandboxId", "sessionId", "owner", "runtime", "phase", "ready" }`.

`runtime` is always `"nsjail"`.

### `POST /v1/exec`

Execute a command inside a sandbox.

| Field | Type | Notes |
|-------|------|-------|
| `sandboxId` | string | Required. |
| `sessionId` | string | Required. |
| `command` | string | Required. Relative names (e.g. `sh`) are resolved against the rootfs PATH. |
| `args` | string[] | |
| `env` | map[string,string] | Per-exec env overrides. |
| `timeoutSeconds` | int | Required. Hard wall-clock cap enforced by nsjail. |

Response: `{ "exitCode", "stdout", "stderr", "timedOut" }`.

### `GET /v1/sandboxes/{sandboxId}`

Status for the sandbox.

### `DELETE /v1/sandboxes/{sandboxId}`

Deletes the sandbox and removes its Sandbox CR.

### `POST /mcp`

[Model Context Protocol](https://modelcontextprotocol.io/) endpoint using Streamable HTTP transport (JSON-RPC 2.0). Built with the [official Go SDK](https://github.com/modelcontextprotocol/go-sdk). Stateless — no session management required.

**Headers:**

| Header | Notes |
|--------|-------|
| `Authorization` | `Bearer <BOXY_ROUTER_TOKEN>` (required). |
| `Content-Type` | `application/json` |
| `Accept` | `application/json, text/event-stream` (required by SDK). |
| `X-Sandbox-Id` | Target sandbox. Omit to use the default sandbox (when enabled). |

**Available tools:**

| Tool | Description |
|------|-------------|
| `bash` | Execute a shell command. Params: `command` (string, required), `timeoutSeconds` (int, default 60). |

**Default sandbox:** When `BOXY_DEFAULT_SANDBOX_ENABLED=true`, MCP clients can call `bash` without `X-Sandbox-Id`. The router creates and manages a long-lived default sandbox automatically.

## Security model

- **Router auth:** `Authorization: Bearer <BOXY_ROUTER_TOKEN>` on every route.
- **Router-to-controller:** mutual TLS with CA pinning (no hostname verification — controllers are dialed by pod DNS name). Disable with `BOXY_MTLS_DISABLED=true` for local dev.
- **Helm auto-generated certs:** The Helm chart creates a CA plus server/client certs in Kubernetes Secrets.
- **NetworkPolicy:** Default-deny egress on controller pods (DNS egress only). Per-sandbox network isolation enforced by nsjail network namespaces.
- **Filesystem isolation:** Sandboxes share a read-only Ubuntu 24.04 rootfs. Each sandbox has a private writable `/workspace`. No cross-sandbox filesystem access is possible.
- **Input validation:** Blocked env prefixes (`KUBERNETES_*`, `BOXY_*`), request body max, output max, per-request timeout, concurrency semaphore, arg/env cardinality caps.
- **Controller pod capabilities:** `SYS_ADMIN` (mount namespaces), `SETUID`/`SETGID` (nsjail uid mapping), `NET_ADMIN` (network namespace setup), `SYS_CHROOT`, `MKNOD`, `SETPCAP` (nsjail `prctl(PR_SET_SECUREBITS)`). All other capabilities dropped.

## Configuration

### Router

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_ROUTER_TOKEN` | — | Required. Bearer token for clients. |
| `BOXY_SANDBOX_NAMESPACE` | `default` | Namespace where Sandbox CRs and controller pods live. |
| `BOXY_LISTEN_ADDR` | `:8080` | |
| `BOXY_CONTROLLER_PORT` | `8080` | Port the controller pods listen on. |
| `BOXY_MTLS_DISABLED` | `false` | Set `true` for local dev. |
| `BOXY_TLS_CA_PATH` | `/tls/ca.crt` | |
| `BOXY_TLS_CLIENT_CERT_PATH` | `/tls/tls.crt` | |
| `BOXY_TLS_CLIENT_KEY_PATH` | `/tls/tls.key` | |
| `BOXY_MAX_BODY_BYTES` | `1048576` | |
| `BOXY_MAX_OUTPUT_BYTES` | `2097152` | |
| `BOXY_MAX_TIMEOUT_SECONDS` | `3600` | |
| `BOXY_MAX_CONCURRENCY` | `100` | Concurrent execs per router replica. |
| `BOXY_MAX_ARGS` | `256` | |
| `BOXY_MAX_ENV_KEYS` | `64` | |
| `BOXY_DEFAULT_SANDBOX_ENABLED` | `false` | Enable the default sandbox for MCP clients. |
| `BOXY_DEFAULT_SANDBOX_CONFIG` | — | Required when enabled. JSON sandbox create body. |

### Operator

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_NAMESPACE` | — | Required. Namespace to watch. |
| `BOXY_CONTROLLER_STATEFULSET_NAME` | — | Name of the controller StatefulSet. |
| `BOXY_CONTROLLER_HEADLESS_SERVICE` | — | Headless service for pod DNS. |
| `BOXY_CONTROLLER_PORT` | `8080` | |
| `BOXY_MAX_SANDBOXES_PER_CONTROLLER` | `20` | Sandboxes per controller pod (bin-packing cap). |
| `BOXY_MAX_CONTROLLER_REPLICAS` | `50` | StatefulSet scale-out ceiling. |
| `BOXY_MIN_CONTROLLER_REPLICAS` | `1` | StatefulSet scale-in floor. |
| `BOXY_TERMINATED_RETENTION_SECONDS` | `3600` | How long to keep Terminated Sandbox CRs before deletion. |
| `BOXY_MTLS_DISABLED` | `false` | |

### Controller

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_CONTROLLER_PORT` | `8080` | |
| `BOXY_MAX_SANDBOXES` | `20` | Max concurrent sandboxes on this pod. |
| `BOXY_MTLS_DISABLED` | `false` | |
| `BOXY_SANDBOX_PROVIDER` | `nsjail` | Sandbox backend (currently only `nsjail`). |
| `BOXY_NSJAIL_PATH` | `/usr/sbin/nsjail` | Path to the nsjail binary. |
| `BOXY_NSJAIL_ROOTFS` | `/rootfs/ubuntu-24.04` | Default read-only rootfs for sandboxes. |
| `BOXY_NSJAIL_SANDBOX_ROOT` | `/var/lib/boxy/sandboxes` | Host path where per-sandbox workspace dirs are created. |
| `BOXY_NSJAIL_BINARIES_DIR` | `/usr/local/bin` | Host directory containing binaries available via `allowedBinaries`. |
| `RUST_LOG` | `warn` | Controller log level (`off`/`error`/`warn`/`info`/`debug`/`trace`). |

## Local development

```bash
make test
make lint
```

Requires Go 1.25+ and Rust 1.82+.

## Build images

```bash
# Router
docker build -f Dockerfile.router -t your.registry/boxy/boxy-router:dev .

# Controller (includes nsjail build + Ubuntu 24.04 rootfs — takes a few minutes)
docker build -f Dockerfile.controller -t your.registry/boxy/boxy-controller:dev .
```

## Helm install

```bash
helm upgrade --install boxy ./deploy/helm/boxy -n boxy --create-namespace \
  --set imageRouter=your.registry/boxy/boxy-router:dev \
  --set controllerImage=your.registry/boxy/boxy-controller:dev \
  --set routerToken="$(openssl rand -hex 16)"
```

No hardware requirements. The controller image bundles nsjail and a pre-baked Ubuntu 24.04 rootfs. Any standard Linux node works.

## E2E tests

Run against a live cluster (kind or real):

```bash
KUBECTL_CTX=kind-boxy-e2e \
NAMESPACE=boxy \
BASE_URL=http://127.0.0.1:18080 \
ROUTER_TOKEN=<token> \
bash test/e2e/scripts/run-all.sh
```

Or the Go suite:

```bash
BOXY_E2E_BASE_URL=http://127.0.0.1:18080 \
BOXY_E2E_ROUTER_TOKEN=<token> \
make e2e-go
```

The shell suite covers 182 checks across 6 suites: infra, security, config, api, isolation, operator.

The **isolation** suite verifies per-sandbox guarantees directly:

- Filesystem isolation: file written to sandbox A is invisible from sandbox B
- Read-only rootfs: writes to `/etc`, `/usr` are rejected; `/workspace` and `/tmp` are writable
- Ephemeral `/tmp`: fresh tmpfs on every exec; `/workspace` persists across execs
- Environment isolation: sandbox-level env vars scoped to their sandbox; exec-level env overrides them
- Network isolation: default sandbox runs in an isolated network namespace, cannot reach external IPs
- Resource limits: `memoryMb` enforced via nsjail `--cgroup_mem_max`

## curl examples

```bash
# Create a sandbox
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sessionId":"demo","sandboxId":"demo-1","owner":"you","ttlSeconds":3600}' \
  http://127.0.0.1:8080/v1/sandboxes | jq .

# Execute a command
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sessionId":"demo","sandboxId":"demo-1","command":"sh","args":["-c","echo hello"],"timeoutSeconds":30}' \
  http://127.0.0.1:8080/v1/exec | jq .

# Create with resource limits and allowed binaries
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d @- http://127.0.0.1:8080/v1/sandboxes <<'JSON' | jq .
{
  "sessionId": "demo",
  "sandboxId": "demo-2",
  "owner": "you",
  "ttlSeconds": 3600,
  "allowedBinaries": ["curl", "python3"],
  "vm": { "memoryMb": 512, "workdir": "/workspace" },
  "network": { "allowInternetAccess": false }
}
JSON

# Delete a sandbox
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8080/v1/sandboxes/demo-1

# MCP: initialize
curl -sS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"cli","version":"1.0"},"capabilities":{}}}' \
  http://127.0.0.1:8080/mcp | jq .

# MCP: execute bash in a specific sandbox
curl -sS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'X-Sandbox-Id: demo-1' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bash","arguments":{"command":"echo hello from nsjail"}}}' \
  http://127.0.0.1:8080/mcp | jq .
```

## API status codes

| Code | Meaning |
|------|---------|
| `400` | Bad request (missing required field, invalid JSON, blocked env prefix). |
| `401` | Missing or invalid router bearer token. |
| `404` | Sandbox not found. |
| `409` | Sandbox already exists. |
| `413` | Output cap exceeded. |
| `429` | Concurrency limit. |
| `502` | Controller pod unreachable or returned an error. |
| `503` | Controller pod not ready. |

## Project layout

```text
cmd/boxy-router/        # Router entry point (Go)
cmd/boxy-operator/      # Operator entry point (Go)
controller/             # nsjail controller (Rust)
  src/
    providers/
      nsjail.rs         # NsjailAdapter: create/exec/delete sandbox lifecycle
    config.rs           # Env-driven config
    routes.rs           # Axum HTTP handlers
    types.rs            # Shared request/response types
internal/
  api/                  # Shared types + validation
  kube/                 # Sandbox CR client, controller pod DNS resolution
  operator/             # Reconciler: StatefulSet scaling, TTL expiry, assignment
  router/               # HTTP server, MCP server, mTLS client, default sandbox
deploy/
  helm/boxy/            # Helm chart (router, operator, controller, RBAC, mTLS, NetworkPolicy)
test/e2e/
  scripts/              # Shell e2e suite (167 checks)
Dockerfile.router
Dockerfile.controller   # Multi-stage: nsjail build + Ubuntu 24.04 rootfs + Rust binary
```

## License

[MIT](LICENSE).
