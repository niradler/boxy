use async_trait::async_trait;
use dashmap::DashMap;
use std::{collections::HashMap, path::PathBuf, sync::Arc, time::Duration};
use tokio::process::Command;
use tracing::warn;

use crate::{
    config::Config,
    error::AppError,
    types::{CreateSandboxRequest, ExecResponse},
};
use super::SandboxAdapter;

// ---------------------------------------------------------------------------
// Stored sandbox state
// ---------------------------------------------------------------------------

#[derive(Clone)]
struct Sandbox {
    req: CreateSandboxRequest,
    /// Per-sandbox writable workspace, bind-mounted read-write at /workspace.
    /// The chroot root (/rootfs/ubuntu-24.04) is mounted read-only by nsjail
    /// by default, so sandboxes cannot contaminate each other's system state.
    workspace: PathBuf,
}

// ---------------------------------------------------------------------------
// Adapter
// ---------------------------------------------------------------------------

pub struct NsjailAdapter {
    sandboxes: Arc<DashMap<String, Sandbox>>,
    nsjail_path: String,
    default_rootfs: String,
    sandbox_root: String,
    binaries_dir: String,
}

impl NsjailAdapter {
    pub fn new(cfg: &Config) -> Self {
        Self {
            sandboxes: Arc::new(DashMap::new()),
            nsjail_path: cfg.nsjail_path.clone(),
            default_rootfs: cfg.nsjail_default_rootfs.clone(),
            sandbox_root: cfg.nsjail_sandbox_root.clone(),
            binaries_dir: cfg.nsjail_binaries_dir.clone(),
        }
    }

    // -----------------------------------------------------------------------
    // Input validation
    // -----------------------------------------------------------------------

    fn validate_create_request(&self, req: &CreateSandboxRequest) -> Result<(), AppError> {
        // vm.image: must stay within the parent of the configured default rootfs.
        // Prevents --chroot / or --chroot /etc attacks.
        if let Some(vm) = &req.vm {
            if let Some(image) = &vm.image {
                if !image.is_empty() {
                    let allowed_prefix = std::path::Path::new(&self.default_rootfs)
                        .parent()
                        .unwrap_or_else(|| std::path::Path::new("/rootfs"));
                    let image_path = std::path::Path::new(image.as_str());
                    if image.contains("..") || !image_path.starts_with(allowed_prefix) {
                        return Err(AppError::BadRequest(format!(
                            "vm.image must be under {}",
                            allowed_prefix.display()
                        )));
                    }
                }
            }

            // vm.user: disallow root to prevent running as real uid 0 inside the sandbox
            // when --disable_clone_newuser is active (no uid remapping).
            if let Some(user) = &vm.user {
                if user == "root" || user == "0" {
                    return Err(AppError::BadRequest(
                        "vm.user 'root'/'0' is not permitted".into(),
                    ));
                }
                // Reject any purely-numeric uid that resolves to 0.
                if user.parse::<u64>() == Ok(0) {
                    return Err(AppError::BadRequest(
                        "vm.user uid 0 is not permitted".into(),
                    ));
                }
            }

            // vm.workdir: must be an absolute path with no .. traversal.
            if let Some(workdir) = &vm.workdir {
                if !workdir.starts_with('/') || workdir.contains("..") {
                    return Err(AppError::BadRequest(
                        "vm.workdir must be an absolute path without '..'".into(),
                    ));
                }
            }

            // vm.hostname: restrict to safe characters to prevent nsjail arg injection.
            if let Some(hostname) = &vm.hostname {
                if !hostname
                    .chars()
                    .all(|c| c.is_ascii_alphanumeric() || c == '-')
                    || hostname.is_empty()
                {
                    return Err(AppError::BadRequest(
                        "vm.hostname must contain only alphanumeric characters and hyphens".into(),
                    ));
                }
            }
        }

        // allowed_binaries: each entry must be a plain filename (no path separators).
        // Prevents ../../../etc/passwd from being bind-mounted into the sandbox.
        for bin in req.allowed_binaries.iter().flatten() {
            if bin.is_empty() || bin.contains('/') || bin.contains("..") {
                return Err(AppError::BadRequest(format!(
                    "allowed_binaries entry {bin:?} must be a plain filename with no path separators"
                )));
            }
        }

        // volumes.host_path: must be under the sandbox root directory.
        // Prevents arbitrary host paths (e.g. /var/run/secrets/...) from being
        // bind-mounted into the sandbox.
        for vol in req.volumes.iter().flatten() {
            if let Some(host_path) = &vol.host_path {
                let sandbox_root = std::path::Path::new(&self.sandbox_root);
                if host_path.contains("..") || !std::path::Path::new(host_path.as_str()).starts_with(sandbox_root) {
                    return Err(AppError::BadRequest(format!(
                        "volume host_path must be under {}",
                        sandbox_root.display()
                    )));
                }
            }
        }

        Ok(())
    }

