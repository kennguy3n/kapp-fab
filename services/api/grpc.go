// Phase A5 wires the in-process gRPC server + grpc-gateway HTTP
// reverse-proxy alongside the existing chi-based REST router. The
// gRPC server stands up on cfg.GRPCAddr (KAPP_GRPC_ADDR) and shares
// every dependency with the HTTP gateway through *apiDeps. The
// gateway dials back into the same listener so HTTP/JSON traffic
// (/api/v2/*) traverses the identical interceptor chain (recovery
// → logging → auth) as native gRPC traffic — no duplicate auth
// middleware, no diverging error mapping.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	gogrpc "google.golang.org/grpc"

	apigrpc "github.com/kennguy3n/kapp-fab/internal/grpc"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// startGRPCServer constructs the gRPC server, starts listening on
// cfg.GRPCAddr, and returns a stop function the caller invokes on
// shutdown. When cfg.GRPCAddr is empty, the function is a no-op
// (returns a non-nil server-handle whose Stop is a no-op too) so
// callers don't need to branch on the config.
//
// The returned listener address is needed so the grpc-gateway can
// dial back; the api binary uses it both for the gateway mount on
// the main router (cfg.GatewayMount) and for the integration test
// helper that pokes the gateway over HTTP.
func startGRPCServer(ctx context.Context, d *apiDeps, logger *slog.Logger) (*grpcRuntime, error) {
	cfg := d.cfg
	if cfg.GRPCAddr == "" {
		logger.Info("grpc: disabled (KAPP_GRPC_ADDR not set)")
		return &grpcRuntime{}, nil
	}

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("grpc: listen %q: %w", cfg.GRPCAddr, err)
	}

	srvCfg := apigrpc.ServerConfig{
		Auth: apigrpc.AuthConfig{
			Signer:        nil, // populated below from authh
			TenantResolve: d.tenantSvc,
			Sessions:      d.sessionStore,
			Logger:        logger,
		},
		Logger:           logger,
		KTypeRegistry:    d.ktypeRegistry,
		EnableReflection: cfg.GRPCReflection,
	}
	if d.authh != nil && d.authh.signer != nil {
		srvCfg.Auth.Signer = d.authh.signer
	}
	if d.authh != nil && d.authh.svc != nil {
		// The SSOService satisfies the AuthServiceBackend
		// interface field-for-field; we cast explicitly so the
		// compiler signals a mismatch if either side drifts.
		srvCfg.AuthSvc = d.authh.svc
	}

	// When the JWT signer is not configured we still stand up the
	// gRPC server, but every interceptor that depends on signer
	// (i.e. the auth interceptor) would NPE on a nil signer. The
	// auth interceptor MUST be wired with a non-nil signer; if
	// signer is nil we refuse to start the server so a partial
	// boot doesn't look healthy at the load balancer. This mirrors
	// the HTTP side's adminChain / tenantChain 503 short-circuit.
	if srvCfg.Auth.Signer == nil {
		_ = lis.Close()
		return nil, errors.New("grpc: signer not configured; set KAPP_JWT_SECRET to enable gRPC")
	}

	srv := apigrpc.NewServer(srvCfg)

	go func() {
		logger.Info("grpc: serving",
			slog.String("addr", cfg.GRPCAddr),
			slog.Bool("reflection", cfg.GRPCReflection),
		)
		if err := srv.Serve(lis); err != nil && !errors.Is(err, gogrpc.ErrServerStopped) {
			logger.Error("grpc: serve failed", slog.String("err", err.Error()))
		}
	}()

	rt := &grpcRuntime{
		srv:      srv,
		listener: lis,
		addr:     lis.Addr().String(),
		logger:   logger,
	}

	// Wire the gateway only when the gRPC server actually came up
	// (the gateway dials the local listener; without a listener
	// there is nothing to dial). Errors here are fatal — a broken
	// gateway would route legitimate /api/v2 traffic to nowhere.
	if cfg.GatewayMount != "" {
		gw, err := apigrpc.NewGateway(ctx, apigrpc.GatewayConfig{
			GRPCEndpoint: rt.addr,
		})
		if err != nil {
			rt.Stop()
			return nil, fmt.Errorf("grpc gateway: %w", err)
		}
		rt.gateway = gw
		rt.gatewayMount = strings.TrimRight(cfg.GatewayMount, "/")
		logger.Info("grpc gateway: mounted",
			slog.String("mount", rt.gatewayMount),
			slog.String("upstream", rt.addr),
		)
	}

	return rt, nil
}

// grpcRuntime is the handle main.go retains for shutdown. The zero
// value is safe to call Stop / MountGateway on — both become no-ops
// when GRPCAddr is empty.
type grpcRuntime struct {
	srv          *gogrpc.Server
	listener     net.Listener
	addr         string
	gateway      http.Handler
	gatewayMount string
	logger       *slog.Logger
}

// Stop performs a graceful gRPC shutdown bounded by the supplied
// deadline (defaults to 5s when called without context). Safe to
// call on a nil receiver / zero-value runtime.
func (g *grpcRuntime) Stop() {
	if g == nil || g.srv == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		g.srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Hard-stop after the grace window so a stuck stream
		// doesn't keep the process alive past SIGTERM. Logged
		// at WARN so operators see it.
		g.logger.Warn("grpc: graceful shutdown timed out; forcing stop")
		g.srv.Stop()
	}
}

// MountGateway plumbs the grpc-gateway HTTP handler onto the
// supplied chi router under the configured prefix. The mount
// happens in registerRoutes (services/api/routes.go) so the
// gateway sits inside the same middleware stack (request id,
// tracing, metrics, recovery) every other HTTP route does.
//
// Caller MUST invoke this AFTER the chi router is built but
// BEFORE the http.Server starts serving — see services/api/routes.go
// for the call site.
func (g *grpcRuntime) MountGateway(r chi.Router) {
	if g == nil || g.gateway == nil || g.gatewayMount == "" {
		return
	}
	// chi.Mount strips the prefix before delegating, so the inner
	// handler sees paths starting at "/" relative to its mount
	// point. grpc-gateway's runtime.ServeMux matches against the
	// rpc's full path declared in the proto annotation
	// (/api/v2/auth/sso), so we need the prefix back on the path
	// before delegation — restorePrefixHandler does exactly that.
	prefix := g.gatewayMount
	r.Mount(prefix, &restorePrefixHandler{prefix: prefix, next: g.gateway})
}

// restorePrefixHandler re-prepends the mount prefix to r.URL.Path
// so grpc-gateway's runtime.ServeMux (which matches the full path
// declared in the proto's google.api.http option) sees the
// expected URL.
type restorePrefixHandler struct {
	prefix string
	next   http.Handler
}

func (h *restorePrefixHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = h.prefix + r.URL.Path
	r2.URL.RawPath = h.prefix + r.URL.RawPath
	h.next.ServeHTTP(w, r2)
}

// Ensure platform.Config is referenced so the build doesn't drop
// the import when KAPP_GRPC_GATEWAY_MOUNT is the only field used
// out of *platform.Config. Compile-time only; no runtime cost.
var _ = (*platform.Config)(nil)
