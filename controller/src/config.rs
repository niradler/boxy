use std::env;

/// Operator-level configuration loaded from environment variables.
/// All fields have sensible defaults so the controller runs out of the box.
pub struct Config {
    // --- Server ---
    pub port: u16,
    pub mtls_disabled: bool,
    pub tls_cert_path: String,
    pub tls_key_path: String,
    pub tls_ca_path: String,

    // --- Bin-packing ---
    pub max_sandboxes: usize,

    // --- VM operator defaults (applied to every sandbox unless overridden per-request) ---

    /// Log verbosity for VM processes.
    /// Values: "off", "error", "warn", "info", "debug", "trace". Default: "warn".
    pub vm_log_level: String,

    /// Metrics sampling interval in milliseconds. 0 = disabled. Default: 0.
    pub vm_metrics_interval_ms: u64,

    /// Override the libkrunfw shared library path. Empty = use microsandbox default.
    pub libkrunfw_path: String,

    /// Image pull policy for VM rootfs OCI images.
    /// Values: "if_missing" (default), "always", "never".
    pub vm_pull_policy: String,
}

impl Config {
    pub fn from_env() -> Self {
        Self {
            port: env_parse("BOXY_CONTROLLER_PORT", 8080),
            mtls_disabled: env_bool("BOXY_MTLS_DISABLED", false),
            tls_cert_path: env_str("BOXY_TLS_CERT_PATH", "/tls/tls.crt"),
            tls_key_path: env_str("BOXY_TLS_KEY_PATH", "/tls/tls.key"),
            tls_ca_path: env_str("BOXY_TLS_CA_PATH", "/tls/ca.crt"),
            max_sandboxes: env_parse("BOXY_MAX_SANDBOXES", 20),
            vm_log_level: env_str("BOXY_VM_LOG_LEVEL", "warn"),
            vm_metrics_interval_ms: env_parse("BOXY_VM_METRICS_INTERVAL_MS", 0),
            libkrunfw_path: env::var("BOXY_LIBKRUNFW_PATH").unwrap_or_default(),
            vm_pull_policy: env_str("BOXY_VM_PULL_POLICY", "if_missing"),
        }
    }
}

fn env_str(key: &str, default: &str) -> String {
    env::var(key).unwrap_or_else(|_| default.to_string())
}

fn env_bool(key: &str, default: bool) -> bool {
    match env::var(key).as_deref() {
        Ok("true") | Ok("1") => true,
        Ok("false") | Ok("0") => false,
        _ => default,
    }
}

fn env_parse<T: std::str::FromStr>(key: &str, default: T) -> T {
    env::var(key)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default)
}
