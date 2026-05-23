package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	kappv1 "github.com/kennguy3n/kapp-fab/gen/go/kapp/v1"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KTypeBackend is the narrow slice of ktype.Registry the gRPC
// surface needs. Matches the interface defined in internal/ktype
// (ktype.Registry) field-for-field so the api binary can pass its
// existing *ktype.PGRegistry directly.
type KTypeBackend interface {
	Register(ctx context.Context, kt ktype.KType) error
	Get(ctx context.Context, name string, version int) (*ktype.KType, error)
	List(ctx context.Context) ([]ktype.KType, error)
}

// ktypeServiceImpl satisfies kappv1.KTypeServiceServer by
// translating proto messages to/from internal/ktype types. Mirrors
// the services/api/ktypes.go HTTP handler one-to-one: same
// arguments to the registry, same error mapping (ErrNotFound ->
// NotFound, every other error -> Internal except the register
// path, where the upstream returns operator-visible validation
// errors as bad-request).
type ktypeServiceImpl struct {
	kappv1.UnimplementedKTypeServiceServer
	registry KTypeBackend
}

// RegisterKType registers a new KType version. Mirrors POST
// /api/v1/ktypes (services/api/ktypes.go:register).
//
// When the registry backend is not configured, returns
// codes.Unavailable (grpc-gateway maps to HTTP 503). This
// unifies the wire response with the HTTP surface, which would
// return 503 in the same condition; see the matching note on
// authServiceImpl.SSO.
func (s *ktypeServiceImpl) RegisterKType(ctx context.Context, req *kappv1.RegisterKTypeRequest) (*kappv1.RegisterKTypeResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "ktype registry not configured")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}
	if len(req.GetSchema()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "schema required")
	}
	// The HTTP handler accepts a json.RawMessage for the schema; we
	// mirror that contract here. The bytes are validated downstream
	// by the registry (jsonschema parse).
	schema := json.RawMessage(req.GetSchema())
	if err := s.registry.Register(ctx, ktype.KType{
		Name:    req.GetName(),
		Version: int(req.GetVersion()),
		Schema:  schema,
	}); err != nil {
		return nil, mapKTypeError(err, codes.InvalidArgument)
	}
	return &kappv1.RegisterKTypeResponse{
		Name:    req.GetName(),
		Version: req.GetVersion(),
	}, nil
}

// GetKType returns a specific KType row. version=0 means "latest"
// to match the registry's behaviour and the HTTP handler. See
// RegisterKType for the registry-nil rationale.
func (s *ktypeServiceImpl) GetKType(ctx context.Context, req *kappv1.GetKTypeRequest) (*kappv1.GetKTypeResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "ktype registry not configured")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}
	if req.GetVersion() < 0 {
		return nil, status.Error(codes.InvalidArgument, "version must be >= 0")
	}
	kt, err := s.registry.Get(ctx, req.GetName(), int(req.GetVersion()))
	if err != nil {
		return nil, mapKTypeError(err, codes.Internal)
	}
	return &kappv1.GetKTypeResponse{Ktype: ktypeToProto(kt)}, nil
}

// ListKTypes returns every KType registered in the shared, non-
// tenant-scoped `ktypes` table — KTypes are platform metadata
// rather than tenant data (see PGRegistry doc comment), so there
// is no per-tenant filter to apply. The HTTP handler at
// services/api/ktypes.go:list has the same shape; authorisation
// for who can register/list KTypes lives in the auth interceptor
// (currently every authenticated caller can read; mutation is a
// platform-admin operation that flows through the same pool).
func (s *ktypeServiceImpl) ListKTypes(ctx context.Context, _ *kappv1.ListKTypesRequest) (*kappv1.ListKTypesResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "ktype registry not configured")
	}
	kts, err := s.registry.List(ctx)
	if err != nil {
		return nil, mapKTypeError(err, codes.Internal)
	}
	out := &kappv1.ListKTypesResponse{Ktypes: make([]*kappv1.KType, 0, len(kts))}
	for i := range kts {
		out.Ktypes = append(out.Ktypes, ktypeToProto(&kts[i]))
	}
	return out, nil
}

// ktypeToProto converts an internal ktype.KType to its proto
// counterpart. The created_at field is serialised as RFC3339Nano
// to match the HTTP surface's writeJSON output, NOT as a
// timestamppb.Timestamp — common.proto:13-21 documents the
// RFC3339-string convention.
func ktypeToProto(kt *ktype.KType) *kappv1.KType {
	if kt == nil {
		return nil
	}
	return &kappv1.KType{
		Name:      kt.Name,
		Version:   int32(kt.Version),
		Schema:    []byte(kt.Schema),
		CreatedAt: kt.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
}

// mapKTypeError translates internal/ktype error sentinels to gRPC
// status codes. Default code is the caller-supplied fallback so
// the register vs get paths can map a generic error to
// InvalidArgument (operator-visible JSON schema errors) or
// Internal (unexpected DB errors) without re-checking the same
// sentinels.
func mapKTypeError(err error, fallback codes.Code) error {
	switch {
	case errors.Is(err, ktype.ErrNotFound):
		return status.Error(codes.NotFound, "ktype not found")
	default:
		return status.Error(fallback, err.Error())
	}
}

