//! In-process test harness: a real tonic gRPC server speaking the
//! same `kapp.v1` proto contract as the production Go server, but
//! implemented in Rust so the tests don't need a Go binary on PATH.
//!
//! The harness implements just enough of the auth + ktype RPCs to
//! exercise the SDK's behaviour end-to-end:
//!
//! - `SSO(code, redirect_uri)` → issues an access+refresh pair
//!   keyed off the OAuth code. Predictable so tests can assert on
//!   the returned token values.
//! - `Refresh(refresh_token)` → rotates the refresh token and
//!   re-issues the access token. The current refresh token is
//!   the only one that's valid; old ones return `Unauthenticated`.
//! - `RegisterKType / GetKType / ListKTypes` → backed by an
//!   in-memory `HashMap<(name, i32), KType>`. Requires a valid
//!   bearer token; missing or stale tokens get `Unauthenticated`.
//!
//! The auth interceptor checks the `authorization` metadata
//! against the currently-issued access token. This is exactly the
//! shape the Go server enforces (see `internal/grpc/auth_interceptor.go`),
//! so the SDK code path is the same as against production.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use kapp_sdk::pb;
use kapp_sdk::pb::auth_service_server::{AuthService, AuthServiceServer};
use kapp_sdk::pb::k_type_service_server::{KTypeService, KTypeServiceServer};
use parking_lot::Mutex;
use tokio::net::TcpListener;
use tokio::sync::oneshot;
use tokio::task::JoinHandle;
use tokio_stream::wrappers::TcpListenerStream;
use tonic::transport::Server;
use tonic::{Request, Response, Status};

/// Spec for a fake user identity the harness will issue tokens for.
#[derive(Debug, Clone)]
pub struct FakeIdentity {
    pub kchat_user_id: String,
    pub email: String,
    pub display_name: String,
    pub tenant_id: String,
    pub tenant_slug: String,
    pub tenant_name: String,
    pub role: String,
}

impl FakeIdentity {
    pub fn alice() -> Self {
        Self {
            kchat_user_id: "kchat-alice".into(),
            email: "alice@example.com".into(),
            display_name: "Alice".into(),
            tenant_id: "tenant-acme".into(),
            tenant_slug: "acme".into(),
            tenant_name: "Acme".into(),
            role: "owner".into(),
        }
    }
}

#[derive(Debug, Default)]
pub struct ServerState {
    /// Authorized access tokens → (refresh_token, identity).
    access_tokens: HashMap<String, (String, FakeIdentity)>,
    /// Authorized refresh tokens → (access_token, identity).
    refresh_tokens: HashMap<String, (String, FakeIdentity)>,
    /// KType registry.
    ktypes: HashMap<(String, i32), pb::KType>,
    /// Monotonic counter so freshly-issued tokens are distinct.
    token_seq: u64,
}

/// Configuration knob: per-RPC counter for asserting on call
/// counts in tests (e.g. "Refresh was called exactly once").
#[derive(Debug, Default, Clone)]
pub struct CallCounters {
    pub sso: Arc<std::sync::atomic::AtomicUsize>,
    pub refresh: Arc<std::sync::atomic::AtomicUsize>,
    pub register: Arc<std::sync::atomic::AtomicUsize>,
    pub list: Arc<std::sync::atomic::AtomicUsize>,
    pub get: Arc<std::sync::atomic::AtomicUsize>,
}

#[derive(Debug)]
pub struct AuthHarness {
    state: Arc<Mutex<ServerState>>,
    counters: CallCounters,
}

