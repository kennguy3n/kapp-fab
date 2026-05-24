package captcha

import "context"

// TurnstileVerifier checks Cloudflare Turnstile tokens.  Turnstile
// is the lightest-weight production option: no third-party scripts
// beyond Cloudflare's own (which most operators already trust as
// CDN), no per-solve user friction in the common case (managed
// challenge transparently inspects the browser fingerprint), and
// no Google-tracking concerns. Tokens are short-lived (~2 min) and
// the siteverify protocol mirrors hCaptcha so the two providers
// share the siteVerifyClient implementation.
//
// Reference: https://developers.cloudflare.com/turnstile/get-started/server-side-validation/
const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// TurnstileVerifier verifies Cloudflare Turnstile tokens.  Wraps
// the shared siteVerifyClient with Turnstile's siteverify URL and
// applies optional hostname pinning on success.
type TurnstileVerifier struct {
	c    *siteVerifyClient
	opts Options
}

// NewTurnstileVerifier returns a TurnstileVerifier for the supplied
// secret key. The secret is the server-side credential Cloudflare
// generates alongside the site key; the site key is rendered in
// the frontend and is not used by the verifier. Empty secret is
// rejected at construction so a misconfigured deployment fails the
// boot instead of silently accepting every token.
func NewTurnstileVerifier(secret string, opts Options) *TurnstileVerifier {
	opts = opts.withDefaults()
	return &TurnstileVerifier{
		c:    newSiteVerifyClient("turnstile", turnstileVerifyURL, secret, opts),
		opts: opts,
	}
}

// Provider returns the canonical provider name ("turnstile").
func (v *TurnstileVerifier) Provider() string { return "turnstile" }

// Verify implements the Verifier interface. The post-check is the
// optional ExpectedHostname pin documented on TurnstileVerifier;
// see the Verifier doc for the error/Outcome contract.
func (v *TurnstileVerifier) Verify(ctx context.Context, token, clientIP string) (Outcome, error) {
	out, err := v.c.verify(ctx, token, clientIP)
	if err != nil {
		return out, err
	}
	if !out.Success {
		return out, nil
	}
	// Hostname binding: when an expected hostname is configured,
	// Turnstile's siteverify reports the origin that minted the
	// token. A mismatch indicates either a token reused from a
	// different deployment (likely accidental, but a security
	// signal regardless) or a phishing site that proxied our site
	// key. Either way, deny.
	if v.opts.ExpectedHostname != "" && out.Hostname != "" && out.Hostname != v.opts.ExpectedHostname {
		return Outcome{
			Success:    false,
			Hostname:   out.Hostname,
			ErrorCodes: []string{"hostname-mismatch"},
		}, nil
	}
	return out, nil
}
