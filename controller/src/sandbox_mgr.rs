use base64::{engine::general_purpose, Engine as _};
use dashmap::DashMap;
use microsandbox::{
    network::{DnsConfig, NetworkPolicy, NetworkPolicyBuilder, NetworkSecret, PortMapping},
    patch::PatchBuilder,
    sandbox::{LogLevel, PullPolicy, Sandbox},
    volume::VolumeMount,
};
use std::{collections::HashMap, path::PathBuf, sync::Arc, time::Duration};
use tokio::time::timeout;

use crate::{
    config::Config,
    error::AppError,
    types::{
        CreateSandboxRequest, ExecResponse, NetworkConfig, NetworkRule, SandboxPatch, VolumeMount as ReqVolume,
    },
};

/// Operator defaults applied to every sandbox.
pub struct OperatorDefaults {
    pub log_level: LogLevel,
    pub pull_policy: PullPolicy,
    pub metrics_interval: Option<Duration>,
    pub libkrunfw_path: Option<PathBuf>,
}

impl OperatorDefaults {
    pub fn from_config(cfg: &Config) -> Self {
        Self {
            log_level: parse_log_level(&cfg.vm_log_level),
            pull_policy: parse_pull_policy(&cfg.vm_pull_policy),
            metrics_interval: if cfg.vm_metrics_interval_ms == 0 {
                None
            } else {
                Some(Duration::from_millis(cfg.vm_metrics_interval_ms))
            },
            libkrunfw_path: if cfg.libkrunfw_path.is_empty() {
                None
            } else {
                Some(PathBuf::from(&cfg.libkrunfw_path))
            },
        }
    }
}

pub struct SandboxManager {
    sandboxes: Arc<DashMap<String, Sandbox>>,
    binaries_dir: String,
    defaults: OperatorDefaults,
}

impl SandboxManager {
    pub fn new(binaries_dir: impl Into<String>, cfg: &Config) -> Self {
        Self {
            sandboxes: Arc::new(DashMap::new()),
            binaries_dir: binaries_dir.into(),
            defaults: OperatorDefaults::from_config(cfg),
        }
    }

    pub async fn create(&self, req: CreateSandboxRequest) -> Result<(), AppError> {
        if self.sandboxes.contains_key(&req.sandbox_id) {
            return Err(AppError::AlreadyExists(req.sandbox_id));
        }

        let mut builder = Sandbox::builder(req.sandbox_id.clone())
            .log_level(self.defaults.log_level)
            .pull_policy(self.defaults.pull_policy);

        if let Some(path) = &self.defaults.libkrunfw_path {
            builder = builder.libkrunfw_path(path);
        }
        match self.defaults.metrics_interval {
            Some(d) => builder = builder.metrics_sample_interval(d),
            None    => builder = builder.disable_metrics_sample(),
        }

        // --- Per-request VM config ---
        if let Some(vm) = &req.vm {
            if vm.memory_mb.unwrap_or(0) > 0 {
                builder = builder.memory(vm.memory_mb.unwrap());
            }
            if vm.vcpus.unwrap_or(0) > 0 {
                builder = builder.cpus(vm.vcpus.unwrap());
            }
            if let Some(w) = &vm.workdir   { builder = builder.workdir(w); }
            if let Some(s) = &vm.shell     { builder = builder.shell(s); }
            if let Some(h) = &vm.hostname  { builder = builder.hostname(h); }
            if let Some(u) = &vm.user      { builder = builder.user(u); }
            if let Some(d) = vm.max_duration_sec.filter(|&s| s > 0) {
                builder = builder.max_duration(Duration::from_secs(d));
            }
            if let Some(d) = vm.idle_timeout_sec.filter(|&s| s > 0) {
                builder = builder.idle_timeout(Duration::from_secs(d));
            }
            for rl in vm.rlimits.iter().flatten() {
                builder = builder.rlimit_range(parse_rlimit(&rl.resource), rl.soft, rl.hard);
            }
            for s in vm.scripts.iter().flatten() {
                builder = builder.script(&s.name, &s.content);
            }
        }

        // --- Env vars ---
        for (k, v) in req.env.iter().flatten() {
            builder = builder.env(k, v);
        }

        // --- Network ---
        if let Some(net) = &req.network {
            builder = builder.network(|nb| apply_network(nb, net));
        }

        // --- Volumes ---
        for v in req.volumes.iter().flatten() {
            builder = builder.volume(&v.guest_path, |vb| apply_volume(vb, v));
        }

        // --- Patches: allowed_binaries shorthand ---
        let mut patch = PatchBuilder::new();
        let mut has_patch = false;
        for bin in req.allowed_binaries.iter().flatten() {
            let src = format!("{}/{}", self.binaries_dir, bin);
            let dst = format!("/usr/local/bin/{}", bin);
            patch = patch.copy_file(&src, &dst, 0o755, false);
            has_patch = true;
        }
        // --- Patches: explicit patch list ---
        for p in req.patches.iter().flatten() {
            patch = apply_patch(patch, p);
            has_patch = true;
        }
        if has_patch {
            builder = builder.patch(patch.build());
        }

        let sandbox = builder.build().await.map_err(|e| AppError::Vm(e.to_string()))?;
        self.sandboxes.insert(req.sandbox_id, sandbox);
        Ok(())
    }

