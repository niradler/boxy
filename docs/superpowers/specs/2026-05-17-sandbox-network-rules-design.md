# Sandbox Lifecycle Hooks and Persistent Network Namespaces

Date: 2026-05-17

## Problem

1. Boxy has no customization point at sandbox creation/deletion. Users can't run setup logic when a sandbox is created (restore sessions, pre-install packages, configure networking, etc.).

2. Sandbox networking is binary: fully isolated or fully open. No middle ground because there's no per-sandbox netns with connectivity where policy could be applied.

## Goals

1. General-purpose sandbox lifecycle hooks (setup/teardown) at the `SandboxCreateBody` level.
2. Persistent per-sandbox network namespace with connectivity (macvlan, pasta, or veth).
3. Hooks and persistent netns are independent features that compose well together.
4. Backward compatible.

## Non-goals

- Boxy does not implement network policy. Users do, via hooks.
- Boxy does not run proxies, DNS filters, or eBPF programs.

## Design

### Feature 1: Sandbox lifecycle hooks

Top-level fields on `SandboxCreateBody`:

```go
type SandboxCreateBody struct {
    SandboxID       string                `json:"sandboxId"`
    TTLSeconds      int                   `json:"ttlSeconds,omitempty"`
    Env             map[string]string     `json:"env,omitempty"`
    AllowedBinaries []string              `json:"allowedBinaries,omitempty"`
    VM              *VMConfig             `json:"vm,omitempty"`
    Network         *SandboxNetworkConfig `json:"network,omitempty"`
    Volumes         []VolumeMount         `json:"volumes,omitempty"`
    Patches         []SandboxPatch        `json:"patches,omitempty"`

    // New
    SetupScript    string `json:"setupScript,omitempty"`
    TeardownScript string `json:"teardownScript,omitempty"`
}
```

`SetupScript` is a path to an executable on the controller filesystem. Boxy runs it after all sandbox resources are provisioned (workspace, bins, netns if applicable) but before returning success. It runs on the controller, not inside the sandbox.

`TeardownScript` runs before sandbox resources are cleaned up.

**Hook contract:**

- stdin: full `SandboxCreateBody` as JSON
- env:
  - `BOXY_SANDBOX_ID` -- sandbox identifier
  - `BOXY_SANDBOX_ROOT` -- sandbox base directory
  - `BOXY_WORKSPACE` -- workspace directory path
  - `BOXY_NETNS_NAME` -- netns name (only set if a persistent netns was created)
- exit 0: success
- exit non-zero: sandbox creation fails, all resources cleaned up

**Examples:**

```bash
#!/bin/bash
# Restore workspace from a previous session
aws s3 sync s3://sessions/$BOXY_SANDBOX_ID/ $BOXY_WORKSPACE/
```

```bash
#!/bin/bash
# Set up nftables egress allowlist in the sandbox's netns
nsenter --net=/var/run/netns/$BOXY_NETNS_NAME nft -f /etc/boxy/egress-allowlist.conf
```

```bash
#!/bin/bash
# Pre-install packages into workspace
cp -r /opt/boxy/preinstalled-packages/* $BOXY_WORKSPACE/
```

```bash
#!/bin/bash
# Start a transparent proxy in the sandbox netns
nsenter --net=/var/run/netns/$BOXY_NETNS_NAME bash -c '
  squid -f /etc/boxy/squid.conf -N &
  nft add table inet sandbox
  nft add chain inet sandbox output "{ type nat hook output priority -100; }"
  nft add rule inet sandbox output tcp dport != 3128 redirect to :3128
'
```

```bash
#!/bin/bash
# Read sandbox config from stdin and apply per-sandbox policy
CONFIG=$(cat)
CIDRS=$(echo "$CONFIG" | jq -r '.network.allowedCidrs[]?')
nsenter --net=/var/run/netns/$BOXY_NETNS_NAME bash -c "
  nft add table inet sandbox
  nft add chain inet sandbox output '{ type filter hook output priority 0; policy drop; }'
  nft add rule inet sandbox output ct state established,related accept
  nft add rule inet sandbox output oifname lo accept
  for cidr in $CIDRS; do
    nft add rule inet sandbox output ip daddr \$cidr accept
  done
"
```

The teardown script runs inside the netns before deletion, same env vars, no stdin. Used for cleanup (kill proxy processes, detach eBPF, etc.).

### Feature 2: Persistent network namespace with connectivity