    // -----------------------------------------------------------------------
    // nsjail command builder
    // -----------------------------------------------------------------------

    fn build_exec_cmd(
        &self,
        sandbox: &Sandbox,
        rootfs: &str,
        command: &str,
        args: &[String],
        env_override: Option<HashMap<String, String>>,
        timeout_secs: u64,
    ) -> Command {
        let mut cmd = Command::new(&self.nsjail_path);

        cmd.args(["--mode", "o"]);
        // Redirect nsjail's own log so our stderr pipe is clean process output.
        cmd.args(["--log", "/dev/null"]);

        // Chroot into the shared read-only base rootfs.
        // nsjail mounts the chroot R/O by default — sandboxes cannot modify
        // system files.  Writable state lives in /workspace (bind-mounted).
        cmd.args(["--chroot", rootfs]);

        // User namespace creation requires writing uid_map which is restricted
        // in containerised environments.  We run as root (euid=0) without
        // remapping, which is sufficient for code execution sandboxes.
        cmd.arg("--disable_clone_newuser");

        // Per-sandbox writable workspace (persistent across execs).
        let workspace_spec = format!("{}:/workspace", sandbox.workspace.display());
        cmd.args(["--bindmount", &workspace_spec]);

        // Ephemeral writable /tmp per exec.
        cmd.args(["--tmpfsmount", "/tmp"]);

        // Default PATH so relative command names (e.g. "sh", "python3") resolve
        // correctly inside the chroot via execvpe(3).
        cmd.args(["--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"]);

        // Per-request VM overrides.
        if let Some(vm) = &sandbox.req.vm {
            if let Some(user) = &vm.user {
                cmd.args(["--user", user]);
            }
            if let Some(hostname) = &vm.hostname {
                cmd.args(["--hostname", hostname]);
            }
            if let Some(workdir) = &vm.workdir {
                cmd.args(["--cwd", workdir]);
            }
            if let Some(mb) = vm.memory_mb.filter(|&v| v > 0) {
                let bytes = (mb as u64) * 1024 * 1024;
                cmd.args(["--cgroup_mem_max", &bytes.to_string()]);
            }
            for rl in vm.rlimits.iter().flatten() {
                if let Some(flag) = rlimit_flag(&rl.resource) {
                    cmd.args([flag, &rl.soft.to_string()]);
                }
            }
        }

        // Wall-clock time limit — nsjail SIGKILLs the child when this fires.
        if timeout_secs > 0 {
            cmd.args(["--time_limit", &timeout_secs.to_string()]);
        }

        // Sandbox env vars, then exec-level overrides.
        for (k, v) in sandbox.req.env.iter().flatten() {
            cmd.args(["--env", &format!("{k}={v}")]);
        }
        for (k, v) in env_override.iter().flatten() {
            cmd.args(["--env", &format!("{k}={v}")]);
        }

        // Network: default = isolated network namespace.
        if let Some(net) = &sandbox.req.network {
            if net.enabled != Some(false) && net.allow_internet_access == Some(true) {
                cmd.arg("--disable_clone_newnet");
            }
        }

        // Volumes.
        for v in sandbox.req.volumes.iter().flatten() {
            match v.volume_type.as_str() {
                "tmpfs" => {
                    cmd.args(["--tmpfsmount", &v.guest_path]);
                }
                _ => {
                    let host = v.host_path.as_deref().unwrap_or(v.guest_path.as_str());
                    let spec = format!("{host}:{}", v.guest_path);
                    if v.readonly.unwrap_or(false) {
                        cmd.args(["--bindmount_ro", &spec]);
                    } else {
                        cmd.args(["--bindmount", &spec]);
                    }
                }
            }
        }

        // allowed_binaries: bind-mount each binary read-only from the host.
        for bin in sandbox.req.allowed_binaries.iter().flatten() {
            let host_path = format!("{}/{bin}", self.binaries_dir);
            let guest_path = format!("/usr/local/bin/{bin}");
            cmd.args(["--bindmount_ro", &format!("{host_path}:{guest_path}")]);
        }

        cmd.stdout(std::process::Stdio::piped());
        cmd.stderr(std::process::Stdio::piped());

        cmd.arg("--");
        cmd.arg(command);
        cmd.args(args);

        cmd
    }
}

// ---------------------------------------------------------------------------
// SandboxAdapter impl
// ---------------------------------------------------------------------------

