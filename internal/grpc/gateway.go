package grpc

import (
	"context"
	"fmt"
	"net/http"

	gwruntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	kappv1 "github.com/kennguy3n/kapp-fab/gen/go/kapp/v1"
)

// GatewayConfig bundles the inputs NewGateway needs. The gateway
// runs as a reverse proxy that translates HTTP/JSON requests
// (matching each rpc's google.api.http annotation) into gRPC
// calls — we deliberately use the "loopback" registration mode
// (RegisterXxxHandlerFromEndpoint, dialing the local gRPC port)
// rather than the in-process mode (RegisterXxxHandlerServer) so
// that gateway-translated requests traverse the SAME interceptor
// chain (recovery → logging → auth) the gRPC server enforces.
// An in-process mount would bypass all interceptors and force a
// duplicate HTTP-side middleware stack — a maintenance footgun.
type GatewayConfig struct {
	// GRPCEndpoint is the address the gateway dials for upstream
	// gRPC calls. In a co-mounted deployment (gateway + server on
	// the same binary) this is the local listener address, e.g.
	// "localhost:9090". In a split deployment the gateway can
	// point at a remote gRPC server.
	GRPCEndpoint string

	// DialOptions lets the caller plug in TLS credentials for the
	// upstream dial. The default (when nil) is insecure dialing
	// — only safe inside a single process; production deployments
	// across a network MUST pass TLS credentials here.
	DialOptions []grpc.DialOption

	// ServeMuxOptions plumbs custom runtime.ServeMux options
	// (header forwarding, error handlers, marshaler overrides,
	// etc.). The default set always carries IncomingHeaderMatcher
	// configured to forward the bearer + request id headers; any
	// caller-supplied options come AFTER the defaults so they can
	// override.
	ServeMuxOptions []gwruntime.ServeMuxOption
}

// NewGateway returns an *http.ServeMux-compatible handler that
// translates HTTP/JSON to gRPC for every kapp.v1 service with HTTP
// annotations. Pass the returned handler to chi's Mux.Mount, or to
// http.Handle directly.
//
// The supplied ctx scopes any background work the gateway runtime
// performs (header propagation, watch-style RPCs); callers should
// pass the same shutdown context the gRPC server uses so a single
// SIGTERM unwinds both surfaces cleanly.
func NewGateway(ctx context.Context, cfg GatewayConfig) (http.Handler, error) {
	if cfg.GRPCEndpoint == "" {
		return nil, fmt.Errorf("grpc gateway: GRPCEndpoint is required")
	}
	mux := gwruntime.NewServeMux(append(defaultServeMuxOptions(), cfg.ServeMuxOptions...)...)
	dialOpts := cfg.DialOptions
	if dialOpts == nil {
		dialOpts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	if err := kappv1.RegisterAuthServiceHandlerFromEndpoint(ctx, mux, cfg.GRPCEndpoint, dialOpts); err != nil {
		return nil, fmt.Errorf("grpc gateway: register auth: %w", err)
	}
	if err := kappv1.RegisterKTypeServiceHandlerFromEndpoint(ctx, mux, cfg.GRPCEndpoint, dialOpts); err != nil {
		return nil, fmt.Errorf("grpc gateway: register ktype: %w", err)
	}
	return mux, nil
}

// defaultServeMuxOptions installs the kapp-fab header forwarding
// policy: authorization + x-request-id always travel across the
// HTTP→gRPC boundary as metadata. grpc-gateway's default already
// translates `Authorization` (the gateway has a built-in case-
// insensitive matcher) but the explicit IncomingHeaderMatcher
// here ALSO forwards x-request-id so a request id minted by a
// caller's HTTP middleware (or browser fetch interceptor) lands
// in the gRPC server's logging interceptor too.
func defaultServeMuxOptions() []gwruntime.ServeMuxOption {
	return []gwruntime.ServeMuxOption{
		gwruntime.WithIncomingHeaderMatcher(func(h string) (string, bool) {
			switch h {
			case "Authorization", "authorization":
				return "authorization", true
			case "X-Request-Id", "x-request-id":
				return "x-request-id", true
			case "X-Helpdesk-Inbound-Token", "x-helpdesk-inbound-token":
				return "x-helpdesk-inbound-token", true
			}
			// Defer to the gateway default (which forwards
			// grpc-prefixed headers verbatim). Returning
			// false-with-empty drops everything else; matches
			// the security posture of the HTTP gateway where
			// only explicitly-allowlisted headers cross the
			// auth boundary.
			return "", false
		}),
	}
}
