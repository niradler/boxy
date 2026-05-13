use axum::{
    extract::{Json, State},
    routing::{delete, get, post},
    Router,
};
use std::{net::SocketAddr, sync::Arc};
use tracing::info;

use crate::{
    config::Config,
    error::AppError,
    sandbox_mgr::SandboxManager,
    tls::build_tls_config,
    types::{CreateSandboxRequest, CreateSandboxResponse, DeleteSandboxRequest, ExecRequest, ExecResponse},
};

#[derive(Clone)]
pub struct AppState {
    pub mgr: Arc<SandboxManager>,
    pub max_sandboxes: usize,
}

pub async fn healthz_handler() -> &'static str {
    "ok"
}

async fn create_sandbox(
    State(state): State<AppState>,
    Json(req): Json<CreateSandboxRequest>,
) -> Result<Json<CreateSandboxResponse>, AppError> {
    if state.mgr.count() >= state.max_sandboxes {
        return Err(AppError::Internal("controller at capacity".into()));
    }
    let id = req.sandbox_id.clone();
    state.mgr.create(req).await?;
    Ok(Json(CreateSandboxResponse { sandbox_id: id }))
}

async fn exec_sandbox(
    State(state): State<AppState>,
    Json(req): Json<ExecRequest>,
) -> Result<Json<ExecResponse>, AppError> {
    let args = req.args.unwrap_or_default();
    let ttl = req.timeout_seconds.unwrap_or(30);
    let out = state.mgr.exec(&req.sandbox_id, &req.command, &args, req.env, ttl).await?;
    Ok(Json(out))
}

async fn delete_sandbox(
    State(state): State<AppState>,
    Json(req): Json<DeleteSandboxRequest>,
) -> Result<(), AppError> {
    state.mgr.delete(&req.sandbox_id).await
}

pub async fn serve(cfg: Config) {
    let state = AppState {
        mgr: Arc::new(SandboxManager::new("/usr/local/bin", &cfg)),
        max_sandboxes: cfg.max_sandboxes,
    };

    let app = Router::new()
        .route("/healthz", get(healthz_handler))
        .route("/v1/sandboxes", post(create_sandbox))
        .route("/v1/exec", post(exec_sandbox))
        .route("/v1/sandboxes", delete(delete_sandbox))
        .with_state(state);

    let addr = SocketAddr::from(([0, 0, 0, 0], cfg.port));

    if cfg.mtls_disabled {
        info!("mTLS DISABLED -- listening plain HTTP on {}", addr);
        axum_server::bind(addr)
            .serve(app.into_make_service())
            .await
            .expect("server error");
    } else {
        let tls_cfg = build_tls_config(&cfg).await;
        info!("mTLS enabled -- listening HTTPS on {}", addr);
        axum_server::bind_rustls(addr, tls_cfg)
            .serve(app.into_make_service())
            .await
            .expect("server error");
    }
}
