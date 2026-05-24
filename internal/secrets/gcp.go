package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GCPProviderConfig is the construction-time configuration for
// the Google Cloud Secret Manager backend.
//
//   - ProjectID is required. It is the GCP project that owns the
//     secrets the API will read; the resulting resource name is
//     projects/<ProjectID>/secrets/<key>/versions/<version>.
//   - Prefix is an optional secret-name prefix. The key fed to
//     GetSecret has slashes replaced with dashes (Secret Manager
//     forbids `/` in secret names) and the prefix prepended, so a
//     key "jwt/primary" with Prefix "kapp-" resolves to the
//     secret name "kapp-jwt-primary".
//   - Version is the version selector — typically "latest" (the
//     default) but operators can pin to a numeric version for a
//     reproducible boot.
//   - ClientOptions are forwarded to secretmanager.NewClient so
//     tests can dial a bufconn endpoint and production callers can
//     thread workload-identity / impersonation credentials through.
type GCPProviderConfig struct {
	ProjectID     string
	Prefix        string
	Version       string
	ClientOptions []option.ClientOption
}

// GCPProvider resolves secrets against Google Cloud Secret
// Manager. Boot-time wiring opens one gRPC client which is
// reused for every GetSecret call; the client is goroutine-safe
// per the package docs.
type GCPProvider struct {
	client    *secretmanager.Client
	projectID string
	prefix    string
	version   string
}

// NewGCPProvider validates the config and opens a Secret Manager
// gRPC client. The supplied context is used only for the dial;
// the client itself holds onto its own background context.
//
// Authentication uses google.FindDefaultCredentials by default —
// in-cluster the API server typically picks up a workload-
// identity binding; on a developer laptop it picks up
// `gcloud auth application-default login`. Operators who need
// to override this (e.g. service-account impersonation) pass
// option.WithCredentialsFile or option.WithTokenSource through
// cfg.ClientOptions.
func NewGCPProvider(ctx context.Context, cfg GCPProviderConfig) (*GCPProvider, error) {
	if strings.TrimSpace(cfg.ProjectID) == "" {
		return nil, fmt.Errorf("%w: gcp provider requires ProjectID", ErrProviderNotConfigured)
	}
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		version = "latest"
	}
	client, err := secretmanager.NewClient(ctx, cfg.ClientOptions...)
	if err != nil {
		return nil, fmt.Errorf("%w: gcp dial: %w", ErrProviderUnavailable, err)
	}
	return &GCPProvider{
		client:    client,
		projectID: cfg.ProjectID,
		prefix:    cfg.Prefix,
		version:   version,
	}, nil
}

// Name returns the literal "gcp".
func (*GCPProvider) Name() string { return "gcp" }

// Close releases the underlying gRPC connection. Safe to call
// multiple times; subsequent GetSecret calls will fail. Callers
// SHOULD register this with their shutdown sequencer alongside
// the DB pool close.
func (p *GCPProvider) Close() error {
	if p == nil || p.client == nil {
		return nil
	}
	return p.client.Close()
}

// GetSecret resolves key against Secret Manager and returns the
// payload bytes plus the resolved version (the numeric version
// the alias mapped to, NOT the literal alias the request used).
// Returning the resolved version is what lets KeyRingRefresher
// detect rotations even when callers ask for "latest" — the
// response Name carries the concrete version the server picked.
//
// Keys that already start with "projects/" are passed through
// verbatim so operators can pin a full resource path
// (cross-project reads, locational secrets).
func (p *GCPProvider) GetSecret(ctx context.Context, key string) (SecretValue, error) {
	if p == nil || p.client == nil {
		return SecretValue{}, fmt.Errorf("%w: gcp client closed", ErrProviderUnavailable)
	}
	name := p.resourceName(key)
	resp, err := p.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		return SecretValue{}, translateGCPError(name, err)
	}
	if resp == nil || resp.Payload == nil {
		return SecretValue{}, fmt.Errorf("%w: gcp empty payload for %s", ErrSecretNotFound, name)
	}
	version := p.version
	if resolved := versionFromResourceName(resp.Name); resolved != "" {
		version = resolved
	}
	return SecretValue{Bytes: resp.Payload.Data, Version: version}, nil
}

// resourceName maps a logical key to a Secret Manager resource
// name. Keys that already look like a full resource path are
// passed through; everything else is normalised (slashes →
// dashes) and prefixed with the configured prefix.
func (p *GCPProvider) resourceName(key string) string {
	trimmed := strings.TrimSpace(key)
	if strings.HasPrefix(trimmed, "projects/") {
		if strings.Contains(trimmed, "/versions/") {
			return trimmed
		}
		return trimmed + "/versions/" + p.version
	}
	name := strings.ReplaceAll(trimmed, "/", "-")
	if p.prefix != "" {
		name = p.prefix + name
	}
	return fmt.Sprintf("projects/%s/secrets/%s/versions/%s", p.projectID, name, p.version)
}

// versionFromResourceName extracts the trailing /versions/<v>
// segment of a Secret Manager resource name. Returns "" when
// the name doesn't contain a version segment.
func versionFromResourceName(name string) string {
	idx := strings.LastIndex(name, "/versions/")
	if idx < 0 {
		return ""
	}
	return name[idx+len("/versions/"):]
}

// translateGCPError maps gRPC status codes to the package's
// sentinel errors. NotFound → ErrSecretNotFound (so callers
// distinguish a missing secret from a hung backend);
// ResourceExhausted / Unavailable / DeadlineExceeded →
// ErrProviderUnavailable (transient, callers may retry);
// PermissionDenied / Unauthenticated → ErrProviderUnavailable
// with a clear message pointing operators at IAM.
func translateGCPError(name string, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: gcp %s: %w", ErrProviderUnavailable, name, err)
	}
	switch st.Code() {
	case codes.NotFound:
		return fmt.Errorf("%w: gcp secret %s missing", ErrSecretNotFound, name)
	case codes.PermissionDenied, codes.Unauthenticated:
		return fmt.Errorf("%w: gcp %s: permission denied — check IAM (secretmanager.versions.access): %w",
			ErrProviderUnavailable, name, err)
	case codes.ResourceExhausted, codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: gcp %s: transient: %w", ErrProviderUnavailable, name, err)
	case codes.Canceled:
		return errors.Join(context.Canceled, fmt.Errorf("gcp %s canceled: %w", name, err))
	default:
		return fmt.Errorf("secrets: gcp AccessSecretVersion %s: %w", name, err)
	}
}
