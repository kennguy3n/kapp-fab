//! KType registry client: register, fetch, and list KType schema
//! definitions.
//!
//! # Client-side schema validation
//!
//! The `Register` path validates the supplied JSON Schema document
//! **client-side** before issuing the RPC. This serves two goals:
//!
//! 1. Fast feedback. A malformed schema fails locally without a
//!    network round trip; the caller gets a typed
//!    [`crate::KappError::SchemaInvalid`] with a list of validation
//!    errors instead of a server-side `InvalidArgument`.
//! 2. Hard error contract. The server's [`gojsonschema`](https://github.com/xeipuuv/gojsonschema)
//!    package validates against JSON Schema Draft-07 (the kapp-fab
//!    KType registry standard). We use the [`jsonschema`] crate's
//!    Draft-07 validator client-side so both sides agree on the
//!    same draft semantics.

use jsonschema::Draft;
use prost::bytes::Bytes;
use serde_json::Value;
use tonic::codegen::InterceptedService;
use tonic::transport::Channel;

use crate::error::{KappError, Result, SchemaError};
use crate::interceptors::BearerInterceptor;
use crate::pb;
use crate::pb::k_type_service_client::KTypeServiceClient;
use crate::refresh::RefreshSingleflight;
use crate::token_store::TokenStore;

type KTypeChannel = InterceptedService<Channel, BearerInterceptor>;

/// A KType, as returned by the registry.
#[derive(Debug, Clone)]
pub struct KType {
    /// Logical KType name (unique per `(name, version)` tuple).
    pub name: String,
    /// Schema version number. Monotonic per `name`.
    pub version: i32,
    /// Raw JSON Schema document bytes. Use [`Self::schema_json`]
    /// for a parsed [`serde_json::Value`].
    pub schema: Vec<u8>,
    /// Server-issued creation timestamp, RFC 3339 (nano).
    pub created_at: String,
}

impl KType {
    /// Parse `schema` as JSON.
    ///
    /// Returns [`KappError::Codec`] if the server-returned bytes
    /// were not valid UTF-8 JSON (should not happen against a
    /// conforming kapp-fab server).
    pub fn schema_json(&self) -> Result<Value> {
        serde_json::from_slice(&self.schema).map_err(KappError::from)
    }
}

impl From<pb::KType> for KType {
    fn from(p: pb::KType) -> Self {
        Self {
            name: p.name,
            version: p.version,
            schema: p.schema,
            created_at: p.created_at,
        }
    }
}

/// High-level helper around the `kapp.v1.KTypeService` gRPC client.
///
/// Cheap to clone — wraps the channel + token store via reference
/// counts.
#[derive(Clone)]
pub struct KTypeClient {
    client: KTypeServiceClient<KTypeChannel>,
    store: TokenStore,
    refresher: RefreshSingleflight,
    auto_refresh: bool,
    auth_for_refresh: crate::auth::SsoFlow,
}

impl KTypeClient {
    pub(crate) fn new(
        channel: Channel,
        store: TokenStore,
        refresher: RefreshSingleflight,
        auto_refresh: bool,
    ) -> Self {
        let interceptor = BearerInterceptor::new(store.clone(), true);
        let client = KTypeServiceClient::with_interceptor(channel.clone(), interceptor);
        // The refresh path uses an UNAUTHENTICATED SsoFlow client
        // sharing the same channel — channels are multiplexed so
        // this is cheap.
        let auth_for_refresh = crate::auth::SsoFlow::new(channel, store.clone());
        Self {
            client,
            store,
            refresher,
            auto_refresh,
            auth_for_refresh,
        }
    }

    /// Register a new KType (or no-op if `(name, version)` already
    /// exists with an identical schema).
    ///
    /// The schema is validated client-side as a JSON Schema
    /// Draft-07 document before the RPC is attempted. Validation
    /// failure surfaces as [`KappError::SchemaInvalid`] with the
    /// list of structural errors.
    ///
    /// `version` must be `> 0` per the proto contract.
    pub async fn register(
        &self,
        name: impl Into<String>,
        version: i32,
        schema: Value,
    ) -> Result<RegisterResult> {
        let name = name.into();
        if name.trim().is_empty() {
            return Err(KappError::InvalidArgument("name is empty".into()));
        }
        if version <= 0 {
            return Err(KappError::InvalidArgument(format!(
                "version must be > 0; got {version}"
            )));
        }

        // Validate the schema document client-side. The standard
        // jsonschema crate rejects invalid Draft-07 keywords here.
        let validator = jsonschema::options()
            .with_draft(Draft::Draft7)
            .build(&schema)
            .map_err(|err| KappError::SchemaInvalid {
                errors: vec![SchemaError {
                    instance_path: String::new(),
                    schema_path: String::new(),
                    message: format!("schema document is not valid JSON Schema Draft-07: {err}"),
                }],
            })?;
        // Successful compile is enough — we are NOT validating data
        // against the schema here; we're validating the schema
        // itself is well-formed. Drop the validator immediately.
        drop(validator);

        let schema_bytes = serde_json::to_vec(&schema)?;

        let req = pb::RegisterKTypeRequest {
            name: name.clone(),
            version,
            schema: schema_bytes,
        };

        let call = || {
            let mut client = self.client.clone();
            let req = req.clone();
            async move {
                client
                    .register_k_type(req)
                    .await
                    .map(tonic::Response::into_inner)
            }
        };
        let resp = self.with_auto_refresh(call).await?;
        Ok(RegisterResult {
            name: resp.name,
            version: resp.version,
        })
    }

