//! Authentication helpers: SSO exchange and token refresh.
//!
//! Wraps the generated `AuthServiceClient` from the `kapp.v1`
//! proto package. Both RPCs are unauthenticated by design — the
//! KChat OAuth code (for SSO) and the refresh token (for Refresh)
//! are the trust anchors. The SDK ensures the bearer-injection
//! interceptor is **not** attached to this surface so the server
//! doesn't reject calls that intentionally lack an `Authorization`
//! header.

use tonic::codegen::InterceptedService;
use tonic::transport::Channel;

use crate::error::{KappError, Result};
use crate::interceptors::AuthSurfaceInterceptor;
use crate::pb;
use crate::pb::auth_service_client::AuthServiceClient;
use crate::token_store::{tokens_from_proto, TokenStore};

type AuthChannel = InterceptedService<Channel, AuthSurfaceInterceptor>;

/// Resolved user record returned alongside an [`ExchangeResult`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedUser {
    /// Opaque server-issued user id (UUIDv4 string).
    pub id: String,
    /// The user's KChat-issued external identifier.
    pub kchat_user_id: String,
    /// Primary email address (verified at SSO time).
    pub email: String,
    /// Human-readable display name.
    pub display_name: String,
    /// True if the user has platform-admin privileges (cross-tenant).
    pub is_platform_admin: bool,
}

impl From<pb::ResolvedUser> for ResolvedUser {
    fn from(p: pb::ResolvedUser) -> Self {
        Self {
            id: p.id,
            kchat_user_id: p.kchat_user_id,
            email: p.email,
            display_name: p.display_name,
            is_platform_admin: p.is_platform_admin,
        }
    }
}

/// Membership summary for one of the tenants the user can access.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TenantRef {
    /// Opaque tenant id.
    pub id: String,
    /// URL-safe tenant slug.
    pub slug: String,
    /// Display name of the tenant.
    pub name: String,
    /// The caller's role inside this tenant (`owner` / `admin` / etc.).
    pub role: String,
}

impl From<pb::TenantRef> for TenantRef {
    fn from(p: pb::TenantRef) -> Self {
        Self {
            id: p.id,
            slug: p.slug,
            name: p.name,
            role: p.role,
        }
    }
}

/// Result of a successful SSO exchange or refresh. Mirrors
/// `auth.ExchangeResult` on the Go side.
#[derive(Debug, Clone)]
pub struct ExchangeResult {
    /// Bearer access token. Lifetime per [`Self::expires_in`].
    pub access_token: String,
    /// Refresh token (rotated on every `Refresh` call).
    pub refresh_token: String,
    /// Resolved user identity.
    pub user: ResolvedUser,
    /// All tenants the user has access to.
    pub tenants: Vec<TenantRef>,
    /// The tenant the access token is scoped to.
    pub tenant_id: String,
    /// Refresh-session row id (server-side bookkeeping).
    pub session_id: String,
    /// Seconds-from-now until `access_token` expires.
    pub expires_in: i64,
}

impl From<pb::ExchangeResult> for ExchangeResult {
    fn from(p: pb::ExchangeResult) -> Self {
        Self {
            access_token: p.access_token,
            refresh_token: p.refresh_token,
            user: p.user.unwrap_or_default().into(),
            tenants: p.tenants.into_iter().map(Into::into).collect(),
            tenant_id: p.tenant_id,
            session_id: p.session_id,
            expires_in: p.expires_in,
        }
    }
}

/// High-level helper around the `kapp.v1.AuthService` gRPC client.
///
/// Cheap to clone; the underlying tonic client clones at the
/// `Channel` level (HTTP/2 multiplexed connection).
#[derive(Clone)]
pub struct SsoFlow {
    client: AuthServiceClient<AuthChannel>,
    store: TokenStore,
}

impl SsoFlow {
    pub(crate) fn new(channel: Channel, store: TokenStore) -> Self {
        let interceptor = AuthSurfaceInterceptor::new();
        let client = AuthServiceClient::with_interceptor(channel, interceptor);
        Self { client, store }
    }

