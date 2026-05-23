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
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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
	srv, err := apigrpc.NewServer(srvCfg)
	if err != nil {
		cancel()
		t.Fatalf("NewServer: %v", err)
	}

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
// — IsUnauthenticatedMethod returns true for SSO so the call MUST
// succeed even without a JWT. Also verifies the response shape is
// preserved byte-for-byte through the gRPC translation.
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

// TestKTypeService_RegisterKType_VersionRequired asserts the
// handler rejects RegisterKType requests with version<=0 with
// codes.InvalidArgument, matching the proto contract documented
// in proto/kapp/v1/ktype.proto:35 ("version is REQUIRED and must
// be > 0"). The check is duplicated at both the gRPC handler
// (this test) and the HTTP register handler (services/api) so
// the wire surface is uniform.
func TestKTypeService_RegisterKType_VersionRequired(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	client := kappv1.NewKTypeServiceClient(ts.clientConn)
	ctx := withBearer(context.Background(), ts.issueToken(t, nil))

	cases := []struct {
		name    string
		version int32
	}{
		{"omitted (proto3 default)", 0},
		{"negative", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.RegisterKType(ctx, &kappv1.RegisterKTypeRequest{
				Name:    "Deal",
				Version: tc.version,
				Schema:  []byte(`{"type":"object"}`),
			})
			if err == nil {
				t.Fatalf("RegisterKType: want error, got nil")
			}
			if got, want := status.Code(err), codes.InvalidArgument; got != want {
				t.Errorf("code = %v; want %v", got, want)
			}
		})
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

// TestGateway_ChiMount exercises the production code path where the
// gateway is mounted on a chi router behind a prefix (e.g. /api/v2).
//
// Subtle invariant the test pins down: chi.Mount does NOT strip
// r.URL.Path the way net/http.ServeMux does. It only updates chi's
// own chi.RouteContext.RoutePath — which grpc-gateway does not look
// at. The gateway's runtime.ServeMux matches r.URL.Path against the
// full path declared in each rpc's google.api.http option (e.g.
// "/api/v2/auth/sso"), so mounting the gateway directly on a chi
// router under "/api/v2" is correct: the inner handler sees the
// full original URL.Path and matches the rpc route without any
// prefix-restore wrapper. A wrapper that prepended the mount
// prefix would in fact DOUBLE it (chi delivered "/api/v2/auth/sso"
// → wrapper produced "/api/v2/api/v2/auth/sso" → 404).
//
// This test guards that invariant: future contributors who add a
// "prefix restore" wrapper "to fix" a misperceived chi-strips-path
// problem will see this test fail.
func TestGateway_ChiMount(t *testing.T) {
	ts, closer := startTestServer(t)
	defer closer()

	ts.authBackend.exchangeResult = &auth.ExchangeResult{
		AccessToken: "mounted-access",
		User:        auth.ResolvedUser{ID: uuid.New(), Email: "carol@example.com"},
		Tenants:     []auth.TenantRef{{ID: ts.tenantID}},
		TenantID:    ts.tenantID,
		SessionID:   uuid.New(),
		ExpiresIn:   300,
	}

	// Inspector records the URL the gateway actually receives so
	// the test pins down what chi.Mount passes through.
	var observedPath, observedRawPath string
	inspector := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedPath = r.URL.Path
		observedRawPath = r.URL.RawPath
		ts.gateway.ServeHTTP(w, r)
	})

	r := chi.NewRouter()
	r.Mount("/api/v2", inspector)

	httpSrv := httptest.NewServer(r)
	defer httpSrv.Close()

	body := strings.NewReader(`{"code":"x","redirect_uri":"https://app.example.com/cb"}`)
	resp, err := http.Post(httpSrv.URL+"/api/v2/auth/sso", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (want 200); body = %s", resp.StatusCode, string(raw))
	}
	if observedPath != "/api/v2/auth/sso" {
		t.Errorf("inner-handler URL.Path = %q; want /api/v2/auth/sso (chi.Mount must NOT strip)", observedPath)
	}
	if observedRawPath != "" {
		t.Errorf("inner-handler URL.RawPath should be empty for an unencoded URL; got %q", observedRawPath)
	}
}

// TestUnaryTimeoutInterceptor_AppliesDeadline asserts that a unary
// handler with no caller-supplied deadline picks up the server's
// configured timeout. Uses a 50ms timeout and a handler that sleeps
// 5s — the call must return well before the handler completes.
func TestUnaryTimeoutInterceptor_AppliesDeadline(t *testing.T) {
	interceptor := apigrpc.UnaryTimeoutInterceptor(50 * time.Millisecond)

	start := time.Now()
	_, err := interceptor(
		context.Background(),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/test/Method"},
		func(ctx context.Context, _ any) (any, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return nil, nil
			}
		},
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected context.DeadlineExceeded; got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("handler exceeded timeout: elapsed = %v", elapsed)
	}
}

