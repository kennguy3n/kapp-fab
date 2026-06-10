package bankfeed

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// PlaidConfig holds the credentials and environment selection for the
// Plaid provider. Sourced from internal/platform/config.go (PlaidClientID
// / PlaidSecret / PlaidEnv) which fail-closed in production when set
// without a secret. CountryCodes defaults to US when empty.
type PlaidConfig struct {
	ClientID     string
	Secret       string
	Env          string // "sandbox" | "development" | "production"
	CountryCodes []string
	ClientName   string
	// BaseURL overrides the env-derived host; used by tests to point at
	// an httptest server. Empty resolves from Env.
	BaseURL string
}

// PlaidProvider implements Provider against Plaid's REST API: Link token
// creation, public_token exchange, and the incremental /transactions/sync
// cursor protocol.
type PlaidProvider struct {
	cfg    PlaidConfig
	client httpDoer
}

// NewPlaidProvider returns a configured Plaid provider, or nil when the
// credentials are absent so the registry simply omits Plaid. Returning
// nil (rather than an error) lets a tenant on a non-Plaid plan boot
// cleanly; the production config gate is what enforces fail-closed when
// an operator *intends* Plaid to be on.
func NewPlaidProvider(cfg PlaidConfig, client httpDoer) *PlaidProvider {
	if cfg.ClientID == "" || cfg.Secret == "" {
		return nil
	}
	if cfg.ClientName == "" {
		cfg.ClientName = "KApp"
	}
	if len(cfg.CountryCodes) == 0 {
		cfg.CountryCodes = []string{"US"}
	}
	if client == nil {
		client = defaultHTTPClient
	}
	return &PlaidProvider{cfg: cfg, client: client}
}

// Name returns the provider key used in the registry and persisted on
// each connection row.
func (p *PlaidProvider) Name() string { return ProviderPlaid }

// baseURL resolves the Plaid host for the configured environment.
func (p *PlaidProvider) baseURL() string {
	if p.cfg.BaseURL != "" {
		return strings.TrimRight(p.cfg.BaseURL, "/")
	}
	switch strings.ToLower(p.cfg.Env) {
	case "production", "prod":
		return "https://production.plaid.com"
	case "development":
		return "https://development.plaid.com"
	default:
		return "https://sandbox.plaid.com"
	}
}

// auth is the client_id/secret pair Plaid expects in every request body.
func (p *PlaidProvider) auth() map[string]any {
	return map[string]any{"client_id": p.cfg.ClientID, "secret": p.cfg.Secret}
}

// InitiateConnect creates a Link token. The frontend opens Plaid Link
// with the returned token; on success Link yields a public_token the
// frontend posts back to the callback route, which calls CompleteConnect.
func (p *PlaidProvider) InitiateConnect(ctx context.Context, tenantID, _ uuid.UUID, redirectURI string) (string, error) {
	body := p.auth()
	body["client_name"] = p.cfg.ClientName
	body["language"] = "en"
	body["country_codes"] = p.cfg.CountryCodes
	body["products"] = []string{"transactions"}
	// client_user_id scopes the Link session to the tenant without
	// leaking a real user identifier to Plaid.
	body["user"] = map[string]any{"client_user_id": tenantID.String()}
	if redirectURI != "" {
		body["redirect_uri"] = redirectURI
	}
	var out struct {
		LinkToken string `json:"link_token"`
	}
	if err := postJSON(ctx, p.client, p.baseURL()+"/link/token/create", nil, body, &out); err != nil {
		return "", err
	}
	if out.LinkToken == "" {
		return "", fmt.Errorf("bankfeed: plaid returned empty link_token")
	}
	return out.LinkToken, nil
}

