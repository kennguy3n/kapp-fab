package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeSecretManager is an in-process implementation of the
// SecretManagerService gRPC surface used by the unit tests. Each
// secret name (after stripping the projects/<p>/secrets/ prefix)
// maps to one or more versions; "latest" resolves to the highest
// numeric version. The version is echoed back in the response
// Name so the provider can detect rotations.
type fakeSecretManager struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer
	mu      sync.Mutex
	secrets map[string]map[string][]byte // name -> version -> payload
	calls   int
}

func newFakeSecretManager() *fakeSecretManager {
	return &fakeSecretManager{secrets: map[string]map[string][]byte{}}
}

func (f *fakeSecretManager) put(name, version string, payload []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.secrets[name] == nil {
		f.secrets[name] = map[string][]byte{}
	}
	f.secrets[name][version] = payload
}

func (f *fakeSecretManager) AccessSecretVersion(_ context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	parts := strings.Split(req.Name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "secrets" || parts[4] != "versions" {
		return nil, status.Errorf(codes.InvalidArgument, "bad name %q", req.Name)
	}
	name := parts[3]
	wantVersion := parts[5]
	versions := f.secrets[name]
	if versions == nil {
		return nil, status.Errorf(codes.NotFound, "no secret %q", name)
	}
	resolved := wantVersion
	if wantVersion == "latest" {
		// resolve "latest" to the lexicographically highest version
		// since fixture versions are zero-padded
		for v := range versions {
			if resolved == "latest" || v > resolved {
				resolved = v
			}
		}
	}
	payload, ok := versions[resolved]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no version %q for %q", resolved, name)
	}
	return &secretmanagerpb.AccessSecretVersionResponse{
		Name:    fmt.Sprintf("projects/%s/secrets/%s/versions/%s", parts[1], name, resolved),
		Payload: &secretmanagerpb.SecretPayload{Data: payload},
	}, nil
}

// startFakeGCP starts the fake Secret Manager on a bufconn
// listener and returns the client options needed to dial it.
// The caller registers cleanup via t.Cleanup so the grpc server
// is stopped at test end.
func startFakeGCP(t *testing.T) (*fakeSecretManager, []option.ClientOption) {
	t.Helper()
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	fake := newFakeSecretManager()
	secretmanagerpb.RegisterSecretManagerServiceServer(srv, fake)
	go func() {
		// ignore the error: srv.Serve returns ErrServerStopped on
		// graceful shutdown, which is expected.
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	opts := []option.ClientOption{
		option.WithGRPCDialOption(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		// disable auth: bufconn doesn't need credentials and the
		// default chain would try to reach metadata.google.internal
		option.WithoutAuthentication(),
	}
	return fake, opts
}

func TestGCPProvider_GetSecret_Success(t *testing.T) {
	fake, opts := startFakeGCP(t)
	fake.put("kapp-jwt-primary", "3", []byte("supersecretkeymaterial32bytesplus"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := NewGCPProvider(ctx, GCPProviderConfig{
		ProjectID:     "demo-project",
		Prefix:        "kapp-",
		ClientOptions: opts,
	})
	if err != nil {
		t.Fatalf("NewGCPProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	val, err := p.GetSecret(ctx, "jwt/primary")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(val.Bytes) != "supersecretkeymaterial32bytesplus" {
		t.Errorf("payload mismatch: %s", val.Bytes)
	}
	if val.Version != "3" {
		t.Errorf("resolved version = %q want 3 (the version latest maps to)", val.Version)
	}
}

func TestGCPProvider_GetSecret_NotFound(t *testing.T) {
	_, opts := startFakeGCP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := NewGCPProvider(ctx, GCPProviderConfig{ProjectID: "demo", ClientOptions: opts})
	if err != nil {
		t.Fatalf("NewGCPProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	_, err = p.GetSecret(ctx, "no-such-secret")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("expected ErrSecretNotFound got %v", err)
	}
}

func TestGCPProvider_GetSecret_DetectsRotation(t *testing.T) {
	fake, opts := startFakeGCP(t)
	fake.put("rotating", "1", []byte("v1material--padding--padding--32+"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := NewGCPProvider(ctx, GCPProviderConfig{ProjectID: "demo", ClientOptions: opts})
	if err != nil {
		t.Fatalf("NewGCPProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	first, err := p.GetSecret(ctx, "rotating")
	if err != nil {
		t.Fatalf("first GetSecret: %v", err)
	}
	if first.Version != "1" {
		t.Fatalf("first version = %q want 1", first.Version)
	}

	// operator rotates the secret upstream
	fake.put("rotating", "2", []byte("v2material--padding--padding--32+"))

	second, err := p.GetSecret(ctx, "rotating")
	if err != nil {
		t.Fatalf("second GetSecret: %v", err)
	}
	if second.Version != "2" {
		t.Errorf("second version = %q want 2 (latest must resolve to highest)", second.Version)
	}
	if bytes.Equal(second.Bytes, first.Bytes) {
		t.Errorf("payload should have rotated; got identical bytes")
	}
}

func TestGCPProvider_ResourceName_ARNPassthrough(t *testing.T) {
	p := &GCPProvider{projectID: "demo", prefix: "kapp-", version: "latest"}
	// keys starting with projects/ pass through verbatim when they
	// already include a version segment
	got := p.resourceName("projects/other-project/secrets/x/versions/4")
	if got != "projects/other-project/secrets/x/versions/4" {
		t.Errorf("ARN passthrough mismatch: %s", got)
	}
	// keys starting with projects/ but missing /versions/ get the
	// default version appended
	got = p.resourceName("projects/other-project/secrets/x")
	if got != "projects/other-project/secrets/x/versions/latest" {
		t.Errorf("ARN missing version mismatch: %s", got)
	}
	// non-ARN keys get prefix + normalisation
	got = p.resourceName("jwt/primary")
	if got != "projects/demo/secrets/kapp-jwt-primary/versions/latest" {
		t.Errorf("prefix composition mismatch: %s", got)
	}
}

func TestNewGCPProvider_RejectsEmptyProjectID(t *testing.T) {
	_, err := NewGCPProvider(context.Background(), GCPProviderConfig{})
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Errorf("expected ErrProviderNotConfigured got %v", err)
	}
}

func TestGCPProvider_TranslatesPermissionDenied(t *testing.T) {
	err := translateGCPError("projects/demo/secrets/x/versions/latest",
		status.Error(codes.PermissionDenied, "iam policy denies access"))
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("expected ErrProviderUnavailable got %v", err)
	}
	if !strings.Contains(err.Error(), "secretmanager.versions.access") {
		t.Errorf("error should mention required IAM permission, got %v", err)
	}
}

func TestGCPProvider_TranslatesTransientCodes(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded} {
		err := translateGCPError("x", status.Error(code, "transient"))
		if !errors.Is(err, ErrProviderUnavailable) {
			t.Errorf("%s: expected ErrProviderUnavailable got %v", code, err)
		}
	}
}
