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
13. [Key Trade-offs](#13-key-trade-offs)
14. [Operational Runbook Sketch](#14-operational-runbook-sketch)

---

## 1. What Is Boxy?

Boxy is a **Kubernetes-native sandbox runtime** that lets callers execute arbitrary shell commands inside isolated, ephemeral Linux environments — accessed through a clean HTTP API and optionally through MCP (Model Context Protocol) for AI clients.

Each sandbox is a lightweight **nsjail** process jail: isolated filesystem, network namespace, and resource limits, backed by a shared read-only Ubuntu 24.04 rootfs. There are no VMs, no hypervisors, no hardware requirements beyond a standard Linux kernel.

The system is built in three tiers:

| Tier | Language | Role |
|------|----------|------|
| Router | Go | Stateless HTTP frontend, auth, Kubernetes resource management |
| Operator | Go | Kubernetes controller — sandbox lifecycle, bin-packing, auto-scaling |
| Controller | Rust | Per-node nsjail daemon — runs actual sandboxes |

---

## 2. Use Cases

| Use Case | Key Properties Needed |
|----------|-----------------------|
| AI agent code execution (Claude, Codex) | Low-latency exec, stateful workspace per session, MCP support |
| CI step isolation | File isolation between jobs, reproducible rootfs |
| Multi-tenant interactive shells | Strong cross-tenant isolation, TTL enforcement |
| Sandboxed script evaluation | Output capture, timeout enforcement, resource caps |
| Secure API that runs user-supplied code | Input validation, network isolation, read-only OS |

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

- Validates bearer token (`Authorization: Bearer <BOXY_ROUTER_TOKEN>`)
- Parses and validates all API requests
- Creates/reads/deletes Sandbox CRs in Kubernetes
- Waits for a sandbox to reach `Running` phase before forwarding exec calls
- Proxies exec requests to the assigned controller pod over mTLS
- Runs an MCP server (`POST /mcp`) for AI clients that need a `bash` tool
- Optionally maintains a default sandbox (for stateless MCP clients)

**Key internal constraints:**

```
Max concurrent execs:    BOXY_MAX_CONCURRENCY        (default 100, semaphore-guarded)
Max request body:        BOXY_MAX_BODY_BYTES          (default 1 MB)
Max response output:     BOXY_MAX_OUTPUT_BYTES        (default 2 MB)
Max exec timeout:        BOXY_MAX_TIMEOUT_SECONDS     (default 3600s)
Create timeout:          BOXY_CREATE_TIMEOUT_SECONDS  (default 30s)
```

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

Written in Rust for low per-sandbox overhead. Exposes a small HTTP API (mTLS-only) consumed by the router and operator:

| Endpoint | Method | Action |
|----------|--------|--------|
| `/v1/sandboxes` | POST | Create a new sandbox (mkdir workspace, validate config) |
| `/v1/sandboxes/{id}/exec` | POST | Run a command in the sandbox via nsjail |
| `/v1/sandboxes/{id}` | DELETE | Remove sandbox (kill processes, clean workspace) |

Internally, each sandbox is just a directory at `/var/lib/boxy/sandboxes/{id}/workspace`. When exec is called, the controller spawns nsjail with that directory bind-mounted as `/workspace` inside the jail.

The `SandboxProvider` trait abstracts the isolation backend — today only `nsjail` is implemented, but the interface allows future providers (e.g. gVisor, microVMs):

```rust
trait SandboxProvider {
    async fn create_sandbox(&self, req: CreateSandboxReq) -> Result<()>;
    async fn exec(&self, sandbox_id: &str, req: ExecReq) -> Result<ExecResult>;
    async fn delete_sandbox(&self, sandbox_id: &str) -> Result<()>;
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
  network           NetworkConfig      enabled, allowInternetAccess, allowedEgressDomains
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

For each exec, the controller spawns an nsjail process with flags roughly equivalent to:

```
nsjail
  --mode o                                 # one-shot: exit when command exits
  --chroot /rootfs/ubuntu-24.04            # read-only base OS
  --bindmount /var/lib/boxy/.../workspace:/workspace   # persistent R/W
  --tmpfsmount /tmp                        # ephemeral scratch (discarded after exec)
  --hostname <hostname>
  --user <user>
  --cgroup_mem_max <memoryMb in bytes>
  --rlimit_as / --rlimit_nofile / ...      # POSIX resource limits
  --time_limit <timeoutSeconds>            # SIGKILL at expiry
  [--disable_clone_newnet]                 # only when allowInternetAccess = true
  -- <command> <args...>
```

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

| Feature | Status | Note |
|---------|--------|------|
| Network egress filtering (domain allowlists) | Not enforced | `allowedEgressDomains` parsed but nsjail has no firewall |
| vCPU count limits | Not enforced | `vm.vcpus` accepted but ignored |
| Per-exec workspace cleanup | N/A | `/workspace` is persistent by design; only `/tmp` is ephemeral |
| Custom rootfs image pull | N/A | `vm.image` must be a pre-baked path on the node; no image fetch |

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

| `network.allowInternetAccess` | Behavior |
|-------------------------------|----------|
| `false` (default) | nsjail creates isolated network namespace — sandbox has no external connectivity |
| `true` | `--disable_clone_newnet` — sandbox inherits the pod's host network |

---

## 9. Security Model

### Authentication & Authorization

```
Client -----(Bearer token)-----> Router
Router -----(mTLS: client cert + CA pin)-----> Controller
Operator ---(mTLS: client cert + CA pin)-----> Controller
```

- **One shared bearer token** gates all API access at the router. There is no per-sandbox or per-user RBAC — callers are trusted equally once they pass the token check.
- **mTLS** uses a Helm-generated CA with server and client certs. Controller pods verify the client cert against the CA; the router/operator verify the server cert against the same CA. No hostname verification — identity is CA membership, not DNS name.
- `BOXY_MTLS_DISABLED=true` removes mutual auth entirely; dev-only.

### Input Validation

| Input | Guard |
|-------|-------|
| `sandboxId` / `sessionId` / `owner` | Non-empty, required |
| `ttlSeconds` | 0–604800 (7 days) |
| `env` | Max 64 keys; blocked prefixes `KUBERNETES_*`, `BOXY_*`; values <= 16 KB |
| `command` + `args` | Max 256 args |
| `timeoutSeconds` | 1–3600 |
| Request body | <= 1 MB |
| Response output | <= 2 MB (truncated, not errored) |

### Container Capabilities (controller pod)

```
Dropped: ALL
Added:   SYS_ADMIN, SETUID, SETGID, NET_ADMIN, SYS_CHROOT, MKNOD, SETPCAP
runAsUser: 0  (root required for namespace setup)
allowPrivilegeEscalation: false
```

The controller runs as root inside its container because nsjail needs `SYS_ADMIN` and `NET_ADMIN` to create namespaces. Each sandbox process inside nsjail drops to the configured user (default `nobody`).

### Threat Model

| Threat | Mitigation |
|--------|-----------|
| Unauthorized API access | Bearer token on router |
| Router/operator impersonating each other toward controller | mTLS with shared CA |
| Sandbox escaping to host filesystem | nsjail mount namespace + R/O rootfs; only `/workspace` and `/tmp` are writable |
| Sandbox reaching other sandboxes over network | Isolated network namespace per sandbox |
| Sandbox exhausting host memory | `--cgroup_mem_max` enforced by nsjail |
| Sandbox running forever | `--time_limit` (nsjail SIGKILLs) + TTL sliding window (operator cleans up CR) |
| Malicious env var injection | Blocked prefixes; max key/value caps |
| Container breakout from controller | `allowPrivilegeEscalation: false`; caps minimal for nsjail only |

**Known gaps:**

- **No per-caller tenant isolation.** All callers share the same token and can name/access any sandbox by ID.
- **`allowedEgressDomains` not enforced.** Network domain allowlisting requires an external egress proxy or eBPF layer.
- **Controller pod runs as root.** A kernel exploit escaping nsjail would have root on the node.

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

| Step | Latency |
|------|---------|
| Bearer token check + body parse | < 1 ms |
| Kubernetes CR read (cached informer) | ~1 ms |
| mTLS dial to controller (connection established) | ~1 ms |
| nsjail spawn + exec (first exec in a sandbox) | 50–200 ms |
| nsjail exec (warm sandbox, small command) | 10–50 ms |
| K8s CR update (lastExecAt, async) | ~5 ms |

The dominant cost is nsjail process spawn. Each exec is a cold spawn — there is no persistent shell process kept alive between calls.

### Sandbox Creation Latency

| Step | Latency |
|------|---------|
| Kubernetes CR creation | ~10 ms |
| Operator reconcile (Pending -> Creating -> Running) | 100 ms – 1 s |
| Controller `POST /v1/sandboxes` (mkdir workspace) | < 10 ms |
| Total (P50, no scale-up needed) | ~200 ms – 500 ms |

Scale-up adds ~30 s (StatefulSet pod scheduling + image pull if not cached).

### Resource Footprint per Sandbox (at rest)

| Resource | Amount |
|----------|--------|
| Kubernetes objects | 1 CR (~2 KB) |
| Host filesystem | `/workspace` dir (empty until used) |
| Memory (at rest) | 0 — no persistent process |
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

**Node-local only.** `/var/lib/boxy/sandboxes/` is a hostPath — there is no distributed storage. A sandbox is pinned to a controller pod; if the pod is rescheduled to a different node, workspace data is lost. This is intentional — sandboxes are ephemeral and workspace data is scoped to the sandbox TTL.

Callers that need durable artifact storage should export files out of the sandbox via exec + stdout before the sandbox is deleted.

---

## 12. Failure Modes & Resilience

| Failure | Behavior |
|---------|---------|
| Router pod restart | Stateless; new pod picks up from Kubernetes cache immediately |
| Operator pod restart | New leader elected; reconciler re-drives all CRs from Kubernetes state |
| Controller pod restart | Sandbox CR stays `Running`; next exec gets a stale-route error; router resets CR to `Pending`; operator reassigns to another pod (workspace lost) |
| Controller pod rescheduled to new node | Same as restart; workspace dir on old node is orphaned (no automatic cleanup today) |
| Kubernetes API server slow | Router times out waiting for sandbox `Running`; returns 504 to client |
| nsjail OOM kill | Exec returns non-zero exit code + truncated stderr; not surfaced as a 5xx |
| Exec timeout | nsjail SIGKILLs child; exec returns exit code 137 |
| TTL expiry during active exec | Operator transitions sandbox to Deleting; in-flight exec may complete (nsjail process is unaffected by CR phase change) |
| Network partition (router <-> controller) | Exec returns 502; no state corruption; retry-safe |

---

## 13. Key Trade-offs

### nsjail vs. MicroVM (e.g. Firecracker)

| Dimension | nsjail (current) | MicroVM |
|-----------|------------------|---------|
| Isolation strength | Kernel namespaces + cgroups; shared host kernel | Hardware virtualization; separate guest kernel |
| Startup latency | ~50 ms | 100–300 ms |
| Memory overhead | ~30–50 MB per exec | ~128 MB+ per VM |
| KVM / hardware requirement | None (pure software) | `/dev/kvm` required |
| Container escape risk | Kernel exploit -> host root | Hypervisor escape is harder |
| Network egress filtering | No — needs external proxy | Can enforce at VM boundary |

**Decision rationale:** nsjail was chosen for simplicity and universality — any standard Linux node works. Trade-off: weaker security boundary than hardware virtualization and no native egress enforcement.

### StatefulSet for Controllers vs. Deployment

A StatefulSet gives controllers **stable DNS names** (`boxy-ctrl-0.boxy-ctrl-headless.boxy.svc.cluster.local`), which allows the operator to dial a specific pod reliably even after restarts. A Deployment would require a per-pod Service or an alternative discovery mechanism. The trade-off is that StatefulSet scaling is ordinal and more conservative (pods created/deleted in order).

### Single Bearer Token vs. Per-Caller RBAC

A single token is simple to operate and integrate with AI clients. The trade-off is that all callers are equally trusted — one leaked token exposes all sandboxes. Adding per-caller RBAC would require a token registry and changes to the router auth middleware.

### Bin-Packing vs. Round-Robin Assignment

Bin-packing (filling pods to capacity before adding a new one) minimizes the number of controller pods running at any time, reducing cost. Round-robin would spread load more evenly but waste capacity on underutilized pods. Bin-packing is preferred when sandboxes are lightweight and controller pods are the scaling unit.

### Cold Spawn per Exec vs. Persistent Shell

Each exec spawns a fresh nsjail process. This simplifies the controller (no process lifecycle management) and ensures clean process isolation. Trade-off: ~50–200 ms spawn overhead per exec. A persistent shell process inside the jail would reduce latency but would require tracking shell state and handling shell death.

---

## 14. Operational Runbook Sketch

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

### Scale Tuning

| Scenario | Knob |
|----------|------|
| Frequent "no capacity" requeues | Increase `BOXY_MAX_CONTROLLER_REPLICAS` |
| Controller pods scaling up too eagerly | Increase `BOXY_MAX_SANDBOXES_PER_CONTROLLER` |
| Idle pods staying up too long | Decrease `BOXY_SCALE_DOWN_COOLDOWN_SECONDS` |
| Disk filling up on controller nodes | Lower `ttlSeconds`; check for zombie `Terminated` CRs |
| High exec latency | Scale out router replicas; check controller pod CPU |

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

Workspace dirs on controller nodes are not garbage-collected if a pod is evicted. To clean up, cross-reference host directories against live Sandbox CRs and remove directories that no longer have a corresponding CR.