    pub async fn exec(
        &self,
        sandbox_id: &str,
        command: &str,
        args: &[String],
        env: Option<HashMap<String, String>>,
        timeout_secs: u64,
    ) -> Result<ExecResponse, AppError> {
        let sb = self
            .sandboxes
            .get(sandbox_id)
            .ok_or_else(|| AppError::NotFound(sandbox_id.to_string()))?;

        let mut cmd = sb.command(command);
        for a in args {
            cmd = cmd.arg(a);
        }
        for (k, v) in env.iter().flatten() {
            cmd = cmd.env(k, v);
        }

        match timeout(Duration::from_secs(timeout_secs), cmd.output()).await {
            Ok(Ok(out)) => Ok(ExecResponse {
                stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
                stderr: String::from_utf8_lossy(&out.stderr).into_owned(),
                exit_code: out.status.code().unwrap_or(-1),
                timed_out: false,
            }),
            Ok(Err(e)) => Err(AppError::Vm(e.to_string())),
            Err(_) => Ok(ExecResponse {
                stdout: String::new(),
                stderr: "timed out".into(),
                exit_code: -1,
                timed_out: true,
            }),
        }
    }

    pub async fn delete(&self, sandbox_id: &str) -> Result<(), AppError> {
        let (_, mut sb) = self
            .sandboxes
            .remove(sandbox_id)
            .ok_or_else(|| AppError::NotFound(sandbox_id.to_string()))?;
        sb.stop().await.map_err(|e| AppError::Vm(e.to_string()))?;
        Ok(())
    }

    pub fn count(&self) -> usize {
        self.sandboxes.len()
    }
}

// --- helpers ---

fn apply_network(mut nb: microsandbox::network::NetworkBuilder, net: &NetworkConfig) -> microsandbox::network::NetworkBuilder {
    if net.enabled == Some(false) {
        return nb.enabled(false);
    }

    // Simple shortcuts -- only used when Rules is empty
    if net.rules.as_ref().map(|r| r.is_empty()).unwrap_or(true) {
        if net.allow_internet_access == Some(true) {
            nb = nb.policy(NetworkPolicy::allow_all());
        } else if let Some(domains) = &net.allowed_egress_domains {
            if !domains.is_empty() {
                nb = nb.policy(NetworkPolicy::allow_domains(domains.clone()));
            }
        }
    } else {
        nb = nb.policy(build_policy(net.rules.as_ref().unwrap()));
    }

    for p in net.ports.iter().flatten() {
        nb = match p.protocol.as_deref().unwrap_or("tcp") {
            "udp" => nb.port_udp(p.host_port, p.guest_port),
            _     => nb.port(p.host_port, p.guest_port),
        };
    }

    if let Some(dns) = &net.dns {
        nb = nb.dns(|db| {
            let mut db = db;
            if let Some(ns) = &dns.nameservers {
                for s in ns { db = db.nameserver(s); }
            }
            if let Some(rp) = dns.rebind_protection {
                db = db.rebind_protection(rp);
            }
            if let Some(ms) = dns.query_timeout_ms.filter(|&v| v > 0) {
                db = db.query_timeout_ms(ms);
            }
            db
        });
    }

    for s in net.secrets.iter().flatten() {
        nb = nb.secret(|sb| {
            let mut sb = sb.env(&s.env_var).value(&s.value);
            for h in s.allowed_hosts.iter().flatten()          { sb = sb.allow_host(h); }
            for p in s.allowed_host_patterns.iter().flatten()  { sb = sb.allow_host_pattern(p); }
            if s.allow_any_host_dangerous == Some(true)        { sb = sb.allow_any_host_dangerous(true); }
            sb
        });
    }

    if let Some(mc) = net.max_connections.filter(|&v| v > 0) {
        nb = nb.max_connections(mc);
    }
    if net.trust_host_cas == Some(true) {
        nb = nb.trust_host_cas(true);
    }
    nb
}

