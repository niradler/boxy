use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde_json::json;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum AppError {
    #[error("sandbox not found: {0}")]
    NotFound(String),
    #[error("sandbox already exists: {0}")]
    AlreadyExists(String),
    #[error("vm error: {0}")]
    Vm(String),
    #[error("exec timeout")]
    Timeout,
    #[error("internal: {0}")]
    Internal(String),
}

impl IntoResponse for AppError {
    fn into_response(self) -> Response {
        let (status, msg) = match &self {
            AppError::NotFound(_) => (StatusCode::NOT_FOUND, self.to_string()),
            AppError::AlreadyExists(_) => (StatusCode::CONFLICT, self.to_string()),
            AppError::Timeout => (StatusCode::REQUEST_TIMEOUT, self.to_string()),
            _ => (StatusCode::INTERNAL_SERVER_ERROR, self.to_string()),
        };
        (status, Json(json!({"error": msg}))).into_response()
    }
}
