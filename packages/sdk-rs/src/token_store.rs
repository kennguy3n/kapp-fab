//! Thread-safe, lock-free token storage with monotonic versioning
//! to coalesce concurrent refresh attempts.
//!
//! # Design rationale
//!
//! When the SDK is used from multiple async tasks concurrently and
//! the access token expires, every in-flight RPC will see
//! `Unauthenticated`. A naive design refreshes the token N times
//! (once per failed RPC); a correct design refreshes **once** and
//! retries the other N-1 calls against the new token.
//!
//! The pattern below uses two primitives:
//!
//! 1. [`ArcSwap`] over `Option<Tokens>` for lock-free reads. Hot
//!    paths (bearer-token injection on every RPC) never block.
//! 2. A monotonically increasing `version: u64` so refresh code can
//!    say *"refresh only if no one else swapped in fresher tokens
//!    while I was running my refresh RPC"*. This is the standard
//!    CAS-with-version pattern used in lock-free data structures.
//!
//! See [`crate::refresh::RefreshSingleflight`] for the singleflight
//! coordination that uses this versioning.

use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use arc_swap::ArcSwap;

/// A bundle of credentials issued by the kapp-fab auth service.
#[derive(Debug, Clone)]
pub struct Tokens {
    /// Bearer access token. Sent as `Authorization: Bearer <token>`
    /// on authenticated RPCs.
    pub access_token: String,
    /// Refresh token. Used to obtain a new access token without
    /// re-running the SSO flow.
    pub refresh_token: String,
    /// Wall-clock time at which `access_token` expires. Computed
    /// from `expires_in` (seconds-from-issue) at swap time.
    pub expires_at: SystemTime,
    /// The tenant the access token is scoped to. Matches one of
    /// the entries in the originating `ExchangeResult.tenants`.
    pub tenant_id: String,
    /// Session id of the refresh-token row on the server. The
    /// refresh path looks this up to rotate the refresh token.
    pub session_id: String,
}

impl Tokens {
    /// True if `now > expires_at - skew`. Callers should default
    /// `skew` to 30s or so to avoid sending a token that expires
    /// mid-flight.
    #[must_use]
    pub fn is_expired(&self, skew: Duration) -> bool {
        self.expires_at
            .checked_sub(skew)
            .is_none_or(|cutoff| SystemTime::now() >= cutoff)
    }

    /// Number of seconds-from-now until expiry. Returns 0 if the
    /// token is already expired.
    #[must_use]
    pub fn seconds_until_expiry(&self) -> i64 {
        let now = SystemTime::now();
        match self.expires_at.duration_since(now) {
            Ok(d) => i64::try_from(d.as_secs()).unwrap_or(i64::MAX),
            Err(_) => 0,
        }
    }
}

/// A snapshot of the [`TokenStore`] at a particular version. Used
/// by [`TokenStore::swap_if_version`] to detect lost updates.
#[derive(Debug, Clone)]
pub struct TokenSnapshot {
    /// The version of the store at snapshot time. Increments on
    /// every successful write.
    pub version: u64,
    /// The tokens present at snapshot time, or `None` if the store
    /// was empty.
    pub tokens: Option<Tokens>,
}

/// Lock-free, atomically-updatable token storage.
///
/// Clones share the same underlying state — `TokenStore` is a
/// cheap handle around an `Arc<Inner>`.
#[derive(Debug, Clone)]
pub struct TokenStore {
    inner: Arc<Inner>,
}

#[derive(Debug)]
struct Inner {
    // ArcSwap gives us atomic load/store of a Box<Option<Tokens>>
    // without blocking readers. Readers do one atomic pointer load,
    // writers do one atomic CAS.
    state: ArcSwap<State>,
}

#[derive(Debug, Clone, Default)]
struct State {
    version: u64,
    tokens: Option<Tokens>,
}

impl TokenStore {
    /// Create an empty store. Equivalent to `TokenStore::default()`.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Create a pre-populated store. The version starts at 1 so any
    /// `swap_if_version(0, ...)` against a snapshot taken before
    /// construction will correctly fail.
    #[must_use]
    pub fn with_tokens(tokens: Tokens) -> Self {
        let store = Self::default();
        // Bypass version checking for the initial seed — there are
        // no concurrent writers possible yet.
        store.inner.state.store(Arc::new(State {
            version: 1,
            tokens: Some(tokens),
        }));
        store
    }

