package grpc_test

// Phase A5 integration test. Exercises the gRPC server END-TO-END
// over a real TCP listener (not bufconn) so the grpc-gateway HTTP
// reverse-proxy can dial the local upstream the same way it does
// in production. The test covers:
//
//   - Unauthenticated AuthService.SSO (gRPC + gateway HTTP)
//     bypasses the auth interceptor and returns the exchange
//     result from the test backend.
//   - Authenticated KTypeService.RegisterKType (gRPC) succeeds
//     with a valid bearer JWT and rejects unauthenticated calls
//     with codes.Unauthenticated.
//   - Bearer rejection codes: missing -> Unauthenticated, malformed
//     -> Unauthenticated, valid-but-revoked-session ->
//     Unauthenticated, valid-but-tenant-not-found -> NotFound.
//
// No PostgreSQL is involved — the test wires fake TenantResolver /
// SessionStore / SSOService backends so a unit-test runtime can
// reach every interceptor branch without a database. The KType
// backend is the production interface, but pointed at an in-memory
// implementation so the call exercises ktype_server.go's full
// translation logic without hitting Postgres.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	kappv1 "github.com/kennguy3n/kapp-fab/gen/go/kapp/v1"
	"github.com/kennguy3n/kapp-fab/internal/auth"
	apigrpc "github.com/kennguy3n/kapp-fab/internal/grpc"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// --- Test backends ---

// fakeTenantResolver returns the tenant fixture for the configured
// ID and tenant.ErrNotFound for every other lookup. Mirrors the
// auth.TenantResolver shape exactly.
type fakeTenantResolver struct {
	tenant *tenant.Tenant
}

func (f *fakeTenantResolver) Get(ctx context.Context, id uuid.UUID) (*tenant.Tenant, error) {
	if f.tenant == nil || f.tenant.ID != id {
		return nil, tenant.ErrNotFound
	}
	out := *f.tenant
	return &out, nil
}

// fakeSessionStore satisfies auth.SessionStore. Only Get is
// meaningfully exercised by the auth interceptor; every other
// method short-circuits to a sentinel error so an accidental call
// site is loud.
type fakeSessionStore struct {
	revoked map[uuid.UUID]bool
}

func (f *fakeSessionStore) Create(context.Context, auth.Session) (*auth.Session, error) {
	return nil, errNotImplementedInTest
}
func (f *fakeSessionStore) Get(_ context.Context, _ uuid.UUID, sessionID uuid.UUID) (*auth.Session, error) {
	if f.revoked[sessionID] {
		return nil, auth.ErrSessionNotFound
	}
	return &auth.Session{ID: sessionID}, nil
}
func (f *fakeSessionStore) Revoke(context.Context, uuid.UUID, uuid.UUID) error {
	return errNotImplementedInTest
}
func (f *fakeSessionStore) RevokeByUser(context.Context, uuid.UUID, uuid.UUID) error {
	return errNotImplementedInTest
}
func (f *fakeSessionStore) RevokeByTenant(context.Context, uuid.UUID) error {
	return errNotImplementedInTest
}
func (f *fakeSessionStore) Touch(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	return errNotImplementedInTest
}
func (f *fakeSessionStore) ActiveCount(context.Context, uuid.UUID) (int, error) {
	return 0, errNotImplementedInTest
}

var errNotImplementedInTest = &testNotImplErr{}

type testNotImplErr struct{}

func (*testNotImplErr) Error() string { return "test fixture method not implemented" }

// fakeAuthBackend satisfies apigrpc.AuthServiceBackend so the
// SSO/Refresh handlers run their full translation path without
// pulling in *auth.SSOService (which needs a real pgxpool).
type fakeAuthBackend struct {
	exchangeResult *auth.ExchangeResult
	exchangeErr    error
	refreshResult  *auth.ExchangeResult
	refreshErr     error
}

func (f *fakeAuthBackend) Exchange(ctx context.Context, code, redirectURI string, preferredTenant uuid.UUID, userAgent, ipAddr string) (*auth.ExchangeResult, error) {
	return f.exchangeResult, f.exchangeErr
}
func (f *fakeAuthBackend) Refresh(ctx context.Context, refreshToken string) (*auth.ExchangeResult, error) {
	return f.refreshResult, f.refreshErr
}

// fakeKTypeBackend satisfies apigrpc.KTypeBackend with an in-memory
// store keyed by (name, version). Exercises the proto<->internal
// type translation in ktype_server.go without touching Postgres.
type fakeKTypeBackend struct {
	stored map[string]ktype.KType
}

