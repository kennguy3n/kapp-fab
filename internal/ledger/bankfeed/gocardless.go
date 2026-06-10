package bankfeed

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// GoCardlessConfig holds the GoCardless Bank Account Data (ex-Nordigen)
// API credentials. These are institution-level secrets shared across all
// of a deployment's connections (not per-tenant), sourced from
// internal/platform/config.go and fail-closed in production.
type GoCardlessConfig struct {
	SecretID  string
	SecretKey string
	// InstitutionID is the default bank to link when InitiateConnect is
	// called without one in context (EU/UK SMEs typically have a single
	// bank). Optional; a real deployment resolves it from a picker.
	InstitutionID string
	BaseURL       string // override for tests; defaults to the GC prod host
}

// GoCardlessProvider implements Provider against the GoCardless Bank
// Account Data API, giving EU/UK Open Banking coverage. The flow is:
// create a requisition (InitiateConnect) → user consents at their bank →
// read the linked account id from the requisition (CompleteConnect) →
// pull booked transactions (FetchTransactions).
//
// GoCardless issues short-lived bearer tokens from the secret pair; the
// provider obtains and caches one in-memory rather than persisting it
// per connection, so a token rotation needs no DB write.
type GoCardlessProvider struct {
	cfg    GoCardlessConfig
	client httpDoer

	mu          sync.Mutex
	accessToken string
	tokenExp    time.Time
	now         func() time.Time
}

// NewGoCardlessProvider returns a configured provider, or nil when the
// secret pair is absent (so the registry omits it).
func NewGoCardlessProvider(cfg GoCardlessConfig, client httpDoer) *GoCardlessProvider {
	if cfg.SecretID == "" || cfg.SecretKey == "" {
		return nil
	}
	if client == nil {
		client = defaultHTTPClient
	}
	return &GoCardlessProvider{cfg: cfg, client: client, now: func() time.Time { return time.Now().UTC() }}
}

// Name returns the provider key used in the registry and persisted on
// each connection row.
func (p *GoCardlessProvider) Name() string { return ProviderGoCardless }

func (p *GoCardlessProvider) baseURL() string {
	if p.cfg.BaseURL != "" {
		return strings.TrimRight(p.cfg.BaseURL, "/")
	}
	return "https://bankaccountdata.gocardless.com/api/v2"
}

// bearer returns a valid access token, refreshing from the secret pair
// when the cached one is missing or within 60s of expiry.
func (p *GoCardlessProvider) bearer(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accessToken != "" && p.now().Before(p.tokenExp.Add(-60*time.Second)) {
		return p.accessToken, nil
	}
	var out struct {
		Access         string `json:"access"`
		AccessExpires  int    `json:"access_expires"`
		Refresh        string `json:"refresh"`
		RefreshExpires int    `json:"refresh_expires"`
	}
	body := map[string]any{"secret_id": p.cfg.SecretID, "secret_key": p.cfg.SecretKey}
	if err := postJSON(ctx, p.client, p.baseURL()+"/token/new/", nil, body, &out); err != nil {
		return "", err
	}
	if out.Access == "" {
		return "", fmt.Errorf("bankfeed: gocardless returned empty access token")
	}
	p.accessToken = out.Access
	exp := out.AccessExpires
	if exp <= 0 {
		exp = 86400
	}
	p.tokenExp = p.now().Add(time.Duration(exp) * time.Second)
	return p.accessToken, nil
}

func (p *GoCardlessProvider) authHeader(ctx context.Context) (map[string]string, error) {
	tok, err := p.bearer(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + tok}, nil
}

// InitiateConnect creates a requisition and returns the bank-consent
// link. The tenant+account is encoded into the requisition reference so
// the callback can correlate it back; CompleteConnect reads the linked
// account id from that requisition.
func (p *GoCardlessProvider) InitiateConnect(ctx context.Context, tenantID, bankAccountID uuid.UUID, redirectURI string) (string, error) {
	hdr, err := p.authHeader(ctx)
	if err != nil {
		return "", err
	}
	if p.cfg.InstitutionID == "" {
		return "", fmt.Errorf("bankfeed: gocardless institution_id not configured")
	}
	body := map[string]any{
		"redirect":       redirectURI,
		"institution_id": p.cfg.InstitutionID,
		"reference":      fmt.Sprintf("%s:%s", tenantID, bankAccountID),
		"user_language":  "EN",
	}
	var out struct {
		ID   string `json:"id"`
		Link string `json:"link"`
	}
	if err := postJSON(ctx, p.client, p.baseURL()+"/requisitions/", hdr, body, &out); err != nil {
		return "", err
	}
	if out.Link == "" {
		return "", fmt.Errorf("bankfeed: gocardless returned empty link")
	}
	// The frontend needs the requisition id to call back with; encode it
	// alongside the link so the connect route can stash it. The link is
	// what the user opens.
	return out.Link + "#requisition=" + out.ID, nil
}

