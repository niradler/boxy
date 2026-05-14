# boxy

`boxy` is a Kubernetes-native sandbox manager that runs isolated workloads inside lightweight **microsandbox KVM VMs**. A stateless Go **router** bin-packs sandboxes across dynamically created **controller pods** (Rust), each hosting multiple VMs via [microsandbox](https://github.com/nicholasgasior/microsandbox). Commands are executed inside VMs through `POST /v1/exec` or the built-in **MCP server** (`POST /mcp`).

## Architecture

| Layer | Language | Responsibility |
|-------|----------|----------------|
| **boxy-router** | Go | Authenticated HTTP API + MCP server; bin-packs sandboxes across controller pods; maintains a ConfigMap-backed routing store; self-healing sync reconciler; TTL reaper for controller pods. |
| **boxy-controller** | Rust | Runs on a Kubernetes pod with `/dev/kvm` access; manages microsandbox VM lifecycle (create/exec/delete); exposes HTTP API over mTLS; each controller hosts up to `BOXY_MAX_SANDBOXES_PER_CONTROLLER` VMs. |

**How it works:**

1. Client calls `POST /v1/sandboxes` on the router.
2. Router picks (or creates) a controller pod with available capacity (bin-packing).
3. Router tells the controller to create a microsandbox VM.
4. Router stores the route (`sandboxId` → controller pod IP) in a ConfigMap.
5. `POST /v1/exec` (or `POST /mcp` tools/call) looks up the route, dials the controller over mTLS, executes inside the VM.

**Self-healing:** A sync reconciler periodically fans out to all controller pods (`GET /v1/sandboxes`), rebuilds the routing store from ground truth, and invalidates stale routes. Triggered on startup, cache conflicts, parse errors, or stale-route detection during exec.

```
Client --(Bearer)--> [boxy-router Deployment x N]
                         |  SelectOrCreateControllerPod (bin-pack)
                         |  mTLS -> https://controllerPodIP:port/v1/...
                         v
                   [Controller Pod: boxy-controller]
                         |  microsandbox SDK -> /dev/kvm
                         v
                   [MicroVM sandbox 1..N]
```

## API contract

### `GET /healthz`

Returns `200` with body `ok`.

### `POST /v1/exec`

Execute a command inside a sandbox VM.

| Field | Type | Notes |
|-------|------|-------|
| `sandboxId` | string | Required. Identifies the sandbox in the routing store. |
| `sessionId` | string | Required. |
| `command` | string | Shell command. |
| `args` | string[] | |
| `env` | map[string,string] | Injected into the VM. |
| `timeoutSeconds` | int | Hard cap from `BOXY_MAX_TIMEOUT_SECONDS`. |

Response: `{ "exitCode", "stdout", "stderr", "timedOut" }`.

### `POST /v1/sandboxes`

Create a sandbox VM on a controller pod.

**Required:** `sessionId`, `sandboxId`, `owner`.

**Optional provisioning:**

| Field | Type | Notes |
|-------|------|-------|
| `ttlSeconds` | int | Sandbox TTL. |
| `maxLifetimeSeconds` | int | Hard lifetime cap. |
| `image` | string | Base OCI image for the VM. |
| `env` | map | Environment variables injected into the VM. |
| `allowedBinaries` | string[] | CLI names (e.g. `"aws"`, `"curl"`) pre-installed in the controller image, injected into the VM. |
| `vm` | object | VM configuration (see below). |
| `network` | object | VM-level network policy (see below). |
| `volumes` | array | Volume mounts inside the VM. |
| `patches` | array | Filesystem modifications before VM startup. |
| `labels` | map | Custom labels (reserved prefixes: `kubernetes.io/`, `k8s.io/`, `boxy.dev/`). |
| `annotations` | map | Custom annotations (same reserved prefixes). |

**`vm` object:**

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `memoryMb` | int | 512 | Guest RAM in MiB. |
| `vcpus` | int | 1 | Virtual CPUs. |
| `workdir` | string | | Working directory inside the VM. |
| `shell` | string | | Default shell (e.g. `/bin/bash`). |
| `hostname` | string | sandbox ID | Guest hostname. |
| `user` | string | | Guest user identity. |
| `maxDurationSec` | int | 0 | Hard VM lifetime cap. 0 = no limit. |
| `idleTimeoutSec` | int | 0 | Kill VM after N seconds of no exec. 0 = no timeout. |
| `rlimits` | array | | POSIX resource limits (`nofile`, `nproc`, `memlock`, etc.). |
| `scripts` | array | | Named shell scripts placed at `/.msb/scripts/<name>`. |

**`network` object:**

| Field | Type | Notes |
|-------|------|-------|
| `enabled` | bool | Default `true`. Toggle networking on/off. |
| `allowInternetAccess` | bool | Unrestricted outbound. Overrides rules. |
| `allowedEgressDomains` | string[] | Deny-all with exceptions (ignored if `allowInternetAccess` or `rules` set). |
| `rules` | array | Full ordered firewall rules (first match wins). |
| `ports` | array | Publish VM ports to the host (`hostPort`, `guestPort`, `protocol`). |
| `dns` | object | Upstream resolvers, rebind protection, query timeout. |
| `secrets` | array | Inject credentials into outbound HTTP by destination host. |
| `maxConnections` | int | Default 256. Cap concurrent outbound connections. |
| `trustHostCAs` | bool | Copy host root CA bundle into VM. |

Response: `{ "sandboxId", "sessionId", "owner", "runtime", "image", "podRef", "phase", "ready" }`.

### `GET /v1/sandboxes/{sandboxId}`

Status for the sandbox (from the routing store).

### `DELETE /v1/sandboxes/{sandboxId}`

Deletes the sandbox VM on its controller and removes the route.

### `POST /mcp`

[Model Context Protocol](https://modelcontextprotocol.io/) endpoint using Streamable HTTP transport (JSON-RPC 2.0). Built with the [official Go SDK](https://github.com/modelcontextprotocol/go-sdk) (`v1.6.0`). Stateless — no session management required.

**Headers:**

| Header | Notes |
|--------|-------|
| `Authorization` | `Bearer <BOXY_ROUTER_TOKEN>` (required). |
| `Content-Type` | `application/json` |
| `Accept` | `application/json, text/event-stream` (required by the SDK). |
| `X-Sandbox-Id` | Target a specific sandbox. If omitted, uses the default sandbox (when enabled). |

**Available tools:**

| Tool | Description |
|------|-------------|
| `bash` | Execute a shell command. Params: `command` (string, required), `timeoutSeconds` (int, default 60). |

**Default sandbox:** When `BOXY_DEFAULT_SANDBOX_ENABLED=true`, MCP clients can call `bash` without specifying `X-Sandbox-Id`. The router creates and manages a long-lived default sandbox automatically.

## Security model

- **Router auth:** `Authorization: Bearer <BOXY_ROUTER_TOKEN>` on every route.
- **Router-to-controller:** mutual TLS with CA pinning (no hostname verification — controllers are dialed by ephemeral pod IP). Disable with `BOXY_MTLS_DISABLED=true` for local dev.
- **Helm auto-generated certs:** The Helm chart creates a CA plus server/client certs in Kubernetes Secrets.
- **NetworkPolicy:** Default-deny egress on controller pods (DNS + router allowed). Per-sandbox network rules enforced inside the VM by microsandbox.
- **Limits:** request body max, output max, per-request timeout, concurrency semaphore, arg/env cardinality caps, max sandboxes per controller.
- **KVM isolation:** Each sandbox runs in a lightweight KVM VM, providing hardware-level isolation.
- **Controller image:** Runs as root (needs `/dev/kvm`). Kubernetes SecurityContext drops capabilities. CLIs available for injection: `curl`, `bash`, `python3`, AWS CLI v2.

## Configuration (router env)

### Core

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_ROUTER_TOKEN` | — | Required. Bearer token for clients. |
| `BOXY_SANDBOX_NAMESPACE` | `default` | Namespace for controller and sandbox pods. |
| `BOXY_LISTEN_ADDR` | `:8080` | |
| `BOXY_MAX_BODY_BYTES` | `1048576` | |
| `BOXY_MAX_OUTPUT_BYTES` | `2097152` | |
| `BOXY_MAX_TIMEOUT_SECONDS` | `3600` | |
| `BOXY_MAX_CONCURRENCY` | `100` | Concurrent execs per router replica. |
| `BOXY_MAX_ARGS` | `256` | |
| `BOXY_MAX_ENV_KEYS` | `64` | |
| `BOXY_REAPER_INTERVAL_SECONDS` | `30` | |

### Controller / bin-packing

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_CONTROLLER_IMAGE` | — | Controller pod image. |
| `BOXY_CONTROLLER_PORT` | `8080` | |
| `BOXY_CONTROLLER_TTL_SECONDS` | `3600` | Auto-refreshed on each operation. |
| `BOXY_MAX_SANDBOXES_PER_CONTROLLER` | `20` | VMs per controller pod. |
| `BOXY_CONTROLLER_SERVICE_ACCOUNT` | `""` | |

### mTLS

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_MTLS_DISABLED` | `false` | Set `true` for local dev. |
| `BOXY_TLS_CA_PATH` | `/tls/ca.crt` | |
| `BOXY_TLS_CLIENT_CERT_PATH` | `/tls/tls.crt` | |
| `BOXY_TLS_CLIENT_KEY_PATH` | `/tls/tls.key` | |
| `BOXY_MTLS_CONTROLLER_SECRET` | `boxy-mtls-controller` | K8s Secret mounted into controller pods. |

### Default sandbox (MCP)

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_DEFAULT_SANDBOX_ENABLED` | `false` | Enable the default sandbox for MCP clients. |
| `BOXY_DEFAULT_SANDBOX_CONFIG` | — | Required when enabled. JSON string with sandbox create body (sandboxId, owner, ttlSeconds, vm, network, etc.). |

### VM operator defaults

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_VM_LOG_LEVEL` | `warn` | `off` / `error` / `warn` / `info` / `debug` / `trace` |
| `BOXY_VM_METRICS_INTERVAL_MS` | `0` | 0 = disabled. |
| `BOXY_VM_PULL_POLICY` | `if_missing` | `if_missing` / `always` / `never` |
| `BOXY_LIBKRUNFW_PATH` | — | Override microsandbox default. |

### Sandbox provisioning limits

| Variable | Default | Notes |
|----------|---------|-------|
| `BOXY_MAX_SANDBOX_TTL_SECONDS` | `86400` | |
| `BOXY_MAX_SANDBOX_LIFETIME_SECONDS` | `604800` | |
| `BOXY_MAX_SANDBOX_ENV_KEYS` | `32` | |
| `BOXY_MAX_SANDBOX_LABELS` | `16` | |
| `BOXY_MAX_SANDBOX_ANNOTATIONS` | `32` | |
| `BOXY_MAX_IMAGE_REF_BYTES` | `512` | |
| `BOXY_MAX_SANDBOX_CPU` / `BOXY_MAX_SANDBOX_MEMORY` | — | Upper bounds for pod resources. |
| `BOXY_WORKER_SERVICE_ACCOUNT` | `boxy-worker` | |
| `BOXY_ALLOWED_SERVICE_ACCOUNTS` | — | Comma-separated extra allowed SAs. |
| `BOXY_IMAGE_PULL_SECRET` | — | Default pull secret. |
| `BOXY_ALLOWED_PULL_SECRETS` | — | Comma-separated extra allowed pull secrets. |

## Local development

```bash
make test
make lint
```

Requires Go 1.25+ and Rust 1.95+ (for the controller).

## Build images

```bash
make docker-build IMAGE_REPO=your.registry/boxy TAG=dev
```

This builds the router image. The controller image is built separately:

```bash
docker build -f Dockerfile.controller -t your.registry/boxy/boxy-controller:dev .
```

## Helm install

```bash
helm upgrade --install boxy ./deploy/helm/boxy -n boxy --create-namespace \
  --set imageRouter=your.registry/boxy/boxy-router:dev \
  --set controllerImage=your.registry/boxy/boxy-controller:dev \
  --set routerToken="$(openssl rand -hex 16)"
```

Set `sandboxNamespace` when sandboxes should live outside the release namespace. Cluster nodes must have `/dev/kvm` available — the controller mounts it via hostPath automatically.

## KVM requirements

microsandbox (the VM engine used by boxy-controller) requires hardware-assisted virtualization. **There is no process-isolation fallback in v0.4.**

| Platform | Requirement |
| -------- | ----------- |
| Linux | KVM kernel module enabled; `/dev/kvm` accessible inside the controller pod |
| macOS Apple Silicon | Apple Hypervisor Framework — Docker Desktop on M-series Macs exposes `/dev/kvm` inside containers automatically |
| x86 cloud VMs | Nested virtualization must be enabled on the host hypervisor (AWS: metal instances or `--cpu-options AmdSevSnp=enabled`; GCP: enable nested virt on the instance; Azure: `Standard_D*v5` or `E*v5` VMs) |

The controller pod always mounts `/dev/kvm` via hostPath and runs privileged. Your cluster nodes must have `/dev/kvm` available.

The e2e scripts detect `/dev/kvm` inside kind nodes automatically. When KVM is unavailable the `api` and `operator` suites are skipped with a clear message rather than hanging.

## E2E tests

Requires `BOXY_E2E_BASE_URL` and `BOXY_E2E_ROUTER_TOKEN` env vars pointing at a running cluster.

```bash
make e2e-go
```

## curl examples

```bash
# Create a sandbox
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sessionId":"demo","sandboxId":"demo-1","owner":"you","ttlSeconds":3600}' \
  http://127.0.0.1:8080/v1/sandboxes | jq .

# Execute a command
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sessionId":"demo","sandboxId":"demo-1","command":"echo","args":["hello"],"env":{},"timeoutSeconds":60}' \
  http://127.0.0.1:8080/v1/exec | jq .

# Create with VM config and network policy
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d @- http://127.0.0.1:8080/v1/sandboxes <<'JSON' | jq .
{
  "sessionId": "demo",
  "sandboxId": "demo-2",
  "owner": "you",
  "ttlSeconds": 3600,
  "allowedBinaries": ["curl", "python3"],
  "vm": { "memoryMb": 1024, "vcpus": 2 },
  "network": { "allowedEgressDomains": ["api.github.com", "pypi.org"] }
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

# MCP: list tools
curl -sS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  http://127.0.0.1:8080/mcp | jq .

# MCP: execute bash (with explicit sandbox)
curl -sS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'X-Sandbox-Id: demo-1' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bash","arguments":{"command":"echo hello"}}}' \
  http://127.0.0.1:8080/mcp | jq .
```

## API status codes

| Code | Meaning |
|------|---------|
| `401` | Missing/invalid router bearer token. |
| `404` | Sandbox not found in routing store. |
| `413` | Output cap exceeded. |
| `429` | Concurrency limit. |
| `502` | Controller pod unreachable or returned an error. |
| `503` | Controller pod not ready (still starting). |

## Project layout

```
cmd/boxy-router/       # Router entry point (Go)
controller/            # Microsandbox controller (Rust)
  src/                 #   Axum HTTP server, sandbox manager, mTLS
  tests/
internal/
  api/                 # Shared types + validation
  kube/                # K8s helpers: route store, controller pod lifecycle, pod validation
  router/              # HTTP server, MCP server, mTLS client, sync reconciler, default sandbox
  session/             # TTL / reaper logic
deploy/
  helm/boxy/           # Helm chart (router, controller RBAC, mTLS secrets, NetworkPolicy)
  manifests/           # Standalone RBAC example
test/e2e/              # Go E2E tests
Dockerfile.router
Dockerfile.controller
```

## License

[MIT](LICENSE).
