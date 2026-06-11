package bankfeed

// RegistryConfig is the credential surface the API/worker boot passes to
// BuildRegistry, mapped from internal/platform.Config. Keeping it a plain
// struct (rather than importing platform here) keeps the bankfeed package
// free of an import cycle and trivially unit-testable.
type RegistryConfig struct {
	PlaidClientID      string
	PlaidSecret        string
	PlaidEnv           string
	PlaidWebhookSecret string

	GoCardlessSecretID      string
	GoCardlessSecretKey     string
	GoCardlessInstitutionID string
	GoCardlessWebhookSecret string
}

// BuildRegistry assembles the provider registry from configuration. The
// CSV provider is always present; Plaid and GoCardless register only
// when their full credential pair is configured. Constructing the
// concrete providers conditionally (rather than relying on NewRegistry's
// nil-skip) avoids the typed-nil interface pitfall — a nil *PlaidProvider
// wrapped in a Provider interface is not == nil.
func BuildRegistry(cfg RegistryConfig) *Registry {
	providers := []Provider{NewCSVProvider()}

	if cfg.PlaidClientID != "" && cfg.PlaidSecret != "" {
		if p := NewPlaidProvider(PlaidConfig{
			ClientID:      cfg.PlaidClientID,
			Secret:        cfg.PlaidSecret,
			Env:           cfg.PlaidEnv,
			WebhookSecret: cfg.PlaidWebhookSecret,
		}, nil); p != nil {
			providers = append(providers, p)
		}
	}

	if cfg.GoCardlessSecretID != "" && cfg.GoCardlessSecretKey != "" {
		if p := NewGoCardlessProvider(GoCardlessConfig{
			SecretID:      cfg.GoCardlessSecretID,
			SecretKey:     cfg.GoCardlessSecretKey,
			InstitutionID: cfg.GoCardlessInstitutionID,
			WebhookSecret: cfg.GoCardlessWebhookSecret,
		}, nil); p != nil {
			providers = append(providers, p)
		}
	}

	return NewRegistry(providers...)
}