// CompleteConnect exchanges the public_token for a durable access_token
// and item_id. The returned Connection carries the access_token in
// memory; the caller persists it (the store encrypts it at rest).
func (p *PlaidProvider) CompleteConnect(ctx context.Context, tenantID uuid.UUID, code string) (*Connection, error) {
	if code == "" {
		return nil, fmt.Errorf("bankfeed: plaid public_token required")
	}
	body := p.auth()
	body["public_token"] = code
	var out struct {
		AccessToken string `json:"access_token"`
		ItemID      string `json:"item_id"`
	}
	if err := postJSON(ctx, p.client, p.baseURL()+"/item/public_token/exchange", nil, body, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("bankfeed: plaid returned empty access_token")
	}
	return &Connection{
		TenantID:    tenantID,
		Provider:    ProviderPlaid,
		AccessToken: out.AccessToken,
		ExternalID:  out.ItemID,
		Status:      StatusActive,
	}, nil
}

// plaidSyncResponse mirrors the subset of /transactions/sync we consume.
//
// We deliberately decode only `added` (and advance past `modified`/`removed`
// via next_cursor). Applying modified/removed means mutating or voiding a
// bank_transaction that may already be reconciled against a journal entry —
// a financially-sensitive operation needing an update/void path, audit, and
// reconciliation-state unwinding that the INSERT-on-conflict-do-nothing ingest
// here intentionally does not perform. Tracked as a follow-up alongside the
// rule-driven auto-poster; the cursor still advances so no page is re-walked.
type plaidSyncResponse struct {
	Added []struct {
		TransactionID string  `json:"transaction_id"`
		Date          string  `json:"date"`
		Name          string  `json:"name"`
		MerchantName  string  `json:"merchant_name"`
		Amount        float64 `json:"amount"`
		ISOCurrency   string  `json:"iso_currency_code"`
		Pending       bool    `json:"pending"`
	} `json:"added"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// FetchTransactions walks /transactions/sync from the connection cursor
// until has_more is false, accumulating added lines. Plaid's amount sign
// convention is the inverse of ours (Plaid: positive = money out), so we
// negate. Pending lines are passed through with the Pending flag set; the
// sync handler decides to skip them.
func (p *PlaidProvider) FetchTransactions(ctx context.Context, conn *Connection, _ time.Time) ([]RawTransaction, string, error) {
	if conn == nil {
		return nil, "", fmt.Errorf("bankfeed: nil connection")
	}
	cursor := conn.Cursor
	var out []RawTransaction
	// Bound the page walk so a pathological has_more loop cannot run
	// unbounded inside a sync tick.
	for page := 0; page < 50; page++ {
		body := p.auth()
		body["access_token"] = conn.AccessToken
		if cursor != "" {
			body["cursor"] = cursor
		}
		var resp plaidSyncResponse
		if err := postJSON(ctx, p.client, p.baseURL()+"/transactions/sync", nil, body, &resp); err != nil {
			return nil, "", err
		}
		for _, a := range resp.Added {
			vd, err := time.Parse("2006-01-02", a.Date)
			if err != nil {
				vd = time.Now().UTC()
			}
			desc := a.Name
			if desc == "" {
				desc = a.MerchantName
			}
			out = append(out, RawTransaction{
				ExternalID:   a.TransactionID,
				ValueDate:    vd.UTC(),
				Description:  desc,
				Amount:       decimal.NewFromFloat(a.Amount).Neg(),
				Currency:     a.ISOCurrency,
				Counterparty: a.MerchantName,
				Pending:      a.Pending,
			})
		}
		cursor = resp.NextCursor
		if !resp.HasMore {
			break
		}
	}
	return out, cursor, nil
}

// Disconnect removes the Item at Plaid so the access_token stops billing
// and the consent is withdrawn.
func (p *PlaidProvider) Disconnect(ctx context.Context, conn *Connection) error {
	if conn == nil || conn.AccessToken == "" {
		return nil
	}
	body := p.auth()
	body["access_token"] = conn.AccessToken
	return postJSON(ctx, p.client, p.baseURL()+"/item/remove", nil, body, nil)
}
