# Boxy — System Design & Architecture

## Table of Contents

1. [What Is Boxy?](#1-what-is-boxy)
2. [Use Cases](#2-use-cases)
3. [High-Level Architecture](#3-high-level-architecture)
4. [Component Deep Dives](#4-component-deep-dives)
5. [Data Model](#5-data-model)
6. [Request Flow](#6-request-flow)
7. [Sandboxing Internals](#7-sandboxing-internals)
8. [Networking](#8-networking)
9. [Security Model](#9-security-model)
10. [Scaling & Performance](#10-scaling--performance)
11. [Storage](#11-storage)
12. [Failure Modes & Resilience](#12-failure-modes--resilience)
13. [Operational Runbook Sketch](#13-operational-runbook-sketch)

---

## 1. What Is Boxy?

Boxy is a **Kubernetes-native sandbox runtime** that lets callers execute arbitrary shell commands inside isolated, ephemeral Linux environments — accessed through a clean HTTP API and optionally through MCP (Model Context Protocol) for AI clients.

Each sandbox is a lightweight **nsjail** process jail: isolated filesystem, network namespace, and resource limits, backed by a shared read-only Ubuntu 24.04 rootfs. There are no VMs, no hypervisors, no hardware requirements beyond a standard Linux kernel.

The system is built in three tiers:

| Tier       | Language | Role                                                                 |
| ---------- | -------- | -------------------------------------------------------------------- |
| Router     | Go       | Stateless HTTP frontend, auth, Kubernetes resource management        |
| Operator   | Go       | Kubernetes controller — sandbox lifecycle, bin-packing, auto-scaling |
| Controller | Go       | Per-node nsjail daemon — runs actual sandboxes                       |

---

## 2. Use Cases

| Use Case                                | Key Properties Needed                                         |
| --------------------------------------- | ------------------------------------------------------------- |
| AI agent code execution (Claude, Codex) | Low-latency exec, stateful workspace per session, MCP support |
| CI step isolation                       | File isolation between jobs, reproducible rootfs              |
| Multi-tenant interactive shells         | Strong cross-tenant isolation, TTL enforcement                |
| Sandboxed script evaluation             | Output capture, timeout enforcement, resource caps            |
| Secure API that runs user-supplied code | Input validation, network isolation, read-only OS             |

---

## 3. High-Level Architecture

```
                              +---------------------------------------------------+
                              |              Kubernetes Cluster                   |
                              |                                                   |
  Client / AI Agent           |  +------------------------------------------+   |
  +--------------+            |  |          boxy namespace                   |   |
  |              | HTTP       |  |                                           |   |
  | REST Client  +------------+--+  +-------------+  CRD Watch  +--------+  |   |
  |  (Bearer)    |            |  |  | boxy-router |             | boxy-  |  |   |
  |              |            |  |  | (Deployment)|<----------->|operator|  |   |
  | MCP Client   +------------+--+  |             |             |(Depl.) |  |   |
  +--------------+            |  |  +------+------+             +----+---+  |   |
                              |  |         | mTLS                    |      |   |
                              |  |         |                StatefulSet     |   |
                              |  |  +------v------+         scale control   |   |
                              |  |  |boxy-ctrl-0  |              |      |   |   |
                              |  |  |(StatefulSet)|<-------------+      |   |   |
                              |  |  | +---------+ |                     |   |   |
                              |  |  | | nsjail  | |  +-----------------+|   |   |
                              |  |  | |sandbox-A| |  |  boxy-ctrl-1    ||   |   |
                              |  |  | +---------+ |  |  +-----------+  ||   |   |
                              |  |  | +---------+ |  |  | nsjail    |  ||   |   |
                              |  |  | | nsjail  | |  |  | sandbox-C |  ||   |   |
                              |  |  | |sandbox-B| |  |  +-----------+  ||   |   |
                              |  |  | +---------+ |  +-----------------+|   |   |
                              |  |  +-------------+                     |   |   |
                              |  +------------------------------------------+   |
                              +---------------------------------------------------+
```

**Key design axiom:** the router and operator are stateless/leader-elected and can be freely scaled or restarted. All sandbox state lives in Kubernetes (Sandbox CRs) and on the controller node filesystem (`/var/lib/boxy/sandboxes/`). A controller pod restart loses in-memory sandbox processes but the workspace data on disk is preserved.

---

## 4. Component Deep Dives

### 4.1 boxy-router

**Deployment:** Kubernetes Deployment (horizontal scale via HPA).

The router is the only public-facing component. It:

- Validates bearer token (`Authorization: Bearer <token>`) — SA token via TokenReview, or static dev bypass if `BOXY_ROUTER_TOKEN` is set
- Parses and validates all API requests
- Creates/reads/deletes Sandbox CRs in Kubernetes
- Waits for a sandbox to reach `Running` phase before forwarding exec calls
- Proxies exec requests to the assigned controller pod over mTLS
- Runs an MCP server (`POST /mcp`) for AI clients that need a `bash` tool
- Optionally maintains a default sandbox (for stateless MCP clients)

**Key internal constraints:**

```
Max concurrent execs:    BOXY_MAX_CONCURRENCY        (default 100, semaphore-guarded)
Max request body:        BOXY_MAX_BODY_BYTES          (default 6 MB — bodies over this limit return HTTP 400)
Max exec timeout:        BOXY_MAX_TIMEOUT_SECONDS     (default 3600s)
Create timeout:          BOXY_CREATE_TIMEOUT_SECONDS  (default 30s)
```

Output size enforcement is at the **controller**, not the router — see §4.3.

**Stale-route detection:** When the router dials a controller and gets an error indicating the sandbox process no longer exists (stale route), it resets the Sandbox CR back to `Pending`, triggering reassignment by the operator. This handles controller pod restarts transparently.

### 4.2 boxy-operator

**Deployment:** Kubernetes Deployment with leader election (1 active replica at a time).

The operator is a standard controller-runtime reconciler watching Sandbox CRs. It drives a state machine:

```
              Create CR         Assign pod        Create on ctrl
             ---------->  Pending  --------->  Creating  -------->  Running
                                                                       |
                                                               TTL expires / delete
                                                                       |
                                                                  Deleting
                                                                       |
                                                               Delete on ctrl
                                                                       |
                                                                 Terminated
                                                          (retained N seconds, then GC'd)
```

**Bin-packing algorithm:**

```
For each controller pod (ordered by ordinal):
  count = active sandboxes assigned to this pod
  if count < BOXY_MAX_SANDBOXES_PER_CONTROLLER:
    assign here, break
If no pod has capacity:
  scale up StatefulSet (add 1 replica, up to BOXY_MAX_CONTROLLER_REPLICAS)
  requeue sandbox after 5s
```

**Auto-scaling loop** (runs every 60 seconds):

```
For each pod from highest ordinal downward:
  if no active sandboxes AND pod age > BOXY_SCALE_DOWN_COOLDOWN_SECONDS:
    remove replica (scale StatefulSet down by 1)
Floor: never go below BOXY_MIN_CONTROLLER_REPLICAS
```

**TTL management:**

- `expiresAt = createdAt + ttlSeconds` (initial)
- On every exec, the router updates `lastExecAt` on the CR
- Operator recomputes `expiresAt = lastExecAt + ttlSeconds` (sliding window)
- When `now > expiresAt`, operator transitions Running -> Deleting

### 4.3 boxy-controller

**Deployment:** Kubernetes StatefulSet (stable DNS names: `boxy-ctrl-{n}.boxy-ctrl-headless.boxy.svc.cluster.local`).

Written in Go. Exposes a small HTTP API (mTLS-only) consumed by the router and operator:

| Endpoint        | Method | Action                                                  |
| --------------- | ------ | ------------------------------------------------------- |
| `/v1/sandboxes` | POST   | Create a new sandbox (mkdir workspace, validate config) |
| `/v1/sandboxes` | GET    | List active sandbox IDs                                 |
| `/v1/sandboxes` | DELETE | Remove sandbox (clean workspace)                        |
| `/v1/exec`      | POST   | Run a command in the sandbox via nsjail                 |
| `/healthz`      | GET    | Liveness check                                          |

Internally, each sandbox is a directory at `/var/lib/boxy/sandboxes/{id}/workspace`. When exec is called, the controller builds an `NsjailConfig` struct, serializes it to protobuf text format, writes it to a temp file, and runs `nsjail --config <file> -- <cmd> <args>`. The temp file is removed after nsjail exits.

**Per-pod concurrency limit:** A semaphore (`BOXY_MAX_EXEC_CONCURRENCY`, default 50) limits parallel exec calls per controller pod. When full, the controller returns HTTP 429. The router has its own independent semaphore (`BOXY_MAX_CONCURRENCY`, default 100).

**Output truncation:** `BOXY_MAX_OUTPUT_BYTES` (default 6 MB) caps the combined size of stdout and stderr returned per exec. Output over this threshold is truncated with a `\n[output truncated]` suffix. The response still returns `200 OK` — the truncation is a data cap, not an error condition.

**Package pre-installation:** `BOXY_PREINSTALL_PACKAGES` (comma-separated) installs apt packages into the Ubuntu rootfs via chroot at pod startup before the controller process starts. This makes those packages available to all sandboxes on the pod as part of the base rootfs. Combined with `allowedBinaries` (per-sandbox bind-mount allowlist), this provides two-level binary control: which packages exist on the pod, and which binaries each sandbox can access.

**Env var isolation:** Sandboxes receive only explicitly configured environment variables. The controller injects exactly two baseline keys — `PATH` (standard search path) and `HOME=/workspace` (needed by tools like npm, pip, and git that expect a writable HOME) — then appends sandbox-level env, then per-exec env overrides. No host environment variables leak into sandboxes.

**User identity:** Sandbox processes run as uid 0 (root) inside the nsjail container. Non-root uid mapping requires Linux user namespaces, which are blocked by most container runtimes (Docker Desktop, kind) when `clone_newuser` is combined with the CAP_SYS_ADMIN requirement for namespace setup. The security boundary is enforced by mount, PID, and network namespace isolation rather than uid separation.

The `Adapter` interface abstracts the isolation backend — today only `nsjail` is implemented, but the interface allows future providers (e.g. gVisor, microVMs):

```go
type Adapter interface {
    Create(ctx context.Context, req *api.SandboxCreateBody) error
    Exec(ctx context.Context, sandboxID string, command string, args []string, env map[string]string, timeoutSecs int) (*api.ExecResponseBody, error)
    Delete(ctx context.Context, sandboxID string) error
    ListIDs() []string
    Count() int
}
```

---

## 5. Data Model

### Sandbox CR (Kubernetes Custom Resource)

```
SandboxSpec
  sandboxId         string             Unique ID (client-supplied or generated)
  sessionId         string             Session correlation
  owner             string             Ownership label
  ttlSeconds        int                Sliding TTL; 0 = no expiry
  env               map[string]string  Sandbox-level env vars (max 64 keys)
  allowedBinaries   []string           Binaries bind-mounted read-only into sandbox
  vm                VMConfig           memoryMb, rlimits, workdir, hostname, user, rootfs image
  network           NetworkConfig      enabled, allowInternetAccess
  volumes           []VolumeMount      Extra mounts (tmpfs, bind)

SandboxStatus
  phase             Pending | Creating | Running | Deleting | Terminated
  controllerPod     string             Assigned StatefulSet pod name
  controllerAddress string             Pod DNS name for mTLS dialing
  port              int32              Controller port
  createdAt         timestamp
  expiresAt         timestamp          Updated on each exec (sliding window)
  terminatedAt      timestamp
  lastExecAt        timestamp
  message           string             Error / status detail
```

---

## 6. Request Flow

### Create Sandbox

```
Client            Router          Kubernetes        Operator         Controller
  |                 |                 |                 |                 |
  |-- POST /v1/sandboxes ------------>|                 |                 |
  |                 |-- Create CR --->|                 |                 |
  |                 |                 |-- reconcile --->|                 |
  |                 |                 |                 |-- bin-pack      |
  |                 |                 |<-- Creating ----|                 |
  |                 |                 |                 |-- POST /v1/sandboxes ->|
  |                 |                 |                 |<-- 200 OK ------|
  |                 |                 |<-- Running -----|                 |
  |                 |<-- CR Running --|                 |                 |
  |<-- 201 Created -|                 |                 |                 |
```

### Execute Command

```
Client            Router           Kubernetes       Controller
  |                 |                  |                |
  |-- POST /sandboxes/{id}/exec ------->                |
  |                 |-- Read CR ------->                |
  |                 |<-- controllerAddress              |
  |                 |-- POST /v1/sandboxes/{id}/exec -->|
  |                 |                  |         nsjail spawn + exec
  |                 |                  |         capture stdout/stderr
  |                 |<-- ExecResult ----|                |
  |                 |-- Update lastExecAt ->             |
  |<-- 200 { stdout, stderr, exitCode } |                |
```

---

## 7. Sandboxing Internals

### nsjail Invocation Model

For each exec the controller builds an `NsjailConfig` struct, serializes it to [protobuf text format](https://developers.google.com/protocol-buffers/docs/text_format_spec), writes it to a temp file, then runs:

```
nsjail --config /tmp/nsjail-<random>.pb.txt -- <command> <args...>
```

The temp file is removed after nsjail exits. Example config content:

```
mode: ONCE
log: "/dev/null"
chroot: "/rootfs/ubuntu-24.04"
disable_clone_newuser: true
disable_clone_newnet: true
time_limit: 30
cgroup_mem_max: 536870912
envar: "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
envar: "HOME=/workspace"
mount {
  src: "/var/lib/boxy/sandboxes/sandbox-A/workspace"
  dst: "/workspace"
  rw: true
  is_bind: true
}
mount {
  dst: "/tmp"
  fstype: "tmpfs"
  rw: true
}
```

`uidmap`/`gidmap` are omitted — sandbox processes run as uid 0. Linux user namespaces (`clone_newuser`) are disabled because most container runtimes block nested uid mapping; the security boundary is enforced by mount/PID/network namespace isolation instead.

Optional fields (`seccomp_string`, `clone_newtime`, `iface_vs*` for MACVLAN, `use_pasta`) are emitted only when set.

### Isolation Layers

```
+------------------------------------------------------------------+
|  Kubernetes Pod (boxy-ctrl-N)                                    |
|                                                                  |
|  +------------------------------------------------------------+  |
|  |  nsjail process (sandbox-A)                               |  |
|  |                                                            |  |
|  |   Mount NS:   /           <- /rootfs/ubuntu-24.04  (R/O)  |  |
|  |               /workspace  <- /var/lib/.../sandbox-A (R/W) |  |
|  |               /tmp        <- fresh tmpfs (ephemeral)       |  |
|  |   Network NS: isolated (no external access by default)     |  |
|  |   UTS NS:     custom hostname                              |  |
|  |   PID NS:     isolated process tree                        |  |
|  |   Cgroup:     memoryMb cap (cgroup_mem_max)                |  |
|  |   rlimits:    as, cpu, fsize, nofile, nproc, stack         |  |
|  |   Timeout:    SIGKILLed at timeoutSeconds                  |  |
|  +------------------------------------------------------------+  |
|                                                                  |
|  +------------------------------------------------------------+  |
|  |  nsjail process (sandbox-B)  -- fully independent          |  |
|  +------------------------------------------------------------+  |
+------------------------------------------------------------------+
```

### What nsjail Does NOT Enforce

| Feature                                      | Status       | Note                                                            |
| -------------------------------------------- | ------------ | --------------------------------------------------------------- |
| vCPU count limits                            | Not enforced | `vm.vcpus` accepted but ignored                                 |
| Per-exec workspace cleanup                   | N/A          | `/workspace` is persistent by design; only `/tmp` is ephemeral  |
| Custom rootfs image pull                     | N/A          | `vm.image` must be a pre-baked path on the node; no image fetch |

---

## 8. Networking

### Service Topology

```
External / Cluster Clients
         |
         | HTTP :8080 (ClusterIP)
         v
  +--------------+
  |  boxy-router |  <- Service: boxy-router (ClusterIP)
  +------+-------+
         | mTLS (client cert + CA pin)
         | Headless Service DNS:
         |   boxy-ctrl-{n}.boxy-ctrl-headless.boxy.svc.cluster.local
         v
  +-------------------------------+
  |  boxy-ctrl-0                  |
  |  boxy-ctrl-1                  |  <- StatefulSet
  |  boxy-ctrl-N                  |
  +-------------------------------+
```

### NetworkPolicy (controller pods)

Default-deny all egress except:

- Port 53 TCP/UDP (DNS)
- Pod selector: `boxy-router` (responses to router-initiated connections)

Sandbox-level network isolation is enforced by nsjail (isolated network namespace), not by Kubernetes NetworkPolicy. NetworkPolicy protects the controller pod; nsjail protects cross-sandbox traffic.

### Per-Sandbox Network Modes

| Setting | Behavior |
| ------- | -------- |
| `allowInternetAccess: false` (default) | nsjail creates an isolated network namespace — sandbox has no external connectivity |
| `allowInternetAccess: true` | `disable_clone_newnet: true` in proto — sandbox inherits the pod's host network |
| `macvlan: { interface: "eth0", ... }` | Clones a MACVLAN interface into the sandbox network namespace with an optional static IP/gateway |
| `usePasta: true` | Uses pasta userland networking — sandbox gets NAT'd internet access without `NET_ADMIN` |

---

## 9. Security Model

### Authentication & Authorization

```
Client -----(Bearer token: SA or static)-----> Router
Router -----(mTLS: client cert + CA pin)-----> Controller
Operator ---(mTLS: client cert + CA pin)-----> Controller
```

- **Router auth** has two modes: (1) **SA token** — any valid Kubernetes ServiceAccount token, validated via the TokenReview API; caller identity is the K8s `UserInfo` (username + groups); (2) **Static token** — `BOXY_ROUTER_TOKEN`, a dev/e2e bypass accepted without a TokenReview call. There is no per-caller RBAC — all authenticated callers have equal access.
- **mTLS** uses a Helm-generated CA with server and client certs. Controller pods verify the client cert against the CA; the router/operator verify the server cert against the same CA. No hostname verification — identity is CA membership, not DNS name.
- `BOXY_MTLS_DISABLED=true` removes mutual auth entirely; dev-only.

### Input Validation

| Input                               | Guard                                                                       |
| ----------------------------------- | --------------------------------------------------------------------------- |
| `sandboxId` / `sessionId` / `owner` | Non-empty, required                                                         |
| `ttlSeconds`                        | 0–604800 (7 days)                                                           |
| `env`                               | Max 64 keys; blocked prefixes `KUBERNETES_*`, `BOXY_*`; values <= 16 KB     |
| `command` + `args`                  | Max 256 args                                                                |
| `timeoutSeconds`                    | 1–3600                                                                      |
| Request body                        | <= 6 MB (enforced at router via `http.MaxBytesReader`; returns HTTP 400)    |
| Exec output (stdout + stderr each)  | <= 6 MB per field (enforced at controller; truncated with notice, not HTTP error) |

### Container Capabilities (controller pod)

```
Dropped: ALL
Added:   SYS_ADMIN, SETUID, SETGID, NET_ADMIN, SYS_CHROOT, MKNOD, SETPCAP
runAsUser: 0  (root required for namespace setup)
allowPrivilegeEscalation: false
```

The controller runs as root inside its container because nsjail needs `SYS_ADMIN` and `NET_ADMIN` to create namespaces. Sandbox processes also run as uid 0 — see §4.3 for why user namespace-based uid remapping is disabled.

### Threat Model

| Threat                                                     | Mitigation                                                                     |
| ---------------------------------------------------------- | ------------------------------------------------------------------------------ |
| Unauthorized API access                                    | Bearer token on router — SA token via TokenReview (production) or static dev bypass |
| Router/operator impersonating each other toward controller | mTLS with shared CA                                                            |
| Sandbox escaping to host filesystem                        | nsjail mount namespace + R/O rootfs; only `/workspace` and `/tmp` are writable |
| Sandbox reaching other sandboxes over network              | Isolated network namespace per sandbox                                         |
| Sandbox exhausting host memory                             | `--cgroup_mem_max` enforced by nsjail                                          |
| Sandbox running forever                                    | `--time_limit` (nsjail SIGKILLs) + TTL sliding window (operator cleans up CR)  |
| Malicious env var injection                                | Blocked prefixes; max key/value caps                                           |
| Container breakout from controller                         | `allowPrivilegeEscalation: false`; caps minimal for nsjail only                |

**Known gaps:**

- **No per-caller tenant isolation.** All callers share the same token and can name/access any sandbox by ID.
- **Sandbox processes run as uid 0.** Linux user namespace-based uid remapping is disabled (blocked by Docker Desktop / most container runtimes when combined with CAP_SYS_ADMIN). The security boundary is mount/PID/network namespace isolation rather than uid separation.

---

## 10. Scaling & Performance

### Horizontal Scaling Model

```
+---------------------------------------------------------------------------+
|                         Capacity Planning                                 |
|                                                                           |
|  boxy-router:    stateless, HPA-scalable                                  |
|                  semaphore at BOXY_MAX_CONCURRENCY (default 100)          |
|                                                                           |
|  boxy-operator:  leader-elected; 1 active replica                        |
|                                                                           |
|  boxy-controller: StatefulSet, auto-scaled by operator                   |
|    Scale-up:  new sandbox, no pod has capacity                            |
|    Scale-down: pod idle > BOXY_SCALE_DOWN_COOLDOWN_SECONDS                |
|    Capacity:  replicas x BOXY_MAX_SANDBOXES_PER_CONTROLLER               |
|    Example:   50 pods x 20 sandboxes = 1000 concurrent sandboxes max     |
+---------------------------------------------------------------------------+
```

### Latency Budget (typical exec)

| Step                                             | Latency   |
| ------------------------------------------------ | --------- |
| Bearer token check + body parse                  | < 1 ms    |
| Kubernetes CR read (cached informer)             | ~1 ms     |
| mTLS dial to controller (connection established) | ~1 ms     |
| nsjail spawn + exec (first exec in a sandbox)    | 50–200 ms |
| nsjail exec (warm sandbox, small command)        | 10–50 ms  |
| K8s CR update (lastExecAt, async)                | ~5 ms     |

The dominant cost is nsjail process spawn. Each exec is a cold spawn — there is no persistent shell process kept alive between calls.

### Sandbox Creation Latency

| Step                                                | Latency          |
| --------------------------------------------------- | ---------------- |
| Kubernetes CR creation                              | ~10 ms           |
| Operator reconcile (Pending -> Creating -> Running) | 100 ms – 1 s     |
| Controller `POST /v1/sandboxes` (mkdir workspace)   | < 10 ms          |
| Total (P50, no scale-up needed)                     | ~200 ms – 500 ms |

Scale-up adds ~30 s (StatefulSet pod scheduling + image pull if not cached).

### Resource Footprint per Sandbox (at rest)

| Resource             | Amount                                    |
| -------------------- | ----------------------------------------- |
| Kubernetes objects   | 1 CR (~2 KB)                              |
| Host filesystem      | `/workspace` dir (empty until used)       |
| Memory (at rest)     | 0 — no persistent process                 |
| Memory (during exec) | Command RSS + nsjail overhead (~30–50 MB) |

---

## 11. Storage

### Storage Layers

```
+----------------------------------------------------------------------+
|  Controller Node Filesystem                                          |
|                                                                      |
|  /rootfs/ubuntu-24.04/            <- baked into controller image    |
|    (shared R/O across all sandboxes on this pod)                     |
|                                                                      |
|  /var/lib/boxy/sandboxes/                                            |
|    {sandbox-id}/                                                     |
|      workspace/       <- persistent R/W, survives across execs       |
|                          lives until sandbox is deleted              |
|                                                                      |
|  /tmp (nsjail tmpfs)  <- ephemeral per-exec, in-kernel, discarded   |
+----------------------------------------------------------------------+
```

**Node-local only.** `/var/lib/boxy/sandboxes/` is an `emptyDir` volume — there is no distributed storage. A sandbox is pinned to a controller pod; if the pod is deleted or rescheduled, workspace data is lost. This is intentional — sandboxes are ephemeral and workspace data is scoped to the sandbox TTL. Because `emptyDir` is pod-scoped, kubelet cleans up workspace dirs automatically on pod termination.

Callers that need durable artifact storage should export files out of the sandbox via exec + stdout before the sandbox is deleted.

---

## 12. Failure Modes & Resilience

| Failure                                   | Behavior                                                                                                                                          |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| Router pod restart                        | Stateless; new pod picks up from Kubernetes cache immediately                                                                                     |
| Operator pod restart                      | New leader elected; reconciler re-drives all CRs from Kubernetes state                                                                            |
| Controller pod restart                    | Sandbox CR stays `Running`; next exec gets a stale-route error; router resets CR to `Pending`; operator reassigns to another pod (workspace lost) |
| Controller pod rescheduled to new node    | Same as restart; workspace data is lost (emptyDir is pod-scoped; no orphaned dirs)                                                               |
| Kubernetes API server slow                | Router times out waiting for sandbox `Running`; returns 504 to client                                                                             |
| nsjail OOM kill                           | Exec returns non-zero exit code + truncated stderr; not surfaced as a 5xx                                                                         |
| Exec timeout                              | nsjail SIGKILLs child; exec returns exit code 137                                                                                                 |
| TTL expiry during active exec             | Operator transitions sandbox to Deleting; in-flight exec may complete (nsjail process is unaffected by CR phase change)                           |
| Network partition (router <-> controller) | Exec returns 502; no state corruption; retry-safe                                                                                                 |

---

## 13. Operational Runbook Sketch

### Health Checks

```
# Router liveness
GET http://boxy-router:8080/healthz

# Check operator is holding leader election
kubectl logs -n boxy deploy/boxy-operator | grep "elected"

# Controller pods readiness
kubectl get pods -n boxy -l boxy.dev/controller=true

# Sandboxes stuck in Creating (operator not reconciling)
kubectl get sandbox -n boxy -o wide | grep Creating
```

### mTLS Certificate Management

Generate CA + server + client certs for a production deployment:

```bash
# Generate to ./certs/ with default 3-year validity
bash local/gen-mtls-certs.sh

# Custom output dir and validity (e.g. 1 year)
bash local/gen-mtls-certs.sh ./my-certs 365

# Load into Kubernetes and deploy
kubectl -n boxy create secret generic boxy-mtls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=tls.crt=certs/server.crt \
  --from-file=tls.key=certs/server.key

kubectl -n boxy create secret generic boxy-mtls-client \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=tls.crt=certs/client.crt \
  --from-file=tls.key=certs/client.key
```

The script generates a 4096-bit CA, a 2048-bit server cert (SAN includes the headless service DNS pattern), and a 2048-bit client cert with `extendedKeyUsage=clientAuth`. Validity is configurable; default is 1095 days (3 years).

### Scale Tuning

| Scenario                               | Knob                                                  |
| -------------------------------------- | ----------------------------------------------------- |
| Frequent "no capacity" requeues        | Increase `BOXY_MAX_CONTROLLER_REPLICAS`               |
| Controller pods scaling up too eagerly | Increase `BOXY_MAX_SANDBOXES_PER_CONTROLLER`          |
| Idle pods staying up too long          | Decrease `BOXY_SCALE_DOWN_COOLDOWN_SECONDS`           |
| Disk filling up on controller nodes    | Lower `ttlSeconds`; check for zombie `Terminated` CRs; workspace dirs are emptyDir-scoped so they vanish with the pod |
| High exec latency                      | Scale out router replicas; check controller pod CPU   |

### Debug a Stuck Sandbox

```
# Inspect sandbox status and assigned controller
kubectl get sandbox -n boxy {id} -o yaml

# Operator logs for this sandbox ID
kubectl logs -n boxy deploy/boxy-operator | grep {id}

# Manually reset to Pending if stuck in Creating
kubectl patch sandbox {id} -n boxy --type=merge -p '{"status":{"phase":"Pending"}}'

# Controller pod logs
kubectl logs -n boxy boxy-ctrl-{n}
```

### Disk Cleanup (orphaned workspaces)

Not applicable. Workspace dirs live in an `emptyDir` volume scoped to the controller pod — kubelet removes them automatically when the pod is deleted or rescheduled. No manual cleanup is needed.