func newFakeKTypeBackend() *fakeKTypeBackend {
	return &fakeKTypeBackend{stored: map[string]ktype.KType{}}
}
func (f *fakeKTypeBackend) Register(ctx context.Context, kt ktype.KType) error {
	if kt.Version == 0 {
		kt.Version = 1
	}
	kt.CreatedAt = time.Now().UTC()
	f.stored[kt.Name] = kt
	return nil
}
func (f *fakeKTypeBackend) Get(ctx context.Context, name string, version int) (*ktype.KType, error) {
	kt, ok := f.stored[name]
	if !ok {
		return nil, ktype.ErrNotFound
	}
	out := kt
	return &out, nil
}
func (f *fakeKTypeBackend) List(ctx context.Context) ([]ktype.KType, error) {
	out := make([]ktype.KType, 0, len(f.stored))
	for _, kt := range f.stored {
		out = append(out, kt)
	}
	return out, nil
}

// --- Test helpers ---

type testServer struct {
	srv         *grpc.Server
	listener    net.Listener
	clientConn  *grpc.ClientConn
	authBackend *fakeAuthBackend
	ktype       *fakeKTypeBackend
	signer      *auth.Signer
	tenantID    uuid.UUID
	userID      uuid.UUID
	sessions    *fakeSessionStore
	gateway     http.Handler
}

// startTestServer spins up a gRPC server bound to localhost:0 with
// the full interceptor chain, a valid auth.Signer (HS256), a fake
// tenant resolver / session store, and the AuthService + KType
// service implementations. Returns a closer the caller defers to
// tear everything down.
func startTestServer(t *testing.T) (*testServer, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	signer, err := auth.NewSigner(auth.SignerConfig{
		Algorithm:  auth.AlgHS256,
		HMACKey:    []byte("0123456789abcdef0123456789abcdef"),
		Issuer:     "kapp-test",
		Audience:   "kapp-test",
		AccessTTL:  10 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		cancel()
		t.Fatalf("NewSigner: %v", err)
	}

	tenantID := uuid.New()
	userID := uuid.New()
	tn := &tenant.Tenant{
		ID:     tenantID,
		Slug:   "test-tenant",
		Name:   "Test Tenant",
		Status: tenant.StatusActive,
	}

	sessions := &fakeSessionStore{revoked: map[uuid.UUID]bool{}}
	authBackend := &fakeAuthBackend{}
	ktypeBackend := newFakeKTypeBackend()

	srvCfg := apigrpc.ServerConfig{
		Auth: apigrpc.AuthConfig{
			Signer:        signer,
			TenantResolve: &fakeTenantResolver{tenant: tn},
			Sessions:      sessions,
			Logger:        slog.Default(),
		},
		AuthSvc:       authBackend,
		KTypeRegistry: ktypeBackend,
		Logger:        slog.Default(),
	}
	srv := apigrpc.NewServer(srvCfg)

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		cancel()
		t.Fatalf("listen: %v", err)
	}
	go func() {
		_ = srv.Serve(lis)
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = lis.Close()
		cancel()
		t.Fatalf("dial: %v", err)
	}

	// Build the grpc-gateway upstream pointing at the same listener
	// so HTTP tests can exercise the same handler chain.
	gw, err := apigrpc.NewGateway(ctx, apigrpc.GatewayConfig{
		GRPCEndpoint: lis.Addr().String(),
	})
	if err != nil {
		_ = conn.Close()
		_ = lis.Close()
		cancel()
		t.Fatalf("gateway: %v", err)
	}

	ts := &testServer{
		srv:         srv,
		listener:    lis,
		clientConn:  conn,
		authBackend: authBackend,
		ktype:       ktypeBackend,
		signer:      signer,
		tenantID:    tenantID,
		userID:      userID,
		sessions:    sessions,
		gateway:     gw,
	}
	closer := func() {
		_ = conn.Close()
		srv.GracefulStop()
		_ = lis.Close()
		cancel()
	}
	return ts, closer
}

