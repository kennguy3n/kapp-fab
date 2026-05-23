package grpc

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// rpcAttrs is the shared mutable struct that lets a downstream
// interceptor (auth) report attributes back UP the chain to a
// previously-run interceptor (logging) for inclusion in the
// completion log line.
//
// gRPC's unary interceptor model is strictly forward-flowing: each
// interceptor receives a ctx, optionally derives a new ctx, and
// passes IT to its `handler` argument. Control unwinds back to the
// outer interceptor with whatever ctx the outer one created — the
// inner enrichments (e.g. auth's WithTenant/WithUserID) are never
// visible to the outer interceptor's post-handler code. That is
// why a naive TenantFromContext(ctx) inside emitRPCComplete
// always returns nil even on authenticated RPCs.
//
// The fix is the shared-pointer pattern: logging allocates an
// rpcAttrs, stores a POINTER to it on the ctx it passes forward,
// and the auth interceptor writes to the SAME struct after a
// successful authentication. context.WithValue chaining preserves
// the pointer through every subsequent ctx derivation in the
// chain, so rpcAttrsFromContext returns the same struct whether
// it's called from auth (write) or from emitRPCComplete (read).
//
// The mutex guards against the race between auth (writing) and
// emitRPCComplete (reading) when a server handler panics partway
// through processing — recovery_interceptor.go would unwind
// control back up to emitRPCComplete while the handler goroutine
// could theoretically still be in the middle of an auth-driven
// background goroutine. The lock is uncontended on the happy
// path; the overhead is negligible.
type rpcAttrs struct {
	mu       sync.Mutex
	tenantID uuid.UUID
	userID   uuid.UUID
}

func (a *rpcAttrs) setIdentity(tenantID, userID uuid.UUID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tenantID = tenantID
	a.userID = userID
}

func (a *rpcAttrs) snapshot() (uuid.UUID, uuid.UUID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tenantID, a.userID
}

type rpcAttrsKey struct{}

func withRPCAttrs(ctx context.Context, a *rpcAttrs) context.Context {
	return context.WithValue(ctx, rpcAttrsKey{}, a)
}

// rpcAttrsFromContext returns the per-RPC shared-attrs struct
// allocated by the logging interceptor. Returns nil when the
// caller is not running under the logging interceptor (e.g. unit
// tests that drive a service handler directly without the
// interceptor chain). Callers MUST nil-check before writing.
func rpcAttrsFromContext(ctx context.Context) *rpcAttrs {
	a, _ := ctx.Value(rpcAttrsKey{}).(*rpcAttrs)
	return a
}

// UnaryLoggingInterceptor stamps a request_id + per-request logger
// onto ctx (matching what platform.RequestIDMiddleware does on the
// HTTP side) and emits one log line per RPC: the start is implicit;
// the end carries method, code, duration, and (when authenticated)
// tenant_id and user_id.
//
// If the caller supplied x-request-id metadata we honour it so a
// trace started in a browser request can be followed across the
// grpc-gateway translation. Otherwise we mint a fresh v4 UUID.
//
// MUST run before the auth interceptor so unauthenticated calls
// (AuthService.SSO, AuthService.Refresh) still get a request_id on
// their log lines.
func UnaryLoggingInterceptor(base *slog.Logger) grpc.UnaryServerInterceptor {
	if base == nil {
		base = slog.Default()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		attrs := &rpcAttrs{}
		ctx = withRPCAttrs(ctx, attrs)
		ctx = injectRequestID(ctx, base, info.FullMethod, false)
		start := time.Now()
		resp, err := handler(ctx, req)
		emitRPCComplete(ctx, attrs, info.FullMethod, start, err, false)
		return resp, err
	}
}

// StreamLoggingInterceptor mirrors UnaryLoggingInterceptor for
// streaming RPCs. The single log line emitted on stream close
// includes the total stream lifetime; per-message events stay the
// responsibility of the service handler.
func StreamLoggingInterceptor(base *slog.Logger) grpc.StreamServerInterceptor {
	if base == nil {
		base = slog.Default()
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		attrs := &rpcAttrs{}
		ctx := withRPCAttrs(ss.Context(), attrs)
		ctx = injectRequestID(ctx, base, info.FullMethod, true)
		start := time.Now()
		err := handler(srv, &wrappedServerStream{ServerStream: ss, ctx: ctx})
		emitRPCComplete(ctx, attrs, info.FullMethod, start, err, true)
		return err
	}
}

// injectRequestID populates ctx with the request_id + per-request
// logger. Returns the new context.
func injectRequestID(ctx context.Context, base *slog.Logger, method string, streaming bool) context.Context {
	rid := RequestIDFromMetadata(ctx)
	if rid == "" {
		rid = platform.NewRequestID()
	}
	ctx = platform.WithRequestID(ctx, rid)
	kind := "unary"
	if streaming {
		kind = "stream"
	}
	reqLogger := base.With(
		slog.String("request_id", rid),
		slog.String("method", method),
		slog.String("rpc_kind", kind),
	)
	ctx = platform.WithLogger(ctx, reqLogger)
	// Echo the request_id back to the caller so the SDK can log it
	// alongside the response. Matches the HTTP middleware which
	// sets it on the response header.
	_ = grpc.SetHeader(ctx, metadata.Pairs(MetadataRequestID, rid))
	return ctx
}

// emitRPCComplete writes the single "rpc complete" log line. Pulls
// tenant + user attributes off the shared rpcAttrs struct that the
// auth interceptor populated (see rpcAttrs doc comment for why a
// shared pointer is necessary instead of a TenantFromContext read).
// Absent attributes are simply omitted, which is the expected
// behaviour for unauthenticated AuthService calls AND for failed
// authentication (auth interceptor returns early before writing
// to the struct).
func emitRPCComplete(ctx context.Context, sharedAttrs *rpcAttrs, method string, start time.Time, err error, streaming bool) {
	logger := platform.LoggerFromContext(ctx)
	code := codes.OK
	if err != nil {
		code = status.Code(err)
	}
	attrs := []any{
		slog.String("code", code.String()),
		slog.Duration("duration", time.Since(start)),
	}
	if sharedAttrs != nil {
		tenantID, userID := sharedAttrs.snapshot()
		if tenantID != uuid.Nil {
			attrs = append(attrs, slog.String("tenant_id", tenantID.String()))
		}
		if userID != uuid.Nil {
			attrs = append(attrs, slog.String("user_id", userID.String()))
		}
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
		logger.Warn("rpc complete", attrs...)
		return
	}
	logger.Info("rpc complete", attrs...)
}