// TestUnaryTimeoutInterceptor_RespectsCallerDeadline asserts that
// when the caller already supplied a tighter deadline, the
// interceptor does NOT extend it — context.WithTimeout honours the
// existing deadline when it is sooner.
func TestUnaryTimeoutInterceptor_RespectsCallerDeadline(t *testing.T) {
	interceptor := apigrpc.UnaryTimeoutInterceptor(10 * time.Second)

	callerCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := interceptor(
		callerCtx,
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/test/Method"},
		func(ctx context.Context, _ any) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ctx.Err; got nil")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("caller deadline was 25ms but call took %v", elapsed)
	}
}

// TestServices_NilBackend_ReturnsUnavailable pins the wire-code
// unification described in authServiceImpl.SSO / ktypeServiceImpl
// doc comments: when AuthSvc / KTypeRegistry are nil on
// ServerConfig, the gRPC handlers MUST still be registered and
// each method MUST return codes.Unavailable — grpc-gateway then
// maps that to HTTP 503, matching the HTTP surface's "503 sso not
// configured" response in services/api/auth.go. If a future
// contributor reverts to conditional registration in server.go,
// this test fails with codes.Unimplemented (501 via gateway).
func TestServices_NilBackend_ReturnsUnavailable(t *testing.T) {
	signer, err := auth.NewSigner(auth.SignerConfig{
		Algorithm:  auth.AlgHS256,
		HMACKey:    []byte("0123456789abcdef0123456789abcdef"),
		Issuer:     "kapp-test",
		Audience:   "kapp-test",
		AccessTTL:  10 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	srvCfg := apigrpc.ServerConfig{
		Auth: apigrpc.AuthConfig{
			Signer:        signer,
			TenantResolve: &fakeTenantResolver{tenant: &tenant.Tenant{ID: uuid.New(), Status: tenant.StatusActive}},
			Logger:        slog.Default(),
		},
		AuthSvc:       nil, // deliberately nil
		KTypeRegistry: nil, // deliberately nil
		Logger:        slog.Default(),
	}
	srv, err := apigrpc.NewServer(srvCfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = lis.Close() }()
	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// SSO with nil AuthSvc must return Unavailable (NOT Unimplemented).
	authClient := kappv1.NewAuthServiceClient(conn)
	_, ssoErr := authClient.SSO(ctx, &kappv1.SSORequest{Code: "x", RedirectUri: "y"})
	if ssoErr == nil {
		t.Fatalf("SSO with nil backend: expected error; got nil")
	}
	if st, _ := status.FromError(ssoErr); st.Code() != codes.Unavailable {
		t.Errorf("SSO nil backend: code = %s, want Unavailable", st.Code())
	}

	// Refresh with nil AuthSvc must return Unavailable as well.
	_, refErr := authClient.Refresh(ctx, &kappv1.RefreshRequest{RefreshToken: "tok"})
	if refErr == nil {
		t.Fatalf("Refresh with nil backend: expected error; got nil")
	}
	if st, _ := status.FromError(refErr); st.Code() != codes.Unavailable {
		t.Errorf("Refresh nil backend: code = %s, want Unavailable", st.Code())
	}

	// KType handlers also gated by auth — issue a real bearer so
	// we reach the handler rather than tripping Unauthenticated.
	tok, err := signer.Issue(auth.Claims{
		UserID:   uuid.New(),
		TenantID: srvCfg.Auth.TenantResolve.(*fakeTenantResolver).tenant.ID,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	mdCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	ktClient := kappv1.NewKTypeServiceClient(conn)
	_, listErr := ktClient.ListKTypes(mdCtx, &kappv1.ListKTypesRequest{})
	if listErr == nil {
		t.Fatalf("ListKTypes with nil registry: expected error; got nil")
	}
	if st, _ := status.FromError(listErr); st.Code() != codes.Unavailable {
		t.Errorf("ListKTypes nil registry: code = %s, want Unavailable", st.Code())
	}
}

// TestNewServer_RejectsMissingAuthDeps pins the construction-time
// nil-checks for AuthConfig dependencies that the interceptor
// invokes on every authenticated RPC. A nil Signer or
// TenantResolve would otherwise NPE inside the interceptor — the
// recovery interceptor would catch it, but the client would see
// codes.Internal which is misleading for a misconfiguration. This
// test asserts NewServer fails fast at construction with a clear
// error message instead.
//
// Sessions and Logger are intentionally NOT tested here: Sessions
// is documented as optional (nil disables session revalidation)
// and Logger has a slog.Default() fallback. See the NewServer
// docstring.
func TestNewServer_RejectsMissingAuthDeps(t *testing.T) {
	signer, err := auth.NewSigner(auth.SignerConfig{
		Algorithm:  auth.AlgHS256,
		HMACKey:    []byte("0123456789abcdef0123456789abcdef"),
		Issuer:     "kapp-test",
		Audience:   "kapp-test",
		AccessTTL:  10 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	resolver := &fakeTenantResolver{tenant: &tenant.Tenant{ID: uuid.New(), Status: tenant.StatusActive}}

	cases := []struct {
		name       string
		cfg        apigrpc.ServerConfig
		wantSubstr string
	}{
		{
			name: "nil Signer",
			cfg: apigrpc.ServerConfig{
				Auth: apigrpc.AuthConfig{
					Signer:        nil,
					TenantResolve: resolver,
				},
			},
			wantSubstr: "Signer is required",
		},
		{
			name: "nil TenantResolve",
			cfg: apigrpc.ServerConfig{
				Auth: apigrpc.AuthConfig{
					Signer:        signer,
					TenantResolve: nil,
				},
			},
			wantSubstr: "TenantResolve is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := apigrpc.NewServer(tc.cfg)
			if err == nil {
				t.Fatalf("NewServer: want error, got nil (server=%v)", srv)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("NewServer err = %q; want substring %q", err.Error(), tc.wantSubstr)
			}
			if srv != nil {
				t.Errorf("NewServer returned non-nil server on error: %v", srv)
			}
		})
	}
}

// TestRequestIDFromMetadata_Sanitisation pins the wire-parity guard
// that the gRPC surface applies the same sanitisation to the
// incoming x-request-id metadata header as the HTTP middleware
// applies to X-Request-ID. Values that exceed
// platform.MaxIncomingRequestIDLen (128) or contain non-printable /
// non-ASCII bytes are rejected (returned as "") so the logging
// interceptor mints a fresh id rather than echoing attacker-
// controlled content into structured logs or back to the caller
// via the response trailer.
//
// Without this guard a direct gRPC client could supply a 10 MB
// x-request-id and the value would land in every log line for the
// RPC (log-injection / log-storage abuse vector). The HTTP surface
// already guards against this via platform.SanitizeIncomingRequestID
// in internal/platform/requestid.go; this test asserts the gRPC
// surface delegates to the same helper.
func TestRequestIDFromMetadata_Sanitisation(t *testing.T) {
	bg := context.Background()
	cases := []struct {
		name     string
		value    string
		wantKept bool
	}{
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"clean ascii", "rid-fixture-12345", true},
		{"too long", strings.Repeat("a", 129), false},
		{"max-len boundary kept", strings.Repeat("a", 128), true},
		{"contains space", "rid with space", false},
		{"contains tab", "rid\twith\ttab", false},
		{"contains newline", "rid\nnewline", false},
		{"contains control char", "rid\x01ctrl", false},
		{"contains non-ascii", "rid-ünicode", false},
		{"contains del", "rid\x7fdel", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := metadata.Pairs(apigrpc.MetadataRequestID, tc.value)
			ctx := metadata.NewIncomingContext(bg, md)
			got := apigrpc.RequestIDFromMetadata(ctx)
			if tc.wantKept {
				want := strings.TrimSpace(tc.value)
				if got != want {
					t.Errorf("RequestIDFromMetadata(%q) = %q; want %q", tc.value, got, want)
				}
			} else {
				if got != "" {
					t.Errorf("RequestIDFromMetadata(%q) = %q; want empty (rejected)", tc.value, got)
				}
			}
		})
	}

	t.Run("no metadata on context", func(t *testing.T) {
		if got := apigrpc.RequestIDFromMetadata(bg); got != "" {
			t.Errorf("RequestIDFromMetadata(no md) = %q; want empty", got)
		}
	})
}

// recordingHandler is the shared backing store for the
// sharedRecordingHandler chain. Every Record emitted through any
// derived handler (from base.With(...) calls inside the logging
// interceptor) lands in the same `records` slice so the test can
// inspect what was written from the gRPC server goroutine. The
// mutex makes the structure safe for the cross-goroutine read
// the test performs after the RPC returns.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

// findRecord returns the first captured record whose message
// matches `msg`, or nil if none was emitted.
func (h *recordingHandler) findRecord(msg string) *slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.records {
		if h.records[i].Message == msg {
			return &h.records[i]
		}
	}
	return nil
}

// TestLoggingInterceptor_RPCComplete_IncludesTenantAndUser pins the
// fix for the chain-of-interceptors context-propagation bug: the
// auth interceptor enriches its OWN ctx with WithTenant/WithUserID
// and passes that to its handler, but control unwinds back to the
// outer logging interceptor with the pre-auth ctx — so a naive
// platform.TenantFromContext(ctx) inside emitRPCComplete returns
// nil even for authenticated RPCs. The fix is the shared-pointer
// rpcAttrs struct on ctx that auth writes to and logging reads
// from. This test wires up a real interceptor chain against a
// real bearer JWT and asserts that the captured "rpc complete"
// log record contains BOTH tenant_id and user_id attributes.
//
// A regression to the pre-fix behaviour (rpc complete missing
// tenant_id/user_id) would fail this test, AND a chain-order
// regression (auth running before logging, breaking unauthed
// SSO/Refresh logging) would fail TestAuthService_SSO's existing
// expectations. Together the two tests pin the contract.
func TestLoggingInterceptor_RPCComplete_IncludesTenantAndUser(t *testing.T) {
	// Custom logger that records every emitted slog.Record. The
	// SAME shared backing slice is propagated through every
	// base.With(...) derivative by capturing a shared pointer in
	// the handler — we accomplish this by ALWAYS writing into the
	// receiver's records slice via WithAttrs returning a clone that
	// embeds the same recordingHandler.
	rec := &recordingHandler{}
	// Re-implement WithAttrs to share the records slice. We
	// shadow the type's WithAttrs by using a wrapper handler
	// instead.
	logger := slog.New(&sharedRecordingHandler{base: rec})

	signer, err := auth.NewSigner(auth.SignerConfig{
		Algorithm:  auth.AlgHS256,
		HMACKey:    []byte("0123456789abcdef0123456789abcdef"),
		Issuer:     "kapp-test",
		Audience:   "kapp-test",
		AccessTTL:  10 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	tenantID := uuid.New()
	userID := uuid.New()
	tn := &tenant.Tenant{ID: tenantID, Status: tenant.StatusActive}

	srvCfg := apigrpc.ServerConfig{
		Auth: apigrpc.AuthConfig{
			Signer:        signer,
			TenantResolve: &fakeTenantResolver{tenant: tn},
			Logger:        logger,
		},
		KTypeRegistry: newFakeKTypeBackend(),
		Logger:        logger,
	}
	srv, err := apigrpc.NewServer(srvCfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = lis.Close() }()
	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	tok, err := signer.Issue(auth.Claims{
		UserID:    userID,
		TenantID:  tenantID,
		SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)

	ktClient := kappv1.NewKTypeServiceClient(conn)
	if _, err := ktClient.ListKTypes(ctx, &kappv1.ListKTypesRequest{}); err != nil {
		t.Fatalf("ListKTypes: %v", err)
	}

	rpcRec := rec.findRecord("rpc complete")
	if rpcRec == nil {
		t.Fatalf("did not see 'rpc complete' log record; got %d records", len(rec.records))
	}

	var sawTenant, sawUser bool
	var gotTenant, gotUser string
	rpcRec.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "tenant_id":
			sawTenant = true
			gotTenant = a.Value.String()
		case "user_id":
			sawUser = true
			gotUser = a.Value.String()
		}
		return true
	})
	if !sawTenant {
		t.Errorf("rpc complete missing tenant_id attribute")
	} else if gotTenant != tenantID.String() {
		t.Errorf("rpc complete tenant_id = %s; want %s", gotTenant, tenantID.String())
	}
	if !sawUser {
		t.Errorf("rpc complete missing user_id attribute")
	} else if gotUser != userID.String() {
		t.Errorf("rpc complete user_id = %s; want %s", gotUser, userID.String())
	}
}

// sharedRecordingHandler wraps recordingHandler so every
// base.With(...) derivative records into the SAME backing slice
// (slog handlers normally return a clone on WithAttrs/WithGroup
// which would lose the records the test wants to read). The
// wrapper keeps the same `base` pointer through all derivatives.
type sharedRecordingHandler struct {
	base  *recordingHandler
	attrs []slog.Attr
	group string
}

func (h *sharedRecordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *sharedRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.base.mu.Lock()
	defer h.base.mu.Unlock()
	cloned := r.Clone()
	if len(h.attrs) > 0 {
		cloned.AddAttrs(h.attrs...)
	}
	h.base.records = append(h.base.records, cloned)
	return nil
}

func (h *sharedRecordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &sharedRecordingHandler{base: h.base, attrs: merged, group: h.group}
}

func (h *sharedRecordingHandler) WithGroup(name string) slog.Handler {
	return &sharedRecordingHandler{base: h.base, attrs: h.attrs, group: name}
}
