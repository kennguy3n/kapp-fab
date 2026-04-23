package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

// FrappeConfig is the JSON shape expected in ImportJob.Config for the
// Frappe REST adapter. It points at a Frappe/ERPNext/HRMS/CRM/LMS
// deployment and lists the DocTypes to mirror.
//
// Authentication uses the Frappe "token <api_key>:<api_secret>" scheme
// since that is the only credential type that works on every app
// (frappe, erpnext, hrms, crm, lms). SSO cookies are not supported —
// the operator is expected to mint an API key pair for the import.
type FrappeConfig struct {
	BaseURL   string          `json:"base_url"`
	APIKey    string          `json:"api_key"`
	APISecret string          `json:"api_secret"`
	DocTypes  []FrappeDocType `json:"doctypes"`
	PageSize  int             `json:"page_size,omitempty"`
	// Optional per-field concept map applied on top of the default
	// DocType→KType mapping table (PROPOSAL.md §5.3). Keyed by
	// doctype name; each entry renames source fields to KType field
	// names. Callers can override the defaults without editing Go code.
	ConceptMap map[string]map[string]string `json:"concept_map,omitempty"`
	// LastSyncAt, when non-zero, is passed through as a
	// `modified > $LastSyncAt` filter on every /api/resource/{doctype}
	// call so the adapter pulls only rows changed since the previous
	// run. Callers persist this on import_jobs.last_sync_at and
	// advance it to the run's start time on success.
	LastSyncAt time.Time `json:"last_sync_at,omitempty"`
}

// FrappeDocType is one source DocType the adapter should mirror.
type FrappeDocType struct {
	Name        string   `json:"name"`
	TargetKType string   `json:"target_ktype,omitempty"`
	// Filters applied to the /api/resource/{doctype} query (stringified
	// Frappe filter JSON). Allows e.g. `docstatus=1` to skip drafts.
	Filters string `json:"filters,omitempty"`
	// Fields explicitly requested. When empty, the adapter asks for
	// `["*"]` which Frappe expands to the DocType's standard fields.
	Fields []string `json:"fields,omitempty"`
}

// DefaultFrappePageSize is the pagination step for /api/resource calls.
// Frappe's default is 20 which is too chatty; we bump it to 200 so a
// mid-size ERPNext import finishes in a bounded number of round trips
// without tripping the default throttle.
const DefaultFrappePageSize = 200

// FrappeAdapter mirrors DocTypes from a Frappe-based deployment into
// the importer staging table.
type FrappeAdapter struct {
	client *http.Client
	now    func() time.Time
}

// NewFrappeAdapter wires the adapter with a sane HTTP timeout. Tests
// can inject a stub transport via the returned client.
func NewFrappeAdapter() *FrappeAdapter {
	return &FrappeAdapter{
		client: &http.Client{Timeout: 60 * time.Second},
		now:    time.Now,
	}
}

// WithHTTPClient lets tests and alternate transports replace the
// default client.
func (a *FrappeAdapter) WithHTTPClient(c *http.Client) *FrappeAdapter {
	a.client = c
	return a
}

// SourceType discriminates the adapter for registry lookup.
func (*FrappeAdapter) SourceType() string { return importer.SourceTypeFrappe }

// Discover issues one count request per DocType so the reconciler has
// a source-side row count to compare against the staging total.
func (a *FrappeAdapter) Discover(ctx context.Context, raw json.RawMessage) (importer.DiscoverResult, error) {
	cfg, err := a.loadConfig(raw)
	if err != nil {
		return importer.DiscoverResult{}, err
	}
	result := importer.DiscoverResult{}
	for _, dt := range cfg.DocTypes {
		count, err := a.count(ctx, cfg, dt)
		if err != nil {
			return importer.DiscoverResult{}, fmt.Errorf("discover %s: %w", dt.Name, err)
		}
		result.Entities = append(result.Entities, importer.DiscoveredEntity{
			Name:     dt.Name,
			RowCount: count,
			TargetKT: dt.TargetKType,
		})
		result.TotalRows += count
	}
	return result, nil
}

