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

// GatewayMountPrefix is the canonical HTTP path prefix the
// grpc-gateway is mounted on. Every `google.api.http` annotation in
// proto/kapp/v1/*.proto starts with this prefix (auth.proto:98,107
// /api/v2/auth/*; ktype.proto:77,87,93 /api/v2/ktypes*; …). The
// constant is the single source of truth that the api binary uses
// to validate KAPP_GRPC_GATEWAY_MOUNT at boot — see
// services/api/grpc.go startGRPCServer().
//
// MAINTAINERS: when a new versioned proto contract (v3, v4, …) is
// introduced, update this constant in lockstep with the new
// `google.api.http` paths. A drift between this value and the
// annotation prefix turns into a silent "every /api/vN request 404s"
// failure at the gateway mount — the operator would see a
// successful boot log but no route would match. The boot-time
// validation refuses to start when the configured mount doesn't
// share this prefix, surfacing the misconfig as an explicit error
// instead.
//
// Validation is intentionally a string-equality check rather than a
// proto-introspection check: grpc-gateway's runtime.ServeMux does
// not expose its registered routes publicly, and parsing the
// generated *_gw.pb.go to extract annotation paths at runtime would
// couple us to codegen internals. A constant kept beside the proto
// file is the practical single source of truth.
const GatewayMountPrefix = "/api/v2"

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
// policy: authorization, x-request-id, x-helpdesk-inbound-token,
// AND any header grpc-gateway's stock DefaultHeaderMatcher already
// passes through (i.e. `Grpc-*` and `User-Agent`/`Authorization`)
// travel across the HTTP→gRPC boundary as metadata. The custom
// matcher must DELEGATE to DefaultHeaderMatcher on unknown headers
// — gwruntime.WithIncomingHeaderMatcher REPLACES the default, it
// does not augment it, so returning `("", false)` for unmatched
// keys would drop every header the gateway normally forwards
// (including grpc-timeout / grpc-encoding which downstream
// observability stacks rely on).
//
// Header keys arrive in canonical (Title-Case) form because Go's
// net/http server canonicalises them on receipt before grpc-gateway
// sees them, so the matcher only needs to handle the canonical
// variants.
func defaultServeMuxOptions() []gwruntime.ServeMuxOption {
	return []gwruntime.ServeMuxOption{
		gwruntime.WithIncomingHeaderMatcher(func(h string) (string, bool) {
			switch h {
			case "X-Request-Id":
				return "x-request-id", true
			case "X-Helpdesk-Inbound-Token":
				return "x-helpdesk-inbound-token", true
			}
			// Delegate to grpc-gateway's stock matcher for every
			// other header so Authorization, Grpc-*, and any other
			// gateway-recognised header still reach the gRPC
			// metadata. This is the documented composition pattern
			// — see runtime.DefaultHeaderMatcher in
			// github.com/grpc-ecosystem/grpc-gateway/v2/runtime.
			return gwruntime.DefaultHeaderMatcher(h)
		}),
	}
}
