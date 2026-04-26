// Package insights implements the Phase L BI layer: tenant-scoped
// saved query definitions, composable dashboards, sharing grants, and
// a TTL-bounded query result cache. The package wraps the existing
// internal/reporting query engine so insights queries reuse the same
// validated grammar (sources, filters, aggregations, sort, limit) and
// extend it with calculated columns and other BI-grade extensions.
//
// Reference: ARCHITECTURE.md §12 and frappe/insights query/dashboard
// models.
package insights

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/reporting"
)

// VizType enumerates the chart / table renderers the dashboard
// frontend understands. Stored verbatim on insights_dashboard_widgets
// so the API surface validates against this list before any write.
const (
	VizTable      = "table"
	VizBar        = "bar"
	VizLine       = "line"
	VizPie        = "pie"
	VizDonut      = "donut"
	VizFunnel     = "funnel"
	VizNumberCard = "number_card"
	VizPivot      = "pivot"
)

var allowedVizTypes = map[string]struct{}{
	VizTable: {}, VizBar: {}, VizLine: {}, VizPie: {},
	VizDonut: {}, VizFunnel: {}, VizNumberCard: {}, VizPivot: {},
}

// ResourceType discriminates rows of insights_shares.
const (
	ResourceQuery     = "query"
	ResourceDashboard = "dashboard"
)

// GranteeType + Permission constants mirror the CHECK constraints in
// migrations/000038_insights.sql.
const (
	GranteeUser = "user"
	GranteeRole = "role"

	PermissionView = "view"
	PermissionEdit = "edit"
)

// CalculatedColumn is the insights extension to reporting.Definition.
// Expression is a whitelisted, validated formula (see
// reporting.Definition extensions in ARCHITECTURE.md §12); the runner
// rejects unknown identifiers / functions.
type CalculatedColumn struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Type       string `json:"type,omitempty"`
}

// QueryDefinition is the insights superset of reporting.Definition.
// The base definition flows verbatim to the reporting runner; the
// insights runner adds calculated columns + filter parameter binding
// before forwarding.
type QueryDefinition struct {
	reporting.Definition
	CalculatedColumns []CalculatedColumn `json:"calculated_columns,omitempty"`
}