    /// Take a snapshot of the current state. The returned snapshot
    /// captures the version *at the moment of this call*; pass it
    /// back to [`Self::swap_if_version`] to detect lost updates.
    #[must_use]
    pub fn snapshot(&self) -> TokenSnapshot {
        let guard = self.inner.state.load_full();
        TokenSnapshot {
            version: guard.version,
            tokens: guard.tokens.clone(),
        }
    }

    /// Current tokens, or `None` if the store is empty. This is the
    /// hot path called on every authenticated RPC.
    #[must_use]
    pub fn current(&self) -> Option<Tokens> {
        self.inner.state.load_full().tokens.clone()
    }

    /// Current version. Increments on every successful write.
    #[must_use]
    pub fn version(&self) -> u64 {
        self.inner.state.load_full().version
    }

    /// Unconditionally store new tokens. Returns the new version.
    pub fn set(&self, tokens: Tokens) -> u64 {
        loop {
            let prev = self.inner.state.load_full();
            let next = Arc::new(State {
                version: prev.version.wrapping_add(1),
                tokens: Some(tokens.clone()),
            });
            let next_version = next.version;
            // compare_and_swap returns a Guard holding the value
            // that was actually stored. We use Arc::ptr_eq against
            // `prev` to detect whether our CAS won the race; if a
            // concurrent writer beat us we retry against the new
            // state. arc-swap is documented to perform pointer
            // equality on the `current` argument.
            let prev_after = self.inner.state.compare_and_swap(&prev, next);
            if Arc::ptr_eq(&prev, &*prev_after) {
                return next_version;
            }
            // Lost the race; retry against the new state.
        }
    }

    /// Atomically swap in new tokens only if the store's version
    /// still matches `snapshot.version`. Returns `true` if the swap
    /// happened, `false` if someone else updated first (in which
    /// case the caller should re-read and decide whether to retry).
    pub fn swap_if_version(&self, snapshot_version: u64, tokens: Option<Tokens>) -> bool {
        let prev = self.inner.state.load_full();
        if prev.version != snapshot_version {
            return false;
        }
        let next = Arc::new(State {
            version: prev.version.wrapping_add(1),
            tokens,
        });
        let prev_after = self.inner.state.compare_and_swap(&prev, next);
        Arc::ptr_eq(&prev, &*prev_after)
    }

    /// Erase the stored tokens (e.g. on logout or after a hard
    /// `Unauthenticated` with no refresh path). Returns the new
    /// version.
    pub fn clear(&self) -> u64 {
        loop {
            let prev = self.inner.state.load_full();
            if prev.tokens.is_none() {
                // Nothing to clear; don't bump version unnecessarily.
                return prev.version;
            }
            let next = Arc::new(State {
                version: prev.version.wrapping_add(1),
                tokens: None,
            });
            let next_version = next.version;
            let prev_after = self.inner.state.compare_and_swap(&prev, next);
            if Arc::ptr_eq(&prev, &*prev_after) {
                return next_version;
            }
        }
    }
}

impl Default for TokenStore {
    fn default() -> Self {
        Self {
            inner: Arc::new(Inner {
                state: ArcSwap::from(Arc::new(State::default())),
            }),
        }
    }
}

