//! Singleflight refresh coordination.
//!
//! When the SDK observes `Unauthenticated` from a server, we want
//! to:
//!
//! 1. Refresh the access token via [`crate::auth::SsoFlow::refresh`].
//! 2. Retry the original call exactly once with the new token.
//!
//! Under concurrency, if N tasks race a 401, we must only run **one**
//! refresh RPC and have the other N-1 wait for its result. That's
//! the singleflight pattern, named for Google's
//! [singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight)
//! package which solves the same problem.
//!
//! # Implementation
//!
//! - The shared state holds an [`InFlight`] slot keyed by the
//!   [`TokenStore`] version at which the refresh was requested.
//! - The outcome is published through a [`OnceCell`] held inside
//!   an [`Arc`] **whose handle is taken by every waiter under the
//!   slot lock**. Once a waiter has the handle, the leader is
//!   free to clear the slot — the waiter can still observe the
//!   outcome via its own `Arc`.
//! - Waiters park on a [`tokio::sync::Notify`] (one per in-flight
//!   attempt) instead of polling. The leader fires
//!   [`Notify::notify_waiters`] after publishing the outcome, so
//!   every parked waiter wakes exactly once when the refresh
//!   completes — no busy-loop, no `yield_now()` spam.
//! - Cleanup is therefore safe even if the leader returns before
//!   every waiter polls.

use std::sync::Arc;
use std::time::Duration;

use parking_lot::Mutex;
use tokio::sync::{Notify, OnceCell};
use tokio::time::timeout;

use crate::error::{AuthError, KappError, Result};
use crate::token_store::{TokenSnapshot, TokenStore};

/// Result of an in-flight refresh attempt.
#[derive(Debug, Clone)]
enum RefreshOutcome {
    /// A refresh actually happened (new tokens swapped in).
    Refreshed,
    /// Someone else's refresh had already advanced the version
    /// past our snapshot when we acquired the lock — we did
    /// nothing and the caller should re-read the store.
    SkippedAlreadyRefreshed,
    /// The refresh RPC failed. The error message is preserved so
    /// every waiter sees the same failure reason.
    Failed { error_message: String },
}

/// Shared state for one in-flight refresh attempt. Waiters take a
/// clone of `outcome` + `done` under the slot lock; once they hold
/// them, the slot can be torn down without losing the result.
#[derive(Debug)]
struct InFlight {
    snapshot_version: u64,
    outcome: Arc<OnceCell<RefreshOutcome>>,
    /// Wakeup primitive. Waiters call `done.notified().await` before
    /// reading `outcome`; the leader calls `done.notify_waiters()`
    /// after publishing the outcome. Tokio guarantees that a future
    /// created by `notified()` and polled before `notify_waiters()`
    /// is called WILL be woken, even if the await point happens
    /// after the notify call (`Notified` registers eagerly on
    /// creation, not on first poll). Late-arriving callers that
    /// missed this notify_waiters cycle observe the bumped store
    /// version on their next snapshot, so they take the fast-path
    /// at the top of `refresh()` instead of trying to join.
    done: Arc<Notify>,
}

/// Coordinator that collapses concurrent refresh attempts into a
/// single underlying refresh RPC.
///
/// Cheap to clone — internally an `Arc`. Pass clones into every
/// task that may need to refresh.
#[derive(Debug, Clone)]
pub(crate) struct RefreshSingleflight {
    inner: Arc<Mutex<Option<InFlight>>>,
    /// Maximum time a waiter will block for. If the in-flight
    /// refresh hangs beyond this, all waiters get a `Transport`-
    /// shaped error rather than hanging the calling task forever.
    wait_timeout: Duration,
}

impl Default for RefreshSingleflight {
    fn default() -> Self {
        Self {
            inner: Arc::new(Mutex::new(None)),
            wait_timeout: Duration::from_secs(20),
        }
    }
}

