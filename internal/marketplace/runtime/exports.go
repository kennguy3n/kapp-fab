package runtime

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file exposes a narrow public surface of dispatch
// primitives so siblings under internal/marketplace (notably
// internal/marketplace/eventrouter, the B4 router) can reuse
// the same audit-log / transport-error machinery as the in-
// package Dispatcher and transportHooks, without duplicating
// the implementation.
//
// The underlying helpers (writeDispatchLogStart /
// writeDispatchLogComplete / isRetryableTransportError /
// noopEncryptor) remain package-private to discourage
// downstream callers outside internal/marketplace/* from
// importing them — the public layer here is the contract
// other marketplace sub-packages use.

// DispatchLogStart is the exported alias of dispatchLogStart.
// Documented on the unexported type in dispatch_log.go. Kept as
// a struct literal so a future field addition surfaces as a
// compile error in every caller — symmetric with how
// dispatchLogStart is constructed inside the runtime package
// itself.
type DispatchLogStart = dispatchLogStart

// WriteDispatchLogStart is the public entry to the audit-row
// INSERT. Calls through to writeDispatchLogStart. See the
// unexported helper for the contract (nullable installation_id,
// returned row UUID for the subsequent UPDATE, etc.).
func WriteDispatchLogStart(ctx context.Context, pool *pgxpool.Pool, in DispatchLogStart) (uuid.UUID, error) {
	return writeDispatchLogStart(ctx, pool, in)
}

// WriteDispatchLogComplete is the public entry to the audit-row
// UPDATE. Calls through to writeDispatchLogComplete.
func WriteDispatchLogComplete(ctx context.Context, pool *pgxpool.Pool, tenantID, rowID uuid.UUID, status int, latency time.Duration, sendErr error) error {
	return writeDispatchLogComplete(ctx, pool, tenantID, rowID, status, latency, sendErr)
}

// IsRetryableTransportError reports whether a transport-level
// error from Transport.Send should trigger a retry. Wraps the
// in-package isRetryableTransportError so siblings consuming
// transport errors can apply the same retry classification as
// Dispatcher.Invoke and transportHooks.Dispatch.
func IsRetryableTransportError(err error) bool {
	return isRetryableTransportError(err)
}

// NoopEncryptor returns a plaintext-passthrough Encryptor.
// Mirrors the construction NewDispatcher uses internally
// (noopEncryptor{}). Public so eventrouter (and tests) can wire
// the same shim without re-implementing it.
func NoopEncryptor() Encryptor {
	return noopEncryptor{}
}
