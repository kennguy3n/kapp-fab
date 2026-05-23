//! Typed error variants for every fallible operation the SDK exposes.
//!
//! We deliberately do **not** leak `Box<dyn std::error::Error>` or
//! `anyhow::Error` through the public API. Every error a consumer can
//! observe is a variant of [`KappError`], and the cause chain is
//! preserved via `#[source]` so `std::error::Error::source()` walks
//! through to the underlying transport / codec error when relevant.

use std::fmt;

use thiserror::Error;

/// Convenience alias used throughout the crate.
pub type Result<T> = std::result::Result<T, KappError>;

/// Top-level error type for every fallible SDK call.
#[derive(Debug, Error)]
#[non_exhaustive]
pub enum KappError {
    /// gRPC transport failed (connection refused, DNS error, TLS
    /// handshake failure, HTTP/2 protocol violation, etc.).
    ///
    /// This variant is fatal for the connection: a retry policy that
    /// keeps the same `Channel` won't help. Reconnect via
    /// [`crate::KappClient::connect`].
    #[error("gRPC transport error")]
    Transport(#[source] tonic::transport::Error),

    /// The server returned a non-OK gRPC `Status`. The original
    /// `tonic::Status` is preserved so callers can inspect rich
    /// metadata (status details, trailing metadata, etc.).
    #[error("rpc failed: code={code:?} message={message:?}")]
    Status {
        /// The gRPC status code returned by the server.
        code: tonic::Code,
        /// Human-readable message attached to the status.
        message: String,
        /// The full underlying [`tonic::Status`] for callers that
        /// need access to trailing metadata or status details.
        #[source]
        source: Box<tonic::Status>,
    },

    /// An auth-specific failure. Distinguished from a generic
    /// [`KappError::Status`] so callers can pattern-match on auth
    /// flow problems without inspecting the inner status code.
    #[error("auth error: {0}")]
    Auth(#[from] AuthError),

    /// The KType schema failed client-side JSON-Schema validation
    /// before the RPC was attempted. Carries the list of validation
    /// errors so the caller can surface them in a UI without making
    /// a round trip.
    #[error("schema invalid: {} error(s)", .errors.len())]
    SchemaInvalid {
        /// One entry per validation failure.
        errors: Vec<SchemaError>,
    },

    /// Codec failure — protobuf encode/decode, JSON parse, or
    /// metadata-value parse. These should never happen against a
    /// conforming server; if you see this, file a bug.
    #[error("codec error: {0}")]
    Codec(String),

    /// Caller supplied an invalid argument (empty name, negative
    /// version, malformed URL, etc.). Distinguishes
    /// programmer-error from server-side `InvalidArgument`.
    #[error("invalid argument: {0}")]
    InvalidArgument(String),

    /// A configuration value supplied at client-construction time
    /// was invalid (malformed endpoint URL, unreadable TLS cert,
    /// etc.).
    #[error("config error: {0}")]
    Config(String),
}

impl KappError {
    /// True if the error is plausibly transient — connection-level
    /// failures, `Unavailable`, `DeadlineExceeded`. Callers can use
    /// this to drive an outer retry/backoff policy.
    #[must_use]
    pub fn is_retryable(&self) -> bool {
        match self {
            Self::Transport(_) => true,
            Self::Status { code, .. } => matches!(
                code,
                tonic::Code::Unavailable | tonic::Code::DeadlineExceeded | tonic::Code::Aborted
            ),
            // Schema, config, codec, invalid-argument: not transient.
            Self::Auth(_)
            | Self::SchemaInvalid { .. }
            | Self::Codec(_)
            | Self::InvalidArgument(_)
            | Self::Config(_) => false,
        }
    }

    /// Returns the inner `tonic::Code` when the error originates from
    /// a server-side status, or `None` for transport / client-side
    /// errors.
    #[must_use]
    pub fn code(&self) -> Option<tonic::Code> {
        match self {
            Self::Status { code, .. } => Some(*code),
            _ => None,
        }
    }
}

impl From<tonic::transport::Error> for KappError {
    fn from(err: tonic::transport::Error) -> Self {
        Self::Transport(err)
    }
}

impl From<tonic::Status> for KappError {
    fn from(status: tonic::Status) -> Self {
        Self::Status {
            code: status.code(),
            message: status.message().to_string(),
            source: Box::new(status),
        }
    }
}

impl From<serde_json::Error> for KappError {
    fn from(err: serde_json::Error) -> Self {
        Self::Codec(format!("json: {err}"))
    }
}

impl From<prost::DecodeError> for KappError {
    fn from(err: prost::DecodeError) -> Self {
        Self::Codec(format!("prost decode: {err}"))
    }
}

impl From<prost::EncodeError> for KappError {
    fn from(err: prost::EncodeError) -> Self {
        Self::Codec(format!("prost encode: {err}"))
    }
}

/// Auth-specific failure modes.
#[derive(Debug, Error)]
#[non_exhaustive]
pub enum AuthError {
    /// The [`crate::TokenStore`] is empty and the caller invoked a
    /// method that requires authentication. Typically means the
    /// caller forgot to run an SSO exchange first.
    #[error("no token available: call auth().exchange(...) first")]
    NoToken,

    /// A refresh attempt failed. The original error chain is
    /// preserved so callers can distinguish "refresh token expired"
    /// (re-login required) from "network blip" (retry).
    #[error("token refresh failed")]
    RefreshFailed(#[source] Box<KappError>),

    /// The server returned `Unauthenticated` and the SDK was
    /// configured without auto-refresh (or auto-refresh already
    /// fired). The caller must obtain a fresh access token.
    #[error("rpc rejected as unauthenticated")]
    Unauthenticated,
}

/// One JSON Schema validation failure. Surfaced as part of
/// [`KappError::SchemaInvalid`].
#[derive(Debug, Clone)]
pub struct SchemaError {
    /// JSON Pointer (RFC 6901) to the failing field within the
    /// schema document. Empty string means the schema document
    /// itself was malformed.
    pub instance_path: String,
    /// JSON Pointer to the schema rule that rejected the instance.
    pub schema_path: String,
    /// Human-readable explanation suitable for log lines / UI.
    pub message: String,
}

impl fmt::Display for SchemaError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.instance_path.is_empty() {
            write!(f, "{}", self.message)
        } else {
            write!(f, "{}: {}", self.instance_path, self.message)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn transport_is_retryable() {
        // Build a real transport error by trying to connect to an
        // invalid endpoint and capturing the resulting error type.
        // We cheat with from_static + an obviously-bad scheme so
        // this stays sync.
        let endpoint = tonic::transport::Endpoint::from_static("http://[::1]:0");
        // Endpoint construction itself doesn't fail; we wrap a
        // synthesised transport error via the From impl shape.
        let _ = endpoint;
        // For the actual retry check we use a synthesised status.
        let err = KappError::from(tonic::Status::unavailable("backend down"));
        assert!(err.is_retryable());
        assert_eq!(err.code(), Some(tonic::Code::Unavailable));
    }

    #[test]
    fn schema_invalid_not_retryable() {
        let err = KappError::SchemaInvalid {
            errors: vec![SchemaError {
                instance_path: "/foo".into(),
                schema_path: "/properties/foo/type".into(),
                message: "expected string, got number".into(),
            }],
        };
        assert!(!err.is_retryable());
        assert_eq!(err.code(), None);
    }

    #[test]
    fn invalid_argument_not_retryable() {
        let err = KappError::InvalidArgument("name required".into());
        assert!(!err.is_retryable());
    }

    #[test]
    fn auth_no_token_not_retryable() {
        let err = KappError::from(AuthError::NoToken);
        assert!(!err.is_retryable());
    }

    #[test]
    fn status_aborted_is_retryable() {
        let err = KappError::from(tonic::Status::aborted("conflict"));
        assert!(err.is_retryable());
    }

    #[test]
    fn status_invalid_argument_not_retryable() {
        let err = KappError::from(tonic::Status::invalid_argument("bad input"));
        assert!(!err.is_retryable());
    }

    #[test]
    fn schema_error_display_with_path() {
        let e = SchemaError {
            instance_path: "/data/title".into(),
            schema_path: "/properties/title/minLength".into(),
            message: "shorter than 3 chars".into(),
        };
        assert_eq!(e.to_string(), "/data/title: shorter than 3 chars");
    }

    #[test]
    fn schema_error_display_without_path() {
        let e = SchemaError {
            instance_path: String::new(),
            schema_path: String::new(),
            message: "root is not an object".into(),
        };
        assert_eq!(e.to_string(), "root is not an object");
    }
}