impl RefreshSingleflight {
    /// Begin (or join) a refresh keyed on `snapshot`. The
    /// `do_refresh` closure is invoked at most **once** across all
    /// concurrent callers for the same `snapshot.version`.
    ///
    /// Returns once the refresh has completed (successfully or not)
    /// **or** the version has already advanced past `snapshot.version`
    /// (someone else refreshed for us).
    pub(crate) async fn refresh<F, Fut>(
        &self,
        store: &TokenStore,
        snapshot: TokenSnapshot,
        do_refresh: F,
    ) -> Result<()>
    where
        F: FnOnce(TokenSnapshot) -> Fut + Send,
        Fut: std::future::Future<Output = Result<()>> + Send,
    {
        // Fast path: someone already refreshed past our snapshot
        // version. No need to coordinate.
        if store.version() != snapshot.version {
            return Ok(());
        }

        // Claim leadership OR grab the in-flight outcome handle.
        // Either way we end up with an `Arc<OnceCell<_>>` plus an
        // `Arc<Notify>` we can wait on independently of the slot's
        // lifetime.
        //
        // IMPORTANT: a waiter MUST register its `notified()` future
        // BEFORE dropping the slot lock — otherwise the leader
        // could finish the refresh, fire `notify_waiters()`, and
        // tear the slot down between the waiter releasing the lock
        // and the waiter calling `notified()` (the classic
        // lost-wakeup race). `Notify::notified()` is the documented
        // remedy: it registers a permit at await-creation time so a
        // subsequent `.await` resolves immediately if the leader
        // notifies in-between.
        let (outcome_handle, done_handle, leader) = {
            let mut guard = self.inner.lock();
            match guard.as_ref() {
                Some(existing) if existing.snapshot_version == snapshot.version => {
                    (existing.outcome.clone(), existing.done.clone(), false)
                }
                _ => {
                    let outcome = Arc::new(OnceCell::<RefreshOutcome>::new());
                    let done = Arc::new(Notify::new());
                    *guard = Some(InFlight {
                        snapshot_version: snapshot.version,
                        outcome: outcome.clone(),
                        done: done.clone(),
                    });
                    (outcome, done, true)
                }
            }
        };

        if leader {
            // Leader: run the user-supplied refresh closure.
            let result = do_refresh(snapshot.clone()).await;
            let outcome_val = match result {
                Ok(()) => {
                    if store.version() == snapshot.version {
                        // do_refresh returned Ok but didn't swap in
                        // new tokens. Treat as a no-op so waiters
                        // retry against the still-stale token.
                        RefreshOutcome::SkippedAlreadyRefreshed
                    } else {
                        RefreshOutcome::Refreshed
                    }
                }
                Err(err) => RefreshOutcome::Failed {
                    error_message: err.to_string(),
                },
            };

            // Publish before clearing the slot. set() is infallible
            // here because we are the unique writer; other tasks
            // only ever call `get()` on the same handle after a
            // `notified().await`. Use a hard panic if set() fails —
            // it indicates a serious bug in our concurrency model.
            outcome_handle
                .set(outcome_val.clone())
                .expect("RefreshSingleflight leader is the unique writer of OnceCell");

            // Wake every waiter that is currently parked on
            // `notified()`. `notify_waiters` does NOT leave a
            // standing permit — late-arriving waiters that joined
            // AFTER `set()` already saw the published `OnceCell`
            // value via the slot-replacement branch (a fresh
            // InFlight gets created for a newer snapshot.version).
            done_handle.notify_waiters();

            // Clear the slot so the next refresh starts fresh. Any
            // concurrent waiters that joined before this point
            // already hold their own `Arc<OnceCell>` + `Arc<Notify>`
            // clones and can observe the outcome regardless of slot
            // lifetime.
            self.clear_if_matches(snapshot.version);

            outcome_to_result(outcome_val)
        } else {
            // Waiter: park on the Notify with a wait_timeout bound.
            //
            // Lost-wakeup avoidance: tokio's `Notify::notify_waiters`
            // does NOT store a permit for late subscribers (unlike
            // `notify_one`). So we must register our `notified()`
            // future BEFORE checking `outcome_handle.get()`, with the
            // following ordering proof:
            //
            //   Case A — outcome already published when we register:
            //     `outcome_handle.get()` returns Some on the next line
            //     and we return immediately. No await needed.
            //
            //   Case B — outcome not yet published when we register:
            //     The leader's `set()` runs strictly BEFORE its
            //     `notify_waiters()` (enforced by the leader branch
            //     above), so when our `notified.await` resolves, the
            //     OnceCell is guaranteed populated.
            //
            // `Notify::notified()` is documented to register the wait
            // at construction time, so a `notify_waiters()` call that
            // happens between this line and the `.await` below will
            // still wake us.
            let notified = done_handle.notified();
            tokio::pin!(notified);

            if let Some(v) = outcome_handle.get() {
                return outcome_to_result(v.clone());
            }

            let outcome_val = timeout(self.wait_timeout, async {
                notified.as_mut().await;
                outcome_handle.get().cloned().expect(
                    "RefreshSingleflight invariant: leader publishes outcome before notifying",
                )
            })
            .await
            .map_err(|_| {
                KappError::Auth(AuthError::RefreshFailed(Box::new(KappError::Internal(
                    format!(
                        "refresh singleflight timed out after {:?}",
                        self.wait_timeout
                    ),
                ))))
            })?;
            outcome_to_result(outcome_val)
        }
    }

