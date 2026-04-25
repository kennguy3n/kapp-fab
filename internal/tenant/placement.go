package tenant

import (
	"encoding/json"
	"strings"
)

// PlacementPolicy mirrors the ZK Object Fabric `placement_policy.Policy`
// struct (zk-object-fabric/metadata/placement_policy/policy.go). The
// fabric's `PUT /api/tenants/{id}/placement` decodes a JSON body of
// this exact shape, so Kapp emits it without bespoke marshalling.
//
// Only the fields the fabric currently understands (Phase 1 of
// zk-object-fabric/docs/PROPOSAL.md §3.9) are surfaced here. Tenant +
// Bucket are populated server-side by `Wizard.RunSetupWizard` /
// `placement_handlers.PutPlacement` so callers cannot point a body at
// a different tenant.
type PlacementPolicy struct {
	Tenant string              `json:"tenant"`
	Bucket string              `json:"bucket,omitempty"`
	Spec   PlacementPolicySpec `json:"policy"`
}

// PlacementPolicySpec is the body of a policy.
type PlacementPolicySpec struct {
	Encryption PlacementEncryptionSpec `json:"encryption"`
	Placement  PlacementPlacementSpec  `json:"placement"`
}

// PlacementEncryptionSpec names the encryption mode and KMS reference.
// Mode mirrors the fabric's `metadata.EncryptionConfig.Mode` enum:
// "client_side", "managed", or "public_distribution".
type PlacementEncryptionSpec struct {
	Mode string `json:"mode"`
	KMS  string `json:"kms,omitempty"`
}

// PlacementPlacementSpec carries the Phase 1 placement knobs. Provider
// is required (at least one entry); the other fields are optional.
type PlacementPlacementSpec struct {
	Provider      []string `json:"provider"`
	Region        []string `json:"region,omitempty"`
	Country       []string `json:"country,omitempty"`
	StorageClass  []string `json:"storage_class,omitempty"`
	CacheLocation string   `json:"cache_location,omitempty"`
}

// PlacementPolicyConfig is the wizard-facing knob set. It captures the
// per-tenant inputs the policy depends on (locale, plan) and the
// platform-wide knobs the operator can tune via env (provider
// allow-list, default cache hint). The wizard derives the actual
// `PlacementPolicy` from this config + the plan tier rules in
// DerivePlacementPolicy so callers never have to assemble the policy
// shape by hand.
type PlacementPolicyConfig struct {
	Plan             string
	Country          string
	DefaultProviders []string
	DefaultCacheHint string
}

// canonical encryption modes used by the policy derivation. The fabric
// validates the literal strings on PUT so we mirror them here.
const (
	EncryptionModeManaged          = "managed"
	EncryptionModeClientSide       = "client_side"
	EncryptionModePublicDistribute = "public_distribution"
)

// DerivePlacementPolicy returns the platform-default placement policy
// for a tenant on the given plan + locale. The mapping mirrors the
// product's privacy positioning:
//
//	free / starter      → managed (gateway-side keys, server search OK)
//	business            → managed (same; paid plan can opt into editing)
//	enterprise          → client_side (zero-access, customer-managed)
//
// Country is normalised to ISO-3166 alpha-2; an empty / malformed
// country yields no residency restriction. Providers default to the
// platform allow-list when the caller passes none.
func DerivePlacementPolicy(cfg PlacementPolicyConfig) PlacementPolicy {
	encMode := EncryptionModeManaged
	switch strings.ToLower(strings.TrimSpace(cfg.Plan)) {
	case PlanEnterprise:
		encMode = EncryptionModeClientSide
	}

	providers := cfg.DefaultProviders
	if len(providers) == 0 {
		providers = []string{"wasabi"}
	}

	pol := PlacementPolicy{
		Spec: PlacementPolicySpec{
			Encryption: PlacementEncryptionSpec{Mode: encMode},
			Placement: PlacementPlacementSpec{
				Provider:      providers,
				CacheLocation: cfg.DefaultCacheHint,
			},
		},
	}
	if iso := normaliseCountry(cfg.Country); iso != "" {
		pol.Spec.Placement.Country = []string{iso}
	}
	return pol
}

// normaliseCountry coerces a country string to ISO-3166 alpha-2 when
// it parses cleanly, otherwise returns "". The fabric's policy.Validate
// enforces alpha-2 on its side; we mirror the check so the wizard
// never sends a body the fabric will reject.
func normaliseCountry(country string) string {
	c := strings.ToUpper(strings.TrimSpace(country))
	if len(c) != 2 {
		return ""
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return c
}

// MarshalJSON / UnmarshalJSON aliases keep the JSONB column round-trip
// in lock-step with the fabric's wire shape — callers persisting a
// policy should always go through json.Marshal so Tenant/Bucket are
// preserved for the local copy.

// PolicyJSON marshals the policy into the JSONB shape stored on the
// tenants row + the body sent to the fabric console. Errors only on
// extreme malformation; callers can safely panic-on-error in tests.
func (p *PlacementPolicy) PolicyJSON() ([]byte, error) {
	return json.Marshal(p)
}

// EnvPlacementSource implements PlacementPolicySource from raw env
// strings. Callers wire it once at startup (api/main.go) so the
// wizard does not have to read os.Getenv directly.
//
// providers is a comma-separated list (e.g. "wasabi,local-cell-1");
// blank yields the default ["wasabi"]. cacheHint is a single token
// (e.g. "linode-sg") and may be blank.
type EnvPlacementSource struct {
	providers []string
	cacheHint string
}

// NewEnvPlacementSource splits the providers env string and returns
// a source the wizard can consume. Whitespace around comma-separated
// entries is trimmed.
func NewEnvPlacementSource(providers, cacheHint string) *EnvPlacementSource {
	src := &EnvPlacementSource{cacheHint: strings.TrimSpace(cacheHint)}
	for _, p := range strings.Split(providers, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		src.providers = append(src.providers, p)
	}
	return src
}

// DefaultProviders implements PlacementPolicySource.
func (s *EnvPlacementSource) DefaultProviders() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.providers))
	copy(out, s.providers)
	return out
}

// DefaultCacheHint implements PlacementPolicySource.
func (s *EnvPlacementSource) DefaultCacheHint() string {
	if s == nil {
		return ""
	}
	return s.cacheHint
}
