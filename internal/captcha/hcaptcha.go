package captcha

import "context"

// HCaptchaVerifier checks hCaptcha tokens. hCaptcha is a drop-in
// reCAPTCHA replacement that doesn't share data with an ad-tech
// company; the siteverify endpoint format matches Turnstile so we
// share siteVerifyClient.
//
// Reference: https://docs.hcaptcha.com/#verify-the-user-response-server-side
const hcaptchaVerifyURL = "https://api.hcaptcha.com/siteverify"

// HCaptchaVerifier verifies hCaptcha tokens via siteverify. Wraps
// the shared siteVerifyClient and applies optional hostname
// pinning on success.
type HCaptchaVerifier struct {
	c    *siteVerifyClient
	opts Options
}

// NewHCaptchaVerifier returns an HCaptchaVerifier for the supplied
// secret. See TurnstileVerifier doc for the broader rationale on
// hostname binding.
func NewHCaptchaVerifier(secret string, opts Options) *HCaptchaVerifier {
	opts = opts.withDefaults()
	return &HCaptchaVerifier{
		c:    newSiteVerifyClient("hcaptcha", hcaptchaVerifyURL, secret, opts),
		opts: opts,
	}
}

// Provider returns the canonical provider name ("hcaptcha").
func (v *HCaptchaVerifier) Provider() string { return "hcaptcha" }

// Verify implements the Verifier interface. See the Verifier doc
// for the meaning of the two error/Outcome states; the additional
// post-check is hostname binding when ExpectedHostname is set.
func (v *HCaptchaVerifier) Verify(ctx context.Context, token, clientIP string) (Outcome, error) {
	out, err := v.c.verify(ctx, token, clientIP)
	if err != nil {
		return out, err
	}
	if !out.Success {
		return out, nil
	}
	if v.opts.ExpectedHostname != "" && out.Hostname != "" && out.Hostname != v.opts.ExpectedHostname {
		return Outcome{
			Success:    false,
			Hostname:   out.Hostname,
			ErrorCodes: []string{"hostname-mismatch"},
		}, nil
	}
	return out, nil
}