    fn clear_if_matches(&self, version: u64) {
        let mut guard = self.inner.lock();
        if let Some(existing) = guard.as_ref() {
            if existing.snapshot_version == version {
                *guard = None;
            }
        }
    }

    /// Test-only constructor exposing the wait timeout. Production
    /// code uses [`Self::default`] (20s).
    #[cfg(test)]
    pub(crate) fn with_wait_timeout(wait_timeout: Duration) -> Self {
        Self {
            inner: Arc::new(Mutex::new(None)),
            wait_timeout,
        }
    }
}

fn outcome_to_result(outcome: RefreshOutcome) -> Result<()> {
    match outcome {
        RefreshOutcome::Refreshed | RefreshOutcome::SkippedAlreadyRefreshed => Ok(()),
        // `error_message` is the `Display` rendering of the underlying
        // `KappError` from `do_refresh`. We can't preserve the original
        // typed error here because `RefreshOutcome` must be `Clone`
        // (shared via `OnceCell` across all waiters) and `KappError`
        // contains non-`Clone` source types (`tonic::transport::Error`,
        // `tonic::Status`). Wrap the flattened message in
        // `KappError::Internal` so pattern-matching callers can tell
        // “refresh RPC failed at runtime” apart from “client was
        // misconfigured” (the latter would surface as
        // `KappError::Config` at construction time, never here).
        RefreshOutcome::Failed { error_message } => Err(KappError::Auth(AuthError::RefreshFailed(
            Box::new(KappError::Internal(error_message)),
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::token_store::Tokens;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Arc;
    use std::time::SystemTime;

    fn fake_tokens() -> Tokens {
        Tokens {
            access_token: "a".into(),
            refresh_token: "r".into(),
            expires_at: SystemTime::now() + Duration::from_secs(60),
            tenant_id: "t".into(),
            session_id: "s".into(),
        }
    }

    #[tokio::test]
    async fn singleflight_only_runs_refresh_once_under_contention() {
        let store = TokenStore::new();
        store.set(fake_tokens());
        let snap = store.snapshot();
        let sf = RefreshSingleflight::default();
        let counter = Arc::new(AtomicUsize::new(0));

        let mut handles = vec![];
        for _ in 0..32 {
            let store = store.clone();
            let sf = sf.clone();
            let snap = snap.clone();
            let counter = Arc::clone(&counter);
            handles.push(tokio::spawn(async move {
                sf.refresh(&store, snap, |snap| {
                    let counter = Arc::clone(&counter);
                    let store = store.clone();
                    async move {
                        counter.fetch_add(1, Ordering::SeqCst);
                        // Simulate a slow refresh RPC.
                        tokio::time::sleep(Duration::from_millis(50)).await;
                        // Swap in a new token at the expected version.
                        store.swap_if_version(
                            snap.version,
                            Some(Tokens {
                                access_token: "new-access-token".into(),
                                ..fake_tokens()
                            }),
                        );
                        Ok(())
                    }
                })
                .await
                .unwrap();
            }));
        }
        for h in handles {
            h.await.unwrap();
        }

        // Exactly one refresh closure ran.
        assert_eq!(counter.load(Ordering::SeqCst), 1);
        // Token was actually swapped.
        assert_eq!(store.current().unwrap().access_token, "new-access-token");
        assert_eq!(store.version(), snap.version + 1);
    }

    #[tokio::test]
    async fn singleflight_skips_when_version_already_advanced() {
        let store = TokenStore::new();
        store.set(fake_tokens());
        let snap = store.snapshot();
        // Someone else already refreshed.
        store.set(Tokens {
            access_token: "advanced".into(),
            ..fake_tokens()
        });

        let sf = RefreshSingleflight::default();
        let counter = Arc::new(AtomicUsize::new(0));
        let counter_clone = Arc::clone(&counter);
        sf.refresh(&store, snap, |_snap| async move {
            counter_clone.fetch_add(1, Ordering::SeqCst);
            Ok(())
        })
        .await
        .unwrap();
        // The refresh closure was NOT invoked because the version
        // had already advanced.
        assert_eq!(counter.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn singleflight_propagates_refresh_failure_to_all_waiters() {
        let store = TokenStore::new();
        store.set(fake_tokens());
        let snap = store.snapshot();
        let sf = RefreshSingleflight::default();
        let counter = Arc::new(AtomicUsize::new(0));

        let mut handles = vec![];
        for _ in 0..8 {
            let store = store.clone();
            let sf = sf.clone();
            let snap = snap.clone();
            let counter = Arc::clone(&counter);
            handles.push(tokio::spawn(async move {
                sf.refresh(&store, snap, move |_snap| {
                    let counter = Arc::clone(&counter);
                    async move {
                        counter.fetch_add(1, Ordering::SeqCst);
                        tokio::time::sleep(Duration::from_millis(20)).await;
                        Err(KappError::Auth(AuthError::Unauthenticated))
                    }
                })
                .await
            }));
        }
        let mut failure_count = 0;
        for h in handles {
            if h.await.unwrap().is_err() {
                failure_count += 1;
            }
        }
        // Refresh closure ran exactly once...
        assert_eq!(counter.load(Ordering::SeqCst), 1);
        // ...and all 8 waiters observed the failure.
        assert_eq!(failure_count, 8);
    }

    #[tokio::test]
    async fn singleflight_times_out_waiters() {
        let store = TokenStore::new();
        store.set(fake_tokens());
        let snap = store.snapshot();
        // Leader takes 500ms; waiter timeout is 50ms.
        let sf = RefreshSingleflight::with_wait_timeout(Duration::from_millis(50));

        // Spawn a leader that's deliberately slow.
        let store_l = store.clone();
        let sf_l = sf.clone();
        let snap_l = snap.clone();
        let _leader = tokio::spawn(async move {
            let _ = sf_l
                .refresh(&store_l, snap_l, |_| async move {
                    tokio::time::sleep(Duration::from_millis(500)).await;
                    Ok(())
                })
                .await;
        });

        // Wait a beat so the leader registers in-flight state.
        tokio::time::sleep(Duration::from_millis(10)).await;

        // Now spawn a waiter; it should hit the 50ms timeout.
        let waiter =
            tokio::spawn(async move { sf.refresh(&store, snap, |_| async { Ok(()) }).await });
        let res = waiter.await.unwrap();
        assert!(res.is_err(), "expected waiter to time out, got {res:?}");
    }

    #[tokio::test]
    async fn second_refresh_after_failure_runs_fresh_closure() {
        // After a failed refresh the slot must clear so a follow-
        // up attempt actually runs the closure again.
        let store = TokenStore::new();
        store.set(fake_tokens());
        let snap = store.snapshot();
        let sf = RefreshSingleflight::default();

        let counter = Arc::new(AtomicUsize::new(0));
        let c1 = Arc::clone(&counter);
        let res1 = sf
            .refresh(&store, snap.clone(), |_| async move {
                c1.fetch_add(1, Ordering::SeqCst);
                Err(KappError::Auth(AuthError::Unauthenticated))
            })
            .await;
        assert!(res1.is_err());

        let c2 = Arc::clone(&counter);
        let res2 = sf
            .refresh(&store, snap, |_| async move {
                c2.fetch_add(1, Ordering::SeqCst);
                Err(KappError::Auth(AuthError::Unauthenticated))
            })
            .await;
        assert!(res2.is_err());

        // BOTH closures ran — the slot from attempt 1 was correctly
        // cleared so attempt 2 didn't piggy-back on the cached
        // outcome.
        assert_eq!(counter.load(Ordering::SeqCst), 2);
    }
}
