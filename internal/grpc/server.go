package grpc

import (
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	kappv1 "github.com/kennguy3n/kapp-fab/gen/go/kapp/v1"
)

// DefaultUnaryTimeout bounds how long a unary RPC handler may run
// when the caller did not supply its own context deadline. Mirrors
// chi's middleware.Timeout(30 * time.Second) on the HTTP side so
// the same wall-clock budget applies whether the call arrived as
// native gRPC, gateway-translated HTTP, or REST. Streaming RPCs
// are exempt — `events/stream` and friends rely on an unbounded
// WriteTimeout, same as their HTTP counterparts.
const DefaultUnaryTimeout = 30 * time.Second

// ServerConfig bundles every dependency the gRPC server needs. The
// api binary's apiDeps populates this from the same sources that
// feed the HTTP gateway (signer, tenant resolver, session store,
// service singletons) so there is exactly one source of truth per
// dependency in the process.
type ServerConfig struct {
	// Auth gates every RPC except those in UnauthenticatedMethods.
	Auth AuthConfig

	// AuthSvc is the business-logic service the AuthService gRPC
	// surface wraps. Nil disables the AuthService registration so
	// the api binary can boot without SSO configured (matches the
	// 503 fall-through on the HTTP side).
	AuthSvc AuthServiceBackend

	// KTypeRegistry is the schema registry the KTypeService gRPC
	// surface wraps. Nil disables KTypeService registration. The
	// api binary always wires this on boot, so a nil here is a
	// configuration mistake — but it's a soft fail (server still
	// boots) rather than a hard panic so a partial outage on the
	// registry doesn't take the entire gRPC port down.
	KTypeRegistry KTypeBackend

	// Logger is the base logger interceptors derive request-scoped
	// children from. Nil falls back to slog.Default().
	Logger *slog.Logger

	// EnableReflection turns on the grpc.reflection.v1alpha service
	// so `grpcurl` and other off-the-shelf tooling can list and
	// invoke RPCs against the running server. Defaults off in
	// production deployments.
	EnableReflection bool

	// UnaryTimeout is the maximum wall-clock duration a unary RPC
	// may run before its context is cancelled. Zero falls back to
	// DefaultUnaryTimeout. Set to a negative value to disable the
	// timeout entirely (only sensible for tests). The deadline is
	// applied AFTER the auth interceptor so an unauthenticated
	// caller cannot tie up an RPC handler indefinitely.
	UnaryTimeout time.Duration

	// AdditionalOptions lets the caller plug in extra grpc.ServerOption
	// values (TLS credentials, custom keepalive policy, larger
	// max-message-size for the file-upload streaming RPC, etc.)
	// without having to fork NewServer.
	AdditionalOptions []grpc.ServerOption
}

// NewServer constructs a *grpc.Server with the standard interceptor
// chain (recovery -> logging -> auth -> timeout) plus reflection /
// health and every kapp.v1 service implementation we have a backend
// for.
//
// Interceptor ordering matters: recovery is OUTERMOST so it catches
// panics from any later interceptor; logging runs BEFORE auth so
// even unauthenticated calls (AuthService.SSO / Refresh) get a
// request_id stamped on their log lines; the timeout is INNERMOST
// in the unary chain so the deadline scopes only the handler body,
// not the auth/session lookups (which are themselves bounded by
// the caller's context).
func NewServer(cfg ServerConfig) *grpc.Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	timeout := cfg.UnaryTimeout
	if timeout == 0 {
		timeout = DefaultUnaryTimeout
	}

	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			UnaryRecoveryInterceptor(logger),
			UnaryLoggingInterceptor(logger),
			UnaryAuthInterceptor(cfg.Auth),
			UnaryTimeoutInterceptor(timeout),
		),
		grpc.ChainStreamInterceptor(
			StreamRecoveryInterceptor(logger),
			StreamLoggingInterceptor(logger),
			StreamAuthInterceptor(cfg.Auth),
		),
	}
	opts = append(opts, cfg.AdditionalOptions...)

	srv := grpc.NewServer(opts...)

	// grpc.health.v1 — the standard liveness probe. We register the
	// upstream package's health.NewServer so callers can switch
	// service status to NotServing during graceful shutdown without
	// implementing the protocol ourselves.
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(srv, healthSrv)
	// Always advertise overall serving status. Service-specific
	// health is added per-service below.
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	if cfg.AuthSvc != nil {
		kappv1.RegisterAuthServiceServer(srv, &authServiceImpl{backend: cfg.AuthSvc})
		healthSrv.SetServingStatus("kapp.v1.AuthService", healthpb.HealthCheckResponse_SERVING)
	}

	if cfg.KTypeRegistry != nil {
		kappv1.RegisterKTypeServiceServer(srv, &ktypeServiceImpl{registry: cfg.KTypeRegistry})
		healthSrv.SetServingStatus("kapp.v1.KTypeService", healthpb.HealthCheckResponse_SERVING)
	}

	if cfg.EnableReflection {
		reflection.Register(srv)
	}

	return srv
}
