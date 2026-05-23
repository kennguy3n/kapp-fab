package grpc

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

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
		ctx = injectRequestID(ctx, base, info.FullMethod, false)
		start := time.Now()
		resp, err := handler(ctx, req)
		emitRPCComplete(ctx, info.FullMethod, start, err, false)
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
		ctx := injectRequestID(ss.Context(), base, info.FullMethod, true)
		start := time.Now()
		err := handler(srv, &wrappedServerStream{ServerStream: ss, ctx: ctx})
		emitRPCComplete(ctx, info.FullMethod, start, err, true)
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
// tenant + user attributes off ctx if the auth interceptor stamped
// them; absent attributes are simply omitted (e.g. unauthenticated
// AuthService calls).
func emitRPCComplete(ctx context.Context, method string, start time.Time, err error, streaming bool) {
	logger := platform.LoggerFromContext(ctx)
	code := codes.OK
	if err != nil {
		code = status.Code(err)
	}
	attrs := []any{
		slog.String("code", code.String()),
		slog.Duration("duration", time.Since(start)),
	}
	if t := platform.TenantFromContext(ctx); t != nil {
		attrs = append(attrs, slog.String("tenant_id", t.ID.String()))
	}
	if uid := platform.UserIDFromContext(ctx); uid != uuid.Nil {
		attrs = append(attrs, slog.String("user_id", uid.String()))
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
		logger.Warn("rpc complete", attrs...)
		return
	}
	logger.Info("rpc complete", attrs...)
}
