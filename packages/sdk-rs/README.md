# kapp-sdk

Official Rust SDK for the [kapp-fab](https://github.com/kennguy3n/kapp-fab) gRPC API (`kapp.v1`).

This crate is a high-level, async client that wraps the auto-generated `tonic` bindings with:

- A connection-pooled [`KappClient`] handle (one HTTP/2 channel, many concurrent streams)
- Automatic bearer-token injection via a [`tower`] interceptor
- `x-request-id` propagation matching the server-side contract
- Real TLS with system trust roots + Mozilla webpki roots (TLS-on-https, plaintext-on-http; mixing the two is rejected at construction)
- Single-flight refresh: N concurrent `Unauthenticated` failures collapse onto exactly one `Refresh` RPC
- Client-side JSON Schema (Draft-07) validation on `RegisterKType` — invalid schemas fail without a round trip
- Typed `KappError` enum across every fallible call (no `Box<dyn Error>` leakage)

## Quickstart

```rust
use kapp_sdk::{ClientConfig, KappClient};

#[tokio::main]
async fn main() -> Result<(), kapp_sdk::KappError> {
    let cfg = ClientConfig::builder("https://api.example.com")
        .user_agent("my-app/1.0")
        .build()?;
    let client = KappClient::connect(cfg).await?;

    // Trade a KChat OAuth code for a kapp access+refresh pair.
    let _result = client
        .auth()
        .exchange("kchat-oauth-code", "https://app.example.com/cb", None)
        .await?;

    // Now make authenticated calls — the bearer token is injected
    // automatically from the shared TokenStore.
    let ktypes = client.ktype().list().await?;
    for kt in ktypes {
        println!("{} v{}", kt.name, kt.version);
    }
    Ok(())
}
```

## Auth flow

```
                 ┌───────────────────────────────────────────────┐
                 │                kapp-fab gRPC API              │
                 └──────────────┬────────────────────────────────┘
                                │
       ┌──────────────────┐     │   ┌────────────────────────┐
       │ SsoFlow.exchange ├─────┼──►│ AuthService.SSO        │
       └──────────────────┘     │   └────────────────────────┘
                                │
       ┌──────────────────┐     │   ┌────────────────────────┐
       │ SsoFlow.refresh  ├─────┼──►│ AuthService.Refresh    │
       └──────────────────┘     │   └────────────────────────┘
                                │
                                │   401 → SDK refreshes → retries once
       ┌──────────────────┐     │   ┌────────────────────────┐
       │ ktype.list/get…  ├─────┼──►│ KTypeService.*         │
       └──────────────────┘     │   └────────────────────────┘
```

### Token storage and persistence

The SDK keeps tokens in an in-memory [`TokenStore`] that is `Arc`-cloneable. By default, nothing is persisted to disk — your application owns persistence. Snapshot/restore:

```rust
use kapp_sdk::{KappClient, ClientConfig, TokenStore};

let store = TokenStore::new();
let cfg = ClientConfig::builder("https://api.example.com")
    .token_store(store.clone())
    .build()?;
let client = KappClient::connect(cfg).await?;
// ... authenticate ...

// Persist before shutdown.
let snapshot = store.snapshot();
my_app_keystore::save(&snapshot.tokens)?;

// On next launch:
let mut new_store = TokenStore::new();
if let Some(tokens) = my_app_keystore::load()? {
    new_store = TokenStore::with_tokens(tokens);
}
# Ok::<(), kapp_sdk::KappError>(())
```

### Single-flight refresh under concurrency

If multiple in-flight RPCs simultaneously see `Unauthenticated` (e.g. all of them were issued just before the access token expired), the SDK collapses them onto **one** `Refresh` RPC and replays each original call with the new token. This is verified by `auto_refresh_singleflight_across_concurrent_calls` in `tests/sdk_e2e.rs`, which fires 8 concurrent `list()` calls into a refreshed-token state and asserts the server observed exactly one `Refresh`.

## Client-side schema validation

`KTypeClient::register` validates the supplied schema **as a JSON Schema Draft-07 document** before issuing the RPC. Invalid schemas fail with a typed `KappError::SchemaInvalid` containing the list of structural errors. No network round trip is made.

```rust
use kapp_sdk::{ClientConfig, KappClient, KappError};
use serde_json::json;

# async fn run() -> Result<(), KappError> {
# let client = KappClient::connect(ClientConfig::builder("http://localhost:9090").build()?).await?;
let bad = json!({ "type": 12345 });  // "type" must be a string or array.
let err = client.ktype().register("broken", 1, bad).await.unwrap_err();
match err {
    KappError::SchemaInvalid { errors } => {
        for e in errors {
            eprintln!("schema problem: {e}");
        }
    }
    other => Err(other)?,
}
# Ok(())
# }
```

## TLS

By default, `https://` endpoints use both the system trust store and the bundled Mozilla webpki roots. For environments using an internal PKI, append your CA via [`TlsMode::WithCustomCa`]:

```rust
use kapp_sdk::{ClientConfig, TlsMode};

let pem = std::fs::read("internal-ca.pem")?;
let cfg = ClientConfig::builder("https://internal.example.com")
    .tls(TlsMode::WithCustomCa(pem))
    .build()?;
# Ok::<(), kapp_sdk::KappError>(())
```

`http://` endpoints are plaintext HTTP/2. Mixing schemes is rejected at builder time — passing a `TlsMode::System` to an `http://` endpoint, or `TlsMode::Disabled` to an `https://` endpoint, returns `KappError::Config`.

## Error handling

Every fallible SDK call returns `kapp_sdk::Result<T> = std::result::Result<T, KappError>`. `KappError` variants:

| Variant | When |
|---|---|
| `Transport(...)` | TCP / TLS / HTTP/2 layer failure |
| `Status { code, message, source }` | gRPC server returned a non-OK status |
| `Auth(AuthError)` | Auth-specific (no token, refresh failed, unauthenticated) |
| `SchemaInvalid { errors }` | Client-side JSON Schema validation failed |
| `Codec(String)` | Encode / decode error (should never happen) |
| `InvalidArgument(String)` | Caller passed an invalid argument |
| `Config(String)` | Builder rejected the configuration |

`KappError::is_retryable()` returns true for `Transport`, `Unavailable`, `DeadlineExceeded`, and `Aborted`. Use it to drive an outer backoff policy.

## Examples

```bash
KAPP_ENDPOINT=https://api.example.com \
KAPP_SSO_CODE=<kchat-oauth-code> \
KAPP_REDIRECT_URI=https://app.example.com/cb \
  cargo run --example sso_then_list_ktypes

KAPP_ENDPOINT=https://api.example.com \
KAPP_SSO_CODE=<kchat-oauth-code> \
KAPP_REDIRECT_URI=https://app.example.com/cb \
  cargo run --example register_ktype_with_validation
```

## Versioning

`kapp-sdk` tracks the `kapp.v1` proto package. Breaking changes to the wire surface (which are enforced by [`buf breaking`](https://buf.build/docs/breaking) in CI on the monorepo) will trigger a major version bump of this crate.

## Build & test

```bash
cargo build
cargo test
cargo clippy --all-targets --all-features -- -D warnings
cargo fmt --check
```

The build script (`build.rs`) compiles the `.proto` files in `proto/kapp/v1/` via [`protox`](https://crates.io/crates/protox) — **no system `protoc` binary required**. Downstream consumers running `cargo install kapp-sdk` likewise don't need protoc on PATH.

## License

Apache 2.0.