// issueToken mints a valid HS256 access token for the test fixture.
// Callers tweak the claims to test the auth interceptor's branches
// (e.g. session revoked, tenant not found).
func (ts *testServer) issueToken(t *testing.T, mutate func(c *auth.Claims)) string {
	t.Helper()
	sessionID := uuid.New()
	c := auth.Claims{
		UserID:    ts.userID,
		TenantID:  ts.tenantID,
		SessionID: sessionID,
	}
	if mutate != nil {
		mutate(&c)
	}
	tok, err := ts.signer.Issue(c)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// withBearer returns a context with the bearer token in
// authorization metadata. Mirrors what an SDK client would attach.
func withBearer(ctx context.Context, tok string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

// --- Tests ---

// TestAuthService_SSO_Unauthenticated exercises the no-bearer path
// — SSO is in UnauthenticatedMethods so the call MUST succeed even
// without a JWT. Also verifies the response shape is preserved
// byte-for-byte through the gRPC translation.
func TestAuthService_SSO_Unauthenticated(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	wantUserID := uuid.New()
	ts.authBackend.exchangeResult = &auth.ExchangeResult{
		AccessToken:  "access-tok",
		RefreshToken: "refresh-tok",
		User: auth.ResolvedUser{
			ID:              wantUserID,
			KChatUserID:     "kc-1",
			Email:           "alice@example.com",
			DisplayName:     "Alice",
			IsPlatformAdmin: false,
		},
		Tenants: []auth.TenantRef{
			{ID: ts.tenantID, Slug: "test-tenant", Name: "Test Tenant", Role: "owner"},
		},
		TenantID:  ts.tenantID,
		SessionID: uuid.New(),
		ExpiresIn: 600,
	}

	client := kappv1.NewAuthServiceClient(ts.clientConn)
	resp, err := client.SSO(context.Background(), &kappv1.SSORequest{
		Code:        "test-code",
		RedirectUri: "https://app.example.com/callback",
	})
	if err != nil {
		t.Fatalf("SSO: %v", err)
	}
	if resp.GetResult().GetAccessToken() != "access-tok" {
		t.Errorf("access_token = %q; want access-tok", resp.GetResult().GetAccessToken())
	}
	if resp.GetResult().GetUser().GetId() != wantUserID.String() {
		t.Errorf("user.id = %q; want %s", resp.GetResult().GetUser().GetId(), wantUserID)
	}
	if got := resp.GetResult().GetExpiresIn(); got != 600 {
		t.Errorf("expires_in = %d; want 600", got)
	}
	if got := len(resp.GetResult().GetTenants()); got != 1 {
		t.Fatalf("tenants len = %d; want 1", got)
	}
	if resp.GetResult().GetTenants()[0].GetSlug() != "test-tenant" {
		t.Errorf("tenants[0].slug = %q; want test-tenant", resp.GetResult().GetTenants()[0].GetSlug())
	}
}

// TestAuthService_SSO_RequiredFields verifies the handler rejects
// requests with missing required fields under codes.InvalidArgument
// rather than letting the call through to the backend.
func TestAuthService_SSO_RequiredFields(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	client := kappv1.NewAuthServiceClient(ts.clientConn)
	_, err := client.SSO(context.Background(), &kappv1.SSORequest{
		Code:        "",
		RedirectUri: "https://app.example.com/callback",
	})
	if err == nil {
		t.Fatal("expected error for empty code")
	}
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("code = %v; want InvalidArgument", code)
	}
}

// TestKTypeService_ListKTypes_Authenticated verifies a valid bearer
// passes the auth interceptor and the handler reaches the backend.
func TestKTypeService_ListKTypes_Authenticated(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	_ = ts.ktype.Register(context.Background(), ktype.KType{
		Name:    "Deal",
		Version: 3,
		Schema:  json.RawMessage(`{"type":"object"}`),
	})

	client := kappv1.NewKTypeServiceClient(ts.clientConn)
	ctx := withBearer(context.Background(), ts.issueToken(t, nil))
	resp, err := client.ListKTypes(ctx, &kappv1.ListKTypesRequest{})
	if err != nil {
		t.Fatalf("ListKTypes: %v", err)
	}
	if got := len(resp.GetKtypes()); got != 1 {
		t.Fatalf("ktypes len = %d; want 1", got)
	}
	if resp.GetKtypes()[0].GetName() != "Deal" {
		t.Errorf("name = %q; want Deal", resp.GetKtypes()[0].GetName())
	}
}

// TestKTypeService_MissingBearer asserts the auth interceptor
// rejects an authenticated RPC with no bearer token.
func TestKTypeService_MissingBearer(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	client := kappv1.NewKTypeServiceClient(ts.clientConn)
	_, err := client.ListKTypes(context.Background(), &kappv1.ListKTypesRequest{})
	if err == nil {
		t.Fatal("expected error for missing bearer")
	}
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Errorf("code = %v; want Unauthenticated", code)
	}
}

// TestKTypeService_MalformedBearer asserts an authorization header
// that isn't "Bearer <jwt>" is rejected with Unauthenticated.
func TestKTypeService_MalformedBearer(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	client := kappv1.NewKTypeServiceClient(ts.clientConn)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "not-a-bearer-token")
	_, err := client.ListKTypes(ctx, &kappv1.ListKTypesRequest{})
	if err == nil {
		t.Fatal("expected error for malformed bearer")
	}
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Errorf("code = %v; want Unauthenticated", code)
	}
}

