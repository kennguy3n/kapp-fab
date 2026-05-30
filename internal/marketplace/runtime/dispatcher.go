package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// Dispatcher invokes registered agent-tools on behalf of the agent
// platform. Each Invoke:
//
//  1. Looks up the (tenant, installation, tool_name) row in
//     marketplace_extension_agent_tools to fetch the endpoint /
//     timeout / retry config.
//  2. Verifies the installation's status == 'active'. Non-active
//     installations refuse dispatch with ErrInstallationNotActive.
//  3. Generates a per-logical-request UUID. Each retry attempt
//     reuses the request ID so the dispatch log can group them.
//  4. Per attempt: sign → POST → record dispatch_log row →
//     classify response. 2xx is terminal success; 4xx (non-408) is
//     terminal failure; 5xx + 408 + transport error retry per the
//     policy.
//
// The Dispatcher does NOT own the install/uninstall lifecycle —
// Engine.Install / Engine.Uninstall do. The Dispatcher is the hot
// path for runtime tool calls and is wired into the agent runtime
// (separate package, B4) which translates LLM tool-call requests
// into invokes.
type Dispatcher struct {
	pool      *pgxpool.Pool
	transport Transport
	now       func() time.Time
}

// NewDispatcher constructs a Dispatcher with the supplied pool +
// transport. If now is nil, time.Now is used.
func NewDispatcher(pool *pgxpool.Pool, transport Transport, now func() time.Time) *Dispatcher {
	if now == nil {
		now = time.Now
	}
	return &Dispatcher{pool: pool, transport: transport, now: now}
}

// InvokeRequest is the per-call payload to Dispatcher.Invoke.
type InvokeRequest struct {
	// TenantID + InstallationID identify the (tenant, install)
	// pair whose registered tool is being invoked. RLS guards the
	// installation lookup.
	TenantID       uuid.UUID
	InstallationID uuid.UUID
	// ToolName is the canonical tool name from
	// marketplace_extension_agent_tools.tool_name.
	ToolName string
	// Body is the JSON request body sent to the tool's endpoint.
	// The dispatcher does NOT alter or wrap the body.
	Body []byte
}

// InvokeResult is the per-call return from Dispatcher.Invoke.
type InvokeResult struct {
	// Status is the final HTTP status code returned by the
	// extension. 0 if all attempts hit a transport-level error.
	Status int
	// Body is the response body (capped at MaxResponseBodyBytes
	// by the transport).
	Body []byte
	// Header is a flat copy of the final attempt's response
	// headers.
	Header map[string]string
	// Attempt is the 1-indexed attempt count that produced the
	// final response.
	Attempt int
	// RequestID groups all dispatch_log rows from this invoke. The
	// caller can correlate retry attempts via SELECT WHERE
	// request_id = $1.
	RequestID uuid.UUID
	// Latency is the per-attempt latency of the FINAL attempt
	// only. Cumulative latency is reconstructible from the
	// dispatch_log rows.
	Latency time.Duration
}

