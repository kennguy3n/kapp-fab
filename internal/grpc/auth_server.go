package grpc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	kappv1 "github.com/kennguy3n/kapp-fab/gen/go/kapp/v1"
	"github.com/kennguy3n/kapp-fab/internal/auth"
)

// AuthServiceBackend is the narrow slice of auth.SSOService the
// AuthService gRPC surface needs. Keeping this as an interface
// (rather than depending on *auth.SSOService directly) lets the
// server test exercise the handler without spinning up a real
// KChat client / pgx pool, and matches the same pattern used by
// auth.TenantResolver / auth.SessionStore.
type AuthServiceBackend interface {
	Exchange(
		ctx context.Context,
		code, redirectURI string,
		preferredTenant uuid.UUID,
		userAgent, ipAddr string,
	) (*auth.ExchangeResult, error)

	Refresh(ctx context.Context, refreshToken string) (*auth.ExchangeResult, error)
}

// authServiceImpl satisfies kappv1.AuthServiceServer by translating
// proto request/response messages to/from the auth.SSOService API.
// Embeds UnimplementedAuthServiceServer for forward-compat per the
// generated code's contract.
type authServiceImpl struct {
	kappv1.UnimplementedAuthServiceServer
	backend AuthServiceBackend
}

// SSO trades a KChat code for an access+refresh pair. Mirrors
// services/api/auth.go's sso() HTTP handler one-to-one: same input
// shape, same dependency call, same error mapping (the HTTP handler
// returns 401 on every Exchange error; we return Unauthenticated).
//
// When the SSO backend is not configured (KAPP_KCHAT_URL unset on
// boot), the HTTP handler returns 503 + "sso not configured". The
// equivalent here is codes.Unavailable, which grpc-gateway maps to
// HTTP 503 — same wire response on both surfaces. We register the
// gRPC service unconditionally (see server.go) so a gateway client
// gets the unified 503 rather than 501/Unimplemented, matching the
// HTTP surface byte-for-byte.
func (s *authServiceImpl) SSO(ctx context.Context, req *kappv1.SSORequest) (*kappv1.SSOResponse, error) {
	if s.backend == nil {
		return nil, status.Error(codes.Unavailable, "sso not configured")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	if req.GetCode() == "" || req.GetRedirectUri() == "" {
		return nil, status.Error(codes.InvalidArgument, "code and redirect_uri required")
	}
	preferred, err := parseOptionalUUID(req.GetPreferredTenant())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "preferred_tenant: %v", err)
	}
	userAgent, ipAddr := callerHints(ctx)
	res, err := s.backend.Exchange(ctx, req.GetCode(), req.GetRedirectUri(), preferred, userAgent, ipAddr)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return &kappv1.SSOResponse{Result: exchangeResultToProto(res)}, nil
}

// Refresh swaps a refresh token for a fresh access token. Same
// translation pattern as SSO — including the "backend not
// configured" branch that mirrors the HTTP 503.
func (s *authServiceImpl) Refresh(ctx context.Context, req *kappv1.RefreshRequest) (*kappv1.RefreshResponse, error) {
	if s.backend == nil {
		return nil, status.Error(codes.Unavailable, "sso not configured")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	if req.GetRefreshToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "refresh_token required")
	}
	res, err := s.backend.Refresh(ctx, req.GetRefreshToken())
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return &kappv1.RefreshResponse{Result: exchangeResultToProto(res)}, nil
}

// parseOptionalUUID returns uuid.Nil for empty input (the proto
// "preferred_tenant omitted" sentinel) and a parsed UUID otherwise.
// Errors carry the offending input so the gRPC client gets a useful
// InvalidArgument message.
func parseOptionalUUID(raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, errors.New("invalid uuid")
	}
	return id, nil
}

// callerHints extracts the best-effort user-agent + IP address from
// the incoming gRPC context. The HTTP equivalent reads r.UserAgent()
// and r.RemoteAddr; gRPC carries user-agent in standard metadata
// (per the gRPC spec) and the peer address via the peer package.
func callerHints(ctx context.Context) (userAgent, ipAddr string) {
	// user-agent: "grpc-go/1.80.0" style strings, set by every
	// google.golang.org/grpc client. May be absent (e.g. cmux'd
	// HTTP/2 clients that didn't advertise themselves).
	ua := requestStringValue(ctx, "user-agent")
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		ipAddr = p.Addr.String()
	}
	return ua, ipAddr
}

// requestStringValue is a small helper that returns the first
// non-empty value for a metadata key on the incoming context, or
// the empty string. Kept out of metadata.go because it's only used
// by the auth service to pull caller hints; bearer extraction and
// request-id propagation use BearerFromMetadata /
// RequestIDFromMetadata which have their own validation rules.
func requestStringValue(ctx context.Context, key string) string {
	md := incomingMetadataKey(ctx, key)
	for _, v := range md {
		if v != "" {
			return v
		}
	}
	return ""
}

// exchangeResultToProto converts auth.ExchangeResult to its proto
// counterpart. The mapping is byte-for-byte equivalent to what the
// HTTP handler returns via writeJSON — UUIDs become canonical 8-4-
// 4-4-12 strings, the int64 expires_in is preserved as-is.
func exchangeResultToProto(res *auth.ExchangeResult) *kappv1.ExchangeResult {
	if res == nil {
		return nil
	}
	out := &kappv1.ExchangeResult{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		User: &kappv1.ResolvedUser{
			Id:              res.User.ID.String(),
			KchatUserId:     res.User.KChatUserID,
			Email:           res.User.Email,
			DisplayName:     res.User.DisplayName,
			IsPlatformAdmin: res.User.IsPlatformAdmin,
		},
		TenantId:  res.TenantID.String(),
		SessionId: res.SessionID.String(),
		ExpiresIn: res.ExpiresIn,
	}
	out.Tenants = make([]*kappv1.TenantRef, len(res.Tenants))
	for i, t := range res.Tenants {
		out.Tenants[i] = &kappv1.TenantRef{
			Id:   t.ID.String(),
			Slug: t.Slug,
			Name: t.Name,
			Role: t.Role,
		}
	}
	return out
}
