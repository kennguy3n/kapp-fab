package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Passwordless delivery methods accepted by iam-core's Management
// passwordless API. email_otp sends a short numeric code; magic_link
// sends a clickable one-time link. The portal uses email_otp by
// default (it round-trips a code the customer pastes back, matching
// the existing magic-link verify UX without a second redirect).
const (
	PasswordlessEmailOTP   = "email_otp"
	PasswordlessMagicLink  = "magic_link"
	passwordlessSendPath   = "/passwordless/send"
	passwordlessVerifyPath = "/passwordless/verify"
)

// PasswordlessSendResult is the subset of iam-core's
// POST /management/passwordless/send reply kapp-fab consumes. iam-core
// deliberately returns a uniform response whether or not the email
// exists (anti-enumeration), so there is no user identifier here.
type PasswordlessSendResult struct {
	Success   bool   `json:"success"`
	ExpiresAt string `json:"expires_at"`
	ExpiresIn int64  `json:"expires_in"`
}

// PasswordlessVerifyResult is the subset of iam-core's
// POST /management/passwordless/verify reply we consume. Headless
// (M2M) verification returns the verified user identity rather than
// tokens — the caller mints its own session (the portal issues a
// portal-scoped JWT) from this identity.
type PasswordlessVerifyResult struct {
	Status   string `json:"status"`
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	TenantID string `json:"tenant_id"`
}

// SendPasswordless asks iam-core to deliver a passwordless OTP / magic
// link to the given email for the supplied iam-core tenant and client.
// iam-core owns delivery (its configured mailer), rate limiting, and
// anti-enumeration, so kapp-fab does not need its own SMTP for this
// path. method is one of PasswordlessEmailOTP / PasswordlessMagicLink.
//
// iamTenantID scopes the call to the right iam-core tenant via the
// Management API's X-Tenant-ID header; clientID is the OAuth2
// application the passwordless login is for (the Kapp web client).
func (c *IAMCoreClient) SendPasswordless(ctx context.Context, iamTenantID, clientID, email, method string) (*PasswordlessSendResult, error) {
	if c.m2m == nil {
		return nil, errors.New("auth: passwordless send requires an m2m client")
	}
	if iamTenantID == "" {
		return nil, errors.New("auth: passwordless send requires an iam tenant id")
	}
	if clientID == "" {
		return nil, errors.New("auth: passwordless send requires a client id")
	}
	method = normalizePasswordlessMethod(method)
	body := map[string]any{
		"email":     strings.TrimSpace(email),
		"method":    method,
		"client_id": clientID,
	}
	var out PasswordlessSendResult
	if err := c.do(ctx, http.MethodPost, c.mgmtBase+passwordlessSendPath, iamTenantID, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VerifyPasswordless verifies a passwordless code/token against
// iam-core for the given tenant and client. On success the returned
// result carries the verified user identity (Status == "verified");
// the caller turns that into its own session. method should match the
// one used in SendPasswordless; iam-core falls back to a length
// heuristic when empty.
func (c *IAMCoreClient) VerifyPasswordless(ctx context.Context, iamTenantID, clientID, email, code, method string) (*PasswordlessVerifyResult, error) {
	if c.m2m == nil {
		return nil, errors.New("auth: passwordless verify requires an m2m client")
	}
	if iamTenantID == "" {
		return nil, errors.New("auth: passwordless verify requires an iam tenant id")
	}
	if clientID == "" {
		return nil, errors.New("auth: passwordless verify requires a client id")
	}
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("auth: passwordless verify requires a code")
	}
	body := map[string]any{
		"email":     strings.TrimSpace(email),
		"code":      strings.TrimSpace(code),
		"client_id": clientID,
		"method":    normalizePasswordlessMethod(method),
	}
	var out PasswordlessVerifyResult
	if err := c.do(ctx, http.MethodPost, c.mgmtBase+passwordlessVerifyPath, iamTenantID, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PasswordlessClientID returns the OAuth2 client_id the passwordless
// flow runs under — the same confidential client used for the
// interactive login. Empty when no OAuth2 client is configured.
func (c *IAMCoreClient) PasswordlessClientID() string {
	if c.oauth2 == nil {
		return ""
	}
	return c.oauth2.cfg.ClientID
}

func normalizePasswordlessMethod(method string) string {
	method = strings.TrimSpace(method)
	switch method {
	case PasswordlessEmailOTP, PasswordlessMagicLink:
		return method
	default:
		return PasswordlessEmailOTP
	}
}