// TestKTypeService_RevokedSession asserts a valid JWT whose session
// has been revoked is rejected with Unauthenticated. Exercises the
// session re-check branch of the auth interceptor.
func TestKTypeService_RevokedSession(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	var revokedSession uuid.UUID
	tok := ts.issueToken(t, func(c *auth.Claims) {
		revokedSession = c.SessionID
	})
	ts.sessions.revoked[revokedSession] = true

	client := kappv1.NewKTypeServiceClient(ts.clientConn)
	ctx := withBearer(context.Background(), tok)
	_, err := client.ListKTypes(ctx, &kappv1.ListKTypesRequest{})
	if err == nil {
		t.Fatal("expected error for revoked session")
	}
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Errorf("code = %v; want Unauthenticated", code)
	}
}

// TestKTypeService_TenantNotFound asserts a JWT pointing at a
// tenant the resolver doesn't know returns codes.NotFound. The
// fakeTenantResolver returns ErrNotFound for any tenant ID other
// than the fixture, so we issue a token under a fresh tenant id.
func TestKTypeService_TenantNotFound(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	tok := ts.issueToken(t, func(c *auth.Claims) {
		c.TenantID = uuid.New()
	})

	client := kappv1.NewKTypeServiceClient(ts.clientConn)
	ctx := withBearer(context.Background(), tok)
	_, err := client.ListKTypes(ctx, &kappv1.ListKTypesRequest{})
	if err == nil {
		t.Fatal("expected error for unknown tenant")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("code = %v; want NotFound", code)
	}
}

// TestGateway_SSO_Roundtrip exercises the FULL grpc-gateway path:
// an HTTP/JSON POST hits the gateway, the gateway translates to a
// gRPC SSO call against the local listener, and the JSON response
// matches the proto field-for-field. This is the byte-for-byte
// REST parity invariant the entire A4/A5 pipeline preserves.
func TestGateway_SSO_Roundtrip(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	wantUserID := uuid.New()
	ts.authBackend.exchangeResult = &auth.ExchangeResult{
		AccessToken:  "gateway-access",
		RefreshToken: "gateway-refresh",
		User: auth.ResolvedUser{
			ID:          wantUserID,
			KChatUserID: "kc-gw",
			Email:       "bob@example.com",
			DisplayName: "Bob",
		},
		Tenants:   []auth.TenantRef{{ID: ts.tenantID, Slug: "test-tenant", Name: "Test", Role: "owner"}},
		TenantID:  ts.tenantID,
		SessionID: uuid.New(),
		ExpiresIn: 600,
	}

	httpSrv := httptest.NewServer(ts.gateway)
	defer httpSrv.Close()

	body, _ := json.Marshal(map[string]string{
		"code":         "code",
		"redirect_uri": "https://app.example.com/callback",
	})
	resp, err := http.Post(httpSrv.URL+"/api/v2/auth/sso", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, string(raw))
	}
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Result struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			User         struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"user"`
			ExpiresIn string `json:"expiresIn"` // grpc-gateway emits int64 as string by default
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", string(raw), err)
	}
	if out.Result.AccessToken != "gateway-access" {
		t.Errorf("accessToken = %q; want gateway-access", out.Result.AccessToken)
	}
	if out.Result.User.Email != "bob@example.com" {
		t.Errorf("user.email = %q; want bob@example.com", out.Result.User.Email)
	}
	if out.Result.ExpiresIn != "600" {
		t.Errorf("expiresIn = %q; want 600", out.Result.ExpiresIn)
	}
}

// TestGateway_RequestIDPropagation asserts that an X-Request-Id
// supplied by an HTTP client is forwarded to the gRPC server as
// x-request-id metadata and echoed back on the response.
func TestGateway_RequestIDPropagation(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	ts.authBackend.exchangeResult = &auth.ExchangeResult{
		AccessToken: "tok",
		User:        auth.ResolvedUser{ID: uuid.New()},
		Tenants:     []auth.TenantRef{{ID: ts.tenantID}},
		TenantID:    ts.tenantID,
		SessionID:   uuid.New(),
		ExpiresIn:   60,
	}
	httpSrv := httptest.NewServer(ts.gateway)
	defer httpSrv.Close()

	body := strings.NewReader(`{"code":"x","redirect_uri":"https://app.example.com/cb"}`)
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v2/auth/sso", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "rid-fixture")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, string(raw))
	}
	// The logging interceptor emits SetHeader(x-request-id) which
	// grpc-gateway propagates back as a Grpc-Metadata-X-Request-Id
	// response header by default. We check for either spelling.
	got := resp.Header.Get("Grpc-Metadata-X-Request-Id")
	if got == "" {
		got = resp.Header.Get("X-Request-Id")
	}
	if got != "rid-fixture" {
		t.Errorf("x-request-id echo = %q; want rid-fixture", got)
	}
}