#[tonic::async_trait]
impl AuthService for AuthHarness {
    async fn sso(
        &self,
        request: Request<pb::SsoRequest>,
    ) -> Result<Response<pb::SsoResponse>, Status> {
        self.counters
            .sso
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let req = request.into_inner();
        if req.code.is_empty() {
            return Err(Status::invalid_argument("code is required"));
        }
        if req.code == "INVALID" {
            return Err(Status::unauthenticated("invalid sso code"));
        }
        let id = FakeIdentity::alice();
        let (access, refresh) = self.state.lock().issue(&id);
        let result = pb::ExchangeResult {
            access_token: access,
            refresh_token: refresh,
            user: Some(pb::ResolvedUser {
                id: format!("user-{}", id.kchat_user_id),
                kchat_user_id: id.kchat_user_id.clone(),
                email: id.email.clone(),
                display_name: id.display_name.clone(),
                is_platform_admin: false,
            }),
            tenants: vec![pb::TenantRef {
                id: id.tenant_id.clone(),
                slug: id.tenant_slug.clone(),
                name: id.tenant_name.clone(),
                role: id.role.clone(),
            }],
            tenant_id: id.tenant_id.clone(),
            session_id: format!("session-{}", id.kchat_user_id),
            expires_in: 3600,
        };
        Ok(Response::new(pb::SsoResponse {
            result: Some(result),
        }))
    }

    async fn refresh(
        &self,
        request: Request<pb::RefreshRequest>,
    ) -> Result<Response<pb::RefreshResponse>, Status> {
        self.counters
            .refresh
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let req = request.into_inner();
        if req.refresh_token.is_empty() {
            return Err(Status::invalid_argument("refresh_token is required"));
        }
        let id = {
            let mut state = self.state.lock();
            let (old_access, identity) = state
                .refresh_tokens
                .remove(&req.refresh_token)
                .ok_or_else(|| Status::unauthenticated("unknown refresh token"))?;
            state.access_tokens.remove(&old_access);
            identity
        };
        let (access, refresh) = self.state.lock().issue(&id);
        let result = pb::ExchangeResult {
            access_token: access,
            refresh_token: refresh,
            user: Some(pb::ResolvedUser::default()),
            tenants: vec![],
            tenant_id: id.tenant_id,
            session_id: format!("session-{}", id.kchat_user_id),
            expires_in: 3600,
        };
        Ok(Response::new(pb::RefreshResponse {
            result: Some(result),
        }))
    }
}

#[derive(Debug)]
pub struct KTypeHarness {
    state: Arc<Mutex<ServerState>>,
    counters: CallCounters,
}

#[tonic::async_trait]
impl KTypeService for KTypeHarness {
    async fn register_k_type(
        &self,
        request: Request<pb::RegisterKTypeRequest>,
    ) -> Result<Response<pb::RegisterKTypeResponse>, Status> {
        self.counters
            .register
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        require_auth(&request, &self.state)?;
        let req = request.into_inner();
        if req.name.is_empty() {
            return Err(Status::invalid_argument("name is required"));
        }
        if req.version <= 0 {
            return Err(Status::invalid_argument("version must be > 0"));
        }
        let key = (req.name.clone(), req.version);
        let mut state = self.state.lock();
        state.ktypes.insert(
            key,
            pb::KType {
                name: req.name.clone(),
                version: req.version,
                schema: req.schema.clone(),
                created_at: "2025-01-01T00:00:00Z".into(),
            },
        );
        Ok(Response::new(pb::RegisterKTypeResponse {
            name: req.name,
            version: req.version,
        }))
    }

    async fn get_k_type(
        &self,
        request: Request<pb::GetKTypeRequest>,
    ) -> Result<Response<pb::GetKTypeResponse>, Status> {
        self.counters
            .get
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        require_auth(&request, &self.state)?;
        let req = request.into_inner();
        if req.name.is_empty() {
            return Err(Status::invalid_argument("name is required"));
        }
        let state = self.state.lock();
        let ktype = if req.version > 0 {
            state.ktypes.get(&(req.name.clone(), req.version)).cloned()
        } else {
            // Latest: highest version for this name.
            state
                .ktypes
                .iter()
                .filter(|((n, _), _)| n == &req.name)
                .max_by_key(|((_, v), _)| *v)
                .map(|(_, v)| v.clone())
        };
        match ktype {
            Some(kt) => Ok(Response::new(pb::GetKTypeResponse { ktype: Some(kt) })),
            None => Err(Status::not_found(format!(
                "ktype not found: {}@v{}",
                req.name, req.version
            ))),
        }
    }