// Invoke runs the dispatch sequence for one logical tool call.
// Returns *InvokeResult with status / body / attempts. A non-nil
// error means the dispatcher could not complete the dispatch at
// all (DB lookup failed, all retries exhausted with transport
// errors, etc.). A 4xx or 5xx response is NOT an error — the
// caller reads InvokeResult.Status.
func (d *Dispatcher) Invoke(ctx context.Context, in *InvokeRequest) (*InvokeResult, error) {
	if d == nil {
		return nil, errors.New("runtime: nil dispatcher")
	}
	if in == nil {
		return nil, errors.New("runtime: nil invoke request")
	}
	if d.pool == nil {
		return nil, errors.New("runtime: dispatcher: nil pool")
	}
	if d.transport == nil {
		return nil, errors.New("runtime: dispatcher: nil transport")
	}
	if in.TenantID == uuid.Nil || in.InstallationID == uuid.Nil || in.ToolName == "" {
		return nil, errors.New("runtime: invoke: tenant_id, installation_id, tool_name required")
	}

	// Look up the install + tool descriptor in a single tenant-
	// scoped tx so RLS gates both reads. The audit log writes
	// happen in their own txns (one per attempt) so a transport-
	// level slowness doesn't hold the descriptor txn open.
	desc, err := d.lookupDescriptor(ctx, in)
	if err != nil {
		return nil, err
	}

	requestID := uuid.New()
	retry := &RetryPolicy{MaxAttempts: desc.RetryMaxAttempts, Backoff: desc.RetryBackoff}
	// Defensive floor on MaxAttempts so the retry loop is guaranteed
	// to execute its body at least once even if a future code path
	// (e.g. a direct INSERT into marketplace_extension_agent_tools
	// that bypasses Registrar's defaulting at registrar.go:347) or
	// an unexpected manifest defaulting bug sets RetryMaxAttempts <
	// 1. Without this guard, the loop header `attempt <=
	// retry.MaxAttempts` would skip the body for MaxAttempts == 0
	// and fall straight through to the `panic("unreachable")` at
	// the end of Invoke, taking down the agent runtime instead of
	// surfacing a dispatch error.
	//
	// The DB CHECK `retry_max_attempts >= 1` at migration line 209
	// already enforces this invariant at write-time, so this guard
	// is belt-and-braces only. Devin Review round-5 on PR #127
	// asked for a code-side floor so the panic invariant is truly
	// unreachable without depending on the constraint — cheaper
	// than re-reasoning about whether every future write path
	// honours the constraint, and consistent with the same defence
	// already in transportHooks.Dispatch's lifecycle path
	// (newLifecycleRetryPolicy returns MaxAttempts: 3 — the
	// constant ensures the value is always >= 1).
	if retry.MaxAttempts < 1 {
		retry.MaxAttempts = 1
	}

	result := &InvokeResult{RequestID: requestID}
	var lastSendErr error

	for attempt := 1; attempt <= retry.MaxAttempts; attempt++ {
		if delay := retry.BackoffDelay(attempt); delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		ts := d.now()
		dispatchReq := &DispatchRequest{
			TenantID:           in.TenantID,
			InstallationID:     in.InstallationID,
			ExtensionID:        desc.ExtensionID,
			ExtensionVersionID: desc.ExtensionVersionID,
			Kind:               KindToolInvoke,
			URL:                desc.Endpoint,
			Body:               in.Body,
			Timeout:            desc.Timeout,
			Retry:              retry,
			SigningSecret:      desc.SigningSecret,
			RequestID:          requestID,
		}
		headers, _, signErr := SignRequest(dispatchReq, ts)
		if signErr != nil {
			return nil, fmt.Errorf("runtime: invoke: sign: %w", signErr)
		}
		signature := headers[SignatureHeaderName]
		bodyHash := BodyHashHex(in.Body)

		// Open the in-flight log row BEFORE the dispatch. If the
		// process dies mid-attempt the row records the attempted
		// dispatch (response_status NULL, error NULL means "in
		// flight at crash time"). Completion is a subsequent
		// UPDATE in the same logical attempt.
		logRowID, err := writeDispatchLogStart(ctx, d.pool, dispatchLogStart{
			TenantID:           in.TenantID,
			InstallationID:     in.InstallationID,
			ExtensionID:        desc.ExtensionID,
			ExtensionVersionID: desc.ExtensionVersionID,
			Kind:               dispatchReq.Kind,
			Endpoint:           desc.Endpoint,
			RequestID:          requestID,
			Attempt:            attempt,
			BodySHA256:         bodyHash,
			Signature:          signature,
			StartedAt:          ts,
		})
		if err != nil {
			return nil, fmt.Errorf("runtime: invoke: open log: %w", err)
		}

		started := time.Now()
		resp, sendErr := d.transport.Send(ctx, desc.Endpoint, in.Body, headers, desc.Timeout)
		latency := time.Since(started)

		if sendErr != nil {
			lastSendErr = sendErr
			if logErr := writeDispatchLogComplete(ctx, d.pool, in.TenantID, logRowID, 0, latency, sendErr); logErr != nil {
				// Audit-log write failure does NOT abort the dispatch
				// (the tool response is the operator-visible outcome),
				// but it IS surfaced via slog.Warn so operators can
				// correlate dispatch_log gaps with underlying causes
				// (DB outage, pool exhaustion, RLS misconfig). Devin
				// Review round-7 ANALYSIS_0002 on PR #127 caught the
				// previous silent `_ = logErr` discard: the comment
				// claimed the failure was "logged" but no actual
				// logging happened, leaving operators blind to gaps
				// in the audit trail.
				logAuditWriteFailure(ctx, "tool_invoke", requestID, attempt, logErr)
			}
			if !isRetryableTransportError(sendErr) || attempt == retry.MaxAttempts {
				if errors.Is(sendErr, ErrDispatchTimeout) {
					return nil, fmt.Errorf("%w: tool %q after %d attempts", ErrDispatchTimeout, in.ToolName, attempt)
				}
				return nil, fmt.Errorf("runtime: invoke: tool %q: %w", in.ToolName, sendErr)
			}
			continue
		}
		if logErr := writeDispatchLogComplete(ctx, d.pool, in.TenantID, logRowID, resp.Status, latency, nil); logErr != nil {
			logAuditWriteFailure(ctx, "tool_invoke", requestID, attempt, logErr)
		}
		result.Status = resp.Status
		result.Body = resp.Body
		result.Header = resp.Header
		result.Attempt = attempt
		result.Latency = latency

		// 2xx = terminal success.
		if is2xx(resp.Status) {
			return result, nil
		}
		// 408 (Request Timeout) and 5xx = retryable.
		if resp.Status == 408 || resp.Status >= 500 {
			if attempt == retry.MaxAttempts {
				return result, nil
			}
			continue
		}
		// 4xx (non-408) = terminal failure; surface to caller.
		return result, nil
	}
	// Unreachable: every path inside the loop returns on the
	// final attempt (transport-error branch returns via
	// `attempt == retry.MaxAttempts`; HTTP branches return via
	// the 2xx / 4xx / 5xx classifiers; the 5xx-retryable path
	// also returns when attempt == MaxAttempts). The DB CHECK
	// `retry_max_attempts >= 1` plus the loop header
	// `attempt <= retry.MaxAttempts` guarantee the loop body
	// always runs at least once. A defensive `return result,
	// nil` here would mislead readers into thinking there is a
	// reachable exit that propagates the in-flight result up
	// without classification -- Devin Review ANALYSIS_0003
	// round-2 on PR #127 flagged exactly that confusion. Using
	// panic("unreachable") follows the same idiom as the standard
	// library (cf. fmt/print.go) for compiler-required terminators
	// on dead paths. lastSendErr is intentionally ignored here:
	// if it were ever reached, the loop's final-attempt branch
	// would have returned the wrapped transport error already.
	_ = lastSendErr
	panic("runtime: dispatcher.Invoke: unreachable: retry loop must terminate inside body")
}

