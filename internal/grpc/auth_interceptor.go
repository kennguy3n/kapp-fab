package grpc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// UnauthenticatedMethods is the set of fully-qualified gRPC method
// names that bypass the auth interceptor. The format matches
// grpc.UnaryServerInfo.FullMethod ("/<package>.<Service>/<Method>").
//
// Currently only the AuthService is exempt — SSO trades a KChat
// code (the trust anchor) for a kapp JWT, and Refresh trades a
// refresh token (also the trust anchor) for a fresh access token.
// Both inputs ARE the credentials so the bearer-token requirement
// would be a chicken-and-egg.
//
// The grpc.health.v1 surface is also exempt: a load balancer's
// health probe should not need a JWT to inquire whether the server
// is up. Standard practice across grpc-go deployments.
var UnauthenticatedMethods = map[string]struct{}{
	"/kapp.v1.AuthService/SSO":     {},
	"/kapp.v1.AuthService/Refresh": {},
	"/grpc.health.v1.Health/Check": {},
	"/grpc.health.v1.Health/Watch": {},
}

// AuthConfig bundles the dependencies the auth interceptor needs.
// Mirrors auth.Middleware on the HTTP side so a future grpc-gateway
// can share the exact same wiring without re-deriving any of these.
type AuthConfig struct {
	Signer        *auth.Signer
	TenantResolve auth.TenantResolver
	Sessions      auth.SessionStore // optional; nil disables session revalidation
	Logger        *slog.Logger      // optional; falls back to slog.Default()
}

// UnaryAuthInterceptor returns a grpc.UnaryServerInterceptor that
// validates the bearer JWT against cfg.Signer, optionally
// re-checks the session row, resolves the tenant, and stamps the
// resulting Claims + tenant + user onto the request context.
//
// Behaviour is intentionally symmetric with auth.Middleware
// (internal/auth/middleware.go) so downstream business logic can
// call platform.TenantFromContext / platform.UserIDFromContext /
// auth.ClaimsFromContext without knowing whether the request
// arrived via HTTP or gRPC.
//
// The recovery-bypass path (platform admin entering via an inactive
// home tenant) is intentionally NOT mirrored here — the recovery
// flow is admin-UI only, served exclusively over HTTP. If a gRPC
// admin surface ever lands, this interceptor will need the same
// bypass logic with the same logging.
func UnaryAuthInterceptor(cfg AuthConfig) grpc.UnaryServerInterceptor {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, exempt := UnauthenticatedMethods[info.FullMethod]; exempt {
			return handler(ctx, req)
		}
		newCtx, err := authenticateContext(ctx, cfg, logger)
		if err != nil {
			return nil, err
		}
		return handler(newCtx, req)
	}
}

// StreamAuthInterceptor mirrors UnaryAuthInterceptor for streaming
// RPCs. The auth check runs once at stream open; per-message
// authorization decisions remain the responsibility of the service
// implementation.
func StreamAuthInterceptor(cfg AuthConfig) grpc.StreamServerInterceptor {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if _, exempt := UnauthenticatedMethods[info.FullMethod]; exempt {
			return handler(srv, ss)
		}
		newCtx, err := authenticateContext(ss.Context(), cfg, logger)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedServerStream{ServerStream: ss, ctx: newCtx})
	}
}

// authenticateContext runs the bearer check + session check +
// tenant resolve and returns the enriched context. Shared between
// the unary and stream interceptors.
func authenticateContext(ctx context.Context, cfg AuthConfig, logger *slog.Logger) (context.Context, error) {
	tok, err := BearerFromMetadata(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	claims, err := cfg.Signer.Verify(tok)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
	if cfg.Sessions != nil && claims.SessionID != uuid.Nil {
		if _, err := cfg.Sessions.Get(ctx, claims.TenantID, claims.SessionID); err != nil {
			if errors.Is(err, auth.ErrSessionNotFound) {
				return nil, status.Error(codes.Unauthenticated, "session revoked")
			}
			logger.Error("grpc auth: session lookup failed",
				slog.String("err", err.Error()),
				slog.String("tenant_id", claims.TenantID.String()),
				slog.String("session_id", claims.SessionID.String()),
			)
			return nil, status.Error(codes.Internal, "session lookup failed")
		}
	}
	t, err := cfg.TenantResolve.Get(ctx, claims.TenantID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "tenant not found")
		}
		logger.Error("grpc auth: tenant lookup failed",
			slog.String("err", err.Error()),
			slog.String("tenant_id", claims.TenantID.String()),
		)
		return nil, status.Error(codes.Internal, "tenant lookup failed")
	}
	if t.Status != tenant.StatusActive && !claims.IsPlatformAdmin {
		// gRPC does NOT mirror the HTTP recovery-bypass: the admin
		// UI runs over HTTP only. A non-admin user logging into a
		// non-active tenant is a hard 403 here, same as HTTP.
		return nil, status.Error(codes.PermissionDenied, "tenant is not active")
	}
	ctx = platform.WithTenant(ctx, t)
	ctx = platform.WithUserID(ctx, claims.UserID)
	ctx = auth.WithClaims(ctx, claims)
	return ctx, nil
}

// wrappedServerStream lets StreamAuthInterceptor swap the context
// the service handler sees, mirroring how the HTTP middleware
// chain replaces r.Context() on the request.
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context { return w.ctx }