    /// Fetch a specific KType by name + version.
    ///
    /// `version = None` (or `Some(0)`) returns the latest version.
    pub async fn get(&self, name: impl Into<String>, version: Option<i32>) -> Result<KType> {
        let name = name.into();
        if name.trim().is_empty() {
            return Err(KappError::InvalidArgument("name is empty".into()));
        }
        let version = version.unwrap_or(0);
        if version < 0 {
            return Err(KappError::InvalidArgument(format!(
                "version must be >= 0 (0 means latest); got {version}"
            )));
        }
        let req = pb::GetKTypeRequest {
            name: name.clone(),
            version,
        };
        let call = || {
            let mut client = self.client.clone();
            let req = req.clone();
            async move {
                client
                    .get_k_type(req)
                    .await
                    .map(tonic::Response::into_inner)
            }
        };
        let resp = self.with_auto_refresh(call).await?;
        let kt = resp.ktype.ok_or_else(|| {
            KappError::Codec("server returned GetKType with empty ktype field".into())
        })?;
        Ok(kt.into())
    }

    /// List every KType registered on the server.
    ///
    /// KTypes are platform metadata — they are NOT tenant-scoped,
    /// so this returns the full registry regardless of which tenant
    /// the caller's access token is scoped to.
    pub async fn list(&self) -> Result<Vec<KType>> {
        let call = || {
            let mut client = self.client.clone();
            async move {
                client
                    .list_k_types(pb::ListKTypesRequest {})
                    .await
                    .map(tonic::Response::into_inner)
            }
        };
        let resp = self.with_auto_refresh(call).await?;
        Ok(resp.ktypes.into_iter().map(Into::into).collect())
    }

    /// Run `call`, and on `Unauthenticated` perform a single-flight
    /// refresh + one retry. Returns the original error otherwise.
    async fn with_auto_refresh<F, Fut, T>(&self, call: F) -> Result<T>
    where
        F: Fn() -> Fut + Send + Sync,
        Fut: std::future::Future<Output = std::result::Result<T, tonic::Status>> + Send,
        T: Send,
    {
        let snap = self.store.snapshot();
        let first = call().await;
        match first {
            Ok(v) => Ok(v),
            Err(status) if status.code() == tonic::Code::Unauthenticated && self.auto_refresh => {
                // Snapshot the version we observed when the call
                // failed. The refresh singleflight ensures only one
                // refresh runs even if many concurrent tasks 401.
                let auth = self.auth_for_refresh.clone();
                self.refresher
                    .refresh(&self.store, snap, |_snap_inner| async move {
                        let result = auth.refresh_from_store().await?;
                        // refresh_from_store already wrote to the
                        // store via set(); we don't need to call
                        // swap_if_version here because set() is
                        // unconditional and bumps the version
                        // monotonically. The debug_assert just
                        // confirms the server returned a non-empty
                        // access token before we declare success.
                        debug_assert!(auth_store_after_refresh(&result));
                        Ok(())
                    })
                    .await?;

                // Retry exactly once with the (now-refreshed) token.
                call().await.map_err(KappError::from)
            }
            Err(status) => Err(KappError::from(status)),
        }
    }
}

/// Tiny sanity-check used only inside the auto-refresh closure to
/// keep `debug_assert!` happy without referencing the store
/// directly (which would borrow into the closure twice).
fn auth_store_after_refresh(result: &crate::auth::ExchangeResult) -> bool {
    !result.access_token.is_empty()
}

/// Result of [`KTypeClient::register`].
///
/// `name` and `version` echo the request payload — the server
/// confirms what was stored. There is no server-assigned version
/// today (see `proto/kapp/v1/ktype.proto:35` for the contract).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RegisterResult {
    /// Echo of the registered KType name.
    pub name: String,
    /// Echo of the registered KType version.
    pub version: i32,
}

// Suppress unused-Bytes warning for prost re-export.
#[allow(dead_code)]
fn _bytes_marker() -> Bytes {
    Bytes::new()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn ktype_schema_json_roundtrip() {
        let raw = serde_json::to_vec(&json!({
            "$schema": "http://json-schema.org/draft-07/schema#",
            "type": "object",
            "properties": { "id": { "type": "string" } },
        }))
        .unwrap();
        let kt = KType {
            name: "person".into(),
            version: 1,
            schema: raw,
            created_at: "2025-01-01T00:00:00Z".into(),
        };
        let parsed = kt.schema_json().unwrap();
        assert_eq!(parsed["type"], "object");
        assert_eq!(parsed["properties"]["id"]["type"], "string");
    }

    #[test]
    fn ktype_schema_json_rejects_invalid_bytes() {
        let kt = KType {
            name: "broken".into(),
            version: 1,
            schema: b"\xff\xff not json".to_vec(),
            created_at: String::new(),
        };
        let err = kt.schema_json().unwrap_err();
        match err {
            KappError::Codec(msg) => assert!(msg.contains("json")),
            other => panic!("expected Codec error, got {other:?}"),
        }
    }

    #[test]
    fn pb_ktype_into_sdk_ktype() {
        let p = pb::KType {
            name: "asset".into(),
            version: 3,
            schema: b"{\"type\":\"object\"}".to_vec(),
            created_at: "2025-01-02T03:04:05Z".into(),
        };
        let kt: KType = p.into();
        assert_eq!(kt.name, "asset");
        assert_eq!(kt.version, 3);
        assert_eq!(kt.schema_json().unwrap()["type"], "object");
    }
}
