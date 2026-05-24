package captcha

import "context"

// RecaptchaV3Verifier checks Google reCAPTCHA v3 tokens. v3 differs
// from Turnstile / hCaptcha / reCAPTCHA v2 in three ways the
// verifier has to model:
//
//  1. The "challenge" is invisible — Google scores the request based
//     on the browser's behaviour profile without a user-facing
//     puzzle. The user therefore never knows they failed; the
//     verifier emits a score (0.0 bot ... 1.0 human) and the caller
//     decides where to draw the line.
//  2. There is no server-side freshness window. We enforce one
//     ourselves via siteVerifyClient.freshnessWindow (default 5
//     minutes) so a leaked token can't be replayed across a long
//     interval.
//  3. The token is bound to an action label the client claims at
//     mint time. The caller is expected to compare it against an
//     expected action server-side; this verifier exposes the
//     received action but does NOT enforce equality — the action
//     to expect depends on which endpoint the captcha is gating
//     (e.g. "login" vs "submit"), and pushing that into the
//     verifier would mean rebuilding the verifier per endpoint.
//
// Reference: https://developers.google.com/recaptcha/docs/v3
const recaptchaVerifyURL = "https://www.google.com/recaptcha/api/siteverify"

// RecaptchaV3Verifier verifies Google reCAPTCHA v3 tokens. Differs
// from Turnstile / hCaptcha in that it returns a continuous score
// (0.0 = bot, 1.0 = human) rather than a binary outcome; the
// score threshold is configurable per deployment.
type RecaptchaV3Verifier struct {
	c        *siteVerifyClient
	minScore float64
}

// NewRecaptchaV3Verifier returns a verifier for reCAPTCHA v3.
// minScore is the lower bound on Google's reported score below
// which the verifier denies even if the upstream API reports
// success=true. Google recommends 0.5 as a starting point; tune
// upward (0.7+) for high-value endpoints and downward (0.3) only
// after monitoring false-positive rates on real traffic.
//
// A negative minScore is treated as "unset" and replaced with the
// 0.5 default; this lets operators distinguish "I want the
// recommended default" (env var unset, parsed as -1 by
// getenvFloat) from "I want every score accepted, including 0.0"
// (KAPP_CAPTCHA_MIN_SCORE=0). Earlier revisions of this code used
// minScore == 0 as the "unset" sentinel, which prevented operators
// from explicitly opting into the lower bound — see Devin Review
// finding ANALYSIS_pr-review-job-104ce38940214afeb0aedce5b15ff028
// _0006.
func NewRecaptchaV3Verifier(secret string, minScore float64, opts Options) *RecaptchaV3Verifier {
	opts = opts.withDefaults()
	if minScore < 0 {
		minScore = 0.5
	}
	return &RecaptchaV3Verifier{
		c:        newSiteVerifyClient("recaptcha_v3", recaptchaVerifyURL, secret, opts),
		minScore: minScore,
	}
}

// Provider returns the canonical provider name ("recaptcha_v3").
func (v *RecaptchaV3Verifier) Provider() string { return "recaptcha_v3" }

// Verify implements the Verifier interface. In addition to the
// shared siteverify checks, a score below MinScore is treated as a
// soft deny (Outcome.Success=false, ErrorCode=score-below-threshold,
// no error returned) so the caller can branch on the soft-deny
// case without parsing the outcome's error chain.
func (v *RecaptchaV3Verifier) Verify(ctx context.Context, token, clientIP string) (Outcome, error) {
	out, err := v.c.verify(ctx, token, clientIP)
	if err != nil {
		return out, err
	}
	if !out.Success {
		return out, nil
	}
	if out.Score < v.minScore {
		// Below threshold — deny with a synthetic error code so
		// the audit log can distinguish "score too low" from
		// "upstream said no entirely". The score itself is
		// surfaced in the returned Outcome so the operator can
		// see where the line currently sits.
		denied := Outcome{
			Success:    false,
			Score:      out.Score,
			Action:     out.Action,
			Hostname:   out.Hostname,
			ErrorCodes: []string{"score-below-threshold"},
		}
		return denied, nil
	}
	return out, nil
}
