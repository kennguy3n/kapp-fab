package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// StripeAPI is the slice of Stripe the billing service depends on.
// Defining it as an interface (rather than calling *Client directly)
// keeps the service unit-testable with an in-process fake — the same
// approach internal/auth takes with KChatClient.
type StripeAPI interface {
	CreateCustomer(ctx context.Context, p CustomerParams) (string, error)
	CreateCheckoutSession(ctx context.Context, p CheckoutParams) (CheckoutSession, error)
	CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error)
	RecordUsage(ctx context.Context, p UsageParams) error
}

// CustomerParams is the input to CreateCustomer.
type CustomerParams struct {
	Email    string
	Name     string
	TenantID string // stored as customer metadata for cross-referencing
}

// CheckoutParams drives a subscription-mode Checkout session.
type CheckoutParams struct {
	CustomerID      string
	PriceID         string
	TrialPeriodDays int
	SuccessURL      string
	CancelURL       string
	TenantID        string // copied onto the subscription's metadata
}

// CheckoutSession is the subset of a Stripe Checkout Session we
// return to the caller — the hosted URL the browser is redirected to
// and the session id.
type CheckoutSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// UsageParams reports metered usage for a subscription item. Stripe
// aggregates usage records per billing period; Action="set" makes the
// daily sync idempotent (re-running the same day overwrites rather
// than doubling).
type UsageParams struct {
	SubscriptionItemID string
	Quantity           int64
	Timestamp          time.Time
	Action             string // "set" or "increment"; defaults to "set"
}

// Client is the live Stripe REST client. It posts form-encoded
// bodies with the secret key as HTTP basic-auth username (Stripe's
// documented auth scheme), matching the lightweight HTTP-client
// convention used by internal/captcha and internal/auth rather than
// pulling in the full stripe-go SDK.
type Client struct {
	secretKey  string
	apiBase    string
	httpClient *http.Client
}

// NewClient builds a Stripe client from cfg. The HTTP timeout is
// deliberately short-ish (15s): a Stripe call that hangs longer is
// indistinguishable from an outage at the request budget of the
// signup / subscribe handlers.
func NewClient(cfg Config) *Client {
	return &Client{
		secretKey:  cfg.SecretKey,
		apiBase:    cfg.APIBase,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// CreateCustomer creates a Stripe customer and returns its id.
func (c *Client) CreateCustomer(ctx context.Context, p CustomerParams) (string, error) {
	form := url.Values{}
	if p.Email != "" {
		form.Set("email", p.Email)
	}
	if p.Name != "" {
		form.Set("name", p.Name)
	}
	if p.TenantID != "" {
		form.Set("metadata[tenant_id]", p.TenantID)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/v1/customers", form, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// CreateCheckoutSession opens a subscription-mode Checkout session.
// The tenant id is copied onto subscription_data[metadata] so the
// webhook can resolve the owning tenant even before the
// billing_subscriptions row carries the Stripe subscription id.
func (c *Client) CreateCheckoutSession(ctx context.Context, p CheckoutParams) (CheckoutSession, error) {
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("customer", p.CustomerID)
	form.Set("line_items[0][price]", p.PriceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", p.SuccessURL)
	form.Set("cancel_url", p.CancelURL)
	if p.TrialPeriodDays > 0 {
		form.Set("subscription_data[trial_period_days]", strconv.Itoa(p.TrialPeriodDays))
	}
	if p.TenantID != "" {
		form.Set("subscription_data[metadata][tenant_id]", p.TenantID)
		form.Set("client_reference_id", p.TenantID)
	}
	var out CheckoutSession
	if err := c.post(ctx, "/v1/checkout/sessions", form, &out); err != nil {
		return CheckoutSession{}, err
	}
	return out, nil
}

// CreatePortalSession opens a Billing Portal session and returns the
// hosted URL the customer manages their subscription from.
func (c *Client) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	if returnURL != "" {
		form.Set("return_url", returnURL)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.post(ctx, "/v1/billing_portal/sessions", form, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

// RecordUsage posts a usage record against a metered subscription
// item. Action defaults to "set" so the daily sync is idempotent.
func (c *Client) RecordUsage(ctx context.Context, p UsageParams) error {
	action := p.Action
	if action == "" {
		action = "set"
	}
	form := url.Values{}
	form.Set("quantity", strconv.FormatInt(p.Quantity, 10))
	form.Set("action", action)
	ts := p.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	form.Set("timestamp", strconv.FormatInt(ts.UTC().Unix(), 10))
	path := fmt.Sprintf("/v1/subscription_items/%s/usage_records",
		url.PathEscape(p.SubscriptionItemID))
	return c.post(ctx, path, form, nil)
}

// post performs a form-encoded POST against the Stripe REST API. A
// non-2xx response is decoded into Stripe's standard error envelope
// and surfaced as a Go error. out may be nil when the caller does
// not need the body.
func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("billing: build stripe req: %w", err)
	}
	req.SetBasicAuth(c.secretKey, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("billing: stripe %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("billing: read stripe resp: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("billing: stripe %s status=%d: %s",
			path, resp.StatusCode, stripeErrorMessage(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("billing: decode stripe resp: %w", err)
		}
	}
	return nil
}

// stripeErrorMessage pulls the human-readable message out of Stripe's
// error envelope ({"error":{"message":"…"}}) for log/error context,
// falling back to the raw body when it doesn't parse.
func stripeErrorMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		if env.Error.Code != "" {
			return fmt.Sprintf("%s (%s)", env.Error.Message, env.Error.Code)
		}
		return env.Error.Message
	}
	return strings.TrimSpace(string(body))
}
