package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LifecycleHooks is the abstraction the Engine uses to invoke
// lifecycle webhooks on the extension. The interface is small on
// purpose — the engine should not own HTTP / signing semantics; it
// just dispatches the phase and decides whether to abort or
// continue based on the result.
//
// Two implementations:
//
//   - transportHooks (the production default) — signs the request
//     via SignRequest and POSTs it via the Transport. Used by
//     Engine.Install / Engine.Uninstall.
//
//   - NoopHooks — accepts every phase as 200 OK. Used by tests
//     that exercise non-lifecycle Engine code paths
//     (registrar-only tests, dispatcher-only tests).
//
// Engine wires whichever it needs at construction time.
type LifecycleHooks interface {
	// Dispatch issues the lifecycle POST and returns the dispatch
	// log row data the engine should record. Returns an error if
	// the extension's hook rejected the install (non-2xx, non-404)
	// or if a transport-level failure exhausted retries.
	//
	// 404 is treated as "extension did not implement this phase"
	// and is NOT an error — the Result.Status is 404, Result.Aborted
	// is false, and the engine moves on.
	//
	// PreInstall / PreUninstall failures BLOCK the engine and
	// surface as ErrPreInstallRejected / ErrPreUninstallRejected;
	// PostInstall / PostUninstall failures are best-effort and
	// surface only via the dispatch log.
	Dispatch(ctx context.Context, in *LifecycleDispatch) (*LifecycleResult, error)
}

// LifecycleDispatch is the input to LifecycleHooks.Dispatch. The
// caller (Engine) builds this struct once per phase.
type LifecycleDispatch struct {
	TenantID           uuid.UUID
	InstallationID     uuid.UUID
	ExtensionID        uuid.UUID
	ExtensionVersionID uuid.UUID
	Phase              LifecyclePhase
	WebhookBase        string
	SigningSecret      SigningSecret
	// Body is the JSON payload sent to the extension. Empty body
	// is allowed — the body_sha256 then signs the SHA of zero
	// bytes.
	Body []byte
	// Timeout is the per-attempt HTTP timeout. Defaults to 30s if
	// zero (lifecycle hooks aren't latency-sensitive but should
	// not block indefinitely).
	Timeout time.Duration
}

// Validate runs cheap field-level sanity checks.
func (d *LifecycleDispatch) Validate() error {
	if d == nil {
		return errors.New("runtime: nil lifecycle dispatch")
	}
	if d.TenantID == uuid.Nil {
		return errors.New("runtime: tenant_id required")
	}
	if d.Phase == "" {
		return errors.New("runtime: phase required")
	}
	// InstallationID is required for every phase EXCEPT pre_install
	// — the install row doesn't exist yet at the time pre_install
	// fires (the engine intentionally dispatches the blocking
	// pre_install hook BEFORE running the registration tx, so a
	// rejection produces zero side effects in the DB). Validation
	// must mirror that lifecycle ordering.
	if d.InstallationID == uuid.Nil && d.Phase != PhasePreInstall {
		return errors.New("runtime: installation_id required (only optional on pre_install)")
	}
	if err := IsValidWebhookBase(d.WebhookBase); err != nil {
		return err
	}
	return nil
}

// timeoutOrDefault returns d.Timeout or a 30s default if zero.
func (d *LifecycleDispatch) timeoutOrDefault() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 30 * time.Second
}

// LifecycleResult is the outcome of a single lifecycle dispatch.
// The Engine reads Aborted to decide whether to short-circuit the
// install/uninstall flow.
type LifecycleResult struct {
	// Status is the HTTP status code returned by the extension
	// (200, 204, 404, 502, ...). Zero means no response was
	// received — Aborted will then reflect the phase semantics.
	Status int
	// Aborted is true iff the engine should NOT proceed past this
	// hook. Pre-phase 4xx/5xx/transport-error → true; post-phase
	// failure → false. 404 → false on all phases (extension did
	// not implement the hook).
	Aborted bool
	// AbortReason is a short human-readable string describing why
	// the engine aborted. Empty when Aborted == false.
	AbortReason string
	// RequestID is the UUID the engine generated for the dispatch.
	// Used by the dispatch log row writer.
	RequestID uuid.UUID
	// Attempt is the 1-indexed attempt number that produced the
	// final response (only retried on 5xx / transport error).
	Attempt int
	// BodySHA256 is the hex SHA-256 of the request body. Used by
	// the dispatch log row writer.
	BodySHA256 string
	// Signature is the value of X-Kapp-Signature as sent. Used by
	// the dispatch log row writer.
	Signature string
	// LatencyMS is the per-attempt latency of the final attempt.
	LatencyMS int
	// Endpoint is the absolute URL the dispatcher POSTed to.
	// Captured for the audit log.
	Endpoint string
	// Err is the transport-level error from the final attempt, if
	// any. nil if the HTTP round-trip completed (even with a non-
	// 2xx status).
	Err error
}