/// Build a [`Tokens`] from an `ExchangeResult` proto and an issue
/// time. `expires_at` is computed as `issue_time + expires_in`.
#[must_use]
pub(crate) fn tokens_from_proto(result: &crate::pb::ExchangeResult) -> Tokens {
    // The server emits seconds-from-now; we compute the absolute
    // expiry against the local clock at receive time. Network
    // latency is sub-second so this is accurate enough; consumers
    // worried about clock skew can call `snapshot()` and inspect.
    let now = SystemTime::now();
    let expires_at = if result.expires_in > 0 {
        now.checked_add(Duration::from_secs(
            u64::try_from(result.expires_in).unwrap_or(0),
        ))
        .unwrap_or(UNIX_EPOCH)
    } else {
        // Server bug or refresh-only response that doesn't surface
        // expires_in. Treat as immediately-expired; the next call
        // will trigger a refresh.
        now
    };
    Tokens {
        access_token: result.access_token.clone(),
        refresh_token: result.refresh_token.clone(),
        expires_at,
        tenant_id: result.tenant_id.clone(),
        session_id: result.session_id.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::thread;

    fn make_tokens(access: &str) -> Tokens {
        Tokens {
            access_token: access.into(),
            refresh_token: "rt".into(),
            expires_at: SystemTime::now() + Duration::from_secs(3600),
            tenant_id: "t1".into(),
            session_id: "s1".into(),
        }
    }

    #[test]
    fn empty_store_returns_none() {
        let s = TokenStore::new();
        assert!(s.current().is_none());
        assert_eq!(s.version(), 0);
    }

    #[test]
    fn set_increments_version() {
        let s = TokenStore::new();
        let v1 = s.set(make_tokens("a"));
        assert_eq!(v1, 1);
        let v2 = s.set(make_tokens("b"));
        assert_eq!(v2, 2);
        assert_eq!(s.current().unwrap().access_token, "b");
    }

    #[test]
    fn snapshot_captures_version_at_call_time() {
        let s = TokenStore::new();
        let snap0 = s.snapshot();
        assert_eq!(snap0.version, 0);
        s.set(make_tokens("a"));
        let snap1 = s.snapshot();
        assert_eq!(snap1.version, 1);
        // snap0 is unaffected by subsequent writes.
        assert_eq!(snap0.version, 0);
        assert!(snap0.tokens.is_none());
    }

    #[test]
    fn swap_if_version_succeeds_when_unchanged() {
        let s = TokenStore::new();
        s.set(make_tokens("a"));
        let snap = s.snapshot();
        let ok = s.swap_if_version(snap.version, Some(make_tokens("b")));
        assert!(ok);
        assert_eq!(s.current().unwrap().access_token, "b");
        assert_eq!(s.version(), snap.version + 1);
    }

    #[test]
    fn swap_if_version_fails_when_version_advanced() {
        let s = TokenStore::new();
        s.set(make_tokens("a"));
        let snap = s.snapshot();
        // Concurrent writer advances the version.
        s.set(make_tokens("c"));
        let ok = s.swap_if_version(snap.version, Some(make_tokens("b")));
        assert!(!ok);
        // The concurrent write is preserved.
        assert_eq!(s.current().unwrap().access_token, "c");
    }

    #[test]
    fn clear_does_not_bump_version_if_already_empty() {
        let s = TokenStore::new();
        let v0 = s.clear();
        assert_eq!(v0, 0);
        assert_eq!(s.version(), 0);
    }

    #[test]
    fn clear_bumps_version_when_tokens_present() {
        let s = TokenStore::new();
        s.set(make_tokens("a"));
        let v = s.clear();
        assert_eq!(v, 2);
        assert!(s.current().is_none());
    }

    #[test]
    fn singleflight_pattern_under_real_contention() {
        // Real multi-thread test: 16 threads attempt to swap from
        // version N to version N+1. Exactly one should succeed; the
        // rest should observe `false`. This validates the CAS-with-
        // version protocol used by RefreshSingleflight.
        let s = TokenStore::new();
        s.set(make_tokens("initial"));
        let snap = s.snapshot();

        let success_count = Arc::new(AtomicUsize::new(0));
        let store = Arc::new(s);

        let mut handles = vec![];
        for i in 0..16 {
            let store = Arc::clone(&store);
            let counter = Arc::clone(&success_count);
            let target_version = snap.version;
            handles.push(thread::spawn(move || {
                let tokens = make_tokens(&format!("refreshed-by-{i}"));
                if store.swap_if_version(target_version, Some(tokens)) {
                    counter.fetch_add(1, Ordering::SeqCst);
                }
            }));
        }
        for h in handles {
            h.join().unwrap();
        }
        // Exactly one writer wins; the rest must retry against the
        // new version (which they will not, because they were given
        // the original snapshot version).
        assert_eq!(success_count.load(Ordering::SeqCst), 1);
        // The store has been bumped exactly once.
        assert_eq!(store.version(), snap.version + 1);
    }

    #[test]
    fn is_expired_with_skew() {
        let mut t = make_tokens("a");
        t.expires_at = SystemTime::now() + Duration::from_secs(10);
        assert!(!t.is_expired(Duration::from_secs(5)));
        assert!(t.is_expired(Duration::from_secs(20)));
    }

    #[test]
    fn is_expired_when_in_past() {
        let mut t = make_tokens("a");
        t.expires_at = SystemTime::now() - Duration::from_secs(60);
        assert!(t.is_expired(Duration::from_secs(0)));
        assert_eq!(t.seconds_until_expiry(), 0);
    }

    #[test]
    fn with_tokens_starts_at_version_1() {
        let s = TokenStore::with_tokens(make_tokens("seed"));
        assert_eq!(s.version(), 1);
        assert_eq!(s.current().unwrap().access_token, "seed");
        // A snapshot taken before this store existed (version 0)
        // must fail to swap.
        assert!(!s.swap_if_version(0, Some(make_tokens("evil"))));
        assert_eq!(s.current().unwrap().access_token, "seed");
    }
}
