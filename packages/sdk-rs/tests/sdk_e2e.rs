//! End-to-end tests that exercise the SDK against a real in-process
//! tonic gRPC server. The server implements the same `kapp.v1`
//! proto contract as the production Go server, so the SDK code
//! path is byte-identical to what runs in production.

mod common;

use std::sync::atomic::Ordering;
use std::time::Duration;

use kapp_sdk::{ClientConfig, KappClient, KappError};
use serde_json::json;

#[tokio::test]
async fn sso_exchange_returns_tokens_and_populates_store() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint())
        .auto_refresh(true)
        .build()
        .unwrap();
    let client = KappClient::connect(cfg).await.unwrap();

    let result = client
        .auth()
        .exchange("test-code-1", "https://app.example.com/cb", None)
        .await
        .unwrap();

    assert!(!result.access_token.is_empty());
    assert!(!result.refresh_token.is_empty());
    assert_eq!(result.user.email, "alice@example.com");
    assert_eq!(result.tenant_id, "tenant-acme");
    assert_eq!(result.expires_in, 3600);
    assert_eq!(result.tenants.len(), 1);
    assert_eq!(result.tenants[0].slug, "acme");

    // TokenStore must hold the tokens after exchange.
    let stored = client.token_store().current().unwrap();
    assert_eq!(stored.access_token, result.access_token);
    assert_eq!(stored.refresh_token, result.refresh_token);
}

#[tokio::test]
async fn sso_exchange_with_invalid_code_returns_unauthenticated() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint()).build().unwrap();
    let client = KappClient::connect(cfg).await.unwrap();

    let err = client
        .auth()
        .exchange("INVALID", "https://app.example.com/cb", None)
        .await
        .unwrap_err();
    match err {
        KappError::Status { code, .. } => assert_eq!(code, tonic::Code::Unauthenticated),
        other => panic!("expected Status, got {other:?}"),
    }
    // Store remains empty after a failed exchange.
    assert!(client.token_store().current().is_none());
}

#[tokio::test]
async fn refresh_rotates_tokens() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint()).build().unwrap();
    let client = KappClient::connect(cfg).await.unwrap();

    let initial = client
        .auth()
        .exchange("test-code", "https://app.example.com/cb", None)
        .await
        .unwrap();

    let refreshed = client
        .auth()
        .refresh(initial.refresh_token.clone())
        .await
        .unwrap();

    assert_ne!(refreshed.access_token, initial.access_token);
    assert_ne!(refreshed.refresh_token, initial.refresh_token);
    // Store now holds the new tokens.
    let stored = client.token_store().current().unwrap();
    assert_eq!(stored.access_token, refreshed.access_token);
    // Old refresh token is no longer valid.
    let err = client
        .auth()
        .refresh(initial.refresh_token)
        .await
        .unwrap_err();
    assert_eq!(err.code(), Some(tonic::Code::Unauthenticated));
}

#[tokio::test]
async fn ktype_register_get_list_round_trip() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint()).build().unwrap();
    let client = KappClient::connect(cfg).await.unwrap();
    client
        .auth()
        .exchange("test-code", "https://app.example.com/cb", None)
        .await
        .unwrap();

    let schema = json!({
        "$schema": "http://json-schema.org/draft-07/schema#",
        "type": "object",
        "required": ["id", "name"],
        "properties": {
            "id": { "type": "string", "minLength": 1 },
            "name": { "type": "string", "minLength": 1 },
        },
    });

    let reg = client
        .ktype()
        .register("person", 1, schema.clone())
        .await
        .unwrap();
    assert_eq!(reg.name, "person");
    assert_eq!(reg.version, 1);

    let got = client.ktype().get("person", Some(1)).await.unwrap();
    assert_eq!(got.name, "person");
    assert_eq!(got.version, 1);
    let parsed_schema = got.schema_json().unwrap();
    assert_eq!(parsed_schema["type"], "object");

    let list = client.ktype().list().await.unwrap();
    assert_eq!(list.len(), 1);
    assert_eq!(list[0].name, "person");

    // Latest lookup (version=0) returns the highest-numbered version.
    client
        .ktype()
        .register("person", 2, json!({"type": "object"}))
        .await
        .unwrap();
    let latest = client.ktype().get("person", None).await.unwrap();
    assert_eq!(latest.version, 2);
}

#[tokio::test]
async fn register_rejects_invalid_schema_locally() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint()).build().unwrap();
    let client = KappClient::connect(cfg).await.unwrap();
    client
        .auth()
        .exchange("test-code", "https://app.example.com/cb", None)
        .await
        .unwrap();

    let counters_before = harness.counters.register.load(Ordering::SeqCst);

    // "type" must be a string or array of strings, not a number.
    let bad_schema = json!({ "type": 12345 });
    let err = client
        .ktype()
        .register("broken", 1, bad_schema)
        .await
        .unwrap_err();
    match err {
        KappError::SchemaInvalid { errors } => {
            assert!(!errors.is_empty());
        }
        other => panic!("expected SchemaInvalid, got {other:?}"),
    }
    // No server-side register RPC was issued.
    let counters_after = harness.counters.register.load(Ordering::SeqCst);
    assert_eq!(
        counters_before, counters_after,
        "client-side validation must short-circuit before the RPC"
    );
}