fn build_policy(rules: &[NetworkRule]) -> NetworkPolicy {
    let mut pb = NetworkPolicyBuilder::new().default_deny();
    for r in rules {
        pb = pb.rule(|rb| {
            let mut rb = match r.direction.as_str() {
                "ingress" => rb.ingress(),
                "any"     => rb.any(),
                _         => rb.egress(),
            };
            for p in r.protocols.iter().flatten() {
                rb = match p.as_str() {
                    "udp"    => rb.udp(),
                    "icmpv4" => rb.icmpv4(),
                    "icmpv6" => rb.icmpv6(),
                    _        => rb.tcp(),
                };
            }
            for p in r.ports.iter().flatten()           { rb = rb.port(*p); }
            for pr in r.port_ranges.iter().flatten()    { rb = rb.port_range(pr.start, pr.end); }
            for d in r.domains.iter().flatten()         { rb = rb.domain(d); }
            for s in r.domain_suffixes.iter().flatten() { rb = rb.domain_suffix(s); }
            for c in r.cidrs.iter().flatten()           { rb = rb.cidr(c); }
            for g in r.groups.iter().flatten()          { rb = apply_group(rb, g); }
            if r.action == "allow" { rb.allow() } else { rb.deny() }
        });
    }
    pb.build()
}

fn apply_group(rb: microsandbox::network::RuleBuilder, group: &str) -> microsandbox::network::RuleBuilder {
    match group {
        "public"     => rb.allow_public(),    // will be overridden by action below
        "private"    => rb.group(microsandbox::network::DestinationGroup::Private),
        "loopback"   => rb.group(microsandbox::network::DestinationGroup::Loopback),
        "link_local" => rb.group(microsandbox::network::DestinationGroup::LinkLocal),
        "metadata"   => rb.group(microsandbox::network::DestinationGroup::Metadata),
        "multicast"  => rb.group(microsandbox::network::DestinationGroup::Multicast),
        "host"       => rb.group(microsandbox::network::DestinationGroup::Host),
        _            => rb.group(microsandbox::network::DestinationGroup::Public),
    }
}

fn apply_volume(vb: microsandbox::volume::VolumeBuilder, v: &ReqVolume) -> microsandbox::volume::VolumeBuilder {
    let readonly = v.readonly.unwrap_or(false);
    let mut vb = match v.volume_type.as_str() {
        "named" => vb.named(v.name.as_deref().unwrap_or("")),
        "tmpfs" => {
            let mut b = vb.tmpfs();
            if let Some(s) = v.size_mb.filter(|&s| s > 0) { b = b.size(s); }
            b
        }
        _ => vb.bind(v.host_path.as_deref().unwrap_or("")),
    };
    if readonly { vb = vb.readonly(); }
    vb
}

fn apply_patch(mut pb: PatchBuilder, p: &SandboxPatch) -> PatchBuilder {
    let replace = p.replace.unwrap_or(false);
    let mode    = p.mode.unwrap_or(0o644);
    match p.patch_type.as_str() {
        "text"      => pb.text(&p.path, p.content.as_deref().unwrap_or(""), mode, replace),
        "bytes"     => {
            let data = general_purpose::STANDARD.decode(p.bytes.as_deref().unwrap_or("")).unwrap_or_default();
            pb.file(&p.path, data, mode, replace)
        }
        "copy_file" => pb.copy_file(p.host_path.as_deref().unwrap_or(""), &p.path, mode, replace),
        "copy_dir"  => pb.copy_dir(p.host_path.as_deref().unwrap_or(""), &p.path, replace),
        "symlink"   => pb.symlink(p.target.as_deref().unwrap_or(""), &p.path, replace),
        "mkdir"     => pb.mkdir(&p.path, mode),
        "remove"    => pb.remove(&p.path),
        "append"    => pb.append(&p.path, p.content.as_deref().unwrap_or("")),
        _           => pb,
    }
}

fn parse_log_level(s: &str) -> LogLevel {
    match s {
        "off"   => LogLevel::Off,
        "error" => LogLevel::Error,
        "info"  => LogLevel::Info,
        "debug" => LogLevel::Debug,
        "trace" => LogLevel::Trace,
        _       => LogLevel::Warn,
    }
}

fn parse_pull_policy(s: &str) -> PullPolicy {
    match s {
        "always" => PullPolicy::Always,
        "never"  => PullPolicy::Never,
        _        => PullPolicy::IfMissing,
    }
}

fn parse_rlimit(s: &str) -> microsandbox::sandbox::RlimitResource {
    use microsandbox::sandbox::RlimitResource::*;
    match s {
        "nproc"      => Nproc,
        "memlock"    => Memlock,
        "msgqueue"   => Msgqueue,
        "sigpending" => Sigpending,
        "nice"       => Nice,
        "rtprio"     => Rtprio,
        "rttime"     => Rttime,
        _            => Nofile,
    }
}
