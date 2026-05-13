use base64::{engine::general_purpose, Engine as _};
use dashmap::DashMap;
use microsandbox::{
    sandbox::{PullPolicy, RlimitResource, Sandbox},
    LogLevel, NetworkPolicy,
};
use std::{collections::HashMap, sync::Arc, time::Duration};

use crate::{
    config::Config,
    error::AppError,
    types::{CreateSandboxRequest, ExecResponse, SandboxPatch, VolumeMount as ReqVolume},
};

/// Default rootfs image when the request does not specify one.
const DEFAULT_IMAGE: &str = "ubuntu:24.04";

/// Operator defaults applied to every sandbox.
pub struct OperatorDefaults {
    pub log_level: LogLevel,
    pub pull_policy: PullPolicy,
    pub metrics_interval: Option<Duration>,
    pub libkrunfw_path: Option<std::path::PathBuf>,
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
                Some(std::path::PathBuf::from(&cfg.libkrunfw_path))
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

        let image = req
            .vm
            .as_ref()
            .and_then(|v| v.image.clone())
            .unwrap_or_else(|| DEFAULT_IMAGE.to_string());

        let mut builder = Sandbox::builder(req.sandbox_id.clone())
            .image(image)
            .log_level(self.defaults.log_level)
            .pull_policy(self.defaults.pull_policy);

        if let Some(path) = &self.defaults.libkrunfw_path {
            builder = builder.libkrunfw_path(path);
        }
        builder = match self.defaults.metrics_interval {
            Some(d) => builder.metrics_sample_interval(d),
            None => builder.disable_metrics_sample(),
        };

        // Per-request VM config.
        if let Some(vm) = &req.vm {
            if let Some(mb) = vm.memory_mb.filter(|&v| v > 0) {
                builder = builder.memory(mb);
            }
            if let Some(c) = vm.vcpus.filter(|&v| v > 0) {
                builder = builder.cpus(c);
            }
            if let Some(w) = &vm.workdir  { builder = builder.workdir(w); }
            if let Some(s) = &vm.shell    { builder = builder.shell(s); }
            if let Some(h) = &vm.hostname { builder = builder.hostname(h); }
            if let Some(u) = &vm.user     { builder = builder.user(u); }
            if let Some(d) = vm.max_duration_sec.filter(|&s| s > 0) {
                builder = builder.max_duration(d);
            }
            if let Some(d) = vm.idle_timeout_sec.filter(|&s| s > 0) {
                builder = builder.idle_timeout(d);
            }
            for rl in vm.rlimits.iter().flatten() {
                builder = builder.rlimit_range(parse_rlimit(&rl.resource), rl.soft, rl.hard);
            }
            for s in vm.scripts.iter().flatten() {
                builder = builder.script(&s.name, &s.content);
            }
        }

        // Env vars.
        for (k, v) in req.env.iter().flatten() {
            builder = builder.env(k, v);
        }

        // Network — supports simple egress shortcuts. Advanced rules (CIDR,
        // domains, per-rule actions) are not exposed via the controller API
        // today; use the host-level Kubernetes NetworkPolicy for those.
        if let Some(net) = req.network.clone() {
            builder = builder.network(move |mut nb| {
                if net.enabled == Some(false) {
                    return nb.enabled(false);
                }
                if net.allow_internet_access == Some(true) {
                    nb = nb.policy(NetworkPolicy::allow_all());
                } else if let Some(domains) = net.allowed_egress_domains.as_ref() {
                    if !domains.is_empty() {
                        if let Ok(policy) = NetworkPolicy::none().allow_domains(domains.iter()) {
                            nb = nb.policy(policy);
                        }
                    }
                }
                for p in net.ports.iter().flatten() {
                    nb = match p.protocol.as_deref().unwrap_or("tcp") {
                        "udp" => nb.port_udp(p.host_port, p.guest_port),
                        _     => nb.port(p.host_port, p.guest_port),
                    };
                }
                if let Some(mc) = net.max_connections.filter(|&v| v > 0) {
                    nb = nb.max_connections(mc);
                }
                if net.trust_host_cas == Some(true) {
                    nb = nb.trust_host_cas(true);
                }
                nb
            });
        }

        // Volumes.
        for v in req.volumes.iter().flatten() {
            let v_clone = v.clone();
            let guest = v.guest_path.clone();
            builder = builder.volume(guest, move |mb| apply_volume(mb, &v_clone));
        }

        // Patches — allowed_binaries shorthand + explicit list.
        let binaries_dir = self.binaries_dir.clone();
        let allowed = req.allowed_binaries.clone().unwrap_or_default();
        let patches = req.patches.clone().unwrap_or_default();
        if !allowed.is_empty() || !patches.is_empty() {
            builder = builder.patch(move |mut pb| {
                for bin in &allowed {
                    let src = format!("{}/{}", binaries_dir, bin);
                    let dst = format!("/usr/local/bin/{}", bin);
                    pb = pb.copy_file(src, dst, Some(0o755), false);
                }
                for p in &patches {
                    pb = apply_patch(pb, p);
                }
                pb
            });
        }

