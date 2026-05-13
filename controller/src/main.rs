use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::from_default_env())
        .init();
    let cfg = boxy_controller::config::Config::from_env();
    tracing::info!(port = cfg.port, mtls_disabled = cfg.mtls_disabled, "boxy-controller starting");
    boxy_controller::routes::serve(cfg).await;
}