// Export paginates through each DocType, maps fields to KType fields
// using the concept map, and emits one NormalizedRow per source row.
// Table child rows are nested under the corresponding KType field as
// a JSON array — the generic JSON type-checker in ktype.ValidateData
// accepts that shape, and downstream KApps (finance, inventory) read
// nested arrays for invoice/bill lines.
func (a *FrappeAdapter) Export(ctx context.Context, raw json.RawMessage, emit func(importer.NormalizedRow) error) error {
	cfg, err := a.loadConfig(raw)
	if err != nil {
		return err
	}
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = DefaultFrappePageSize
	}
	for _, dt := range cfg.DocTypes {
		mapping := mergedConceptMap(dt.Name, cfg.ConceptMap)
		offset := 0
		for {
			page, err := a.listPage(ctx, cfg, dt, offset, pageSize)
			if err != nil {
				return fmt.Errorf("export %s: %w", dt.Name, err)
			}
			if len(page) == 0 {
				break
			}
			for _, row := range page {
				name, _ := row["name"].(string)
				normalized := applyFieldMap(row, mapping)
				if err := emit(importer.NormalizedRow{
					Entity:   dt.Name,
					SourceID: name,
					Data:     normalized,
				}); err != nil {
					return err
				}
			}
			if len(page) < pageSize {
				break
			}
			offset += len(page)
		}
	}
	return nil
}

// count calls /api/method/frappe.client.get_count, which returns an
// integer under the `message` key. We prefer that over fetching every
// row just to count them because ERPNext installations routinely have
// 100k+ Sales Invoice rows and a count call is O(1) on the DB side.
func (a *FrappeAdapter) count(ctx context.Context, cfg FrappeConfig, dt FrappeDocType) (int64, error) {
	q := url.Values{}
	q.Set("doctype", dt.Name)
	if dt.Filters != "" {
		q.Set("filters", dt.Filters)
	}
	target := joinURL(cfg.BaseURL, "/api/method/frappe.client.get_count") + "?" + q.Encode()
	var resp struct {
		Message int64 `json:"message"`
	}
	if err := a.doJSON(ctx, cfg, http.MethodGet, target, &resp); err != nil {
		return 0, err
	}
	return resp.Message, nil
}

