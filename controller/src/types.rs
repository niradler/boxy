use serde::{Deserialize, Serialize};
use std::collections::HashMap;

#[derive(Debug, Clone, Deserialize)]
pub struct CreateSandboxRequest {
    pub sandbox_id: String,
    pub env: Option<HashMap<String, String>>,
    pub allowed_binaries: Option<Vec<String>>,
    pub vm: Option<VmConfig>,
    pub network: Option<NetworkConfig>,
    pub volumes: Option<Vec<VolumeMount>>,
    pub patches: Option<Vec<SandboxPatch>>,
    pub ttl_seconds: Option<u64>,
}

/// VM resource and runtime config (per-request overrides).
#[derive(Debug, Clone, Deserialize)]
pub struct VmConfig {
    /// OCI image reference for the VM rootfs. Required.
    pub image: Option<String>,
    pub memory_mb: Option<u32>,
    pub vcpus: Option<u8>,
    pub workdir: Option<String>,
    pub shell: Option<String>,
    pub hostname: Option<String>,
    pub user: Option<String>,
    pub max_duration_sec: Option<u64>,
    pub idle_timeout_sec: Option<u64>,
    pub rlimits: Option<Vec<VmRlimit>>,
    pub scripts: Option<Vec<VmScript>>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct VmRlimit {
    /// "nofile", "nproc", "memlock", "msgqueue", "sigpending", "nice", "rtprio", "rttime"
    pub resource: String,
    pub soft: u64,
    pub hard: u64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct VmScript {
    pub name: String,
    pub content: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct NetworkConfig {
    pub enabled: Option<bool>,
    // Simple shortcuts
    pub allow_internet_access: Option<bool>,
    pub allowed_egress_domains: Option<Vec<String>>,
    // Advanced
    pub rules: Option<Vec<NetworkRule>>,
    pub ports: Option<Vec<PortMapping>>,
    pub dns: Option<DnsConfig>,
    pub secrets: Option<Vec<NetworkSecret>>,
    pub max_connections: Option<usize>,
    pub trust_host_cas: Option<bool>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct NetworkRule {
    /// "egress", "ingress", or "any"
    pub direction: String,
    /// "allow" or "deny"
    pub action: String,
    pub protocols: Option<Vec<String>>,
    pub ports: Option<Vec<u16>>,
    pub port_ranges: Option<Vec<PortRange>>,
    pub domains: Option<Vec<String>>,
    pub domain_suffixes: Option<Vec<String>>,
    pub cidrs: Option<Vec<String>>,
    /// "public", "private", "loopback", "link_local", "metadata", "multicast", "host"
    pub groups: Option<Vec<String>>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct PortRange {
    pub start: u16,
    pub end: u16,
}

#[derive(Debug, Clone, Deserialize)]
pub struct PortMapping {
    pub host_port: u16,
    pub guest_port: u16,
    /// "tcp" (default) or "udp"
    pub protocol: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct DnsConfig {
    pub nameservers: Option<Vec<String>>,
    pub rebind_protection: Option<bool>,
    pub query_timeout_ms: Option<u64>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct NetworkSecret {
    pub env_var: String,
    pub value: String,
    pub allowed_hosts: Option<Vec<String>>,
    pub allowed_host_patterns: Option<Vec<String>>,
    pub allow_any_host_dangerous: Option<bool>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct VolumeMount {
    pub guest_path: String,
    /// "bind", "named", or "tmpfs"
    pub volume_type: String,
    pub host_path: Option<String>,
    pub name: Option<String>,
    pub size_mb: Option<u32>,
    pub readonly: Option<bool>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct SandboxPatch {
    /// "text", "bytes", "copy_file", "copy_dir", "symlink", "mkdir", "remove", "append"
    pub patch_type: String,
    pub path: String,
    pub content: Option<String>,
    pub bytes: Option<String>,
    pub host_path: Option<String>,
    pub target: Option<String>,
    pub mode: Option<u32>,
    pub replace: Option<bool>,
}

#[derive(Debug, Serialize)]
pub struct CreateSandboxResponse {
    pub sandbox_id: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ExecRequest {
    pub sandbox_id: String,
    pub command: String,
    pub args: Option<Vec<String>>,
    pub env: Option<HashMap<String, String>>,
    pub timeout_seconds: Option<u64>,
}

#[derive(Debug, Serialize)]
pub struct ExecResponse {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
    pub timed_out: bool,
}

#[derive(Debug, Clone, Deserialize)]
pub struct DeleteSandboxRequest {
    pub sandbox_id: String,
}

#[derive(Debug, Serialize)]
pub struct SandboxSummary {
    pub sandbox_id: String,
}

#[derive(Debug, Serialize)]
pub struct ListSandboxesResponse {
    pub sandboxes: Vec<SandboxSummary>,
}