// CompleteConnect reads the linked account id from the requisition
// referenced by code (the requisition id). The first account becomes the
// connection's external account; multi-account requisitions would create
// one connection per account in a fuller build.
func (p *GoCardlessProvider) CompleteConnect(ctx context.Context, tenantID uuid.UUID, code string) (*Connection, error) {
	if code == "" {
		return nil, fmt.Errorf("bankfeed: gocardless requisition id required")
	}
	hdr, err := p.authHeader(ctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		ID       string   `json:"id"`
		Accounts []string `json:"accounts"`
		Status   string   `json:"status"`
	}
	if err := getJSON(ctx, p.client, p.baseURL()+"/requisitions/"+code+"/", hdr, &out); err != nil {
		return nil, err
	}
	if len(out.Accounts) == 0 {
		return nil, fmt.Errorf("bankfeed: gocardless requisition %s has no linked accounts (status %s)", code, out.Status)
	}
	// Provider asymmetry worth flagging: for Plaid, AccessToken holds an
	// actual secret OAuth token. For GoCardless the per-request bearer is
	// derived from the secret pair and cached in-memory (never persisted),
	// so what we store here is the *account resource id* — a non-secret
	// public identifier, not a credential. It rides the encrypted
	// AccessToken column purely so FetchTransactions has one field to read;
	// the encryption is harmless defense-in-depth rather than a requirement.
	return &Connection{
		TenantID:    tenantID,
		Provider:    ProviderGoCardless,
		AccessToken: out.Accounts[0], // GC account resource id (not a secret)
		ExternalID:  code,            // requisition id, for Disconnect
		Status:      StatusActive,
	}, nil
}

// gcTransactionsResponse mirrors the subset we consume.
type gcTransactionsResponse struct {
	Transactions struct {
		Booked  []gcTxn `json:"booked"`
		Pending []gcTxn `json:"pending"`
	} `json:"transactions"`
}

type gcTxn struct {
	TransactionID     string `json:"transactionId"`
	BookingDate       string `json:"bookingDate"`
	ValueDate         string `json:"valueDate"`
	TransactionAmount struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"transactionAmount"`
	RemittanceInformationUnstructured string `json:"remittanceInformationUnstructured"`
	CreditorName                      string `json:"creditorName"`
	DebtorName                        string `json:"debtorName"`
}

// FetchTransactions pulls booked transactions for the linked account.
// GoCardless does not expose an incremental cursor on this endpoint, so
// the caller's `since` bounds the window client-side and the returned
// cursor is the max booking date seen (ISO date) for next-sync filtering.
func (p *GoCardlessProvider) FetchTransactions(ctx context.Context, conn *Connection, since time.Time) ([]RawTransaction, string, error) {
	if conn == nil || conn.AccessToken == "" {
		return nil, "", fmt.Errorf("bankfeed: gocardless connection missing account id")
	}
	hdr, err := p.authHeader(ctx)
	if err != nil {
		return nil, "", err
	}
	// Prefer the connection cursor (last booking date) over `since` so a
	// re-sync after a manual backfill stays incremental.
	from := since
	if conn.Cursor != "" {
		if c, err := time.Parse("2006-01-02", conn.Cursor); err == nil {
			from = c
		}
	}
	url := p.baseURL() + "/accounts/" + conn.AccessToken + "/transactions/"
	if !from.IsZero() {
		url += "?date_from=" + from.UTC().Format("2006-01-02")
	}
	var resp gcTransactionsResponse
	if err := getJSON(ctx, p.client, url, hdr, &resp); err != nil {
		return nil, "", err
	}
	out := make([]RawTransaction, 0, len(resp.Transactions.Booked))
	maxDate := conn.Cursor
	for i := range resp.Transactions.Booked {
		t := &resp.Transactions.Booked[i]
		dateStr := t.BookingDate
		if dateStr == "" {
			dateStr = t.ValueDate
		}
		vd, dateErr := time.Parse("2006-01-02", dateStr)
		// Advance the cursor watermark from any well-formed booking date —
		// even for a line we skip below for a bad amount. Otherwise a single
		// permanently-malformed latest line pins the cursor at the prior max
		// and forces a full re-pull of the window on every hourly tick. The
		// skipped line still falls within the inclusive next date_from, so a
		// provider-side amount fix is still picked up. Guarding on a clean
		// parse also ensures a garbage date string can never corrupt the
		// cursor (it would otherwise fail to parse on the next sync and reset
		// incrementality).
		if dateErr == nil && dateStr > maxDate {
			maxDate = dateStr
		}
		amt, err := decimal.NewFromString(t.TransactionAmount.Amount)
		if err != nil {
			continue // skip malformed amount rather than fail the whole sync
		}
		if dateErr != nil {
			vd = p.now()
		}
		counterparty := t.CreditorName
		if counterparty == "" {
			counterparty = t.DebtorName
		}
		out = append(out, RawTransaction{
			ExternalID:   t.TransactionID,
			ValueDate:    vd.UTC(),
			Description:  t.RemittanceInformationUnstructured,
			Amount:       amt,
			Currency:     t.TransactionAmount.Currency,
			Counterparty: counterparty,
		})
	}
	return out, maxDate, nil
}

// Disconnect deletes the requisition, withdrawing consent at the bank.
func (p *GoCardlessProvider) Disconnect(ctx context.Context, conn *Connection) error {
	if conn == nil || conn.ExternalID == "" {
		return nil
	}
	hdr, err := p.authHeader(ctx)
	if err != nil {
		return err
	}
	return deleteResource(ctx, p.client, p.baseURL()+"/requisitions/"+conn.ExternalID+"/", hdr)
}