    /// Trade a KChat OAuth code for a kapp access+refresh pair.
    ///
    /// On success the tokens are stored in the shared [`TokenStore`]
    /// — subsequent authenticated RPCs through the same
    /// [`crate::KappClient`] will automatically use them.
    ///
    /// `preferred_tenant` may be `None` (use the user's default
    /// tenant) or `Some(slug)` to pin a specific tenant when the
    /// account has multiple memberships.
    pub async fn exchange(
        &self,
        code: impl Into<String>,
        redirect_uri: impl Into<String>,
        preferred_tenant: Option<String>,
    ) -> Result<ExchangeResult> {
        let req = pb::SsoRequest {
            code: code.into(),
            redirect_uri: redirect_uri.into(),
            preferred_tenant: preferred_tenant.unwrap_or_default(),
        };
        let resp = self
            .client
            .clone()
            .sso(req)
            .await
            .map_err(KappError::from)?;
        let proto_result = resp.into_inner().result.ok_or_else(|| {
            KappError::Codec("server returned SSO response with empty result".into())
        })?;
        // Persist to the shared store BEFORE returning so the next
        // RPC the caller issues already has a token to use.
        self.store.set(tokens_from_proto(&proto_result));
        Ok(proto_result.into())
    }

    /// Refresh the access token using the supplied refresh token.
    ///
    /// On success the new tokens are atomically swapped into the
    /// shared [`TokenStore`]. The previous refresh token is
    /// rotated server-side, so this method MUST be called with the
    /// most recent refresh token (which is what the store always
    /// holds — the SDK's [`crate::TokenStore::current`] is the
    /// canonical source).
    pub async fn refresh(&self, refresh_token: impl Into<String>) -> Result<ExchangeResult> {
        let rt = refresh_token.into();
        if rt.is_empty() {
            return Err(KappError::InvalidArgument("refresh_token is empty".into()));
        }
        let resp = self
            .client
            .clone()
            .refresh(pb::RefreshRequest { refresh_token: rt })
            .await
            .map_err(KappError::from)?;
        let proto_result = resp.into_inner().result.ok_or_else(|| {
            KappError::Codec("server returned Refresh response with empty result".into())
        })?;
        self.store.set(tokens_from_proto(&proto_result));
        Ok(proto_result.into())
    }

    /// Convenience: refresh using the refresh token currently held
    /// in the [`TokenStore`]. Used by the auto-refresh path.
    pub async fn refresh_from_store(&self) -> Result<ExchangeResult> {
        let current = self
            .store
            .current()
            .ok_or(crate::error::AuthError::NoToken)?;
        self.refresh(current.refresh_token).await
    }
}

// Re-export so callers can type-annotate the share-store handle.
pub use crate::token_store::Tokens;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resolved_user_from_proto() {
        let p = pb::ResolvedUser {
            id: "u1".into(),
            kchat_user_id: "k1".into(),
            email: "alice@example.com".into(),
            display_name: "Alice".into(),
            is_platform_admin: true,
        };
        let r: ResolvedUser = p.into();
        assert_eq!(r.id, "u1");
        assert!(r.is_platform_admin);
    }

    #[test]
    fn tenant_ref_from_proto() {
        let p = pb::TenantRef {
            id: "t1".into(),
            slug: "acme".into(),
            name: "Acme".into(),
            role: "owner".into(),
        };
        let r: TenantRef = p.into();
        assert_eq!(r.slug, "acme");
        assert_eq!(r.role, "owner");
    }

    #[test]
    fn exchange_result_with_empty_user_uses_default() {
        let p = pb::ExchangeResult {
            access_token: "a".into(),
            refresh_token: "r".into(),
            user: None,
            tenants: vec![],
            tenant_id: "t1".into(),
            session_id: "s1".into(),
            expires_in: 3600,
        };
        let r: ExchangeResult = p.into();
        // Default user is all-zero-value fields.
        assert!(r.user.id.is_empty());
        assert_eq!(r.tenant_id, "t1");
        assert_eq!(r.expires_in, 3600);
    }
}
