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
		logRowID, err := d.writeDispatchLogStart(ctx, in.TenantID, in.InstallationID, desc, requestID, attempt, dispatchReq.Kind, bodyHash, signature, ts)
		if err != nil {
			return nil, fmt.Errorf("runtime: invoke: open log: %w", err)
		}

		started := time.Now()
		resp, sendErr := d.transport.Send(ctx, desc.Endpoint, in.Body, headers, desc.Timeout)
		latency := time.Since(started)

		if sendErr != nil {
			lastSendErr = sendErr
			if logErr := d.writeDispatchLogComplete(ctx, in.TenantID, logRowID, 0, latency, sendErr); logErr != nil {
				// Audit log write failure is logged but does not
				// abort the dispatch — the response is still
				// surfaced to the caller. Operators see the gap
				// via the row's NULL completed_at.
				_ = logErr
			}
			if !isRetryableTransportError(sendErr) || attempt == retry.MaxAttempts {
				if errors.Is(sendErr, ErrDispatchTimeout) {
					return nil, fmt.Errorf("%w: tool %q after %d attempts", ErrDispatchTimeout, in.ToolName, attempt)
				}
				return nil, fmt.Errorf("runtime: invoke: tool %q: %w", in.ToolName, sendErr)
			}
			continue
		}
		if logErr := d.writeDispatchLogComplete(ctx, in.TenantID, logRowID, resp.Status, latency, nil); logErr != nil {
			_ = logErr
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
	// Exhausted retries without ever getting an HTTP round-trip.
	if lastSendErr != nil {
		return nil, fmt.Errorf("runtime: invoke: tool %q exhausted retries: %w", in.ToolName, lastSendErr)
	}
	return result, nil
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

// writeDispatchLogStart inserts the per-attempt row before the HTTP
// round-trip. Returns the row's UUID so writeDispatchLogComplete
// can update it.
func (d *Dispatcher) writeDispatchLogStart(ctx context.Context, tenantID, installID uuid.UUID, desc *agentToolDescriptor, requestID uuid.UUID, attempt int, kind DispatchKind, bodyHash, signature string, startedAt time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	err := dbutil.WithTenantTx(ctx, d.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO marketplace_dispatch_log
				(tenant_id, installation_id, extension_id, extension_version_id,
				 kind, endpoint, request_id, attempt,
				 request_body_sha256, signature, started_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING id`,
			tenantID, installID, desc.ExtensionID, desc.ExtensionVersionID,
			string(kind), desc.Endpoint, requestID, attempt,
			bodyHash, signature, startedAt)
		return row.Scan(&id)
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// writeDispatchLogComplete updates the row inserted by
// writeDispatchLogStart with the response status, latency, and any
// transport-level error.
func (d *Dispatcher) writeDispatchLogComplete(ctx context.Context, tenantID, rowID uuid.UUID, status int, latency time.Duration, sendErr error) error {
	var (
		statusPtr  *int
		latencyPtr *int
		errorPtr   *string
	)
	if status > 0 {
		statusPtr = &status
	}
	if latency > 0 {
		ms := int(latency / time.Millisecond)
		latencyPtr = &ms
	}
	if sendErr != nil {
		s := sendErr.Error()
		errorPtr = &s
	}
	return dbutil.WithTenantTx(ctx, d.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE marketplace_dispatch_log
			   SET response_status = $2,
			       response_latency_ms = $3,
			       error = $4,
			       completed_at = now()
			 WHERE tenant_id = $1
			   AND id = $5`,
			tenantID, statusPtr, latencyPtr, errorPtr, rowID)
		return err
	})
}