// Validate runs the base reporting validator + insights-specific
// rules. Calculated-column names must be unique and non-empty.
func (q QueryDefinition) Validate() error {
	if err := q.Definition.Validate(); err != nil {
		// Tag reporting validation errors as ErrValidation so the
		// HTTP layer can map them to 400 alongside the insights-
		// native validation surface.
		return fmt.Errorf("%w: %w", ErrValidation, err)
	}
	seen := make(map[string]struct{}, len(q.CalculatedColumns))
	for _, c := range q.CalculatedColumns {
		if c.Name == "" {
			return validationErr("calculated column name required")
		}
		if c.Expression == "" {
			return validationErr("calculated column %q expression required", c.Name)
		}
		if _, dup := seen[c.Name]; dup {
			return validationErr("duplicate calculated column %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

// Query mirrors one row of insights_queries.
type Query struct {
	TenantID        uuid.UUID       `json:"tenant_id"`
	ID              uuid.UUID       `json:"id"`
	Name            string          `json:"name"`
	Description     string          `json:"description,omitempty"`
	Definition      QueryDefinition `json:"definition"`
	// CacheTTLSeconds is a pointer so the JSON-decoder can distinguish
	// "field omitted → use server default" (nil) from "0 → disable
	// caching for this query". The migration column itself is still
	// INT NOT NULL DEFAULT 300; the store dereferences with that
	// default applied.
	CacheTTLSeconds *int `json:"cache_ttl_seconds,omitempty"`
	CreatedBy       *uuid.UUID      `json:"created_by,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// Dashboard mirrors one row of insights_dashboards.
type Dashboard struct {
	TenantID           uuid.UUID         `json:"tenant_id"`
	ID                 uuid.UUID         `json:"id"`
	Name               string            `json:"name"`
	Description        string            `json:"description,omitempty"`
	Layout             json.RawMessage   `json:"layout"`
	AutoRefreshSeconds int               `json:"auto_refresh_seconds"`
	CreatedBy          *uuid.UUID        `json:"created_by,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	Widgets            []DashboardWidget `json:"widgets,omitempty"`
}

// DashboardWidget mirrors one row of insights_dashboard_widgets.
type DashboardWidget struct {
	TenantID    uuid.UUID       `json:"tenant_id"`
	ID          uuid.UUID       `json:"id"`
	DashboardID uuid.UUID       `json:"dashboard_id"`
	QueryID     *uuid.UUID      `json:"query_id,omitempty"`
	VizType     string          `json:"viz_type"`
	Position    json.RawMessage `json:"position"`
	Config      json.RawMessage `json:"config"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// QueryCache mirrors one row of insights_query_cache.
type QueryCache struct {
	TenantID   uuid.UUID       `json:"tenant_id"`
	QueryHash  string          `json:"query_hash"`
	FilterHash string          `json:"filter_hash"`
	QueryID    *uuid.UUID      `json:"query_id,omitempty"`
	Result     json.RawMessage `json:"result"`
	RowCount   int             `json:"row_count"`
	CreatedAt  time.Time       `json:"created_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
}

// Share mirrors one row of insights_shares.
type Share struct {
	TenantID     uuid.UUID `json:"tenant_id"`
	ID           uuid.UUID `json:"id"`
	ResourceType string    `json:"resource_type"`
	ResourceID   uuid.UUID `json:"resource_id"`
	GranteeType  string    `json:"grantee_type"`
	Grantee      string    `json:"grantee"`
	Permission   string    `json:"permission"`
	CreatedAt    time.Time `json:"created_at"`
}

// ValidateVizType rejects unknown visualization types so the JSONB
// column never holds a value the frontend does not understand.
func ValidateVizType(v string) error {
	if v == "" {
		return validationErr("viz_type required")
	}
	if _, ok := allowedVizTypes[v]; !ok {
		return validationErr("viz_type %q invalid", v)
	}
	return nil
}

// ValidateResourceType / ValidateGranteeType / ValidatePermission
// guard the share inputs at the API layer so the DB CHECK only ever
// rejects bugs, not malformed user input.
func ValidateResourceType(v string) error {
	switch v {
	case ResourceQuery, ResourceDashboard:
		return nil
	default:
		return validationErr("resource_type %q invalid", v)
	}
}

func ValidateGranteeType(v string) error {
	switch v {
	case GranteeUser, GranteeRole:
		return nil
	default:
		return validationErr("grantee_type %q invalid", v)
	}
}

func ValidatePermission(v string) error {
	switch v {
	case PermissionView, PermissionEdit:
		return nil
	default:
		return validationErr("permission %q invalid", v)
	}
}

// CacheKey computes the deterministic cache key SHA-256 hex digest
// for a (tenant_id, definition, filter_params) triple. Used by the
// runner before any DB lookup so cache hits / misses do not depend
// on JSON key ordering.
func CacheKey(tenantID uuid.UUID, def QueryDefinition, filterParams map[string]any) (string, string, error) {
	defJSON, err := canonicalJSON(def)
	if err != nil {
		return "", "", fmt.Errorf("insights: marshal definition: %w", err)
	}
	queryHashBytes := sha256.Sum256(append([]byte(tenantID.String()+":"), defJSON...))
	queryHash := hex.EncodeToString(queryHashBytes[:])

	filterHash := ""
	if len(filterParams) > 0 {
		fpJSON, err := canonicalJSON(filterParams)
		if err != nil {
			return "", "", fmt.Errorf("insights: marshal filter params: %w", err)
		}
		fhBytes := sha256.Sum256(fpJSON)
		filterHash = hex.EncodeToString(fhBytes[:])
	}
	return queryHash, filterHash, nil
}

// canonicalJSON serializes v with sorted map keys so the same logical
// payload always produces the same bytes. The standard library
// encoder sorts map keys but does NOT sort struct fields; serializing
// through map[string]any guarantees a stable order for both shapes.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}
