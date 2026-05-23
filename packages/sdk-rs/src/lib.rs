//! kapp-fab Rust SDK.
//!
//! High-level, async client for the kapp-fab gRPC API (v1). Built
//! on top of [`tonic`] for transport and [`prost`] for codecs.
//!
//! # What this crate gives you
//!
//! - [`KappClient`]: a connection-pooled gRPC channel handle with
//!   automatic bearer-token injection, request-id propagation, and
//!   single-flight refresh on `Unauthenticated`.
//! - [`auth::SsoFlow`]: exchange a KChat OAuth code for an access +
//!   refresh token pair; refresh on demand.
//! - [`ktype::KTypeClient`]: register, fetch, and list KType schema
//!   definitions, with client-side JSON Schema validation **before**
//!   the RPC so callers get a typed error instead of a remote
//!   `InvalidArgument`.
//! - [`token_store::TokenStore`]: a lock-free, version-counted
//!   storage handle. Concurrent `Unauthenticated` failures coalesce
//!   onto a single refresh attempt; callers can snapshot/restore
//!   for persistence.
//! - [`error::KappError`]: typed error variants — no `Box<dyn Error>`
//!   leakage. Every error a public method can return is enumerable.
//!
//! # Example
//!
//! ```no_run
//! use kapp_sdk::{KappClient, ClientConfig};
//!
//! # async fn run() -> Result<(), kapp_sdk::KappError> {
//! let cfg = ClientConfig::builder("https://api.example.com")
//!     .user_agent("my-app/1.0")
//!     .build()?;
//! let client = KappClient::connect(cfg).await?;
//!
//! // Authenticate via SSO.
//! let result = client
//!     .auth()
//!     .exchange("kchat-oauth-code", "https://app.example.com/cb", None)
//!     .await?;
//! tracing::info!(tenant = %result.tenant_id, "logged in");
//!
//! // Now make authenticated calls. The bearer token is injected
//! // automatically from the shared TokenStore.
//! let ktypes = client.ktype().list().await?;
//! for kt in ktypes {
//!     println!("{} v{}", kt.name, kt.version);
//! }
//! # Ok(())
//! # }
//! ```
//!
//! # Wire compatibility
//!
//! The SDK targets the `kapp.v1` proto package as served by the
//! kapp-fab gRPC server (see `internal/grpc/server.go` in the
//! monorepo). Wire-format guarantees:
//!
//! - Timestamps are RFC3339-nano strings, **not**
//!   `google.protobuf.Timestamp`. This matches the existing REST
//!   serialisation; SDK consumers calling [`chrono::DateTime::parse_from_rfc3339`]
//!   on `created_at` fields get the expected behaviour.
//! - `bytes schema` carries raw JSON. The SDK exposes a typed
//!   [`serde_json::Value`] accessor in [`ktype::KType`] so callers
//!   don't have to handle the conversion.

#![cfg_attr(docsrs, feature(doc_auto_cfg))]
#![warn(rust_2018_idioms)]
#![deny(unsafe_code)]

pub mod auth;
pub mod client;
pub mod config;
pub mod error;
pub mod ktype;
pub mod token_store;

mod interceptors;
mod refresh;

/// Generated protobuf bindings for the `kapp.v1` package.
///
/// Consumers normally interact with the typed wrappers in
/// [`auth`] and [`ktype`] rather than these raw protobuf types
/// directly. Re-exported for advanced use cases (custom retry
/// policy, raw streaming RPCs, etc.).
///
/// The `missing_docs` lint is intentionally relaxed for this
/// module — every field/struct here is generated from the proto
/// schema and is documented at the proto level (see `proto/kapp/v1/*.proto`
/// in the kapp-fab monorepo); duplicating those comments into
/// the generated Rust would only create a maintenance hazard.
#[allow(missing_docs, clippy::all, clippy::pedantic, clippy::nursery)]
pub mod pb {
    // tonic-build emits a single file `kapp.v1.rs` into OUT_DIR
    // and the canonical way to mount it is `include_proto!`.
    tonic::include_proto!("kapp.v1");
}

pub use client::KappClient;
pub use config::{ClientConfig, ClientConfigBuilder, TlsMode};
pub use error::{AuthError, KappError, Result, SchemaError};
pub use token_store::{TokenSnapshot, TokenStore, Tokens};