// noopHooks accepts every phase with a 200 response and never aborts.
// Used by tests that exercise non-lifecycle code paths.
type noopHooks struct{}

// NoopHooks returns a LifecycleHooks that never dispatches and never
// aborts. Useful for tests of registrar / dispatcher / engine paths
// that do not exercise the hook layer.
func NoopHooks() LifecycleHooks { return noopHooks{} }

// Dispatch is the noop implementation: every call validates the
// payload, then returns a fixed 200/empty success with no side
// effects.
func (noopHooks) Dispatch(_ context.Context, in *LifecycleDispatch) (*LifecycleResult, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	return &LifecycleResult{
		Status:    200,
		Aborted:   false,
		RequestID: uuid.New(),
		Attempt:   1,
	}, nil
}

// transportHooks is the production LifecycleHooks. Signs the request
// via SignRequest and POSTs it via the Transport with a fixed retry
// budget. Same backoff as agent-tool dispatch: 1 initial attempt + up
// to 2 retries on 5xx / transport error, exponential backoff (1s, 2s).
//
// pool is used to write per-attempt rows into marketplace_dispatch_log
// — symmetric with Dispatcher.Invoke (Devin Review round-7
// ANALYSIS_0003 on PR #127). When pool is nil (NewTransportHooks
// called without a DB pool), audit-row writes are skipped silently
// and the hook returns its LifecycleResult unchanged. The latter
// path exists for tests that exercise the retry/classification
// logic without booting a Postgres harness.
type transportHooks struct {
	transport Transport
	pool      *pgxpool.Pool
	now       func() time.Time
}

// NewTransportHooks wraps a Transport with lifecycle-hook semantics.
// If now is nil, time.Now is used. If pool is non-nil, every
// lifecycle dispatch attempt also writes a marketplace_dispatch_log
// row (symmetric with Dispatcher.Invoke). Engine.Install /
// Engine.Uninstall always pass a non-nil pool; tests that exercise
// only the HTTP-classification path may pass nil to skip audit
// writes.
func NewTransportHooks(t Transport, pool *pgxpool.Pool, now func() time.Time) LifecycleHooks {
	if now == nil {
		now = time.Now
	}
	return &transportHooks{transport: t, pool: pool, now: now}
}

// lifecycleRetryAttempts / lifecycleRetryBackoff are the immutable
// scalar constants that define the lifecycle-hook retry policy.
// Lifecycle dispatches are not as latency-sensitive as tool invokes
// so we use a fixed 3-attempt exponential policy regardless of
// manifest configuration (there is no manifest field for lifecycle
// retry).
//
// Constants rather than a package-level `*RetryPolicy` pointer:
// Devin Review round-4 on PR #127 flagged the previous
// `var lifecycleRetry = &RetryPolicy{...}` form as fragile —
// although today only read paths reference it, a future test or
// refactor that mutated `lifecycleRetry.MaxAttempts` (e.g. to drive
// down hook attempts in a fast-path test) would silently race every
// other goroutine still dispatching through `transportHooks`.
// Scalar constants + a per-call constructor eliminate that footgun:
// each Dispatch invocation now mints its own *RetryPolicy on the
// heap, so no shared mutable state is reachable from outside this
// function. The DB-side dispatch CHECK (`retry_max_attempts >= 1`)
// is irrelevant here — lifecycle hooks are not configurable per
// manifest — but the same invariant is enforced statically by the
// constant value.
const (
	lifecycleRetryAttempts = 3
	lifecycleRetryBackoff  = "exponential"
)

// newLifecycleRetryPolicy returns a fresh *RetryPolicy with the
// hard-coded lifecycle-hook attempts/backoff. Each call mints a
// new heap allocation so the returned pointer is not aliased by
// any other caller.
func newLifecycleRetryPolicy() *RetryPolicy {
	return &RetryPolicy{MaxAttempts: lifecycleRetryAttempts, Backoff: lifecycleRetryBackoff}
}

// is2xx reports whether status is in [200, 300).
func is2xx(status int) bool { return status >= 200 && status < 300 }

// isPreLifecycle reports whether the phase is a pre_-style phase
// where extension failure should abort the engine.
func isPreLifecycle(p LifecyclePhase) bool {
	return p == PhasePreInstall || p == PhasePreUninstall
}

