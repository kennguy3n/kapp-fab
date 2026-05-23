//! Configuration & builder for [`crate::KappClient`].
//!
//! All client knobs land here so the [`crate::KappClient::connect`]
//! constructor takes a single typed value, not a 12-parameter
//! function call.

use std::time::Duration;

use tonic::transport::{Channel, ClientTlsConfig, Endpoint};

use crate::error::{KappError, Result};
use crate::token_store::TokenStore;

/// How TLS is configured for the gRPC channel.
#[derive(Debug, Clone)]
pub enum TlsMode {
    /// Force TLS on. Uses both the system trust store
    /// (`rustls-native-certs`) **and** the bundled Mozilla webpki
    /// roots (`webpki-roots`). Custom CAs can be appended via
    /// [`Self::WithCustomCa`].
    System,
    /// TLS on with an additional, caller-supplied trust root.
    /// Useful for environments using an internal PKI alongside
    /// public CAs.
    ///
    /// The bytes must be a PEM-encoded certificate (one or more
    /// `-----BEGIN CERTIFICATE-----` blocks).
    WithCustomCa(Vec<u8>),
    /// Plaintext HTTP/2. Used for `http://` endpoints. The SDK
    /// will refuse to use this against an `https://` endpoint —
    /// passing it explicitly is the only way to disable TLS.
    Disabled,
}

impl TlsMode {
    /// True if the connection should use TLS.
    fn is_tls(&self) -> bool {
        !matches!(self, Self::Disabled)
    }
}

/// Immutable, validated client configuration. Construct via
/// [`ClientConfig::builder`].
#[derive(Debug, Clone)]
pub struct ClientConfig {
    pub(crate) endpoint: Endpoint,
    pub(crate) token_store: TokenStore,
    pub(crate) auto_refresh: bool,
    pub(crate) timeout: Duration,
    pub(crate) connect_timeout: Duration,
    pub(crate) keep_alive: Option<Duration>,
    pub(crate) keep_alive_timeout: Duration,
    pub(crate) user_agent: String,
}

impl ClientConfig {
    /// Start building a configuration for the given endpoint URL.
    ///
    /// The URL must be a full origin (`http://...` or `https://...`).
    /// Path / query components are ignored; gRPC uses the origin
    /// only.
    pub fn builder(endpoint: impl Into<String>) -> ClientConfigBuilder {
        ClientConfigBuilder {
            endpoint: endpoint.into(),
            tls: None,
            token_store: None,
            auto_refresh: true,
            timeout: Duration::from_secs(30),
            connect_timeout: Duration::from_secs(10),
            keep_alive: Some(Duration::from_secs(30)),
            // gRPC convention: timeout shorter than the ping interval
            // so a stalled peer is declared dead before the next ping
            // goes out. gRPC-Go uses 20s by default; grpc-c++ uses
            // 20s. We match that. See:
            //   https://github.com/grpc/grpc/blob/master/doc/keepalive.md
            keep_alive_timeout: Duration::from_secs(20),
            user_agent: default_user_agent(),
        }
    }

    /// The shared token store. Cloning is cheap; both the SDK and
    /// caller code can hold a handle and observe updates.
    #[must_use]
    pub fn token_store(&self) -> TokenStore {
        self.token_store.clone()
    }

    /// Whether the client will transparently refresh tokens on
    /// `Unauthenticated`.
    #[must_use]
    pub fn auto_refresh(&self) -> bool {
        self.auto_refresh
    }

    /// Per-RPC timeout (also surfaced for introspection / logging).
    #[must_use]
    pub fn timeout(&self) -> Duration {
        self.timeout
    }

    /// Initial TCP / TLS connection timeout.
    #[must_use]
    pub fn connect_timeout(&self) -> Duration {
        self.connect_timeout
    }

    /// HTTP/2 keep-alive interval. `None` if keep-alive is disabled.
    #[must_use]
    pub fn keep_alive(&self) -> Option<Duration> {
        self.keep_alive
    }

    /// HTTP/2 keep-alive ack timeout. If a keep-alive ping is not
    /// acknowledged within this window, the channel is torn down.
    /// Independent from [`Self::keep_alive`] (the interval).
    #[must_use]
    pub fn keep_alive_timeout(&self) -> Duration {
        self.keep_alive_timeout
    }

    /// User-Agent string the client will advertise.
    #[must_use]
    pub fn user_agent(&self) -> &str {
        &self.user_agent
    }

