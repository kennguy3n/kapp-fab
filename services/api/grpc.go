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
//
// The supplied ctx parameter is reserved for future use (e.g.
// propagating cancellation into the Serve goroutine or threading
// it through a deps refresh). It is intentionally NOT used for
// the gateway dial context: grpc-gateway tears down its upstream
// client connection the moment its dial-ctx fires Done, which on
// SIGTERM would happen BEFORE the HTTP server has finished
// draining in-flight gateway-translated requests. That produces
// transient 5xx errors at the gateway surface during rolling
// restarts. We avoid the race by giving the gateway its own
// cancel function and only firing it AFTER http.Server.Shutdown
// completes — see grpcRuntime.Stop and main.go's defer order.
func startGRPCServer(_ context.Context, d *apiDeps, logger *slog.Logger) (*grpcRuntime, error) {
	cfg := d.cfg
	if cfg.GRPCAddr == "" {
		// Refuse to silently no-op when the operator clearly *wanted*
		// the gateway to be reachable but forgot to enable the gRPC
		// listener it dials. Without this guard a misconfigured
		// deployment would 404 every /api/v2 request with no signal
		// other than an info-level "disabled" log; the symptom looks
		// like a missing handler rather than a config mistake.
		if cfg.GatewayMount != "" {
			return nil, fmt.Errorf(
				"grpc: KAPP_GRPC_GATEWAY_MOUNT=%q requires KAPP_GRPC_ADDR to be set (the gateway dials the gRPC listener)",
				cfg.GatewayMount,
			)
		}
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

	srv, err := apigrpc.NewServer(srvCfg)
	if err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("grpc: build server: %w", err)
	}

	go func() {
		// Log the listener's bound address rather than the configured
		// string so the operator sees the actual port when GRPCAddr
		// is e.g. ":0" (ephemeral port, common in tests + containerised
		// deployments). The configured value remains visible upstream
		// in the boot manifest.
		logger.Info("grpc: serving",
			slog.String("addr", lis.Addr().String()),
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
		// Validate the trimmed mount before allocating the gateway
		// handler. `strings.TrimRight("/", "/")` returns "", and an
		// empty `rt.gatewayMount` would later cause `MountGateway` to
		// silently no-op while `startGRPCServer` still logs
		// "grpc gateway: mounted" — the operator would see a success
		// log but every /api/v2 path would 404. Fail loudly at boot
		// instead. (Mounting at literal "/" is also not what the
		// operator wants here: chi.Router.Mount("/", ...) would
		// capture EVERY HTTP path and shadow every other chi route.
		// The gateway is meant to be a sub-router behind a versioned
		// prefix like /api/v2.)
		mount := strings.TrimRight(cfg.GatewayMount, "/")
		if mount == "" {
			rt.Stop()
			return nil, fmt.Errorf(
				"grpc gateway: KAPP_GRPC_GATEWAY_MOUNT=%q resolves to empty path after trimming trailing slashes; use a non-root prefix like /api/v2",
				cfg.GatewayMount,
			)
		}
		// Pin the mount prefix to the value every proto
		// google.api.http annotation shares (apigrpc.GatewayMountPrefix).
		// Without this guard, an operator setting
		// KAPP_GRPC_GATEWAY_MOUNT=/api/v3 (or any value that doesn't
		// match the proto annotation prefix) would see a successful
		// boot log but every /api/v3 request would 404 silently,
		// because chi delivers the full URL.Path to the gateway and
		// the gateway's runtime.ServeMux only matches the literal
		// paths declared in each rpc's google.api.http option. The
		// existing trim+empty+root validation catches the most
		// common typos; this check catches the "wrong version" /
		// "drifted from proto" class.
		if mount != apigrpc.GatewayMountPrefix {
			rt.Stop()
			return nil, fmt.Errorf(
				"grpc gateway: KAPP_GRPC_GATEWAY_MOUNT=%q does not match the proto annotation prefix %q; "+
					"every google.api.http path in proto/kapp/v1/*.proto starts with %q, so any other mount value "+
					"would 404 every gateway request -- update KAPP_GRPC_GATEWAY_MOUNT (or, if introducing a new "+
					"proto version, update apigrpc.GatewayMountPrefix in internal/grpc/gateway.go to match)",
				cfg.GatewayMount, apigrpc.GatewayMountPrefix, apigrpc.GatewayMountPrefix,
			)
		}
		// Decouple the gateway's dial-ctx from the SIGTERM ctx so
		// http.Server.Shutdown can drain in-flight gateway-translated
		// requests without the gateway's upstream gRPC connection being
		// torn down mid-flight. See the doc comment on this function
		// and grpcRuntime.Stop for the full lifecycle contract.
		gatewayCtx, gatewayCancel := context.WithCancel(context.Background())
		gw, err := apigrpc.NewGateway(gatewayCtx, apigrpc.GatewayConfig{
			GRPCEndpoint: rt.addr,
		})
		if err != nil {
			gatewayCancel()
			rt.Stop()
			return nil, fmt.Errorf("grpc gateway: %w", err)
		}
		rt.gateway = gw
		rt.gatewayMount = mount
		rt.gatewayCancel = gatewayCancel
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
	// gatewayCancel cancels the context the grpc-gateway dial
	// goroutine watches. It is intentionally separate from the
	// SIGTERM ctx so the gateway's upstream gRPC connection stays
	// up until AFTER http.Server.Shutdown drains in-flight
	// gateway-translated requests. Nil when the gateway was not
	// wired (KAPP_GRPC_GATEWAY_MOUNT empty or KAPP_GRPC_ADDR empty).
	gatewayCancel context.CancelFunc
}

// Stop performs a graceful gRPC shutdown bounded by a 5s deadline.
// Safe to call on a nil receiver / zero-value runtime.
//
// The shutdown order is deliberate: (1) close the gateway's
// upstream dial-ctx so the gateway stops accepting NEW upstream
// calls, (2) GracefulStop the gRPC server which drains in-flight
// RPCs (including any final gateway-translated calls already in
// flight when http.Server.Shutdown returned). Caller (main.go)
// MUST invoke this AFTER http.Server.Shutdown completes — doing
// it earlier would tear down the upstream conn mid-request and
// produce transient 5xx at the gateway surface during rolling
// restarts. See startGRPCServer's doc comment for the rationale.
func (g *grpcRuntime) Stop() {
	if g == nil {
		return
	}
	// Always cancel the gateway dial context, even on the
	// no-server / no-gateway zero-value paths — it's a no-op when
	// the context wasn't constructed. Without this the
	// context.WithCancel allocation would leak its goroutine if
	// gateway construction succeeded but the caller hit an early
	// shutdown path that bypasses gRPC drain (test cleanup, etc.).
	if g.gatewayCancel != nil {
		g.gatewayCancel()
	}
	if g.srv == nil {
		return
	}
	// Fall back to slog.Default() when the runtime was constructed
	// without a logger. startGRPCServer always sets g.logger, but a
	// future refactor that splits construction from startup could
	// hit the timeout branch with a nil logger and panic; the cost
	// of a defensive nil-check here is one line vs. a panic during
	// SIGTERM handling, which is the worst time to hit one.
	logger := g.logger
	if logger == nil {
		logger = slog.Default()
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
		logger.Warn("grpc: graceful shutdown timed out; forcing stop")
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
	// chi.Mount preserves r.URL.Path verbatim — it does NOT strip
	// the mount prefix the way net/http.ServeMux does. Only chi's
	// internal chi.RouteContext.RoutePath gets stripped, which is
	// not what grpc-gateway looks at. grpc-gateway's runtime.ServeMux
	// matches r.URL.Path against the full path declared in each
	// rpc's google.api.http option (e.g. "/api/v2/auth/sso"), so
	// mounting the gateway directly is correct: the inner handler
	// sees the full original path and matches the rpc route.
	r.Mount(g.gatewayMount, g.gateway)
}

// Ensure platform.Config is referenced so the build doesn't drop
// the import when KAPP_GRPC_GATEWAY_MOUNT is the only field used
// out of *platform.Config. Compile-time only; no runtime cost.
var _ = (*platform.Config)(nil)
