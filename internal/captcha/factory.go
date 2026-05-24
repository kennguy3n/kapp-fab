package captcha

import (
	"errors"
	"fmt"
	"time"
)

// Config is the operator-supplied configuration the factory
// consumes. Mirrors the KAPP_CAPTCHA_* env-var surface in
// internal/platform/config.go so the wiring stays single-purpose:
// LoadConfig populates the strings, NewFromConfig validates and
// constructs the verifier.
type Config struct {
	// Provider selects which verifier to construct. One of:
	// "turnstile", "hcaptcha", "recaptcha_v3", "pow", "disabled".
	Provider string
	// Secret is the server-side credential for siteverify-style
	// providers (Turnstile, hCaptcha, reCAPTCHA v3). Ignored for
	// PoW and Disabled.
	Secret string
	// PoWHMACKey is the symmetric key used to sign PoW challenge
	// envelopes. Required for PoW provider; ignored for others.
	// MUST be at least 32 bytes; the factory rejects shorter
	// keys.
	PoWHMACKey []byte
	// PoWDifficulty is the number of leading zero bits required
	// in the solution hash. 0 → default 16.
	PoWDifficulty uint8
	// PoWExpiry is how long an issued PoW challenge is valid.
	// 0 → default 5 min.
	PoWExpiry time.Duration
	// MinScore is the reCAPTCHA v3 acceptance threshold (0.0
	// accepts every non-negative score, 1.0 accepts only
	// perfect-confidence). Ignored by the other providers.
	// Negative values are treated as "unset" and resolve to
	// the package default of 0.5; this lets operators
	// explicitly opt into a low threshold (MinScore=0) without
	// the previous "0 → 0.5" sentinel overriding their choice.
	MinScore float64
	// ExpectedHostname optionally pins the hostname the provider
	// reports as the token's origin. Empty disables the check.
	// Useful when multiple Kapp deployments share a site key by
	// accident.
	ExpectedHostname string
}

// NewFromConfig constructs a Verifier from the supplied Config.
// Returns an error when the config selects an unknown provider or
// when a required field is missing for the chosen provider.
//
// The factory deliberately accepts an empty Provider as a
// disabled verifier rather than an error — production deployments
// should set KAPP_CAPTCHA_PROVIDER explicitly, but bare-bones dev
// shells (`go run ./services/api`) shouldn't fail to boot purely
// because the operator hasn't picked a captcha provider yet. The
// boot logger should emit a WARN in this case (see the call site
// in deps_build.go).
func NewFromConfig(cfg Config) (Verifier, error) {
	switch cfg.Provider {
	case "", "disabled":
		return DisabledVerifier{}, nil
	case "turnstile":
		if cfg.Secret == "" {
			return nil, errors.New("captcha: turnstile provider requires KAPP_CAPTCHA_SECRET")
		}
		return NewTurnstileVerifier(cfg.Secret, Options{
			ExpectedHostname: cfg.ExpectedHostname,
		}), nil
	case "hcaptcha":
		if cfg.Secret == "" {
			return nil, errors.New("captcha: hcaptcha provider requires KAPP_CAPTCHA_SECRET")
		}
		return NewHCaptchaVerifier(cfg.Secret, Options{
			ExpectedHostname: cfg.ExpectedHostname,
		}), nil
	case "recaptcha_v3":
		if cfg.Secret == "" {
			return nil, errors.New("captcha: recaptcha_v3 provider requires KAPP_CAPTCHA_SECRET")
		}
		return NewRecaptchaV3Verifier(cfg.Secret, Options{
			ExpectedHostname: cfg.ExpectedHostname,
			MinScore:         cfg.MinScore,
		}), nil
	case "pow":
		if len(cfg.PoWHMACKey) < 32 {
			return nil, fmt.Errorf("captcha: pow provider requires KAPP_POW_HMAC_KEY of at least 32 bytes (got %d)", len(cfg.PoWHMACKey))
		}
		return NewPoWVerifier(cfg.PoWHMACKey, cfg.PoWDifficulty, cfg.PoWExpiry), nil
	default:
		return nil, fmt.Errorf("captcha: unknown provider %q", cfg.Provider)
	}
}