    async fn list_k_types(
        &self,
        request: Request<pb::ListKTypesRequest>,
    ) -> Result<Response<pb::ListKTypesResponse>, Status> {
        self.counters
            .list
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        require_auth(&request, &self.state)?;
        let state = self.state.lock();
        let mut ktypes: Vec<pb::KType> = state.ktypes.values().cloned().collect();
        ktypes.sort_by(|a, b| a.name.cmp(&b.name).then_with(|| a.version.cmp(&b.version)));
        Ok(Response::new(pb::ListKTypesResponse { ktypes }))
    }
}

impl ServerState {
    fn issue(&mut self, id: &FakeIdentity) -> (String, String) {
        self.token_seq += 1;
        let access = format!("access-{}-{}", id.kchat_user_id, self.token_seq);
        let refresh = format!("refresh-{}-{}", id.kchat_user_id, self.token_seq);
        self.access_tokens
            .insert(access.clone(), (refresh.clone(), id.clone()));
        self.refresh_tokens
            .insert(refresh.clone(), (access.clone(), id.clone()));
        (access, refresh)
    }
}

fn require_auth<T>(request: &Request<T>, state: &Mutex<ServerState>) -> Result<(), Status> {
    let token = request
        .metadata()
        .get("authorization")
        .ok_or_else(|| Status::unauthenticated("missing authorization metadata"))?
        .to_str()
        .map_err(|_| Status::unauthenticated("authorization not utf-8"))?;
    let stripped = token
        .strip_prefix("Bearer ")
        .ok_or_else(|| Status::unauthenticated("authorization not Bearer scheme"))?;
    if !state.lock().access_tokens.contains_key(stripped) {
        return Err(Status::unauthenticated("unknown access token"));
    }
    Ok(())
}

/// Running harness handle. Drop to shut down the server.
pub struct Harness {
    pub addr: SocketAddr,
    pub state: Arc<Mutex<ServerState>>,
    pub counters: CallCounters,
    shutdown: Option<oneshot::Sender<()>>,
    join: Option<JoinHandle<()>>,
}

impl Harness {
    /// Start a fresh harness bound to an OS-assigned port on
    /// 127.0.0.1. Returns once the server is accepting connections.
    pub async fn start() -> Self {
        let state = Arc::new(Mutex::new(ServerState::default()));
        let counters = CallCounters::default();

        let auth = AuthServiceServer::new(AuthHarness {
            state: state.clone(),
            counters: counters.clone(),
        });
        let ktype = KTypeServiceServer::new(KTypeHarness {
            state: state.clone(),
            counters: counters.clone(),
        });

        let listener = TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind 127.0.0.1:0");
        let addr = listener.local_addr().expect("local_addr");
        let stream = TcpListenerStream::new(listener);

        let (tx, rx) = oneshot::channel::<()>();
        let join = tokio::spawn(async move {
            let server = Server::builder()
                .timeout(Duration::from_secs(30))
                .add_service(auth)
                .add_service(ktype);
            let _ = server
                .serve_with_incoming_shutdown(stream, async move {
                    let _ = rx.await;
                })
                .await;
        });

        // Give the server a beat to actually start listening. tonic's
        // serve_with_incoming returns the future without polling it
        // synchronously; one yield is enough.
        tokio::task::yield_now().await;
        tokio::time::sleep(Duration::from_millis(10)).await;

        Self {
            addr,
            state,
            counters,
            shutdown: Some(tx),
            join: Some(join),
        }
    }

    /// HTTP URL of the harness. Use as the `endpoint` argument to
    /// `ClientConfig::builder`.
    pub fn endpoint(&self) -> String {
        format!("http://{}", self.addr)
    }

    /// Invalidate the access token currently issued under
    /// `refresh_token`. The next bearer-authenticated RPC using
    /// that access token will return `Unauthenticated`. Used to
    /// trigger the auto-refresh flow under test.
    pub fn invalidate_access_for(&self, refresh_token: &str) {
        let mut state = self.state.lock();
        if let Some((access, _)) = state.refresh_tokens.get(refresh_token).cloned() {
            state.access_tokens.remove(&access);
        }
    }
}

impl Drop for Harness {
    fn drop(&mut self) {
        if let Some(tx) = self.shutdown.take() {
            let _ = tx.send(());
        }
        if let Some(join) = self.join.take() {
            join.abort();
        }
    }
}
