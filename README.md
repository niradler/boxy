# boxy

**Kubernetes-native sandbox runtime.** Run isolated shell commands inside ephemeral Linux environments via a clean HTTP API or MCP — no VMs, no hypervisors, no hardware dependencies.

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](go.mod)
[![Rust](https://img.shields.io/badge/Rust-1.82+-CE412B?logo=rust)](controller/Cargo.toml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Helm chart](https://img.shields.io/badge/Helm-v0.0.2-0F1689?logo=helm)](oci://ghcr.io/niradler/charts/boxy)

Each sandbox is an **nsjail** process jail: isolated filesystem, network namespace, and resource limits backed by a shared read-only Ubuntu 24.04 rootfs. No kernel modules, no container runtimes, no `/dev/kvm` — just a standard Linux node.

## Architecture

| Component | Language | Role |
|---|---|---|
| **boxy-router** | Go | Stateless HTTP frontend — auth, API, MCP server, sandbox CR management |
| **boxy-operator** | Go | Kubernetes controller — bin-packing, StatefulSet auto-scaling, TTL expiry |
| **boxy-controller** | Rust | Per-node nsjail daemon — runs actual sandboxes, exposes mTLS HTTP API |

```
Client --(Bearer)--> [boxy-router  Deployment × N]
                          |  Sandbox CR create/read
                          |  mTLS → ctrl-pod-dns:port/v1/...
                          v
                   [boxy-operator  Deployment]
                          |  Scales StatefulSet, assigns pods
                          v
                   [boxy-controller  StatefulSet pod]
                          |  nsjail --chroot /rootfs/ubuntu-24.04
                          v
                   [nsjail sandbox 1…N]
                      /workspace  (per-sandbox, persistent)
                      /tmp        (per-exec tmpfs, ephemeral)
                      /           (shared read-only Ubuntu 24.04)
```

For a deep dive into request flows, scaling algorithms, storage, failure modes, and security trade-offs, see [docs/architecture.md](docs/architecture.md).

## Sandbox isolation

Every sandbox gets:

- **Read-only base OS** — Ubuntu 24.04 rootfs mounted read-only. Sandboxes cannot modify system files or contaminate each other.
- **Private `/workspace`** — writable directory bind-mounted per sandbox. Persists across multiple execs within the same sandbox lifetime.
- **Ephemeral `/tmp`** — fresh tmpfs per exec, discarded when the command exits.
- **Network namespace isolation** — sandboxes have no external network access by default. `network.allowInternetAccess: true` opts out.
- **cgroup memory cap** — `vm.memoryMb` enforced via nsjail `--cgroup_mem_max`.
- **POSIX rlimits** — `as`, `core`, `cpu`, `fsize`, `nofile`, `nproc`, `stack` configurable per sandbox.

## Quick start

### Prerequisites

- Kubernetes cluster (kind works fine)
- `kubectl` and `helm` ≥ 3.x
- Docker for building images
- Go ≥ 1.26 and Rust ≥ 1.82 for local development

### Install with Helm (OCI registry — recommended)

Images are published to Docker Hub and the Helm chart to GHCR OCI on every release.

```bash
helm upgrade --install boxy oci://ghcr.io/niradler/charts/boxy \
  --version 0.0.2 \
  --namespace boxy --create-namespace \
  --set routerToken="$(openssl rand -hex 16)"
```

### Install with Helm (from source)

```bash
helm upgrade --install boxy ./deploy/helm/boxy \
  --namespace boxy --create-namespace \
  --set routerToken="$(openssl rand -hex 16)"
```

### First API calls

```bash
export TOKEN=<your-router-token>
export BASE=http://<router-service>:8080

# Create a sandbox
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sessionId":"demo","sandboxId":"demo-1","owner":"you","ttlSeconds":3600}' \
  $BASE/v1/sandboxes | jq .

# Execute a command
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sessionId":"demo","sandboxId":"demo-1","command":"sh","args":["-c","echo hello from nsjail"],"timeoutSeconds":30}' \
  $BASE/v1/exec | jq .

# Delete the sandbox
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" $BASE/v1/sandboxes/demo-1
```

## API reference

### `GET /healthz`

Returns `200 OK` with body `ok`.

### `POST /v1/sandboxes`

Create a sandbox. Returns `201` when the sandbox is `Running`.

**Required fields:** `sessionId`, `sandboxId`, `owner`.

| Field | Type | Description |
|---|---|---|
| `ttlSeconds` | int | Sandbox TTL (sliding window, refreshed on each exec). `0` = no expiry. |
| `env` | map | Environment variables. `KUBERNETES_*` and `BOXY_*` prefixes are blocked. Max 64 keys. |
| `allowedBinaries` | string[] | Binaries (e.g. `"curl"`, `"python3"`) bind-mounted read-only from the controller image. |
| `vm` | object | Resource and identity config — see below. |
| `network` | object | Network policy — see below. |
| `volumes` | array | Extra mounts inside the sandbox. |

**`vm` fields:**

| Field | Type | Description |
|---|---|---|
| `user` | string | Run as this user. |
| `hostname` | string | Sandbox hostname. Default: sandbox ID. |
| `workdir` | string | Working directory inside the sandbox. |
| `memoryMb` | int | Memory cap via cgroup. |
| `rlimits` | array | `[{ "resource": "nofile", "soft": 1024 }]`. Supported: `as`, `core`, `cpu`, `fsize`, `nofile`, `nproc`, `stack`. |
| `image` | string | Override rootfs path (must be pre-baked on the controller node). Default: `/rootfs/ubuntu-24.04`. |

**`network` fields:**

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Default `true`. |
| `allowInternetAccess` | bool | When `true`, disables network namespace isolation (sandbox shares the pod's network). |

Response: `{ "sandboxId", "sessionId", "owner", "runtime", "phase", "ready" }`. `runtime` is always `"nsjail"`.

### `POST /v1/exec`

Execute a command inside an existing sandbox.

| Field | Type | Description |
|---|---|---|
| `sandboxId` | string | Required. |
| `sessionId` | string | Required. |
| `command` | string | Required. Resolved against rootfs `PATH` if relative. |
| `args` | string[] | Command arguments. |
| `env` | map | Per-exec env overrides. |
| `timeoutSeconds` | int | Required. Hard wall-clock timeout enforced by nsjail (SIGKILL). |

Response: `{ "exitCode", "stdout", "stderr", "timedOut" }`.

### `GET /v1/sandboxes/{sandboxId}`

Returns sandbox status.

### `DELETE /v1/sandboxes/{sandboxId}`

Deletes the sandbox and removes its CR.

### HTTP status codes

| Code | Meaning |
|---|---|
| `400` | Bad request (missing required field, invalid JSON, blocked env prefix). |
| `401` | Missing or invalid bearer token. |
| `404` | Sandbox not found. |
| `409` | Sandbox already exists. |
| `413` | Output cap exceeded. |
| `429` | Concurrency limit hit. |
| `502` | Controller pod unreachable or returned an error. |
| `503` | Controller pod not ready. |

## MCP server

`POST /mcp` — [Model Context Protocol](https://modelcontextprotocol.io/) endpoint using Streamable HTTP transport (JSON-RPC 2.0), built with the [official Go SDK](https://github.com/modelcontextprotocol/go-sdk). Stateless, no session management required.

**Required headers:**

```
Authorization: Bearer <BOXY_ROUTER_TOKEN>
Content-Type: application/json
Accept: application/json, text/event-stream
```

**Sandbox routing:** set `X-Sandbox-Id` to target a specific sandbox. Omit to use the default sandbox (when `BOXY_DEFAULT_SANDBOX_ENABLED=true`).

**Available tools:**

| Tool | Parameters |
|---|---|
| `bash` | `command` (string, required), `timeoutSeconds` (int, default 60) |

```bash
# MCP initialize
curl -sS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"cli","version":"1.0"},"capabilities":{}}}' \
  $BASE/mcp | jq .

# MCP bash call
curl -sS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H 'X-Sandbox-Id: demo-1' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bash","arguments":{"command":"echo hello from nsjail"}}}' \
  $BASE/mcp | jq .
```

## Security model

- **Router auth:** `Authorization: Bearer <BOXY_ROUTER_TOKEN>` required on every route. One shared token — no per-caller RBAC.
- **mTLS:** Router and operator dial controllers using a Helm-generated CA with mutual cert verification. No hostname verification; identity is CA membership. Disable with `BOXY_MTLS_DISABLED=true` for local dev.
- **NetworkPolicy:** Default-deny egress on controller pods (DNS only). Per-sandbox isolation enforced by nsjail network namespaces, not Kubernetes policy.
- **Controller pod capabilities:** `SYS_ADMIN`, `SETUID`, `SETGID`, `NET_ADMIN`, `SYS_CHROOT`, `MKNOD`, `SETPCAP`. All others dropped. `allowPrivilegeEscalation: false`.

> [!WARNING]
> The controller pod runs as root; a kernel exploit escaping nsjail would have root on the node. `allowedEgressDomains` is accepted in the API but not enforced — domain filtering requires an external egress proxy or eBPF layer. See [docs/architecture.md § Security Model](docs/architecture.md#9-security-model) for the full threat model.

## Configuration

### Router

| Variable | Default | Description |
|---|---|---|
| `BOXY_ROUTER_TOKEN` | — | **Required.** Bearer token for clients. |
| `BOXY_SANDBOX_NAMESPACE` | `default` | Namespace where Sandbox CRs and controller pods live. |
| `BOXY_LISTEN_ADDR` | `:8080` | |
| `BOXY_CONTROLLER_PORT` | `8080` | Port the controller pods listen on. |
| `BOXY_MTLS_DISABLED` | `false` | Set `true` for local dev. |
| `BOXY_TLS_CA_PATH` | `/tls/ca.crt` | |
| `BOXY_TLS_CLIENT_CERT_PATH` | `/tls/tls.crt` | |
| `BOXY_TLS_CLIENT_KEY_PATH` | `/tls/tls.key` | |
| `BOXY_MAX_BODY_BYTES` | `1048576` | Max request body size (1 MB). |
| `BOXY_MAX_OUTPUT_BYTES` | `2097152` | Max exec output size (2 MB, truncated not errored). |
| `BOXY_MAX_TIMEOUT_SECONDS` | `3600` | |
| `BOXY_MAX_CONCURRENCY` | `100` | Concurrent execs per router replica. |
| `BOXY_MAX_ARGS` | `256` | |
| `BOXY_MAX_ENV_KEYS` | `64` | |
| `BOXY_DEFAULT_SANDBOX_ENABLED` | `false` | Enable default sandbox for stateless MCP clients. |
| `BOXY_DEFAULT_SANDBOX_CONFIG` | — | Required when enabled. JSON sandbox create body. |

### Operator

| Variable | Default | Description |
|---|---|---|
| `BOXY_NAMESPACE` | — | **Required.** Namespace to watch. |
| `BOXY_CONTROLLER_STATEFULSET_NAME` | — | Name of the controller StatefulSet. |
| `BOXY_CONTROLLER_HEADLESS_SERVICE` | — | Headless service for pod DNS. |
| `BOXY_CONTROLLER_PORT` | `8080` | |
| `BOXY_MAX_SANDBOXES_PER_CONTROLLER` | `20` | Sandboxes per controller pod (bin-packing cap). |
| `BOXY_MAX_CONTROLLER_REPLICAS` | `50` | StatefulSet scale-out ceiling. |
| `BOXY_MIN_CONTROLLER_REPLICAS` | `1` | StatefulSet scale-in floor. |
| `BOXY_TERMINATED_RETENTION_SECONDS` | `3600` | How long to retain Terminated Sandbox CRs before deletion. |
| `BOXY_MTLS_DISABLED` | `false` | |

### Controller

| Variable | Default | Description |
|---|---|---|
| `BOXY_CONTROLLER_PORT` | `8080` | |
| `BOXY_MAX_SANDBOXES` | `20` | Max concurrent sandboxes on this pod. |
| `BOXY_MTLS_DISABLED` | `false` | |
| `BOXY_SANDBOX_PROVIDER` | `nsjail` | Sandbox backend. Only `nsjail` is implemented. |
| `BOXY_NSJAIL_PATH` | `/usr/sbin/nsjail` | Path to the nsjail binary. |
| `BOXY_NSJAIL_ROOTFS` | `/rootfs/ubuntu-24.04` | Default read-only rootfs. |
| `BOXY_NSJAIL_SANDBOX_ROOT` | `/var/lib/boxy/sandboxes` | Host path for per-sandbox workspace directories. |
| `BOXY_NSJAIL_BINARIES_DIR` | `/usr/local/bin` | Host directory for `allowedBinaries`. |
| `RUST_LOG` | `warn` | Log level: `off` / `error` / `warn` / `info` / `debug` / `trace`. |

## Development

```bash
# Unit tests
make test

# Lint
make lint

# Format
make fmt
```

Requires Go ≥ 1.26 and Rust ≥ 1.82.

### E2E tests

Build and load images into a kind cluster, then run the test suites against a live deployment:

```bash
# Build and load into kind
make kind-load

# Go e2e suite
BOXY_E2E_BASE_URL=http://127.0.0.1:18080 \
BOXY_E2E_ROUTER_TOKEN=<token> \
make e2e-go

# Shell e2e suite (182 checks across infra, security, config, api, isolation, operator)
BASE_URL=http://127.0.0.1:18080 ROUTER_TOKEN=<token> NAMESPACE=boxy \
make e2e-scripts
```

A [kind config and full setup script](local/) is included for spinning up a local cluster.

## Project layout

```
cmd/boxy-router/         Router entry point (Go)
cmd/boxy-operator/       Operator entry point (Go)
controller/              nsjail controller (Rust)
  src/
    providers/nsjail.rs  Sandbox create/exec/delete lifecycle
    config.rs            Env-driven configuration
    routes.rs            Axum HTTP handlers
    types.rs             Request/response types
internal/
  api/                   Shared types and validation
  kube/                  Sandbox CR client, controller pod DNS
  operator/              Reconciler, StatefulSet scaling, TTL expiry
  router/                HTTP server, MCP server, mTLS client
deploy/
  helm/boxy/             Helm chart (router, operator, controller, RBAC, mTLS, NetworkPolicy)
  manifests/             Raw RBAC manifests
test/e2e/                Go and shell end-to-end test suites
docs/
  architecture.md        Full system design and architecture reference
Dockerfile.router
Dockerfile.controller    Multi-stage: nsjail build + Ubuntu 24.04 rootfs + Rust binary
Dockerfile.operator
```

## License

[MIT](LICENSE)
