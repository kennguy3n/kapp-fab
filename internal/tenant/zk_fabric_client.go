package tenant

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// errAlreadyExists is returned by do() when the console replies
// 409 Conflict. Callers decide whether that is a tolerable
// idempotent re-run (createTenant, createBucket) or a hard error
// (createKey, where the platform must rotate or recover the
// existing secret out-of-band).
var errAlreadyExists = errors.New("zk fabric: already exists")

// ZKFabricClient is a thin HTTP client for the ZK Object Fabric
// console at :8081. It implements ZKFabricProvisioner so the setup
// wizard can call it during tenant onboarding to mint per-tenant
// HMAC credentials and bucket bindings.
//
// The console endpoints used here:
//
//	POST /api/tenants/{id}            — create tenant record (admin)
//	POST /api/tenants/{id}/keys       — issue HMAC access key pair
//	POST /api/tenants/{id}/buckets    — create per-tenant bucket
//
// All three are admin-token gated; the platform operator supplies
// the token via the ZK_FABRIC_ADMIN_TOKEN env var. If the token is
// blank the client returns nil from NewZKFabricClient so the wizard
// falls back to the legacy MinIO path (no ZK encryption).
type ZKFabricClient struct {
	endpoint   string
	adminToken string
	bucketTmpl string
	httpClient *http.Client
}

// ZKFabricClientConfig configures the console client. Endpoint is
// the console base URL (e.g. http://zk-fabric:8081). AdminToken is
// the bearer token sent in the Authorization header. BucketTemplate
// renders the per-tenant bucket name from {tenant_id} / {slug}; the
// default is "kapp-{slug}".
type ZKFabricClientConfig struct {
	Endpoint       string
	AdminToken     string
	BucketTemplate string
	HTTPClient     *http.Client
}