// listPage calls /api/resource/{doctype}?fields=["*"]&limit_start=…&limit_page_length=…
// and returns the decoded slice of records. When cfg.LastSyncAt is
// non-zero the call adds a `modified > $ts` clause to the filter list
// so successive runs only pull delta rows.
func (a *FrappeAdapter) listPage(ctx context.Context, cfg FrappeConfig, dt FrappeDocType, offset, size int) ([]map[string]any, error) {
	q := url.Values{}
	fields := dt.Fields
	if len(fields) == 0 {
		fields = []string{"*"}
	}
	fieldsJSON, _ := json.Marshal(fields)
	q.Set("fields", string(fieldsJSON))
	q.Set("limit_start", fmt.Sprintf("%d", offset))
	q.Set("limit_page_length", fmt.Sprintf("%d", size))
	if filter := mergeDeltaFilter(dt.Filters, cfg.LastSyncAt); filter != "" {
		q.Set("filters", filter)
	}
	target := joinURL(cfg.BaseURL, "/api/resource/"+url.PathEscape(dt.Name)) + "?" + q.Encode()
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := a.doJSON(ctx, cfg, http.MethodGet, target, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// doJSON performs the HTTP exchange and decodes the response body into
// `out`. The token header works on every Frappe deployment we care
// about (frappe, erpnext, hrms, crm, lms).
func (a *FrappeAdapter) doJSON(ctx context.Context, cfg FrappeConfig, method, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return err
	}
	if cfg.APIKey != "" && cfg.APISecret != "" {
		req.Header.Set("Authorization", fmt.Sprintf("token %s:%s", cfg.APIKey, cfg.APISecret))
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("frappe: %s %s: HTTP %d: %s", method, target, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// loadConfig parses the job's config blob and validates the mandatory
// fields so failures surface early at Discover time rather than deep
// inside the first paginated call.
func (a *FrappeAdapter) loadConfig(raw json.RawMessage) (FrappeConfig, error) {
	var cfg FrappeConfig
	if len(raw) == 0 {
		return cfg, fmt.Errorf("frappe: config required")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("frappe: parse config: %w", err)
	}
	if cfg.BaseURL == "" {
		return cfg, fmt.Errorf("frappe: base_url required")
	}
	if len(cfg.DocTypes) == 0 {
		return cfg, fmt.Errorf("frappe: doctypes[] required")
	}
	return cfg, nil
}

// defaultConceptMap carries the curated DocType→KType concept mapping
// from PROPOSAL.md §5.3. Operators can override any row by providing a
// `concept_map` entry in the job config.
var defaultConceptMap = map[string]map[string]string{
	// CRM / ERPNext shared
	"Lead":     {"lead_name": "name", "company_name": "company", "email_id": "email", "mobile_no": "phone"},
	"Customer": {"customer_name": "name", "customer_group": "group"},
	"Supplier": {"supplier_name": "name", "supplier_group": "group"},
	"Contact":  {"first_name": "first_name", "last_name": "last_name", "email_id": "email"},
	// ERPNext Finance
	"Sales Invoice":    {"name": "number", "customer": "customer", "grand_total": "total", "posting_date": "issue_date", "due_date": "due_date", "items": "lines"},
	"Purchase Invoice": {"name": "number", "supplier": "supplier", "grand_total": "total", "posting_date": "issue_date", "due_date": "due_date", "items": "lines"},
	"Journal Entry":    {"name": "number", "posting_date": "date", "accounts": "lines"},
	// ERPNext Inventory
	"Item":               {"item_code": "sku", "item_name": "name", "item_group": "group", "stock_uom": "uom"},
	"Warehouse":          {"warehouse_name": "name"},
	"Stock Entry":        {"name": "number", "posting_date": "date", "items": "lines"},
	// HRMS
	"Employee":        {"employee_name": "name", "user_id": "user", "reports_to": "reporting_to", "company": "company", "department": "department"},
	"Leave Type":      {"leave_type_name": "name"},
	"Leave Allocation":{"employee": "employee", "leave_type": "leave_type", "total_leaves_allocated": "total", "from_date": "from_date", "to_date": "to_date"},
	"Leave Application":{"employee": "employee", "leave_type": "leave_type", "from_date": "from_date", "to_date": "to_date", "status": "status"},
	// LMS
	"LMS Course":     {"title": "title", "short_introduction": "description"},
	"Course Lesson":  {"title": "title", "body": "content"},
	"LMS Enrollment": {"member": "learner", "course": "course"},
}

// mergedConceptMap returns the per-DocType field map with overrides
// from the job config layered on top of the package defaults.
func mergedConceptMap(doctype string, overrides map[string]map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range defaultConceptMap[doctype] {
		out[k] = v
	}
	for k, v := range overrides[doctype] {
		out[k] = v
	}
	return out
}

// applyFieldMap renames keys in the source row according to `mapping`
// (source → target). Unmapped keys pass through verbatim so operators
// can still reach unusual fields through the raw data blob.
func applyFieldMap(row map[string]any, mapping map[string]string) map[string]any {
	if len(mapping) == 0 {
		return row
	}
	out := make(map[string]any, len(row))
	for k, v := range row {
		if target, ok := mapping[k]; ok && target != "" {
			out[target] = v
			continue
		}
		out[k] = v
	}
	return out
}

// joinURL is a tolerant `path.Join` for HTTP base URLs — it does not
// collapse `://` and trims exactly one trailing / on base and one
// leading / on suffix so callers can mix and match.
func joinURL(base, suffix string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}

// mergeDeltaFilter fuses an operator-supplied filter (already in
// Frappe's `[[field, op, value], …]` stringified-JSON shape) with the
// incremental-sync clause `modified > <ts>`. When `since` is zero the
// original filter is returned unchanged. When both sides have a
// clause the two filter arrays are concatenated — Frappe evaluates
// them as an AND.
func mergeDeltaFilter(existing string, since time.Time) string {
	if since.IsZero() {
		return existing
	}
	deltaClause := [][]any{{"modified", ">", since.UTC().Format("2006-01-02 15:04:05")}}
	if existing == "" {
		b, _ := json.Marshal(deltaClause)
		return string(b)
	}
	var parsed [][]any
	if err := json.Unmarshal([]byte(existing), &parsed); err != nil {
		// Leave the operator's string untouched — they may have used
		// Frappe's dict syntax or some other form we do not parse.
		return existing
	}
	merged := append(parsed, deltaClause...)
	b, _ := json.Marshal(merged)
	return string(b)
}

// SuggestFieldMapping returns a DocType→KType field map computed from
// name similarity alone. For each source field it finds the best
// matching target KType field whose similarity score clears
// minScore (0–1). The concept map published in PROPOSAL.md is still
// the authoritative default — this helper is for DocTypes the default
// doesn't cover and for surfacing pre-filled defaults in
// ImportMappingPage.tsx.
//
// Scoring is Jaccard over the normalised field tokens with a
// Levenshtein-based tiebreaker so similar but differently-tokenised
// names (`customer_name` vs `customer`, `posting_date` vs `post_date`)
// rank high. The returned map only contains pairs whose score clears
// the threshold, so unmatched source fields fall through to the raw
// passthrough path in applyFieldMap.
func SuggestFieldMapping(sourceFields, targetFields []string, minScore float64) map[string]string {
	if minScore <= 0 {
		minScore = 0.5
	}
	out := make(map[string]string, len(sourceFields))
	used := make(map[string]bool, len(targetFields))
	for _, src := range sourceFields {
		var (
			bestTarget string
			bestScore  float64
		)
		for _, tgt := range targetFields {
			if used[tgt] {
				continue
			}
			s := fieldSimilarity(src, tgt)
			if s > bestScore {
				bestScore = s
				bestTarget = tgt
			}
		}
		if bestTarget != "" && bestScore >= minScore {
			out[src] = bestTarget
			used[bestTarget] = true
		}
	}
	return out
}

// fieldSimilarity returns a 0..1 similarity score between two field
// names. It averages Jaccard-on-tokens (split on `_` and lowercased)
// with a normalised Levenshtein distance so `customer_name` /
// `customer` stays close (shared token) while `posting_date` /
// `post_date` also stays close (shared prefix).
func fieldSimilarity(a, b string) float64 {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return 1.0
	}
	if a == "" || b == "" {
		return 0.0
	}
	j := jaccardTokens(a, b)
	l := 1.0 - float64(levenshtein(a, b))/float64(maxInt(len(a), len(b)))
	if l < 0 {
		l = 0
	}
	return (j + l) / 2
}

func jaccardTokens(a, b string) float64 {
	ta := strings.Split(a, "_")
	tb := strings.Split(b, "_")
	set := make(map[string]struct{}, len(ta)+len(tb))
	inter := 0
	for _, t := range ta {
		set[t] = struct{}{}
	}
	seen := make(map[string]struct{}, len(tb))
	for _, t := range tb {
		if _, ok := set[t]; ok {
			if _, dup := seen[t]; !dup {
				inter++
				seen[t] = struct{}{}
			}
		}
		set[t] = struct{}{}
	}
	if len(set) == 0 {
		return 0
	}
	return float64(inter) / float64(len(set))
}

// levenshtein returns the classic edit distance between a and b. The
// implementation uses the two-row optimisation (O(min(len(a), len(b)))
// memory) since field names are short and the importer may call this
// hundreds of times per job.
func levenshtein(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	if len(ra) < len(rb) {
		ra, rb = rb, ra
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = minInt(
				curr[j-1]+1,
				minInt(prev[j]+1, prev[j-1]+cost),
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
