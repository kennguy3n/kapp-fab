package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// MultiVerifier routes a bearer token to the correct issuer-specific
// validator by inspecting its (unverified) `iss` claim, then delegates
// to that validator for cryptographic verification. It is the
// dual-issuer entry point the API middleware uses when a deployment
// enables iam-core: legacy KChat-minted HS256 tokens continue to flow
// through the existing *Signer, while iam-core RS256/ES256 tokens are
// validated against the JWKS endpoint.
//
// Reading `iss` before verification is safe: it only *selects* which
// key set to verify against. The chosen validator still performs the
// full signature + issuer + audience + expiry checks, so a token that
// lies about its issuer simply gets verified against the wrong keys
// and fails. There is no trust placed in the unverified claim beyond
// routing.
//
// A token whose issuer matches neither validator is rejected. When no
// iam-core validator is configured, MultiVerifier behaves exactly like
// the bare legacy signer (the iss-routing collapses to "always
// legacy"), preserving backward compatibility.
type MultiVerifier struct {
	// legacy verifies Kapp's own tokens (KChat SSO + refresh). It is
	// the fallback for any token whose issuer is not the iam-core
	// issuer, which includes legacy tokens that carry no `iss` claim
	// at all.
	legacy Verifier
	// iam verifies iam-core tokens. May be nil (legacy-only).
	iam *JWKSValidator
}

// NewMultiVerifier wires a dual-issuer verifier. legacy is required
// (it is the backward-compatible default path); iam may be nil, in
// which case every token is routed to legacy.
func NewMultiVerifier(legacy Verifier, iam *JWKSValidator) *MultiVerifier {
	return &MultiVerifier{legacy: legacy, iam: iam}
}

// Verify routes by issuer and delegates. When the iam-core validator
// is configured and the token's issuer equals the iam-core issuer,
// the token is verified as an iam-core token; otherwise it falls
// through to the legacy signer.
func (m *MultiVerifier) Verify(tok string) (*Claims, error) {
	if m.iam != nil {
		iss, err := unverifiedIssuer(tok)
		if err != nil {
			return nil, err
		}
		if iss != "" && iss == m.iam.Issuer() {
			return m.iam.Verify(tok)
		}
	}
	return m.legacy.Verify(tok)
}

// unverifiedIssuer extracts the `iss` claim from a compact-JWS payload
// WITHOUT verifying the signature. It is used solely to pick a
// validation path; the selected validator re-checks everything. A
// malformed token returns ErrTokenInvalid so the caller surfaces a
// 401 rather than leaking parse details.
func unverifiedIssuer(tok string) (string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", ErrTokenInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	var body struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return "", fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	return body.Iss, nil
}