// NewZKFabricClient returns a console client or nil when the
// integration is disabled (Endpoint or AdminToken blank). Returning
// a nil interface lets callers wire the result straight into
// Wizard.WithZKFabricProvisioner without an extra branch.
func NewZKFabricClient(cfg ZKFabricClientConfig) *ZKFabricClient {
	if cfg.Endpoint == "" || cfg.AdminToken == "" {
		return nil
	}
	if cfg.BucketTemplate == "" {
		cfg.BucketTemplate = "kapp-{slug}"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &ZKFabricClient{
		endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
		adminToken: cfg.AdminToken,
		bucketTmpl: cfg.BucketTemplate,
		httpClient: hc,
	}
}

// ProvisionTenant implements ZKFabricProvisioner. The flow is:
//
//  1. POST /api/tenants/{id}            — create tenant record
//  2. POST /api/tenants/{id}/keys       — mint access/secret pair
//  3. POST /api/tenants/{id}/buckets    — bind a per-tenant bucket
//  4. PUT  /api/tenants/{id}/placement  — set placement policy
//     (when policy non-empty)
//
// All three (now four) calls are idempotent on the fabric side:
// re-running the wizard after a partial failure converges. The
// returned ZKCredentials hold the access/secret pair the gateway
// accepts on SigV4 sign and the bucket name the kapp client should
// write to.
func (c *ZKFabricClient) ProvisionTenant(ctx context.Context, tenantID uuid.UUID, slug string) (ZKCredentials, error) {
	return c.ProvisionTenantWithPolicy(ctx, tenantID, slug, "", PlacementPolicy{})
}

// ProvisionTenantWithPolicy is the policy-aware variant of
// ProvisionTenant. The wizard derives a plan-appropriate policy via
// DerivePlacementPolicy and threads it through here so each new
// tenant lands on the fabric with provider/country/cache hints
// already in place. plan maps to fabric `contract_type`:
//
//	free                            → b2c_pooled
//	starter / business / paid       → b2b_shared
//	enterprise                      → b2b_dedicated
//
// An empty policy.Spec.Placement.Provider skips the placement PUT
// (callers that opt out of policy management still get the legacy
// flow). bucket on the policy is forced to match the bucket the
// fabric just minted so the policy and the credential row agree.
func (c *ZKFabricClient) ProvisionTenantWithPolicy(ctx context.Context, tenantID uuid.UUID, slug, plan string, policy PlacementPolicy) (ZKCredentials, error) {
	if c == nil {
		return ZKCredentials{}, errors.New("zk fabric: client not configured")
	}
	if err := c.createTenantWithContract(ctx, tenantID, slug, plan); err != nil && !errors.Is(err, errAlreadyExists) {
		return ZKCredentials{}, fmt.Errorf("zk fabric: create tenant: %w", err)
	}
	access, secret, err := c.createKey(ctx, tenantID)
	if err != nil {
		return ZKCredentials{}, fmt.Errorf("zk fabric: create key: %w", err)
	}
	bucket := c.bucketName(tenantID, slug)
	if err := c.createBucket(ctx, tenantID, bucket); err != nil && !errors.Is(err, errAlreadyExists) {
		return ZKCredentials{}, fmt.Errorf("zk fabric: create bucket: %w", err)
	}
	if len(policy.Spec.Placement.Provider) > 0 {
		policy.Tenant = tenantID.String()
		policy.Bucket = bucket
		if err := c.SetPlacementPolicy(ctx, tenantID, policy); err != nil {
			return ZKCredentials{}, fmt.Errorf("zk fabric: set placement policy: %w", err)
		}
	}
	return ZKCredentials{AccessKey: access, SecretKey: secret, Bucket: bucket}, nil
}

// SetPlacementPolicy calls PUT /api/tenants/{id}/placement on the
// fabric console. The body shape mirrors
// `placement_policy.Policy` (zk-object-fabric/metadata/placement_policy/policy.go);
// the console enforces structural validation, so a 4xx here means
// the wizard built a malformed body and Kapp should surface the
// detail back to the operator rather than retry blindly.
func (c *ZKFabricClient) SetPlacementPolicy(ctx context.Context, tenantID uuid.UUID, policy PlacementPolicy) error {
	if c == nil {
		return errors.New("zk fabric: client not configured")
	}
	policy.Tenant = tenantID.String()
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/api/tenants/%s/placement", tenantID), policy, nil)
}

func (c *ZKFabricClient) bucketName(tenantID uuid.UUID, slug string) string {
	out := c.bucketTmpl
	out = strings.ReplaceAll(out, "{slug}", slug)
	out = strings.ReplaceAll(out, "{tenant_id}", tenantID.String())
	return out
}

// createTenantWithContract maps the Kapp plan tier to the fabric's
// `contract_type`. Free plans share the pooled b2c contract; paid
// plans default to the b2b_shared contract; enterprise lands on
// b2b_dedicated which the fabric routes to dedicated cells. An
// empty plan stays on the historical b2b_shared default so the
// signature change is backward-compatible.
func (c *ZKFabricClient) createTenantWithContract(ctx context.Context, tenantID uuid.UUID, slug, plan string) error {
	contract := contractTypeForPlan(plan)
	body := map[string]any{
		"id":            tenantID.String(),
		"name":          slug,
		"contract_type": contract,
		"license_tier":  "beta",
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/tenants/%s", tenantID), body, nil)
}

// contractTypeForPlan maps a Kapp plan tier to the corresponding
// fabric contract_type. Kept package-private so the wizard always
// goes through createTenantWithContract.
func contractTypeForPlan(plan string) string {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case PlanFree:
		return "b2c_pooled"
	case PlanEnterprise:
		return "b2b_dedicated"
	default:
		return "b2b_shared"
	}
}

// createKey calls POST /api/tenants/{id}/keys and parses the
// returned descriptor. The access key is generated on the fabric
// side; we pass an empty body and read back the {accessKey,
// secretKey} pair. Secret is shown exactly once — the platform
// operator must persist it on the tenants row immediately.
func (c *ZKFabricClient) createKey(ctx context.Context, tenantID uuid.UUID) (string, string, error) {
	resp := struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}{}
	// Generate a random suggested access key client-side so the
	// fabric demo (which echoes the supplied accessKey when
	// present) returns a deterministic value the wizard can pin.
	suggested := suggestedAccessKey()
	body := map[string]any{"accessKey": suggested}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/tenants/%s/keys", tenantID), body, &resp); err != nil {
		return "", "", err
	}
	if resp.AccessKey == "" || resp.SecretKey == "" {
		return "", "", errors.New("zk fabric: console returned empty access/secret pair")
	}
	return resp.AccessKey, resp.SecretKey, nil
}

func (c *ZKFabricClient) createBucket(ctx context.Context, tenantID uuid.UUID, bucket string) error {
	body := map[string]any{"name": bucket}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/tenants/%s/buckets", tenantID), body, nil)
}

// do issues a JSON request against the console with bearer auth.
// 2xx responses unmarshal into out (when non-nil); 409 is treated
// as success so re-running the wizard is idempotent.
func (c *ZKFabricClient) do(ctx context.Context, method, path string, in any, out any) error {
	var rdr io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.adminToken)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return errAlreadyExists
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// suggestedAccessKey returns a 16-byte random URL-safe access key.
// The actual secret is minted by the fabric console; we only need
// a stable identifier so the wizard can assert idempotency on
// retries.
func suggestedAccessKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "kapp-" + base64.RawURLEncoding.EncodeToString(b[:])
}
