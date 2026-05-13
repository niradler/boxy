use axum_server::tls_rustls::RustlsConfig;
use rustls::{
    server::WebPkiClientVerifier,
    ServerConfig, RootCertStore,
};
use rustls_pemfile::{certs, private_key};
use std::{fs, io::BufReader, sync::Arc};

use crate::config::Config;

pub async fn build_tls_config(cfg: &Config) -> RustlsConfig {
    let cert_pem = fs::read(&cfg.tls_cert_path)
        .unwrap_or_else(|e| panic!("read cert {}: {}", cfg.tls_cert_path, e));
    let key_pem = fs::read(&cfg.tls_key_path)
        .unwrap_or_else(|e| panic!("read key {}: {}", cfg.tls_key_path, e));
    let ca_pem = fs::read(&cfg.tls_ca_path)
        .unwrap_or_else(|e| panic!("read CA {}: {}", cfg.tls_ca_path, e));

    let mut root_store = RootCertStore::empty();
    let ca_certs = certs(&mut BufReader::new(ca_pem.as_slice()))
        .collect::<Result<Vec<_>, _>>()
        .expect("parse CA cert");
    for c in ca_certs {
        root_store.add(c).expect("add CA cert");
    }
    let verifier = WebPkiClientVerifier::builder(Arc::new(root_store))
        .build()
        .expect("build client verifier");

    let server_certs = certs(&mut BufReader::new(cert_pem.as_slice()))
        .collect::<Result<Vec<_>, _>>()
        .expect("parse server cert");
    let key = private_key(&mut BufReader::new(key_pem.as_slice()))
        .expect("parse private key")
        .expect("at least one private key in TLS secret");

    let server_cfg = ServerConfig::builder()
        .with_client_cert_verifier(verifier)
        .with_single_cert(server_certs, key)
        .expect("build TLS config");

    RustlsConfig::from_config(Arc::new(server_cfg))
}
