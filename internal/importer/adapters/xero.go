package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

// XeroConfig is the JSON shape expected in ImportJob.Config for the
// Xero adapter. It targets the Xero Accounting API v2
// (https://api.xero.com/api.xro/2.0/).
//
// Auth uses an OAuth2 bearer token plus the mandatory Xero-Tenant-Id
// header that scopes the call to one Xero organisation. Callers either
// pass a still-valid AccessToken or supply RefreshToken +
// ClientID/ClientSecret for a refresh-token grant.
type XeroConfig struct {
	// BaseURL overrides the API host (mainly for tests). Defaults to
	// the production Accounting API base.
	BaseURL string `json:"base_url,omitempty"`
	// XeroTenantID is the connected organisation id sent on the
	// mandatory Xero-Tenant-Id header. Required.
	XeroTenantID string `json:"xero_tenant_id"`
	// AccessToken is an already-valid bearer token. Optional when
	// RefreshToken + client credentials are supplied.
	AccessToken string `json:"access_token,omitempty"`
	// RefreshToken + ClientID + ClientSecret drive the refresh-token
	// grant when AccessToken is empty.
	RefreshToken string `json:"refresh_token,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// TokenURL overrides the OAuth2 token endpoint. Defaults to Xero's
	// identity token endpoint.
	TokenURL string `json:"token_url,omitempty"`
	// Entities narrows the import to a subset. When empty the adapter
	// imports the full default set.
	Entities []XeroEntity `json:"entities,omitempty"`
	// ConceptMap layers per-entity field renames on top of the
	// adapter's built-in mapping table, keyed by Xero entity name.
	ConceptMap map[string]map[string]string `json:"concept_map,omitempty"`
	// LastSyncAt, when non-zero, is sent as the If-Modified-Since
	// header so Xero returns only rows changed since the last run.
	LastSyncAt time.Time `json:"last_sync_at,omitempty"`
}

// XeroEntity names one Xero entity to mirror plus an optional default
// target KType. Names are matched against the built-in spec table so
// pagination behaviour and the source-id field are known.
type XeroEntity struct {
	Name        string `json:"name"`
	TargetKType string `json:"target_ktype,omitempty"`
}

const (
	// defaultXeroBaseURL is the Xero Accounting API v2 base.
	defaultXeroBaseURL = "https://api.xero.com/api.xro/2.0"
	// defaultXeroOAuthURL is Xero's OAuth2 token endpoint.
	defaultXeroOAuthURL = "https://identity.xero.com/connect/token"
	// xeroPageSize is Xero's fixed page length for the paginated
	// endpoints. There is no server-side override on v2, so it doubles
	// as the "is this the last page?" threshold.
	xeroPageSize = 100
)

// xeroEntitySpec captures the per-entity behaviour the adapter needs:
// the envelope/path key, whether the endpoint paginates, whether it
// honours summaryOnly (a lighter projection used during the discovery
// counting pass), and which field holds the source id.
type xeroEntitySpec struct {
	targetKType string
	idField     string
	paginated   bool
	summaryOnly bool
}

// xeroEntitySpecs is the built-in catalogue of supported Xero
// entities. Items and Employees are not paginated by the v2 API, so we
// fetch them in a single call and never advance the page cursor (doing
// so would re-fetch the same rows forever).
var xeroEntitySpecs = map[string]xeroEntitySpec{
	"Contacts":         {targetKType: "crm.contact", idField: "ContactID", paginated: true, summaryOnly: true},
	"Invoices":         {targetKType: "finance.ar_invoice", idField: "InvoiceID", paginated: true, summaryOnly: true},
	"BankTransactions": {targetKType: "finance.bank_transaction", idField: "BankTransactionID", paginated: true},
	"ManualJournals":   {targetKType: "finance.journal_entry", idField: "ManualJournalID", paginated: true},
	"Items":            {targetKType: "inventory.item", idField: "ItemID"},
	"Employees":        {targetKType: "hr.employee", idField: "EmployeeID"},
}

// XeroAdapter mirrors entities from one Xero organisation into the
// importer staging table.
type XeroAdapter struct {
	client *http.Client
	tokens oauthTokenCache
}

// NewXeroAdapter returns the Xero source adapter wired with a bounded
// HTTP client.
func NewXeroAdapter() *XeroAdapter {
	return &XeroAdapter{client: &http.Client{Timeout: defaultHTTPTimeout}}
}

// WithHTTPClient swaps the HTTP client, letting tests point the adapter
// at an httptest server.
func (a *XeroAdapter) WithHTTPClient(c *http.Client) *XeroAdapter {
	a.client = c
	return a
}

// SourceType discriminates the adapter for registry lookup.
func (*XeroAdapter) SourceType() string { return SourceTypeXero }

// Discover establishes a per-entity source count. Xero's v2 API has no
// count endpoint, so the adapter performs a lightweight pass: paginated
// entities are walked page by page (with summaryOnly=true where the
// endpoint supports it to shrink the payload), and single-shot
// entities are fetched once. The counts feed the reconciler's
// source-vs-staged check.
func (a *XeroAdapter) Discover(ctx context.Context, raw json.RawMessage) (importer.DiscoverResult, error) {
	cfg, err := a.loadConfig(raw)
	if err != nil {
		return importer.DiscoverResult{}, err
	}
	token, notes, err := a.resolveToken(ctx, cfg)
	if err != nil {
		return importer.DiscoverResult{}, err
	}
	result := importer.DiscoverResult{Notes: notes}
	for _, ent := range cfg.Entities {
		spec := xeroEntitySpecs[ent.Name]
		var count int64
		err := a.eachRow(ctx, cfg, token, ent.Name, spec, spec.summaryOnly, func(map[string]any) error {
			count++
			return nil
		})
		if err != nil {
			return importer.DiscoverResult{}, fmt.Errorf("discover %s: %w", ent.Name, err)
		}
		result.Entities = append(result.Entities, importer.DiscoveredEntity{
			Name:     ent.Name,
			RowCount: count,
			TargetKT: ent.TargetKType,
		})
		result.TotalRows += count
	}
	return result, nil
}

// Export walks each entity, maps the Xero fields onto KType field
// names, and streams one NormalizedRow per record. The Xero record id
// (ContactID, InvoiceID, …) becomes the staging SourceID.
func (a *XeroAdapter) Export(ctx context.Context, raw json.RawMessage, emit func(importer.NormalizedRow) error) error {
	cfg, err := a.loadConfig(raw)
	if err != nil {
		return err
	}
	token, _, err := a.resolveToken(ctx, cfg)
	if err != nil {
		return err
	}
	for _, ent := range cfg.Entities {
		spec := xeroEntitySpecs[ent.Name]
		mapping := mergeFieldMaps(defaultXeroFieldMap[ent.Name], cfg.ConceptMap[ent.Name])
		err := a.eachRow(ctx, cfg, token, ent.Name, spec, false, func(row map[string]any) error {
			sourceID := stringID(row[spec.idField])
			return emit(importer.NormalizedRow{
				Entity:   ent.Name,
				SourceID: sourceID,
				Data:     applyFieldMap(row, mapping),
			})
		})
		if err != nil {
			return fmt.Errorf("export %s: %w", ent.Name, err)
		}
	}
	return nil
}

// eachRow invokes fn for every row of an entity, transparently handling
// pagination. Non-paginated entities are fetched once. The summaryOnly
// flag adds Xero's lighter projection, used only by the discovery
// counting pass.
func (a *XeroAdapter) eachRow(ctx context.Context, cfg XeroConfig, token, entity string, spec xeroEntitySpec, summaryOnly bool, fn func(map[string]any) error) error {
	page := 1
	for {
		rows, err := a.listPage(ctx, cfg, token, entity, page, summaryOnly)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := fn(row); err != nil {
				return err
			}
		}
		// Single-shot endpoints return everything in one response.
		if !spec.paginated {
			return nil
		}
		// A short (or empty) page means we have reached the end.
		if len(rows) < xeroPageSize {
			return nil
		}
		page++
	}
}

// listPage fetches one page of an entity and returns the decoded rows.
// The response envelope keys the row array on the entity name.
func (a *XeroAdapter) listPage(ctx context.Context, cfg XeroConfig, token, entity string, page int, summaryOnly bool) ([]map[string]any, error) {
	q := url.Values{}
	if xeroEntitySpecs[entity].paginated {
		q.Set("page", fmt.Sprintf("%d", page))
	}
	if summaryOnly {
		q.Set("summaryOnly", "true")
	}
	target := joinURL(a.baseURL(cfg), entity)
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	headers := map[string]string{"Xero-Tenant-Id": cfg.XeroTenantID}
	if !cfg.LastSyncAt.IsZero() {
		// Xero's delta mechanism is the HTTP If-Modified-Since header
		// (RFC1123 in UTC), not a query filter.
		headers["If-Modified-Since"] = cfg.LastSyncAt.UTC().Format(http.TimeFormat)
	}
	var resp map[string]json.RawMessage
	if err := getJSON(ctx, a.client, target, token, headers, &resp); err != nil {
		return nil, err
	}
	raw, ok := resp[entity]
	if !ok {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode %s rows: %w", entity, err)
	}
	return rows, nil
}

// resolveToken returns the bearer token for a run, refreshing when only
// a refresh token is supplied. Rotated refresh tokens surface on the
// returned notes so callers can persist them.
func (a *XeroAdapter) resolveToken(ctx context.Context, cfg XeroConfig) (token string, notes []string, err error) {
	if cfg.AccessToken != "" {
		return cfg.AccessToken, nil, nil
	}
	token, rotated, err := a.tokens.resolve(ctx, a.client, oauth2Config{
		TokenURL:     a.tokenURL(cfg),
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RefreshToken: cfg.RefreshToken,
		AuthStyle:    authStyleHeader,
	})
	if err != nil {
		return "", nil, fmt.Errorf("xero: %w", err)
	}
	if rotated {
		notes = append(notes, "xero: refresh token rotated; persist the new refresh_token for the next run")
	}
	return token, notes, nil
}

func (a *XeroAdapter) loadConfig(raw json.RawMessage) (XeroConfig, error) {
	var cfg XeroConfig
	if len(raw) == 0 {
		return cfg, fmt.Errorf("xero: config required")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("xero: parse config: %w", err)
	}
	if cfg.XeroTenantID == "" {
		return cfg, fmt.Errorf("xero: xero_tenant_id required")
	}
	if err := validateOAuthCreds("xero", cfg.AccessToken, cfg.RefreshToken, cfg.ClientID, cfg.ClientSecret); err != nil {
		return cfg, err
	}
	if len(cfg.Entities) == 0 {
		cfg.Entities = defaultXeroEntities()
	}
	for _, ent := range cfg.Entities {
		if _, ok := xeroEntitySpecs[ent.Name]; !ok {
			return cfg, fmt.Errorf("xero: unsupported entity %q", ent.Name)
		}
	}
	return cfg, nil
}

func (a *XeroAdapter) baseURL(cfg XeroConfig) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return defaultXeroBaseURL
}

func (a *XeroAdapter) tokenURL(cfg XeroConfig) string {
	if cfg.TokenURL != "" {
		return cfg.TokenURL
	}
	return defaultXeroOAuthURL
}

// defaultXeroEntities is the full set offered when the operator does
// not narrow the selection. Target KTypes follow the platform's module
// taxonomy; the default for Invoices is finance.ar_invoice (Xero
// returns both ACCREC and ACCPAY here — operators split them via a
// concept_map / mapping override when needed).
func defaultXeroEntities() []XeroEntity {
	names := []string{"Contacts", "Invoices", "BankTransactions", "ManualJournals", "Items", "Employees"}
	out := make([]XeroEntity, 0, len(names))
	for _, n := range names {
		out = append(out, XeroEntity{Name: n, TargetKType: xeroEntitySpecs[n].targetKType})
	}
	return out
}

// defaultXeroFieldMap maps Xero source fields onto KType field names
// per entity. Unmapped keys pass through verbatim.
var defaultXeroFieldMap = map[string]map[string]string{
	"Contacts":         {"Name": "name", "EmailAddress": "email", "FirstName": "first_name", "LastName": "last_name", "ContactNumber": "code"},
	"Invoices":         {"InvoiceNumber": "number", "Date": "issue_date", "DueDate": "due_date", "Total": "total", "Contact": "customer", "LineItems": "lines", "CurrencyCode": "currency"},
	"BankTransactions": {"Date": "date", "Total": "total", "Reference": "reference", "Type": "type", "Contact": "contact", "LineItems": "lines"},
	"ManualJournals":   {"Date": "date", "Narration": "memo", "JournalLines": "lines", "Status": "status"},
	"Items":            {"Code": "sku", "Name": "name", "Description": "description"},
	"Employees":        {"FirstName": "first_name", "LastName": "last_name", "Status": "status"},
}

// SuggestXeroFieldMapping surfaces a best-effort source→target field
// map for an entity the built-in table does not cover.
func SuggestXeroFieldMapping(sourceFields, targetFields []string) map[string]string {
	return SuggestFieldMapping(sourceFields, targetFields, 0.5)
}
