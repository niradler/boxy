pub mod nsjail;

use std::collections::HashMap;
use std::sync::Arc;

use async_trait::async_trait;

use crate::{config::Config, error::AppError, types::{CreateSandboxRequest, ExecResponse}};

/// Every sandbox backend must implement this trait.
/// Adding a new provider: create a module under providers/, implement this trait,
/// add a match arm in build_adapter.
#[async_trait]
pub trait SandboxAdapter: Send + Sync {
    async fn create(&self, req: CreateSandboxRequest) -> Result<(), AppError>;
    async fn exec(
        &self,
        sandbox_id: &str,
        command: &str,
        args: &[String],
        env: Option<HashMap<String, String>>,
        timeout_secs: u64,
    ) -> Result<ExecResponse, AppError>;
    async fn delete(&self, sandbox_id: &str) -> Result<(), AppError>;
    fn list_ids(&self) -> Vec<String>;
    fn count(&self) -> usize;
}

/// Instantiate the adapter named by cfg.sandbox_provider.
pub fn build_adapter(cfg: &Config) -> Result<Arc<dyn SandboxAdapter>, AppError> {
    match cfg.sandbox_provider.as_str() {
        "nsjail" => Ok(Arc::new(nsjail::NsjailAdapter::new(cfg))),
        other => Err(AppError::Internal(format!(
            "unknown sandbox provider: {other}; supported: nsjail"
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_cfg(provider: &str) -> Config {
        Config {
            port: 8080,
            mtls_disabled: true,
            tls_cert_path: String::new(),
            tls_key_path: String::new(),
            tls_ca_path: String::new(),
            max_sandboxes: 5,
            sandbox_provider: provider.to_string(),
            nsjail_path: "/usr/sbin/nsjail".to_string(),
            nsjail_default_rootfs: "/rootfs/ubuntu-24.04".to_string(),
            nsjail_sandbox_root: "/tmp/boxy-test-sandboxes".to_string(),
            nsjail_binaries_dir: "/usr/local/bin".to_string(),
        }
    }

    #[test]
    fn build_nsjail_returns_ok() {
        assert!(build_adapter(&test_cfg("nsjail")).is_ok());
    }

    #[test]
    fn build_unknown_returns_err() {
        let err = build_adapter(&test_cfg("unknown")).unwrap_err();
        assert!(err.to_string().contains("unknown sandbox provider"));
        assert!(err.to_string().contains("unknown"));
    }
}