**Trigger:** When `SandboxNetworkConfig` specifies a connectivity backend (`macvlan`, `usePasta`, or new `useVeth`).

```go
type SandboxNetworkConfig struct {
    Enabled             *bool          `json:"enabled,omitempty"`
    AllowInternetAccess bool           `json:"allowInternetAccess,omitempty"`
    Macvlan             *MacvlanConfig `json:"macvlan,omitempty"`
    UsePasta            bool           `json:"usePasta,omitempty"`
    UseVeth             bool           `json:"useVeth,omitempty"`
}
```

**Create-time flow:**

```text
If macvlan, usePasta, or useVeth is set:
  1. Create named netns: ip netns add boxy-<sandboxID>
  2. Set up connectivity:
     - macvlan: create macvlan iface in the netns
     - pasta: start pasta process attached to the netns
     - veth: veth pair, one end in netns, other in pod netns with masquerade
  3. Bind-mount /etc/resolv.conf and CA certs into sandbox workspace
  4. Store netns name on sandbox struct
  5. Set BOXY_NETNS_NAME env var for setup script
```

This runs before the lifecycle hooks.

**Exec-time flow:**

```text
If sandbox has netnsName:
  1. Set DisableCloneNewNet = true (clone_newnet: false)
  2. Prepend: nsenter --net=/var/run/netns/boxy-<id>
  3. Run nsjail as usual
```

**Delete-time flow:**

```text
1. Run TeardownScript (if set)
2. Kill pasta process (if pasta was used)
3. ip netns del boxy-<sandboxID>
4. Remove workspace dirs (existing)
```

### How the two features compose

| Configuration | What happens |
|---|---|
| No hooks, no netns backend | Unchanged behavior |
| `setupScript` only | Hook runs at create time. No network changes. |
| `useVeth` only | Own netns with NAT'd connectivity, no policy. |
| `useVeth` + `setupScript` | Own netns with connectivity, then hook runs. Hook can nsenter and apply nftables, start proxy, etc. |
| `usePasta` + `setupScript` | Same, pasta connectivity. |
| `macvlan` + `setupScript` | Same, macvlan connectivity. |
| `allowInternetAccess: true` | Unchanged -- shares pod netns. |

### Internal changes

| Component | Change |
|---|---|
| `SandboxCreateBody` | Add `SetupScript`, `TeardownScript` fields |
| `SandboxNetworkConfig` | Add `UseVeth` field. Dead fields removed (Rules, Ports, DNS, Secrets, MaxConnections, TrustHostCAs). |
| `sandbox` struct | Add `netnsName string`, `pastaCmd *exec.Cmd` fields |
| `NsjailAdapter.Create` | Set up persistent netns if connectivity backend specified, then run setup script if set. |
| `NsjailAdapter.Exec` | If `sb.netnsName != ""`, prepend `nsenter --net=...`, set `DisableCloneNewNet = true` |
| `NsjailAdapter.Delete` | Run teardown script, clean up netns, then existing cleanup |
| New: `internal/netns/` | Netns create/delete, connectivity setup (veth/macvlan/pasta) |
| New: `internal/hooks/` | Script runner with stdin/env/timeout handling |
| Helm chart | No changes (`CAP_NET_ADMIN` already on controller) |
| Controller image | `nft` binary (for user scripts). `pasta` binary if pasta used. |

### Backward compatibility

No existing behavior changes. Both features activate only when new fields are set.

### Security considerations

- `SetupScript` path comes from the operator/CRD. The operator is trusted.
- Scripts run as the controller process, not inside the sandbox.
- `CAP_NET_ADMIN` is already granted to the controller pod.
- Named netns files cleaned up on delete. Controller startup cleans orphan `boxy-*` entries.
- Sandbox processes cannot modify nftables rules -- nsjail drops `CAP_NET_ADMIN`.

### Testing strategy

1. **Unit tests** (`internal/netns/`): netns create/delete, veth setup.
2. **Unit tests** (`internal/hooks/`): script runner with mock scripts, stdin piping, env vars, exit code handling.
3. **Integration tests** (`internal/nsjail/adapter_test.go`): exec with named netns produces correct nsjail config and nsenter wrapper.
4. **E2E tests**: sandbox with setup script that blocks egress, verify enforcement. Sandbox with setup script that copies files, verify files present.

### Future work

- Ship reference setup scripts for common patterns (egress allowlist, transparent proxy).
- Network profiles -- named presets selectable by name.
