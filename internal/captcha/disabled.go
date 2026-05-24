package captcha

import "context"

// DisabledVerifier always returns Success=true. It exists so the
// HTTP gateway can wire captcha middleware unconditionally and
// have it become a no-op when captcha is disabled in config —
// rather than requiring conditional `if cfg.captcha != nil` checks
// at every route registration.
//
// Production deployments MUST NOT use DisabledVerifier; the
// factory wires it only when the operator explicitly sets
// KAPP_CAPTCHA_PROVIDER=disabled. The boot log emits a WARN when
// this happens so the choice is auditable in operator logs.
type DisabledVerifier struct{}

// Provider satisfies Verifier. Returns the literal "disabled" so
// the boot logger and the captcha middleware can branch on it.
func (DisabledVerifier) Provider() string { return "disabled" }

// Verify satisfies Verifier. Always returns Outcome{Success: true}
// with no error so the captcha middleware is a no-op pass-through
// when KAPP_CAPTCHA_PROVIDER is unset or "disabled".
func (DisabledVerifier) Verify(_ context.Context, _, _ string) (Outcome, error) {
	return Outcome{Success: true}, nil
}
