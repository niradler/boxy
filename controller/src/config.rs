use std::env;

pub struct Config {
    // --- Server ---
    pub port: u16,
    pub mtls_disabled: bool,
    pub tls_cert_path: String,
    pub tls_key_path: String,
    pub tls_ca_path: String,

    // --- Bin-packing ---
    pub max_sandboxes: usize,

    // --- Provider ---
    /// Sandbox backend. Currently only "nsjail" is supported.
    pub sandbox_provider: String,

    // --- nsjail ---
    /// Path to the nsjail binary. Default: /usr/sbin/nsjail.
    pub nsjail_path: String,
    /// Default rootfs for sandboxes (overlayfs lower dir).
    /// Requests may override via vm.image (a local directory path).
    pub nsjail_default_rootfs: String,
    /// Directory where per-sandbox overlay work dirs are created.
    pub nsjail_sandbox_root: String,
    /// Host directory scanned for allowed_binaries. Each listed binary is bind-mounted
    /// from here into /usr/local/bin/<name> inside the sandbox.
    pub nsjail_binaries_dir: String,
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
            sandbox_provider: env_str("BOXY_SANDBOX_PROVIDER", "nsjail"),
            nsjail_path: env_str("BOXY_NSJAIL_PATH", "/usr/sbin/nsjail"),
            nsjail_default_rootfs: env_str("BOXY_NSJAIL_ROOTFS", "/rootfs/ubuntu-24.04"),
            nsjail_sandbox_root: env_str("BOXY_NSJAIL_SANDBOX_ROOT", "/var/lib/boxy/sandboxes"),
            nsjail_binaries_dir: env_str("BOXY_NSJAIL_BINARIES_DIR", "/usr/local/bin"),
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