// agentToolDescriptor is the row Dispatcher.lookupDescriptor pulls
// from the runtime tables. Includes both the tool config and the
// installation's signing secret + extension/version IDs (needed for
// the dispatch_log audit row).
type agentToolDescriptor struct {
	ExtensionID        uuid.UUID
	ExtensionVersionID uuid.UUID
	InstallStatus      string
	SigningSecret      SigningSecret
	Endpoint           string
	Handler            string
	Timeout            time.Duration
	RetryMaxAttempts   int
	RetryBackoff       string
}

func (d *Dispatcher) lookupDescriptor(ctx context.Context, in *InvokeRequest) (*agentToolDescriptor, error) {
	desc := &agentToolDescriptor{}
	err := dbutil.WithTenantTx(ctx, d.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// One JOIN reads both the install row and the tool row in
		// the same RLS-scoped query.
		var (
			extID, verID uuid.UUID
			status       string
			signing      string
			endpoint     string
			handler      string
			timeoutMs    int
			maxAttempts  int
			backoff      string
		)
		row := tx.QueryRow(ctx, `
			SELECT i.extension_id,
			       i.extension_version_id,
			       i.status,
			       i.signing_secret,
			       t.endpoint,
			       t.handler,
			       t.timeout_ms,
			       t.retry_max_attempts,
			       t.retry_backoff
			  FROM marketplace_extension_installations AS i
			  JOIN marketplace_extension_agent_tools AS t
			    ON t.tenant_id = i.tenant_id
			   AND t.installation_id = i.id
			 WHERE i.id = $1
			   AND t.tool_name = $2`,
			in.InstallationID, in.ToolName,
		)
		scanErr := row.Scan(&extID, &verID, &status, &signing, &endpoint, &handler, &timeoutMs, &maxAttempts, &backoff)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return ErrToolNotRegistered
			}
			return fmt.Errorf("runtime: dispatcher: lookup descriptor: %w", scanErr)
		}
		desc.ExtensionID = extID
		desc.ExtensionVersionID = verID
		desc.InstallStatus = status
		desc.SigningSecret = SigningSecret(signing)
		desc.Endpoint = endpoint
		desc.Handler = handler
		desc.Timeout = time.Duration(timeoutMs) * time.Millisecond
		desc.RetryMaxAttempts = maxAttempts
		desc.RetryBackoff = backoff
		return nil
	})
	if err != nil {
		return nil, err
	}
	if desc.InstallStatus != string(marketplace.InstallStatusActive) {
		return nil, fmt.Errorf("%w: installation %s is %q", ErrInstallationNotActive, in.InstallationID, desc.InstallStatus)
	}
	if desc.Handler != "webhook" {
		return nil, fmt.Errorf("runtime: dispatcher: unsupported handler %q for tool %q", desc.Handler, in.ToolName)
	}
	if desc.Endpoint == "" {
		return nil, fmt.Errorf("runtime: dispatcher: tool %q has empty endpoint", in.ToolName)
	}
	if desc.SigningSecret == "" {
		return nil, fmt.Errorf("runtime: dispatcher: installation %s has empty signing secret (created outside the engine?)", in.InstallationID)
	}
	return desc, nil
}

// writeDispatchLogStart / writeDispatchLogComplete previously
// lived on *Dispatcher as methods. They were extracted to module-
// level helpers in dispatch_log.go (Devin Review round-7
// ANALYSIS_0003 on PR #127) so transportHooks.Dispatch can reuse
// the same audit-row INSERT/UPDATE path. See dispatch_log.go for
// the implementation. The Dispatcher's previous comment block on
// the in-flight semantics (row inserted before HTTP attempt;
// completed_at left NULL on process crash) is preserved at the
// call site above (line 182-186).