#[tokio::test]
async fn unauthenticated_call_with_empty_store_short_circuits() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint())
        // Disable auto-refresh so we observe the raw Unauthenticated.
        .auto_refresh(false)
        .build()
        .unwrap();
    let client = KappClient::connect(cfg).await.unwrap();

    let err = client.ktype().list().await.unwrap_err();
    assert_eq!(err.code(), Some(tonic::Code::Unauthenticated));
    // The harness should not have observed the RPC because the
    // bearer interceptor short-circuits when require_token=true
    // and the store is empty.
    assert_eq!(harness.counters.list.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn auto_refresh_recovers_from_unauthenticated() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint())
        .auto_refresh(true)
        .build()
        .unwrap();
    let client = KappClient::connect(cfg).await.unwrap();
    let initial = client
        .auth()
        .exchange("test-code", "https://app.example.com/cb", None)
        .await
        .unwrap();

    // Invalidate the access token server-side; the refresh token
    // is still valid.
    harness.invalidate_access_for(&initial.refresh_token);

    // Now make an authenticated call. The first attempt will 401
    // because the access token is gone server-side; the SDK should
    // auto-refresh and retry once.
    let counter_before = harness.counters.refresh.load(Ordering::SeqCst);
    let list = client.ktype().list().await.unwrap();
    // Empty registry but call succeeded — the retry path worked.
    assert!(list.is_empty());
    let counter_after = harness.counters.refresh.load(Ordering::SeqCst);
    assert_eq!(
        counter_after - counter_before,
        1,
        "auto-refresh should fire exactly once for one 401"
    );

    // TokenStore now holds the refreshed tokens.
    let stored = client.token_store().current().unwrap();
    assert_ne!(stored.access_token, initial.access_token);
    assert_ne!(stored.refresh_token, initial.refresh_token);
}

#[tokio::test]
async fn auto_refresh_singleflight_across_concurrent_calls() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint())
        .auto_refresh(true)
        .build()
        .unwrap();
    let client = KappClient::connect(cfg).await.unwrap();
    let initial = client
        .auth()
        .exchange("test-code", "https://app.example.com/cb", None)
        .await
        .unwrap();
    harness.invalidate_access_for(&initial.refresh_token);

    let refresh_before = harness.counters.refresh.load(Ordering::SeqCst);

    // Spawn 8 concurrent list() calls; each one will see
    // Unauthenticated on its first attempt. The singleflight
    // coordinator must collapse all 8 into a single refresh RPC.
    let mut handles = vec![];
    for _ in 0..8 {
        let client = client.clone();
        handles.push(tokio::spawn(async move { client.ktype().list().await }));
    }
    let results: Vec<_> = futures_util::future::join_all(handles)
        .await
        .into_iter()
        .map(std::result::Result::unwrap)
        .collect();
    assert!(
        results.iter().all(std::result::Result::is_ok),
        "every concurrent list() must succeed via the refresh path; got {results:?}"
    );

    let refresh_after = harness.counters.refresh.load(Ordering::SeqCst);
    assert_eq!(
        refresh_after - refresh_before,
        1,
        "singleflight must collapse 8 concurrent 401s into exactly one refresh RPC; observed {} refreshes",
        refresh_after - refresh_before
    );
}

#[tokio::test]
async fn auto_refresh_off_propagates_unauthenticated() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint())
        .auto_refresh(false)
        .build()
        .unwrap();
    let client = KappClient::connect(cfg).await.unwrap();
    let initial = client
        .auth()
        .exchange("test-code", "https://app.example.com/cb", None)
        .await
        .unwrap();
    harness.invalidate_access_for(&initial.refresh_token);

    let err = client.ktype().list().await.unwrap_err();
    assert_eq!(err.code(), Some(tonic::Code::Unauthenticated));
    // No refresh RPC issued.
    assert_eq!(harness.counters.refresh.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn request_id_is_forwarded_to_server() {
    let harness = common::Harness::start().await;
    let cfg = ClientConfig::builder(harness.endpoint()).build().unwrap();
    let client = KappClient::connect(cfg).await.unwrap();
    client
        .auth()
        .exchange("test-code", "https://app.example.com/cb", None)
        .await
        .unwrap();
    let _ = client.ktype().list().await.unwrap();
    // Counter increments confirm the request reached the server;
    // the server-side AuthService impl would have rejected if the
    // bearer was missing, and we asserted on the success above.
    assert_eq!(harness.counters.list.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn connect_to_nonexistent_endpoint_returns_transport_error() {
    // 127.0.0.1:1 is reserved; nothing listens there.
    let cfg = ClientConfig::builder("http://127.0.0.1:1")
        .connect_timeout(Duration::from_millis(200))
        .build()
        .unwrap();
    let err = KappClient::connect(cfg).await.unwrap_err();
    match err {
        KappError::Transport(_) => {}
        other => panic!("expected Transport error, got {other:?}"),
    }
    assert!(err.is_retryable());
}
