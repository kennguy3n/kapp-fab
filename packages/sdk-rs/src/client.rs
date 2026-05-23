//! Top-level [`KappClient`] handle.
//!
//! Owns the live gRPC channel and the shared token store. The
//! per-service helpers ([`crate::auth::SsoFlow`], [`crate::ktype::KTypeClient`])
//! are cheap to construct from a `KappClient` and share its
//! channel + token store — they don't open new connections.

use tonic::transport::Channel;

use crate::auth::SsoFlow;
use crate::config::ClientConfig;
use crate::error::Result;
use crate::ktype::KTypeClient;
use crate::refresh::RefreshSingleflight;
use crate::token_store::TokenStore;

/// Connected gRPC client for the kapp-fab `kapp.v1` API.
#[derive(Clone, Debug)]
pub struct KappClient {
    channel: Channel,
    store: TokenStore,
    refresher: RefreshSingleflight,
    auto_refresh: bool,
}

impl KappClient {
    /// Connect to the server described by `cfg`. The constructor
    /// performs a synchronous TCP/TLS handshake; once it returns,
    /// the underlying HTTP/2 channel is ready to multiplex RPCs.
    pub async fn connect(cfg: ClientConfig) -> Result<Self> {
        let store = cfg.token_store.clone();
        let auto_refresh = cfg.auto_refresh;
        let channel = cfg.connect_channel().await?;
        Ok(Self {
            channel,
            store,
            refresher: RefreshSingleflight::default(),
            auto_refresh,
        })
    }

    /// Adopt a pre-existing channel. Useful for tests that spin up
    /// an in-process gRPC server and pass its channel directly,
    /// skipping the TCP/TLS handshake.
    #[doc(hidden)]
    pub fn from_channel(channel: Channel, store: TokenStore, auto_refresh: bool) -> Self {
        Self {
            channel,
            store,
            refresher: RefreshSingleflight::default(),
            auto_refresh,
        }
    }

    /// Returns the auth flow helper — exchange / refresh.
    #[must_use]
    pub fn auth(&self) -> SsoFlow {
        SsoFlow::new(self.channel.clone(), self.store.clone())
    }

    /// Returns the KType registry helper — register / get / list.
    #[must_use]
    pub fn ktype(&self) -> KTypeClient {
        KTypeClient::new(
            self.channel.clone(),
            self.store.clone(),
            self.refresher.clone(),
            self.auto_refresh,
        )
    }

    /// Shared [`TokenStore`] handle. Useful for persistence
    /// (snapshot on shutdown, restore on startup) or for callers
    /// that want to observe token changes from outside the SDK.
    #[must_use]
    pub fn token_store(&self) -> TokenStore {
        self.store.clone()
    }
}

#[cfg(test)]
mod tests {
    // KappClient::connect is exercised end-to-end by tests/e2e.rs
    // against a real in-process tonic server; unit tests here would
    // duplicate that work without adding coverage.
}
