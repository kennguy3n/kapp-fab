package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// VaultProvider resolves secrets from a HashiCorp Vault KV v2
// store via the HTTP API. We talk to Vault directly rather than
// pulling in github.com/hashicorp/vault/api because that SDK
// drags in 30+ transitive dependencies for a feature footprint
// (KV v2 read + auth-token header) that fits in 60 lines of
// net/http.
//
// # KV v2 path mapping
//
// Vault's KV v2 secrets engine exposes secrets at
//
//	GET /v1/<mount>/data/<key>
//
// (the literal "/data/" segment is mandatory and distinguishes
// KV v2 from KV v1). MountPath defaults to "secret" -- the
// engine name Vault ships with `vault secrets enable -path=secret
// kv-v2`. Operators on a different mount can override it.
//
// # Authentication
//
// PR-6 ships token-auth only: the operator supplies a Vault
// token via KAPP_SECRETS_VAULT_TOKEN and the provider stamps it
// on every request as X-Vault-Token. AppRole / Kubernetes /
// AWS-IAM auth flows are out of scope for this PR and will land
// alongside the orchestrator integrations in later PRs (those
// flows mint a token from a deeper credential and need their
// own renewal loop, which is its own can of worms). The token
// is treated as a regular Provider input -- operators should
// rotate it via the same mechanism they rotate other secrets.
//
// # Versioning
//
// The KV v2 response carries metadata.version, an integer that
// increments on every write. SecretValue.Version is set to that
// integer as a decimal string -- distinct upstream writes always
// produce distinct version strings, which is the rotation
// detection contract.
//
// # Secret shape
//
// KV v2 returns a JSON object `data.data` with arbitrary keys.
// The provider expects exactly one key named "value" by default
// (matching the convention from `vault kv put secret/foo
// value=...`). Operators with a different convention can
// override via the SecretKey config field.
type VaultProvider struct {
	addr      string
	mountPath string
	token     string
	secretKey string
	client    *http.Client
}

// VaultProviderConfig is the construction-time configuration
// for a Vault provider.
type VaultProviderConfig struct {
	// Addr is the Vault server base URL, e.g.
	// https://vault.kapp.internal:8200. Required.
	Addr string
	// Token is the Vault token used to authenticate every
	// request via the X-Vault-Token header. Required.
	Token string
	// MountPath is the KV v2 mount name. Defaults to "secret".
	MountPath string
	// SecretKey is the JSON key under data.data that holds the
	// secret value. Defaults to "value".
	SecretKey string
	// Timeout caps per-request HTTP timeout. Default 5s.
	Timeout time.Duration
}

// NewVaultProvider returns a VaultProvider wired against the
// supplied Vault address. The address must include the scheme
// (https://) and may include a non-default port.
func NewVaultProvider(cfg VaultProviderConfig) (*VaultProvider, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("%w: vault addr empty", ErrProviderNotConfigured)
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("%w: vault token empty", ErrProviderNotConfigured)
	}
	if _, err := url.Parse(cfg.Addr); err != nil {
		return nil, fmt.Errorf("secrets: parse vault addr %q: %w", cfg.Addr, err)
	}
	mountPath := cfg.MountPath
	if mountPath == "" {
		mountPath = "secret"
	}
	secretKey := cfg.SecretKey
	if secretKey == "" {
		secretKey = "value"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &VaultProvider{
		addr:      strings.TrimRight(cfg.Addr, "/"),
		mountPath: strings.Trim(mountPath, "/"),
		token:     cfg.Token,
		secretKey: secretKey,
		client:    &http.Client{Timeout: timeout},
	}, nil
}

// Name returns the literal "vault".
func (*VaultProvider) Name() string { return "vault" }

// GetSecret reads the named secret from the configured Vault
// mount. Returns ErrSecretNotFound for the 404 response,
// ErrProviderUnavailable for any 5xx, and a wrapped error
// otherwise.
func (p *VaultProvider) GetSecret(ctx context.Context, key string) (SecretValue, error) {
	endpoint := fmt.Sprintf("%s/v1/%s/data/%s", p.addr, p.mountPath, encodeVaultPath(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return SecretValue{}, fmt.Errorf("secrets: build vault request: %w", err)
	}
	req.Header.Set("X-Vault-Token", p.token)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return SecretValue{}, fmt.Errorf("%w: vault %s: %w", ErrProviderUnavailable, key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return SecretValue{}, fmt.Errorf("%w: vault key %s missing", ErrSecretNotFound, key)
	case resp.StatusCode >= 500:
		return SecretValue{}, fmt.Errorf("%w: vault status %d", ErrProviderUnavailable, resp.StatusCode)
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return SecretValue{}, fmt.Errorf("secrets: vault status %d: %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return SecretValue{}, fmt.Errorf("secrets: read vault body: %w", err)
	}
	var decoded vaultResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return SecretValue{}, fmt.Errorf("secrets: decode vault body: %w", err)
	}
	value, ok := decoded.Data.Data[p.secretKey]
	if !ok {
		return SecretValue{}, fmt.Errorf("%w: vault key %s has no %s field", ErrSecretNotFound, key, p.secretKey)
	}
	raw, ok := value.(string)
	if !ok {
		return SecretValue{}, fmt.Errorf("secrets: vault key %s %s is not a string", key, p.secretKey)
	}
	if raw == "" {
		return SecretValue{}, fmt.Errorf("%w: vault key %s %s empty", ErrSecretNotFound, key, p.secretKey)
	}
	return SecretValue{
		Bytes:   []byte(raw),
		Version: strconv.Itoa(decoded.Data.Metadata.Version),
	}, nil
}

// vaultResponse is the subset of the KV v2 read-response shape
// we actually consume. The full shape includes lease info, warnings,
// and request_id; we ignore those.
type vaultResponse struct {
	Data struct {
		Data     map[string]any `json:"data"`
		Metadata struct {
			Version int `json:"version"`
		} `json:"metadata"`
	} `json:"data"`
}

// Compile-time guard: VaultProvider must satisfy Provider.
var _ Provider = (*VaultProvider)(nil)

// encodeVaultPath URL-encodes a Vault KV v2 secret key for safe
// inclusion in the request URL. Vault paths are conventionally
// "/"-separated namespaces (e.g. "jwt/primary"), so we preserve
// "/" as a structural separator and percent-encode each segment
// independently. This protects against malformed requests when an
// operator stores a secret under a path containing URL-special
// characters ('?', '#', '%', spaces) — without this, those
// characters would be interpreted as URL syntax rather than path
// segments and would produce a 404 or worse, leak the configured
// path as a query parameter.
//
// The leading "/" is stripped (matching the pre-encode behaviour
// of strings.TrimLeft(key, "/")) to keep the final URL well-formed
// — "/v1/mount/data//foo" is invalid in Vault.
func encodeVaultPath(key string) string {
	trimmed := strings.TrimLeft(key, "/")
	if trimmed == "" {
		return ""
	}
	segments := strings.Split(trimmed, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}
