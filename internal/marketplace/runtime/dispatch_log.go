package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// dispatchLogStart captures the inputs to writeDispatchLogStart.
// Extracted to a struct so both the agent-tool Dispatcher and the
// lifecycle-hook transportHooks layer use the same call surface for
// audit-row writes (Devin Review round-7 ANALYSIS_0003 on PR #127:
// lifecycle dispatches were not previously written to dispatch_log,
// creating an asymmetry where tool invokes were forensically
// traceable but lifecycle hooks were not).
type dispatchLogStart struct {
	TenantID           uuid.UUID
	InstallationID     uuid.UUID // uuid.Nil → NULL (pre_install before install row exists)
	ExtensionID        uuid.UUID
	ExtensionVersionID uuid.UUID
	Kind               DispatchKind
	Endpoint           string
	RequestID          uuid.UUID
	Attempt            int
	BodySHA256         string
	Signature          string
	StartedAt          time.Time
}

// writeDispatchLogStart inserts the per-attempt audit row before
// the HTTP round-trip. Returns the row's UUID so
// writeDispatchLogComplete can UPDATE it with response/latency/
// error fields once the transport call returns.
//
// installation_id is nullable in the schema (`REFERENCES
// marketplace_extension_installations(id) ON DELETE SET NULL`,
// migration 000069 line 284) so the pre_install dispatch — which
// fires BEFORE the install row exists — can pass uuid.Nil and the
// helper translates it to NULL on the wire.
func writeDispatchLogStart(ctx context.Context, pool *pgxpool.Pool, in dispatchLogStart) (uuid.UUID, error) {
	// nullable handling: a raw uuid.Nil would be inserted as the
	// all-zero UUID and trip the FK to marketplace_extension_
	// installations(id). The schema lets us write NULL instead;
	// pgx serializes a typed-nil *uuid.UUID as SQL NULL.
	var installIDArg interface{}
	if in.InstallationID != uuid.Nil {
		installIDArg = in.InstallationID
	}
	var id uuid.UUID
	err := dbutil.WithTenantTx(ctx, pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO marketplace_dispatch_log
				(tenant_id, installation_id, extension_id, extension_version_id,
				 kind, endpoint, request_id, attempt,
				 request_body_sha256, signature, started_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING id`,
			in.TenantID, installIDArg, in.ExtensionID, in.ExtensionVersionID,
			string(in.Kind), in.Endpoint, in.RequestID, in.Attempt,
			in.BodySHA256, in.Signature, in.StartedAt)
		return row.Scan(&id)
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// writeDispatchLogComplete updates the row inserted by
// writeDispatchLogStart with the response status, latency, and any
// transport-level error from the HTTP attempt.
//
// A nil rowID is a no-op — the in-flight INSERT failed, so there's
// no row to complete; the caller already surfaced the start error.
func writeDispatchLogComplete(ctx context.Context, pool *pgxpool.Pool, tenantID, rowID uuid.UUID, status int, latency time.Duration, sendErr error) error {
	if rowID == uuid.Nil {
		return nil
	}
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
	return dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
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

// logAuditWriteFailure surfaces dispatch_log audit-row write
// failures to slog at WARN level. Both Dispatcher.Invoke and
// transportHooks.Dispatch deliberately do NOT abort the dispatch
// when the audit row write fails (the dispatch outcome is more
// important than the audit record), but the failure must still be
// visible to operators — a silent `_ = err` would leave gaps in
// the dispatch_log with no signal. Devin Review round-7
// ANALYSIS_0002 on PR #127 caught the original silent-discard
// pattern. Operators can grep the structured log on `event=
// "marketplace_dispatch_log_write_failed"` to correlate gaps with
// underlying causes (DB outage, pool exhaustion, RLS misconfig).
func logAuditWriteFailure(ctx context.Context, phase string, requestID uuid.UUID, attempt int, err error) {
	slog.Default().WarnContext(ctx, "marketplace_dispatch_log_write_failed",
		"phase", phase,
		"request_id", requestID,
		"attempt", attempt,
		"err", err.Error(),
	)
}
