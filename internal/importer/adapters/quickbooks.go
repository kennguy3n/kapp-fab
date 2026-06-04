package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

// QuickBooksConfig is the JSON shape expected in ImportJob.Config for
// the QuickBooks Online adapter. It targets the QBO REST API v3 query
// surface (https://quickbooks.api.intuit.com/v3/company/{realmId}/).
//
// Auth uses an OAuth2 bearer token. Callers may either pass a
// still-valid AccessToken directly, or supply RefreshToken +
// ClientID/ClientSecret so the adapter mints a fresh access token at
// run time. QuickBooks rotates the refresh token on every exchange, so
// when the adapter refreshes it returns the new value on the
// DiscoverResult notes (callers persist it for the next run).
type QuickBooksConfig struct {
	// BaseURL overrides the API host (e.g. the sandbox host
	// https://sandbox-quickbooks.api.intuit.com). Defaults to the
	// production host.
	BaseURL string `json:"base_url,omitempty"`
	// RealmID is the QuickBooks company id; it forms the
	// /v3/company/{realmId}/ path segment and is mandatory.
	RealmID string `json:"realm_id"`
	// AccessToken is an already-valid OAuth2 bearer token. Optional
	// when RefreshToken + client credentials are supplied.
	AccessToken string `json:"access_token,omitempty"`
	// RefreshToken + ClientID + ClientSecret drive the refresh-token
	// grant when AccessToken is empty.
	RefreshToken string `json:"refresh_token,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// TokenURL overrides the OAuth2 token endpoint. Defaults to
	// Intuit's production bearer-token endpoint.
	TokenURL string `json:"token_url,omitempty"`
	// MinorVersion pins the QBO API minor version. Defaults to a
	// recent, stable value.
	MinorVersion string `json:"minor_version,omitempty"`
	// Entities selects which QBO entities to import. When empty the
	// adapter imports the full default set.
	Entities []QuickBooksEntity `json:"entities,omitempty"`
	// PageSize bounds the MAXRESULTS clause. QBO caps a single query
	// at 1000 rows; values above that are clamped.
	PageSize int `json:"page_size,omitempty"`
	// ConceptMap layers per-entity field renames on top of the
	// adapter's built-in mapping table, keyed by QBO entity name.
	ConceptMap map[string]map[string]string `json:"concept_map,omitempty"`
	// LastSyncAt, when non-zero, restricts every query to rows whose
	// Metadata.LastUpdatedTime is newer, enabling incremental sync.
	LastSyncAt time.Time `json:"last_sync_at,omitempty"`
}

// QuickBooksEntity names one QBO entity to mirror plus an optional
// default target KType. The name must match a QBO entity (Invoice,
// Customer, …) since it is interpolated into the query's FROM clause.
type QuickBooksEntity struct {
	Name        string `json:"name"`
	TargetKType string `json:"target_ktype,omitempty"`
}

const (
	// defaultQuickBooksBaseURL is the QBO production API host.
	defaultQuickBooksBaseURL = "https://quickbooks.api.intuit.com"
	// defaultQuickBooksOAuthURL is Intuit's OAuth2 bearer-token
	// endpoint used for the refresh-token grant.
	defaultQuickBooksOAuthURL = "https://oauth.platform.intuit.com/oauth2/v1/tokens/bearer"
	// defaultQuickBooksMinorVersion pins a stable QBO API minor
	// version so field availability does not drift under us.
	defaultQuickBooksMinorVersion = "70"
	// defaultQuickBooksPageSize is the MAXRESULTS step. QBO hard-caps
	// a single query at 1000 rows.
	defaultQuickBooksPageSize = 200
	maxQuickBooksPageSize     = 1000
)

// Source type discriminators for the adapters added by the importer
// stream. They are defined here (rather than alongside SourceTypeCSV
// /Frappe in the importer core package) so this change stays scoped to
// the adapters package; the registry keys on the returned string, so
// the location is immaterial at runtime. The values match the
// source_type the import wizard submits.
const (
	SourceTypeQuickBooks = "quickbooks"
	SourceTypeXero       = "xero"
	SourceTypeTally      = "tally"
	SourceTypeSage       = "sage"
)

// quickBooksEntityName validates an entity name before it is spliced
// into the SQL-ish QBO query. QBO entity names are alphabetic, so this
// rejects anything that could smuggle additional clauses into the
// query string.
var quickBooksEntityName = regexp.MustCompile(`^[A-Za-z]+$`)

// QuickBooksAdapter mirrors entities from a QuickBooks Online company
// into the importer staging table via the QBO v3 query API.
type QuickBooksAdapter struct {
	client *http.Client
}

// NewQuickBooksAdapter returns the QuickBooks Online source adapter
// wired with a bounded HTTP client.
func NewQuickBooksAdapter() *QuickBooksAdapter {
	return &QuickBooksAdapter{client: &http.Client{Timeout: defaultHTTPTimeout}}
}

// WithHTTPClient swaps the HTTP client, letting tests point the adapter
// at an httptest server.
func (a *QuickBooksAdapter) WithHTTPClient(c *http.Client) *QuickBooksAdapter {
	a.client = c
	return a
}

// SourceType discriminates the adapter for registry lookup.
func (*QuickBooksAdapter) SourceType() string { return SourceTypeQuickBooks }

// Discover runs a `SELECT COUNT(*)` per configured entity so the
// reconciler has a source-side row count to compare against staging.
// When the adapter mints a fresh access token, the rotated refresh
// token is surfaced on the result notes.
func (a *QuickBooksAdapter) Discover(ctx context.Context, raw json.RawMessage) (importer.DiscoverResult, error) {
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
		count, err := a.count(ctx, cfg, token, ent.Name)
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

// Export paginates each entity with STARTPOSITION/MAXRESULTS, maps the
// QBO fields onto KType field names, and streams one NormalizedRow per
// record. The QBO `Id` becomes the staging row's SourceID so re-runs
// upsert rather than duplicate.
func (a *QuickBooksAdapter) Export(ctx context.Context, raw json.RawMessage, emit func(importer.NormalizedRow) error) error {
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
		mapping := mergeFieldMaps(defaultQuickBooksFieldMap[ent.Name], cfg.ConceptMap[ent.Name])
		// QBO STARTPOSITION is 1-based.
		start := 1
		for {
			rows, err := a.queryPage(ctx, cfg, token, ent.Name, start, pageSize)
			if err != nil {
				return fmt.Errorf("export %s: %w", ent.Name, err)
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				sourceID, _ := row["Id"].(string)
				if err := emit(importer.NormalizedRow{
					Entity:   ent.Name,
					SourceID: sourceID,
					Data:     applyFieldMap(row, mapping),
				}); err != nil {
					return err
				}
			}
			if len(rows) < pageSize {
				break
			}
			start += len(rows)
		}
	}
	return nil
}

// count issues SELECT COUNT(*) FROM <entity> and reads the totalCount
// QBO returns in the QueryResponse envelope.
func (a *QuickBooksAdapter) count(ctx context.Context, cfg QuickBooksConfig, token, entity string) (int64, error) {
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s%s", entity, quickBooksWhere(cfg.LastSyncAt))
	var resp struct {
		QueryResponse struct {
			TotalCount int64 `json:"totalCount"`
		} `json:"QueryResponse"`
	}
	if err := a.doQuery(ctx, cfg, token, query, &resp); err != nil {
		return 0, err
	}
	return resp.QueryResponse.TotalCount, nil
}

// queryPage fetches one MAXRESULTS-sized window of an entity and
// returns the decoded rows. The QueryResponse envelope keys the row
// array on the entity name, so the result is decoded generically and
// the entity slice pulled out by name.
func (a *QuickBooksAdapter) queryPage(ctx context.Context, cfg QuickBooksConfig, token, entity string, start, size int) ([]map[string]any, error) {
	query := fmt.Sprintf("SELECT * FROM %s%s STARTPOSITION %d MAXRESULTS %d", entity, quickBooksWhere(cfg.LastSyncAt), start, size)
	var resp struct {
		QueryResponse map[string]json.RawMessage `json:"QueryResponse"`
	}
	if err := a.doQuery(ctx, cfg, token, query, &resp); err != nil {
		return nil, err
	}
	raw, ok := resp.QueryResponse[entity]
	if !ok {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode %s rows: %w", entity, err)
	}
	return rows, nil
}

// doQuery executes a QBO query and decodes the response into out.
func (a *QuickBooksAdapter) doQuery(ctx context.Context, cfg QuickBooksConfig, token, query string, out any) error {
	q := url.Values{}
	q.Set("query", query)
	q.Set("minorversion", a.minorVersion(cfg))
	target := fmt.Sprintf("%s/v3/company/%s/query?%s",
		strings.TrimRight(a.baseURL(cfg), "/"),
		url.PathEscape(cfg.RealmID),
		q.Encode(),
	)
	return getJSON(ctx, a.client, target, token, nil, out)
}

// resolveToken returns the bearer token to use for a run. When an
// AccessToken is supplied it is used as-is; otherwise the adapter
// performs a refresh-token grant. The returned notes carry the rotated
// refresh token (QBO invalidates the old one) so the caller can persist
// it.
func (a *QuickBooksAdapter) resolveToken(ctx context.Context, cfg QuickBooksConfig) (token string, notes []string, err error) {
	if cfg.AccessToken != "" {
		return cfg.AccessToken, nil, nil
	}
	tok, err := refreshOAuth2Token(ctx, a.client, oauth2Config{
		TokenURL:     a.tokenURL(cfg),
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RefreshToken: cfg.RefreshToken,
		AuthStyle:    authStyleHeader,
	})
	if err != nil {
		return "", nil, fmt.Errorf("quickbooks: %w", err)
	}
	if tok.RefreshToken != "" && tok.RefreshToken != cfg.RefreshToken {
		notes = append(notes, "quickbooks: refresh token rotated; persist the new refresh_token for the next run")
	}
	return tok.AccessToken, notes, nil
}

func (a *QuickBooksAdapter) loadConfig(raw json.RawMessage) (QuickBooksConfig, error) {
	var cfg QuickBooksConfig
	if len(raw) == 0 {
		return cfg, fmt.Errorf("quickbooks: config required")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("quickbooks: parse config: %w", err)
	}
	if cfg.RealmID == "" {
		return cfg, fmt.Errorf("quickbooks: realm_id required")
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		return cfg, fmt.Errorf("quickbooks: access_token or refresh_token required")
	}
	if len(cfg.Entities) == 0 {
		cfg.Entities = defaultQuickBooksEntities()
	}
	for _, ent := range cfg.Entities {
		if !quickBooksEntityName.MatchString(ent.Name) {
			return cfg, fmt.Errorf("quickbooks: invalid entity name %q", ent.Name)
		}
	}
	return cfg, nil
}

func (a *QuickBooksAdapter) baseURL(cfg QuickBooksConfig) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return defaultQuickBooksBaseURL
}

func (a *QuickBooksAdapter) tokenURL(cfg QuickBooksConfig) string {
	if cfg.TokenURL != "" {
		return cfg.TokenURL
	}
	return defaultQuickBooksOAuthURL
}

func (a *QuickBooksAdapter) minorVersion(cfg QuickBooksConfig) string {
	if cfg.MinorVersion != "" {
		return cfg.MinorVersion
	}
	return defaultQuickBooksMinorVersion
}

func (a *QuickBooksAdapter) pageSize(cfg QuickBooksConfig) int {
	size := cfg.PageSize
	if size <= 0 {
		size = defaultQuickBooksPageSize
	}
	if size > maxQuickBooksPageSize {
		size = maxQuickBooksPageSize
	}
	return size
}

// quickBooksWhere builds the optional incremental-sync clause. QBO
// expects an ISO-8601 timestamp and compares against the system
// Metadata.LastUpdatedTime field present on every entity.
func quickBooksWhere(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return fmt.Sprintf(" WHERE Metadata.LastUpdatedTime > '%s'", since.UTC().Format(time.RFC3339))
}

// defaultQuickBooksEntities is the full set the wizard offers when the
// operator does not narrow the selection. Target KTypes follow the
// platform's finance/crm/inventory/hr module taxonomy.
func defaultQuickBooksEntities() []QuickBooksEntity {
	return []QuickBooksEntity{
		{Name: "Invoice", TargetKType: "finance.ar_invoice"},
		{Name: "Customer", TargetKType: "crm.customer"},
		{Name: "Vendor", TargetKType: "crm.supplier"},
		{Name: "Item", TargetKType: "inventory.item"},
		{Name: "Bill", TargetKType: "finance.ap_bill"},
		{Name: "Payment", TargetKType: "finance.payment"},
		{Name: "JournalEntry", TargetKType: "finance.journal_entry"},
		{Name: "Employee", TargetKType: "hr.employee"},
	}
}

// defaultQuickBooksFieldMap maps QBO source fields onto KType field
// names per entity. Only top-level scalar/ref fields are renamed;
// unmapped keys (and QBO's nested ref objects) pass through verbatim so
// downstream KApps can still read them from the raw data blob.
var defaultQuickBooksFieldMap = map[string]map[string]string{
	"Customer":     {"DisplayName": "name", "CompanyName": "company", "GivenName": "first_name", "FamilyName": "last_name"},
	"Vendor":       {"DisplayName": "name", "CompanyName": "company"},
	"Invoice":      {"DocNumber": "number", "TxnDate": "issue_date", "DueDate": "due_date", "TotalAmt": "total", "CustomerRef": "customer", "Line": "lines"},
	"Bill":         {"DocNumber": "number", "TxnDate": "issue_date", "DueDate": "due_date", "TotalAmt": "total", "VendorRef": "supplier", "Line": "lines"},
	"Item":         {"Name": "name", "Sku": "sku", "Type": "type", "UnitPrice": "price"},
	"Payment":      {"TxnDate": "date", "TotalAmt": "amount", "CustomerRef": "customer"},
	"JournalEntry": {"DocNumber": "number", "TxnDate": "date", "Line": "lines"},
	"Employee":     {"DisplayName": "name", "GivenName": "first_name", "FamilyName": "last_name"},
}

// SuggestQuickBooksFieldMapping surfaces a best-effort source→target
// field map for an entity the built-in table does not cover, reusing
// the shared name-similarity scorer.
func SuggestQuickBooksFieldMapping(sourceFields, targetFields []string) map[string]string {
	return SuggestFieldMapping(sourceFields, targetFields, 0.5)
}