        let sandbox = builder
            .create()
            .await
            .map_err(|e| AppError::Vm(e.to_string()))?;
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
            .ok_or_else(|| AppError::NotFound(sandbox_id.to_string()))?
            .clone();

        let args_vec: Vec<String> = args.to_vec();
        let env_vec: Vec<(String, String)> = env.into_iter().flatten().collect();

        let result = sb
            .exec_with(command.to_string(), move |mut e| {
                e = e.args(args_vec);
                for (k, v) in env_vec {
                    e = e.env(k, v);
                }
                if timeout_secs > 0 {
                    e = e.timeout(Duration::from_secs(timeout_secs));
                }
                e
            })
            .await;

        match result {
            Ok(out) => Ok(ExecResponse {
                stdout: out.stdout().unwrap_or_default(),
                stderr: out.stderr().unwrap_or_default(),
                exit_code: out.status().code,
                timed_out: false,
            }),
            Err(e) => {
                let msg = e.to_string();
                if msg.contains("ExecTimeout") || msg.to_lowercase().contains("timed out") {
                    Ok(ExecResponse {
                        stdout: String::new(),
                        stderr: msg,
                        exit_code: -1,
                        timed_out: true,
                    })
                } else {
                    Err(AppError::Vm(msg))
                }
            }
        }
    }

    pub async fn delete(&self, sandbox_id: &str) -> Result<(), AppError> {
        let (_, sb) = self
            .sandboxes
            .remove(sandbox_id)
            .ok_or_else(|| AppError::NotFound(sandbox_id.to_string()))?;
        let _ = sb.stop().await;
        sb.remove_persisted()
            .await
            .map_err(|e| AppError::Vm(e.to_string()))?;
        Ok(())
    }

    pub fn count(&self) -> usize {
        self.sandboxes.len()
    }

    /// Returns the IDs of all live sandboxes managed by this controller.
    /// Used by the router's SyncReconciler to rebuild routing state from
    /// ground truth after a restart or cache divergence.
    pub fn list_ids(&self) -> Vec<String> {
        self.sandboxes.iter().map(|kv| kv.key().clone()).collect()
    }
}

// --- helpers ---

fn apply_volume(
    mut mb: microsandbox::sandbox::MountBuilder,
    v: &ReqVolume,
) -> microsandbox::sandbox::MountBuilder {
    let readonly = v.readonly.unwrap_or(false);
    mb = match v.volume_type.as_str() {
        "named" => mb.named(v.name.clone().unwrap_or_default()),
        "tmpfs" => {
            let mut b = mb.tmpfs();
            if let Some(s) = v.size_mb.filter(|&s| s > 0) { b = b.size(s); }
            b
        }
        _ => mb.bind(v.host_path.clone().unwrap_or_default()),
    };
    if readonly { mb = mb.readonly(); }
    mb
}

fn apply_patch(
    pb: microsandbox::sandbox::PatchBuilder,
    p: &SandboxPatch,
) -> microsandbox::sandbox::PatchBuilder {
    let replace = p.replace.unwrap_or(false);
    let mode = p.mode;
    match p.patch_type.as_str() {
        "text" => pb.text(&p.path, p.content.clone().unwrap_or_default(), mode, replace),
        "bytes" => {
            let data = general_purpose::STANDARD
                .decode(p.bytes.as_deref().unwrap_or(""))
                .unwrap_or_default();
            pb.file(&p.path, data, mode, replace)
        }
        "copy_file" => pb.copy_file(
            p.host_path.clone().unwrap_or_default(),
            &p.path,
            mode,
            replace,
        ),
        "copy_dir" => pb.copy_dir(
            p.host_path.clone().unwrap_or_default(),
            &p.path,
            replace,
        ),
        "symlink" => pb.symlink(p.target.clone().unwrap_or_default(), &p.path, replace),
        "mkdir"   => pb.mkdir(&p.path, mode),
        "remove"  => pb.remove(&p.path),
        "append"  => pb.append(&p.path, p.content.clone().unwrap_or_default()),
        _ => pb,
    }
}

fn parse_log_level(s: &str) -> LogLevel {
    match s {
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

fn parse_rlimit(s: &str) -> RlimitResource {
    match s {
        "cpu"        => RlimitResource::Cpu,
        "fsize"      => RlimitResource::Fsize,
        "data"       => RlimitResource::Data,
        "stack"      => RlimitResource::Stack,
        "core"       => RlimitResource::Core,
        "rss"        => RlimitResource::Rss,
        "nproc"      => RlimitResource::Nproc,
        "memlock"    => RlimitResource::Memlock,
        "as"         => RlimitResource::As,
        "locks"      => RlimitResource::Locks,
        "sigpending" => RlimitResource::Sigpending,
        _            => RlimitResource::Nofile,
    }
}
