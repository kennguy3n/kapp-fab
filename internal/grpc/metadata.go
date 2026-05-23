// Package grpc wires the kapp-fab gRPC server skeleton (Pillar A5).
//
// The server runs as an in-process listener on the api binary (see
// services/api/grpc.go). It speaks the proto contract defined in
// proto/kapp/v1 and shares every business-logic dependency
// (stores, services, JWT signer, tenant resolver) with the HTTP
// gateway via the api binary's apiDeps struct — there is no
// duplicated config wiring.
//
// The interceptor chain mirrors the HTTP middleware chain so that
// any future grpc-gateway translation layer (Pillar A5b) is a
// trivial protocol shim rather than a re-implementation of auth /
// tenant scoping / tracing / logging.
package grpc

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Standard metadata keys consumed by the interceptor chain. gRPC
// metadata keys are case-insensitive by spec; we use lowercase
// throughout the codebase because google.golang.org/grpc normalises
// to lowercase on the wire.
const (
	// MetadataAuthorization carries the bearer token: "Bearer <jwt>".
	// Matches the HTTP `Authorization` header so grpc-gateway can
	// translate one-to-one without rewriting the value.
	MetadataAuthorization = "authorization"

	// MetadataRequestID carries the request id for log correlation.
	// Matches platform.RequestIDHeader on the HTTP side.
	MetadataRequestID = "x-request-id"

	// MetadataHelpdeskInboundToken carries the shared-secret for
	// the inbound-email surface (helpdesk_inbound.proto). See the
	// file-level docstring on that proto for the auth rationale.
	MetadataHelpdeskInboundToken = "x-helpdesk-inbound-token"
)

// ErrMissingBearer is returned by BearerFromMetadata when the
// authorization header is absent. Callers (the auth interceptor in
// particular) translate this into codes.Unauthenticated.
var ErrMissingBearer = errors.New("grpc: missing authorization bearer token")

// ErrMalformedBearer is returned when the authorization header is
// present but does not start with "Bearer ".
var ErrMalformedBearer = errors.New("grpc: authorization header is not a bearer token")

// BearerFromMetadata pulls "authorization: Bearer <tok>" out of the
// incoming gRPC metadata and returns the token portion. The lookup
// is case-insensitive on both the header name (metadata is always
// lowercase on the wire) and the "Bearer " prefix; this matches the
// HTTP Authorization header parsing in services/api/auth so a single
// JWT can be replayed across both surfaces verbatim.
func BearerFromMetadata(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ErrMissingBearer
	}
	values := md.Get(MetadataAuthorization)
	if len(values) == 0 {
		return "", ErrMissingBearer
	}
	// Multiple values are unusual but permitted by the spec; use
	// the first non-empty one. Matches net/http's behaviour for
	// canonical-case headers with multiple entries.
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if len(raw) < len("Bearer ") || !strings.EqualFold(raw[:len("Bearer ")], "Bearer ") {
			return "", ErrMalformedBearer
		}
		tok := strings.TrimSpace(raw[len("Bearer "):])
		if tok == "" {
			return "", ErrMalformedBearer
		}
		return tok, nil
	}
	return "", ErrMissingBearer
}

// RequestIDFromMetadata returns the x-request-id metadata value if
// present (after applying the same sanitisation the HTTP middleware
// applies to incoming X-Request-ID headers), or empty string. Empty
// triggers the logging interceptor to mint a fresh id.
//
// Sanitisation is delegated to platform.SanitizeIncomingRequestID so
// the gRPC and HTTP surfaces enforce identical length + charset
// rules (MaxIncomingRequestIDLen = 128, ASCII printable only). A
// direct gRPC client cannot inject an arbitrarily long or
// specially-crafted value into structured log lines or into the
// outgoing trailer set by the logging interceptor — values that
// fail validation are silently replaced with a freshly minted id,
// matching the HTTP behaviour exactly.
func RequestIDFromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	values := md.Get(MetadataRequestID)
	if len(values) == 0 {
		return ""
	}
	return platform.SanitizeIncomingRequestID(values[0])
}

// incomingMetadataKey returns all values for a metadata key on the
// incoming context, or nil if the key is absent / context has no
// metadata. Internal helper; service handlers normally use the
// typed wrappers (BearerFromMetadata, RequestIDFromMetadata).
func incomingMetadataKey(ctx context.Context, key string) []string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	return md.Get(key)
}
