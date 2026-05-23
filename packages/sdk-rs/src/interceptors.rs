//! tonic interceptors used by the SDK.
//!
//! Two responsibilities:
//!
//! 1. **Bearer token injection** — read the latest access token
//!    from the shared [`TokenStore`] and set the `authorization:
//!    Bearer <tok>` metadata on every outbound RPC.
//! 2. **Request-id propagation** — generate a UUIDv7 per RPC and
//!    set `x-request-id`. The Go server's `RequestIDFromMetadata`
//!    contract is matched exactly; the same id will appear in the
//!    server's structured logs.
//!
//! Both behaviours are wired through tonic's [`Interceptor`]
//! mechanism, which runs on every unary / streaming call before
//! the request leaves the client. We use a closure-based
//! interceptor for the auth client (no bearer injection — the SSO
//! and Refresh RPCs are unauthenticated by design) and a separate
//! `BearerInterceptor` for the bearer-required services.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use tonic::metadata::MetadataValue;
use tonic::service::Interceptor;
use tonic::{Request, Status};

use crate::token_store::TokenStore;

/// Interceptor that injects the access token from a [`TokenStore`]
/// into every request. If `require_token` is true and the store is
/// empty, the call is short-circuited with `Unauthenticated`
/// **before** hitting the wire — saves a round trip.
#[derive(Clone)]
pub(crate) struct BearerInterceptor {
    store: TokenStore,
    require_token: bool,
}

impl BearerInterceptor {
    pub(crate) fn new(store: TokenStore, require_token: bool) -> Self {
        Self {
            store,
            require_token,
        }
    }
}

impl Interceptor for BearerInterceptor {
    fn call(&mut self, mut req: Request<()>) -> Result<Request<()>, Status> {
        ensure_request_id(&mut req)?;

        let tokens = self.store.current();
        match (tokens, self.require_token) {
            (Some(t), _) => {
                let val: MetadataValue<_> =
                    format!("Bearer {}", t.access_token).parse().map_err(|e| {
                        Status::internal(format!(
                            "kapp-sdk: access token contained invalid header bytes: {e}"
                        ))
                    })?;
                req.metadata_mut().insert("authorization", val);
            }
            (None, true) => {
                // Fail fast — no point making a server round-trip
                // for a call we know will be rejected.
                return Err(Status::unauthenticated(
                    "kapp-sdk: TokenStore is empty; call auth().exchange(...) first",
                ));
            }
            (None, false) => {
                // SSO / Refresh path. Continue without auth header.
            }
        }
        Ok(req)
    }
}

/// Interceptor used by the auth (SSO / Refresh) client. Adds the
/// request-id but never injects a bearer token — the auth RPCs are
/// unauthenticated by design and the server's auth interceptor
/// has them on the allowlist.
#[derive(Clone)]
pub(crate) struct AuthSurfaceInterceptor {
    /// Optional marker used by tests to assert the interceptor ran.
    /// Cheap (one atomic load + store) and won't show up in
    /// release builds.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) called: Arc<AtomicBool>,
}

impl AuthSurfaceInterceptor {
    pub(crate) fn new() -> Self {
        Self {
            called: Arc::new(AtomicBool::new(false)),
        }
    }
}

impl Interceptor for AuthSurfaceInterceptor {
    fn call(&mut self, mut req: Request<()>) -> Result<Request<()>, Status> {
        ensure_request_id(&mut req)?;
        self.called.store(true, Ordering::Relaxed);
        Ok(req)
    }
}

/// Inject an `x-request-id` metadata header if the caller didn't
/// supply one. The Go server's `internal/platform.RequestIDFromMetadata`
/// canonicalises whatever we send here; we generate UUIDv7 because
/// the embedded timestamp lets server-side logs sort by request-id
/// prefix.
fn ensure_request_id(req: &mut Request<()>) -> Result<(), Status> {
    if req.metadata().get("x-request-id").is_some() {
        return Ok(());
    }
    let rid = uuid::Uuid::now_v7().to_string();
    let val: MetadataValue<_> = rid.parse().map_err(|e| {
        Status::internal(format!(
            "kapp-sdk: generated request-id was not header-safe: {e}"
        ))
    })?;
    req.metadata_mut().insert("x-request-id", val);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::token_store::Tokens;
    use std::time::{Duration, SystemTime};

    fn fake_tokens(at: &str) -> Tokens {
        Tokens {
            access_token: at.into(),
            refresh_token: "rt".into(),
            expires_at: SystemTime::now() + Duration::from_secs(60),
            tenant_id: "t".into(),
            session_id: "s".into(),
        }
    }

    #[test]
    fn bearer_interceptor_injects_token() {
        let store = TokenStore::new();
        store.set(fake_tokens("abc"));
        let mut interceptor = BearerInterceptor::new(store, true);
        let req = Request::new(());
        let out = interceptor.call(req).unwrap();
        let auth = out.metadata().get("authorization").unwrap();
        assert_eq!(auth.to_str().unwrap(), "Bearer abc");
        // Request-id was auto-generated.
        assert!(out.metadata().get("x-request-id").is_some());
    }

    #[test]
    fn bearer_interceptor_short_circuits_when_required_and_empty() {
        let store = TokenStore::new();
        let mut interceptor = BearerInterceptor::new(store, true);
        let req = Request::new(());
        let err = interceptor.call(req).unwrap_err();
        assert_eq!(err.code(), tonic::Code::Unauthenticated);
    }

    #[test]
    fn bearer_interceptor_passes_through_when_not_required_and_empty() {
        let store = TokenStore::new();
        let mut interceptor = BearerInterceptor::new(store, false);
        let req = Request::new(());
        let out = interceptor.call(req).unwrap();
        assert!(out.metadata().get("authorization").is_none());
        assert!(out.metadata().get("x-request-id").is_some());
    }

    #[test]
    fn caller_supplied_request_id_preserved() {
        let store = TokenStore::new();
        store.set(fake_tokens("abc"));
        let mut interceptor = BearerInterceptor::new(store, true);
        let mut req = Request::new(());
        req.metadata_mut()
            .insert("x-request-id", "caller-12345".parse().unwrap());
        let out = interceptor.call(req).unwrap();
        assert_eq!(
            out.metadata()
                .get("x-request-id")
                .unwrap()
                .to_str()
                .unwrap(),
            "caller-12345"
        );
    }

    #[test]
    fn auth_surface_interceptor_runs_without_token() {
        let mut interceptor = AuthSurfaceInterceptor::new();
        let req = Request::new(());
        let out = interceptor.call(req).unwrap();
        assert!(out.metadata().get("authorization").is_none());
        assert!(out.metadata().get("x-request-id").is_some());
        assert!(interceptor.called.load(Ordering::Relaxed));
    }

    #[test]
    fn request_id_is_uuidv7_format() {
        let mut interceptor = AuthSurfaceInterceptor::new();
        let out = interceptor.call(Request::new(())).unwrap();
        let rid = out
            .metadata()
            .get("x-request-id")
            .unwrap()
            .to_str()
            .unwrap();
        // Length: 36 chars, dashes at fixed positions.
        assert_eq!(rid.len(), 36);
        let chars: Vec<char> = rid.chars().collect();
        assert_eq!(chars[8], '-');
        assert_eq!(chars[13], '-');
        // Version field (14th char, 0-indexed 14) is '7' for UUIDv7.
        assert_eq!(chars[14], '7');
    }
}