    /// Establish the gRPC channel using the configured endpoint
    /// and TLS settings. Returns the live `Channel` ready to be
    /// wrapped by tonic-generated clients.
    pub(crate) async fn connect_channel(&self) -> Result<Channel> {
        self.endpoint
            .clone()
            .connect()
            .await
            .map_err(KappError::Transport)
    }
}

/// Builder for [`ClientConfig`]. All setters return `self` so they
/// can be chained.
#[derive(Debug, Clone)]
pub struct ClientConfigBuilder {
    endpoint: String,
    tls: Option<TlsMode>,
    token_store: Option<TokenStore>,
    auto_refresh: bool,
    timeout: Duration,
    connect_timeout: Duration,
    keep_alive: Option<Duration>,
    keep_alive_timeout: Duration,
    user_agent: String,
}

impl ClientConfigBuilder {
    /// Set TLS mode explicitly. Default behaviour: TLS is enabled
    /// for `https://` endpoints, disabled for `http://` endpoints.
    /// Calling this overrides the URL-scheme inference.
    #[must_use]
    pub fn tls(mut self, tls: TlsMode) -> Self {
        self.tls = Some(tls);
        self
    }

    /// Attach a pre-existing token store. Default: a fresh empty
    /// store is created.
    #[must_use]
    pub fn token_store(mut self, store: TokenStore) -> Self {
        self.token_store = Some(store);
        self
    }

    /// Disable automatic refresh on `Unauthenticated`. Default:
    /// enabled. Disabling is useful for tests that want to assert
    /// on the unwrapped `Unauthenticated` error.
    #[must_use]
    pub fn auto_refresh(mut self, enabled: bool) -> Self {
        self.auto_refresh = enabled;
        self
    }

    /// Per-RPC timeout. Applied via the transport-level timeout;
    /// individual calls can override by setting `Request::set_timeout`.
    #[must_use]
    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    /// Initial TCP / TLS connection timeout.
    #[must_use]
    pub fn connect_timeout(mut self, timeout: Duration) -> Self {
        self.connect_timeout = timeout;
        self
    }

    /// HTTP/2 keep-alive interval. `None` disables keep-alive
    /// (default: 30s).
    #[must_use]
    pub fn keep_alive(mut self, interval: Option<Duration>) -> Self {
        self.keep_alive = interval;
        self
    }

    /// HTTP/2 keep-alive ack timeout (default: 20s, matching gRPC-Go
    /// / grpc-c++). If a keep-alive PING goes unanswered for this
    /// long the channel is torn down so the next RPC reconnects.
    /// Should be strictly less than [`Self::keep_alive`] so a dead
    /// peer is detected within `interval + timeout` rather than
    /// `2 * interval`. The builder does not enforce that constraint
    /// (some operators run pathologically high-latency networks
    /// where `timeout >= interval` is intentional), but the default
    /// of 20s vs 30s interval is the gRPC project's recommendation.
    #[must_use]
    pub fn keep_alive_timeout(mut self, timeout: Duration) -> Self {
        self.keep_alive_timeout = timeout;
        self
    }

    /// Custom User-Agent string. Sent on every RPC as
    /// `user-agent: <ua>`. Default:
    /// `kapp-sdk-rust/<crate-version> grpc-go-tonic/<tonic-version>`.
    #[must_use]
    pub fn user_agent(mut self, ua: impl Into<String>) -> Self {
        self.user_agent = ua.into();
        self
    }