// Dispatch signs and POSTs the lifecycle payload to the extension's
// /lifecycle/{phase} endpoint via the configured Transport, then
// classifies the response per the spec (5xx → retry-eligible per
// phase semantics; non-2xx on pre_ phases → aborted; transport
// error on pre_ phases → aborted with ErrLifecycleTransport).
// Writes start/complete rows to marketplace_dispatch_log around
// each attempt when pool is non-nil.
func (h *transportHooks) Dispatch(ctx context.Context, in *LifecycleDispatch) (*LifecycleResult, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if h == nil || h.transport == nil {
		return nil, errors.New("runtime: lifecycle hooks: nil transport")
	}
	requestID := uuid.New()
	endpoint := in.WebhookBase + in.Phase.LifecyclePath()
	bodyHash := BodyHashHex(in.Body)
	timeout := in.timeoutOrDefault()

	result := &LifecycleResult{
		RequestID:  requestID,
		BodySHA256: bodyHash,
		Endpoint:   endpoint,
	}

	// Mint a per-call *RetryPolicy via newLifecycleRetryPolicy so
	// no two goroutines share an aliased pointer (round-4 finding
	// on the previous package-level `lifecycleRetry` global).
	retry := newLifecycleRetryPolicy()

	dispatchReq := &DispatchRequest{
		TenantID:           in.TenantID,
		InstallationID:     in.InstallationID,
		ExtensionID:        in.ExtensionID,
		ExtensionVersionID: in.ExtensionVersionID,
		Kind:               DispatchKindForPhase(in.Phase),
		URL:                endpoint,
		Body:               in.Body,
		Timeout:            timeout,
		Retry:              retry,
		SigningSecret:      in.SigningSecret,
		RequestID:          requestID,
	}

	var lastErr error
	for attempt := 1; attempt <= retry.MaxAttempts; attempt++ {
		if delay := retry.BackoffDelay(attempt); delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		ts := h.now()
		headers, _, err := SignRequest(dispatchReq, ts)
		if err != nil {
			return nil, fmt.Errorf("runtime: lifecycle sign: %w", err)
		}
		signature := headers[SignatureHeaderName]
		result.Signature = signature

		// Open the in-flight audit row BEFORE the HTTP send.
		// Mirrors Dispatcher.Invoke's pattern: if the process
		// dies mid-attempt, the row's NULL completed_at flags
		// "in flight at crash time". Devin Review round-7
		// ANALYSIS_0003 on PR #127 added this write — previously
		// lifecycle hooks were dispatched without any audit
		// trail, asymmetric with tool invokes. installation_id
		// is uuid.Nil for pre_install (the install row doesn't
		// exist yet at that phase); writeDispatchLogStart
		// translates uuid.Nil → SQL NULL.
		var logRowID uuid.UUID
		if h.pool != nil {
			startedID, startErr := writeDispatchLogStart(ctx, h.pool, dispatchLogStart{
				TenantID:           in.TenantID,
				InstallationID:     in.InstallationID,
				ExtensionID:        in.ExtensionID,
				ExtensionVersionID: in.ExtensionVersionID,
				Kind:               dispatchReq.Kind,
				Endpoint:           endpoint,
				RequestID:          requestID,
				Attempt:            attempt,
				BodySHA256:         bodyHash,
				Signature:          signature,
				StartedAt:          ts,
			})
			if startErr != nil {
				// Audit-row INSERT failure does NOT abort the
				// dispatch — the lifecycle outcome is the
				// operator-visible truth and a missing audit
				// row is a degraded-mode concern, not a fatal
				// one. Surface via slog so operators can
				// correlate gaps with cause.
				logAuditWriteFailure(ctx, string(in.Phase), requestID, attempt, startErr)
			} else {
				logRowID = startedID
			}
		}

		started := time.Now()
		resp, sendErr := h.transport.Send(ctx, endpoint, in.Body, headers, timeout)
		latency := time.Since(started)
		result.Attempt = attempt
		if sendErr != nil {
			lastErr = sendErr
			result.Err = sendErr
			if h.pool != nil {
				if logErr := writeDispatchLogComplete(ctx, h.pool, in.TenantID, logRowID, 0, latency, sendErr); logErr != nil {
					logAuditWriteFailure(ctx, string(in.Phase), requestID, attempt, logErr)
				}
			}
			if !isRetryableTransportError(sendErr) {
				break
			}
			continue
		}
		if h.pool != nil {
			if logErr := writeDispatchLogComplete(ctx, h.pool, in.TenantID, logRowID, resp.Status, latency, nil); logErr != nil {
				logAuditWriteFailure(ctx, string(in.Phase), requestID, attempt, logErr)
			}
		}
		// Clear any transport-error state captured by a prior
		// attempt — this attempt got an HTTP round-trip, so
		// AbortReason / result.Err must reflect the http result,
		// not a stale lastErr from attempt N-1. Devin Review
		// ANALYSIS_0001 round-2 on PR #127 — without this clear,
		// a DNS failure on attempt 1 followed by a 5xx-exhaust
		// reported "transport: dns failure" instead of
		// "http 5xx after N attempts", and result.Err violated
		// its documented contract ("final attempt's transport
		// error").
		lastErr = nil
		result.Err = nil
		// LatencyMS is wall-clock time measured locally around
		// the transport.Send call (see line 329-331 above), NOT
		// the transport-self-reported resp.Latency. This matches
		// (a) the dispatch_log audit row written by
		// writeDispatchLogComplete at line 347, which also uses
		// the local `latency`, keeping LifecycleResult.LatencyMS
		// in lock-step with what operators see in the DB;
		// (b) Dispatcher.Invoke's result.Latency at
		// dispatcher.go:238, which uses time.Since(started)
		// locally for the same reason. resp.Latency is 0 for
		// InMemoryTransport (used in tests) since it never
		// populates the field, so reading from it would silently
		// report 0ms in every unit/integration test even when
		// real wall-clock time elapsed. Devin Review round-8
		// BUG_0002 on PR #127.
		result.LatencyMS = int(latency / time.Millisecond)
		result.Status = resp.Status

		// 404 = "extension did not implement this phase". Not an
		// abort; the dispatch log records it but the engine moves on.
		if resp.Status == 404 {
			result.Aborted = false
			return result, nil
		}
		// 2xx = success.
		if is2xx(resp.Status) {
			result.Aborted = false
			return result, nil
		}
		// 408 (Request Timeout) is retryable — mirrors the
		// agent-tool Dispatcher behaviour at dispatcher.go:203
		// so the two retry classifiers stay in lock-step. Devin
		// Review ANALYSIS_0002 round-2 on PR #127 caught the
		// drift: extensions whose webhook server returns 408 on
		// startup would have aborted pre_install without retry
		// even though the same 408 from a tool invoke retries.
		if resp.Status == 408 || resp.Status >= 500 {
			// 5xx + 408 = retryable. Continue the loop.
			continue
		}
		// Catch-all terminal: anything else (3xx, 4xx non-
		// 404/408, 1xx, or any other unexpected non-2xx code)
		// is treated as a terminal extension-side response.
		// Mirrors the agent-tool Dispatcher catch-all at
		// dispatcher.go:209-210 (`return result, nil` for any
		// status not already classified) so the two retry
		// classifiers stay in lock-step. Devin Review round-3
		// on PR #127 caught the previous drift: a 3xx response
		// (e.g. 301/302 — the transport refuses to follow
		// redirects via CheckRedirect=http.ErrUseLastResponse
		// so they bubble up as a raw status) fell through every
		// if-block and silently retried until MaxAttempts, then
		// reported "transport: <stale-err>" as the AbortReason
		// because result.Err / lastErr were nil after the
		// successful HTTP round-trip — completely the wrong
		// signal to operators. Unconditional break here means
		// the post-loop pre-phase block reports the correct
		// `http <status> after 1 attempts`.
		break
	}

	// Exhausted retries OR terminal 4xx. Classify based on phase.
	if isPreLifecycle(in.Phase) {
		result.Aborted = true
		if lastErr != nil {
			result.AbortReason = fmt.Sprintf("transport: %v", lastErr)
		} else {
			result.AbortReason = fmt.Sprintf("http %d after %d attempts", result.Status, result.Attempt)
		}
		return result, fmt.Errorf("%w: phase=%s status=%d", phaseRejectedError(in.Phase), in.Phase, result.Status)
	}
	// Post-phase: best-effort; surface the status but don't abort.
	result.Aborted = false
	return result, nil
}

// phaseRejectedError returns the sentinel error for a rejected pre-
// phase. Engine.Install / Engine.Uninstall test for these with
// errors.Is.
func phaseRejectedError(p LifecyclePhase) error {
	switch p {
	case PhasePreInstall:
		return ErrPreInstallRejected
	case PhasePreUninstall:
		return ErrPreUninstallRejected
	default:
		return errors.New("runtime: unknown pre-phase rejection")
	}
}

// isRetryableTransportError reports whether a transport-level error
// should trigger a retry. We retry on:
//   - context-deadline exceeded (matches ErrDispatchTimeout)
//   - any other non-context error (network failures, TLS failures)
//
// We do NOT retry on ctx.Done() because that's the caller cancelling
// the whole operation — retrying inside a cancelled ctx would still
// fail.
func isRetryableTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// MarshalLifecyclePayload is a small helper for callers to JSON-
// encode the standard lifecycle body. The engine uses this to pass
// the operator's settings plus a few metadata fields to the
// extension's hook handler.
func MarshalLifecyclePayload(payload any) ([]byte, error) {
	if payload == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("runtime: marshal lifecycle payload: %w", err)
	}
	return b, nil
}
