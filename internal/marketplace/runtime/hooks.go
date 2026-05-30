package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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

func (noopHooks) Dispatch(ctx context.Context, in *LifecycleDispatch) (*LifecycleResult, error) {
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
type transportHooks struct {
	transport Transport
	now       func() time.Time
}

// NewTransportHooks wraps a Transport with lifecycle-hook semantics.
// If now is nil, time.Now is used.
func NewTransportHooks(t Transport, now func() time.Time) LifecycleHooks {
	if now == nil {
		now = time.Now
	}
	return &transportHooks{transport: t, now: now}
}

// lifecycleRetry is the retry policy for lifecycle hooks. Lifecycle
// dispatches are not as latency-sensitive as tool invokes so we use
// a fixed 3-attempt exponential policy regardless of manifest
// configuration (there is no manifest field for lifecycle retry).
var lifecycleRetry = &RetryPolicy{MaxAttempts: 3, Backoff: "exponential"}

// is2xx reports whether status is in [200, 300).
func is2xx(status int) bool { return status >= 200 && status < 300 }

// isPreLifecycle reports whether the phase is a pre_-style phase
// where extension failure should abort the engine.
func isPreLifecycle(p LifecyclePhase) bool {
	return p == PhasePreInstall || p == PhasePreUninstall
}

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

	dispatchReq := &DispatchRequest{
		TenantID:           in.TenantID,
		InstallationID:     in.InstallationID,
		ExtensionID:        in.ExtensionID,
		ExtensionVersionID: in.ExtensionVersionID,
		Kind:               DispatchKindForPhase(in.Phase),
		URL:                endpoint,
		Body:               in.Body,
		Timeout:            timeout,
		Retry:              lifecycleRetry,
		SigningSecret:      in.SigningSecret,
		RequestID:          requestID,
	}

	var lastErr error
	for attempt := 1; attempt <= lifecycleRetry.MaxAttempts; attempt++ {
		if delay := lifecycleRetry.BackoffDelay(attempt); delay > 0 {
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
		result.Signature = headers[SignatureHeaderName]
		resp, sendErr := h.transport.Send(ctx, endpoint, in.Body, headers, timeout)
		result.Attempt = attempt
		if sendErr != nil {
			lastErr = sendErr
			result.Err = sendErr
			if !isRetryableTransportError(sendErr) {
				break
			}
			continue
		}
		result.LatencyMS = int(resp.Latency / time.Millisecond)
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
		// 4xx (except 404) = terminal extension-side rejection.
		// Pre-phases abort; post-phases just log.
		if resp.Status >= 400 && resp.Status < 500 {
			break
		}
		// 5xx = retryable. Continue the loop.
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
