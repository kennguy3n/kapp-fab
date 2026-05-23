package grpc

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

// UnaryTimeoutInterceptor returns a grpc.UnaryServerInterceptor that
// bounds every unary handler with a wall-clock deadline. The
// deadline only applies when the caller did NOT supply a tighter
// deadline of their own — gRPC clients can already attach a
// per-call deadline via `context.WithTimeout` before dialing or
// `grpc.WithTimeout`; this interceptor exists so callers who do
// not (notably HTTP requests translated by grpc-gateway, which
// arrive with the http.Server's ReadHeaderTimeout-bounded context
// but not a per-request deadline) still get a hard upper bound
// matching the HTTP middleware.Timeout policy.
//
// A timeout <= 0 disables the bound (handler runs with the
// caller's context unchanged). Reserved for test fixtures.
//
// The interceptor is positioned INNERMOST in the unary chain
// (recovery -> logging -> auth -> timeout) so the deadline scopes
// only the handler body, not the auth / session lookups. The auth
// path remains bounded by the caller's context, which is the
// correct behaviour: a slow tenant lookup MUST surface to the
// load balancer as a slow auth response, not as an obscure
// codes.DeadlineExceeded inside the handler.
func UnaryTimeoutInterceptor(timeout time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if timeout <= 0 {
			return handler(ctx, req)
		}
		// If the caller already attached a tighter deadline,
		// respect it. context.WithTimeout honours the parent's
		// existing deadline when it is sooner, so this becomes a
		// no-op for clients that pre-supply their own budget.
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return handler(ctx, req)
	}
}