#[async_trait]
impl SandboxAdapter for NsjailAdapter {
    async fn create(&self, req: CreateSandboxRequest) -> Result<(), AppError> {
        self.validate_create_request(&req)?;
        if self.sandboxes.contains_key(&req.sandbox_id) {
            return Err(AppError::AlreadyExists(req.sandbox_id));
        }

        let workspace = PathBuf::from(&self.sandbox_root)
            .join(&req.sandbox_id)
            .join("workspace");

        tokio::fs::create_dir_all(&workspace).await.map_err(|e| {
            AppError::Internal(format!("mkdir workspace {}: {e}", workspace.display()))
        })?;

        self.sandboxes.insert(req.sandbox_id.clone(), Sandbox { req, workspace });
        Ok(())
    }

    async fn exec(
        &self,
        sandbox_id: &str,
        command: &str,
        args: &[String],
        env: Option<HashMap<String, String>>,
        timeout_secs: u64,
    ) -> Result<ExecResponse, AppError> {
        let sandbox = self
            .sandboxes
            .get(sandbox_id)
            .ok_or_else(|| AppError::NotFound(sandbox_id.to_string()))?
            .clone();

        let rootfs = sandbox.req
            .vm
            .as_ref()
            .and_then(|v| v.image.as_deref())
            .filter(|s| !s.is_empty())
            .unwrap_or(&self.default_rootfs)
            .to_string();

        // nsjail uses execve(2), not execvpe — no automatic PATH search.
        // Resolve relative command names against the chroot so callers can
        // pass "sh" instead of "/bin/sh".
        let resolved_cmd = resolve_command_in_chroot(command, &rootfs);

        let mut cmd = self.build_exec_cmd(&sandbox, &rootfs, &resolved_cmd, args, env, timeout_secs);

        let out = if timeout_secs > 0 {
            let deadline = Duration::from_secs(timeout_secs + 5);
            match tokio::time::timeout(deadline, cmd.output()).await {
                Err(_elapsed) => {
                    return Ok(ExecResponse {
                        stdout: String::new(),
                        stderr: "execution timed out".to_string(),
                        exit_code: -1,
                        timed_out: true,
                    });
                }
                Ok(r) => r.map_err(|e| AppError::Vm(format!("nsjail error: {e}")))?,
            }
        } else {
            cmd.output().await.map_err(|e| AppError::Vm(format!("nsjail error: {e}")))?
        };

        Ok(ExecResponse {
            stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
            stderr: String::from_utf8_lossy(&out.stderr).into_owned(),
            exit_code: out.status.code().unwrap_or(-1),
            timed_out: false,
        })
    }

    async fn delete(&self, sandbox_id: &str) -> Result<(), AppError> {
        let (_, sandbox) = self
            .sandboxes
            .remove(sandbox_id)
            .ok_or_else(|| AppError::NotFound(sandbox_id.to_string()))?;

        let base = sandbox.workspace.parent().unwrap_or(&sandbox.workspace);
        if let Err(e) = tokio::fs::remove_dir_all(base).await {
            warn!(path = %base.display(), "sandbox cleanup failed: {e}");
        }
        Ok(())
    }

    fn list_ids(&self) -> Vec<String> {
        self.sandboxes.iter().map(|kv| kv.key().clone()).collect()
    }

    fn count(&self) -> usize {
        self.sandboxes.len()
    }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// Resolve a relative command name to an absolute path inside the chroot.
/// nsjail calls execve(2) directly so there is no automatic PATH lookup.
/// We walk common PATH directories inside the chroot and return the first
/// match.  If the command already starts with '/' or is not found, we return
/// it unchanged and let nsjail produce the appropriate error.
fn resolve_command_in_chroot(command: &str, chroot: &str) -> String {
    if command.starts_with('/') || command.contains('/') {
        return command.to_string();
    }
    const SEARCH_DIRS: &[&str] = &[
        "/usr/local/sbin", "/usr/local/bin",
        "/usr/sbin", "/usr/bin",
        "/sbin", "/bin",
    ];
    for dir in SEARCH_DIRS {
        let host_path = format!("{chroot}{dir}/{command}");
        if std::path::Path::new(&host_path).exists() {
            return format!("{dir}/{command}");
        }
    }
    command.to_string()
}

fn rlimit_flag(resource: &str) -> Option<&'static str> {
    match resource {
        "as"     => Some("--rlimit_as"),
        "core"   => Some("--rlimit_core"),
        "cpu"    => Some("--rlimit_cpu"),
        "fsize"  => Some("--rlimit_fsize"),
        "nofile" => Some("--rlimit_nofile"),
        "nproc"  => Some("--rlimit_nproc"),
        "stack"  => Some("--rlimit_stack"),
        _ => None,
    }
}
