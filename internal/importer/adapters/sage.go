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

// SageConfig is the JSON shape expected in ImportJob.Config for the
// Sage adapter. It targets the Sage Business Cloud Accounting API v3.1
// (https://api.accounting.sage.com/v3.1/).
//
// Auth uses an OAuth2 bearer token. Callers either pass a still-valid
// AccessToken or supply RefreshToken + ClientID/ClientSecret for a
// refresh-token grant. Sage presents client credentials in the token
// request body rather than as Basic auth.
type SageConfig struct {
	// BaseURL overrides the API host (mainly for tests). Defaults to
	// the production Accounting API base.
	BaseURL string `json:"base_url,omitempty"`
	// AccessToken is an already-valid bearer token. Optional when
	// RefreshToken + client credentials are supplied.
	AccessToken string `json:"access_token,omitempty"`
	// RefreshToken + ClientID + ClientSecret drive the refresh-token
	// grant when AccessToken is empty.
	RefreshToken string `json:"refresh_token,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// TokenURL overrides the OAuth2 token endpoint. Defaults to Sage's
	// token endpoint.
	TokenURL string `json:"token_url,omitempty"`
	// Entities narrows the import to a subset. When empty the adapter
	// imports the full default set.
	Entities []SageEntity `json:"entities,omitempty"`
	// PageSize bounds items_per_page. Sage caps it at 200; larger
	// values are clamped.
	PageSize int `json:"page_size,omitempty"`
	// ConceptMap layers per-entity field renames on top of the
	// adapter's built-in mapping table, keyed by Sage entity name.
	ConceptMap map[string]map[string]string `json:"concept_map,omitempty"`
	// LastSyncAt, when non-zero, is passed as the
	// updated_or_created_since query parameter for incremental sync.
	LastSyncAt time.Time `json:"last_sync_at,omitempty"`
}

// SageEntity names one Sage resource collection to mirror plus an
// optional default target KType. The name is the API path segment
// (e.g. "sales_invoices").
type SageEntity struct {
	Name        string `json:"name"`
	TargetKType string `json:"target_ktype,omitempty"`
}

const (
	// defaultSageBaseURL is the Sage Accounting API v3.1 base.
	defaultSageBaseURL = "https://api.accounting.sage.com/v3.1"
	// defaultSageOAuthURL is Sage's OAuth2 token endpoint.
	defaultSageOAuthURL = "https://oauth.accounting.sage.com/token"
	// defaultSagePageSize is the items_per_page step.
	defaultSagePageSize = 100
	// maxSagePageSize is Sage's documented ceiling for items_per_page.
	maxSagePageSize = 200
)

// sageList is the paged-collection envelope Sage returns. The `$total`
// field lets Discover establish a count without walking every page.
type sageList struct {
	Total        int64            `json:"$total"`
	Page         int              `json:"$page"`
	ItemsPerPage int              `json:"$items_per_page"`
	Next         string           `json:"$next"`
	Items        []map[string]any `json:"$items"`
}

// sageEntityIDField names the source-id field per entity; Sage uses a
// stable string "id" on every resource.
const sageEntityIDField = "id"

// SageAdapter mirrors collections from a Sage Business Cloud Accounting
// company into the importer staging table.
type SageAdapter struct {
	client *http.Client
}

// NewSageAdapter returns the Sage source adapter wired with a bounded
// HTTP client.
func NewSageAdapter() *SageAdapter {
	return &SageAdapter{client: &http.Client{Timeout: defaultHTTPTimeout}}
}

// WithHTTPClient swaps the HTTP client, letting tests point the adapter
// at an httptest server.
func (a *SageAdapter) WithHTTPClient(c *http.Client) *SageAdapter {
	a.client = c
	return a
}

// SourceType discriminates the adapter for registry lookup.
func (*SageAdapter) SourceType() string { return SourceTypeSage }

// Discover reads each collection's `$total` with a single
// items_per_page=1 request, giving the reconciler an accurate
// source-side count cheaply.
func (a *SageAdapter) Discover(ctx context.Context, raw json.RawMessage) (importer.DiscoverResult, error) {
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
		list, err := a.listPage(ctx, cfg, token, ent.Name, 1, 1)
		if err != nil {
			return importer.DiscoverResult{}, fmt.Errorf("discover %s: %w", ent.Name, err)
		}
		result.Entities = append(result.Entities, importer.DiscoveredEntity{
			Name:     ent.Name,
			RowCount: list.Total,
			TargetKT: ent.TargetKType,
		})
		result.TotalRows += list.Total
	}
	return result, nil
}

// Export paginates each collection, maps the Sage fields onto KType
// field names, and streams one NormalizedRow per record. Sage's `id`
// becomes the staging SourceID.
func (a *SageAdapter) Export(ctx context.Context, raw json.RawMessage, emit func(importer.NormalizedRow) error) error {
	cfg, err := a.loadConfig(raw)
	if err != nil {
		return err
	}
	token, _, err := a.resolveToken(ctx, cfg)
	if err != nil {
		return err
	}
	pageSize := a.pageSize(cfg)
	for _, ent := range cfg.Entities {
		mapping := mergeFieldMaps(defaultSageFieldMap[ent.Name], cfg.ConceptMap[ent.Name])
		page := 1
		for {
			list, err := a.listPage(ctx, cfg, token, ent.Name, page, pageSize)
			if err != nil {
				return fmt.Errorf("export %s: %w", ent.Name, err)
			}
			for _, row := range list.Items {
				sourceID, _ := row[sageEntityIDField].(string)
				if err := emit(importer.NormalizedRow{
					Entity:   ent.Name,
					SourceID: sourceID,
					Data:     applyFieldMap(row, mapping),
				}); err != nil {
					return err
				}
			}
			// Stop when Sage signals no further page, or when the page
			// came back short of the requested size.
			if list.Next == "" || len(list.Items) < pageSize {
				break
			}
			page++
		}
	}
	return nil
}

// listPage fetches one page of a Sage collection.
func (a *SageAdapter) listPage(ctx context.Context, cfg SageConfig, token, entity string, page, perPage int) (sageList, error) {
	q := url.Values{}
	q.Set("items_per_page", fmt.Sprintf("%d", perPage))
	q.Set("page", fmt.Sprintf("%d", page))
	if !cfg.LastSyncAt.IsZero() {
		q.Set("updated_or_created_since", cfg.LastSyncAt.UTC().Format(time.RFC3339))
	}
	target := joinURL(a.baseURL(cfg), entity) + "?" + q.Encode()
	var list sageList
	if err := getJSON(ctx, a.client, target, token, nil, &list); err != nil {
		return sageList{}, err
	}
	return list, nil
}

// resolveToken returns the bearer token for a run, refreshing when only
// a refresh token is supplied. Sage rotates refresh tokens, so a
// rotation note surfaces on the returned slice.
func (a *SageAdapter) resolveToken(ctx context.Context, cfg SageConfig) (token string, notes []string, err error) {
	if cfg.AccessToken != "" {
		return cfg.AccessToken, nil, nil
	}
	tok, err := refreshOAuth2Token(ctx, a.client, oauth2Config{
		TokenURL:     a.tokenURL(cfg),
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RefreshToken: cfg.RefreshToken,
		AuthStyle:    authStyleBody,
	})
	if err != nil {
		return "", nil, fmt.Errorf("sage: %w", err)
	}
	if tok.RefreshToken != "" && tok.RefreshToken != cfg.RefreshToken {
		notes = append(notes, "sage: refresh token rotated; persist the new refresh_token for the next run")
	}
	return tok.AccessToken, notes, nil
}

func (a *SageAdapter) loadConfig(raw json.RawMessage) (SageConfig, error) {
	var cfg SageConfig
	if len(raw) == 0 {
		return cfg, fmt.Errorf("sage: config required")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("sage: parse config: %w", err)
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		return cfg, fmt.Errorf("sage: access_token or refresh_token required")
	}
	if len(cfg.Entities) == 0 {
		cfg.Entities = defaultSageEntities()
	}
	return cfg, nil
}

func (a *SageAdapter) baseURL(cfg SageConfig) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return defaultSageBaseURL
}

func (a *SageAdapter) tokenURL(cfg SageConfig) string {
	if cfg.TokenURL != "" {
		return cfg.TokenURL
	}
	return defaultSageOAuthURL
}

func (a *SageAdapter) pageSize(cfg SageConfig) int {
	size := cfg.PageSize
	if size <= 0 {
		size = defaultSagePageSize
	}
	if size > maxSagePageSize {
		size = maxSagePageSize
	}
	return size
}

// defaultSageEntities is the full set offered when the operator does
// not narrow the selection. Target KTypes follow the platform's module
// taxonomy.
func defaultSageEntities() []SageEntity {
	return []SageEntity{
		{Name: "contacts", TargetKType: "crm.contact"},
		{Name: "sales_invoices", TargetKType: "finance.ar_invoice"},
		{Name: "purchase_invoices", TargetKType: "finance.ap_bill"},
		{Name: "ledger_accounts", TargetKType: "finance.account"},
		{Name: "products", TargetKType: "inventory.item"},
	}
}

// defaultSageFieldMap maps Sage source fields onto KType field names
// per entity. Unmapped keys pass through verbatim.
var defaultSageFieldMap = map[string]map[string]string{
	"contacts":          {"displayed_as": "name", "reference": "code", "email": "email"},
	"sales_invoices":    {"invoice_number": "number", "date": "issue_date", "due_date": "due_date", "total_amount": "total", "contact": "customer", "invoice_lines": "lines", "currency": "currency"},
	"purchase_invoices": {"vendor_reference": "number", "date": "issue_date", "due_date": "due_date", "total_amount": "total", "contact": "supplier", "invoice_lines": "lines", "currency": "currency"},
	"ledger_accounts":   {"displayed_as": "name", "nominal_code": "code", "ledger_account_type": "type"},
	"products":          {"item_code": "sku", "description": "name", "sales_ledger_account": "account"},
}

// SuggestSageFieldMapping surfaces a best-effort source→target field
// map for an entity the built-in table does not cover.
func SuggestSageFieldMapping(sourceFields, targetFields []string) map[string]string {
	return SuggestFieldMapping(sourceFields, targetFields, 0.5)
}