    /// Validate and freeze the configuration.
    pub fn build(self) -> Result<ClientConfig> {
        let mut endpoint = Endpoint::from_shared(self.endpoint.clone())
            .map_err(|e| KappError::Config(format!("invalid endpoint URL: {e}")))?
            .timeout(self.timeout)
            .connect_timeout(self.connect_timeout)
            .user_agent(self.user_agent.clone())
            .map_err(|e| KappError::Config(format!("invalid user-agent: {e}")))?;

        if let Some(interval) = self.keep_alive {
            endpoint = endpoint
                .http2_keep_alive_interval(interval)
                .keep_alive_timeout(self.keep_alive_timeout)
                .keep_alive_while_idle(true);
        }

        // TLS mode: explicit setting wins, otherwise infer from URL
        // scheme. `https://` ⇒ system roots; `http://` ⇒ plaintext.
        let scheme_is_https = self.endpoint.starts_with("https://");
        let tls = match self.tls {
            Some(t) => t,
            None if scheme_is_https => TlsMode::System,
            None => TlsMode::Disabled,
        };

        if tls.is_tls() && !scheme_is_https {
            return Err(KappError::Config(format!(
                "TLS configured but endpoint scheme is not https: {}",
                self.endpoint
            )));
        }
        if !tls.is_tls() && scheme_is_https {
            return Err(KappError::Config(format!(
                "endpoint scheme is https but TLS is disabled: {}",
                self.endpoint
            )));
        }

        match tls {
            TlsMode::Disabled => {}
            TlsMode::System => {
                let tls_cfg = ClientTlsConfig::new()
                    .with_native_roots()
                    .with_webpki_roots();
                endpoint = endpoint
                    .tls_config(tls_cfg)
                    .map_err(|e| KappError::Config(format!("tls config: {e}")))?;
            }
            TlsMode::WithCustomCa(pem) => {
                let ca = tonic::transport::Certificate::from_pem(pem);
                let tls_cfg = ClientTlsConfig::new()
                    .with_native_roots()
                    .with_webpki_roots()
                    .ca_certificate(ca);
                endpoint = endpoint
                    .tls_config(tls_cfg)
                    .map_err(|e| KappError::Config(format!("tls config: {e}")))?;
            }
        }

        Ok(ClientConfig {
            endpoint,
            token_store: self.token_store.unwrap_or_default(),
            auto_refresh: self.auto_refresh,
            timeout: self.timeout,
            connect_timeout: self.connect_timeout,
            keep_alive: self.keep_alive,
            keep_alive_timeout: self.keep_alive_timeout,
            user_agent: self.user_agent,
        })
    }
}

fn default_user_agent() -> String {
    format!("kapp-sdk-rust/{}", env!("CARGO_PKG_VERSION"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn builds_with_http_endpoint() {
        let cfg = ClientConfig::builder("http://localhost:9090")
            .build()
            .unwrap();
        assert!(cfg.auto_refresh);
        assert_eq!(cfg.timeout, Duration::from_secs(30));
    }

    #[test]
    fn builds_with_https_endpoint() {
        // Real builder; will not attempt to connect.
        let cfg = ClientConfig::builder("https://api.example.com")
            .build()
            .unwrap();
        assert!(cfg.auto_refresh);
    }

    #[test]
    fn invalid_url_rejected() {
        let err = ClientConfig::builder("not a url at all")
            .build()
            .unwrap_err();
        assert!(matches!(err, KappError::Config(_)));
    }

    #[test]
    fn tls_disabled_against_https_rejected() {
        let err = ClientConfig::builder("https://api.example.com")
            .tls(TlsMode::Disabled)
            .build()
            .unwrap_err();
        match err {
            KappError::Config(msg) => assert!(msg.contains("TLS is disabled")),
            other => panic!("expected Config error, got {other:?}"),
        }
    }

    #[test]
    fn tls_enabled_against_http_rejected() {
        let err = ClientConfig::builder("http://localhost:9090")
            .tls(TlsMode::System)
            .build()
            .unwrap_err();
        match err {
            KappError::Config(msg) => assert!(msg.contains("scheme is not https")),
            other => panic!("expected Config error, got {other:?}"),
        }
    }

    #[test]
    fn custom_user_agent_threads_through() {
        let cfg = ClientConfig::builder("http://localhost:9090")
            .user_agent("my-tool/1.2.3")
            .build()
            .unwrap();
        assert_eq!(cfg.user_agent, "my-tool/1.2.3");
    }

    #[test]
    fn shared_token_store_observable_from_config() {
        let store = TokenStore::new();
        let cfg = ClientConfig::builder("http://localhost:9090")
            .token_store(store.clone())
            .build()
            .unwrap();
        // Mutations through `store` are visible through the config's
        // own handle because they're both Arc'd internally.
        let snap_before = cfg.token_store().snapshot();
        assert_eq!(snap_before.version, 0);
        store.set(crate::token_store::Tokens {
            access_token: "a".into(),
            refresh_token: "r".into(),
            expires_at: std::time::SystemTime::now() + Duration::from_secs(60),
            tenant_id: "t".into(),
            session_id: "s".into(),
        });
        let snap_after = cfg.token_store().snapshot();
        assert_eq!(snap_after.version, 1);
    }

    #[test]
    fn auto_refresh_can_be_disabled() {
        let cfg = ClientConfig::builder("http://localhost:9090")
            .auto_refresh(false)
            .build()
            .unwrap();
        assert!(!cfg.auto_refresh);
    }

    #[test]
    fn default_user_agent_advertises_sdk_version() {
        let ua = default_user_agent();
        assert!(ua.starts_with("kapp-sdk-rust/"));
        assert!(ua.contains(env!("CARGO_PKG_VERSION")));
    }
}
