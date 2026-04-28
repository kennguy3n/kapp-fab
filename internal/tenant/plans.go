package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Canonical plan identifiers. Keep in lock-step with the rows seeded
// by migrations/000022_tenant_metering.sql and with the per-plan
// feature defaults in DefaultFeaturesForPlan.
const (
	PlanFree       = "free"
	PlanStarter    = "starter"
	PlanBusiness   = "business"
	PlanEnterprise = "enterprise"
)

// Canonical feature keys. The feature-flag middleware gates KApp
// domains on these strings; the constants live here so the wizard,
// API handlers, and middleware all reference the same literals.
const (
	FeatureCRM           = "crm"
	FeatureFinance       = "finance"
	FeatureInventory     = "inventory"
	FeatureHR            = "hr"
	FeatureLMS           = "lms"
	FeatureHelpdesk      = "helpdesk"
	FeatureReporting     = "reporting"
	FeatureWebhook       = "webhook"
	FeaturePortal        = "portal"
	FeaturePrint         = "print"
	FeatureImporter      = "importer"
	FeatureReportBuilder = "report_builder"
	FeatureInsights      = "insights"
	// FeatureInsightsExternal gates Insights queries that resolve
	// `source: "external:<datasource_id>"` to per-tenant external
	// connection pools. Off by default on free/starter; on for
	// business/enterprise (DefaultFeaturesForPlan keeps the per-plan
	// matrix authoritative).
	FeatureInsightsExternal = "insights_external"
	// FeatureInsightsEmbed gates the long-lived bearer token
	// dashboard-embed surface. Off by default on free/starter
	// plans because anonymous fetches consume the owning tenant's
	// rate-limit + quota bucket.
	FeatureInsightsEmbed = "insights_embed"
	// FeatureInsightsSQLEditor gates the Phase M raw-SQL editor mode
	// on insights_queries. Visual queries flow through the
	// reporting.Definition grammar; SQL queries skip the builder and
	// run a parameterised statement under SET LOCAL
	// statement_timeout + RLS. Off by default on every plan except
	// enterprise so a stolen tenant header on a non-enterprise plan
	// can't reach the raw-SQL surface even with a valid `insights`
	// flag.
	FeatureInsightsSQLEditor = "insights_sql_editor"
	// FeaturePOS gates the Phase M Task 6 POS surface
	// (sales.pos_profile, sales.pos_invoice, /api/v1/pos/*). Off
	// on free / starter, on for business / enterprise — POS
	// storefronts are typically business-tier and the offline
	// queue + idempotent finalize handler add measurable load
	// per active terminal.
	FeaturePOS = "pos"
	// FeatureProjects gates the Phase M Task 5 projects +
	// milestones surface. Off on free, on for starter / business
	// / enterprise so a free-tier tenant doesn't pay for the
	// extra KType registration work in their cell. Honoured by
	// the dynamic feature middleware on /api/v1/projects/* and
	// /api/v1/records/projects.* and by the agent tool registry.
	FeatureProjects = "projects"
)

// AllFeatures is the canonical list of feature keys. Handlers that
// need to enumerate (e.g. the wizard seeder and the feature-list API)
// iterate this slice so a new feature only has to be declared in one
// place.
var AllFeatures = []string{
	FeatureCRM,
	FeatureFinance,
	FeatureInventory,
	FeatureHR,
	FeatureLMS,
	FeatureHelpdesk,
	FeatureReporting,
	FeatureWebhook,
	FeaturePortal,
	FeaturePrint,
	FeatureImporter,
	FeatureReportBuilder,
	FeatureInsights,
	FeatureInsightsExternal,
	FeatureInsightsEmbed,
	FeatureInsightsSQLEditor,
	FeaturePOS,
	FeatureProjects,
}

// PlanLimits is the numeric ceiling each plan enforces per billing
// period. Zero means "unlimited" at the API layer — the control
// plane still stores the real value for billing; enforcement just
// short-circuits.
type PlanLimits struct {
	APICalls     int64 `json:"api_calls"`
	StorageBytes int64 `json:"storage_bytes"`
	KRecordCount int64 `json:"krecord_count"`
	UserSeats    int64 `json:"user_seats"`
}

// Plan mirrors one row of plan_definitions. Features is the subset
// of AllFeatures enabled by default for new tenants on this plan.
type Plan struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"display_name"`
	Limits      PlanLimits      `json:"limits"`
	Features    map[string]bool `json:"features"`
}

// MaxJoinsForPlan returns the per-query JOIN ceiling allowed for the
// named plan. The reporting engine has its own hard ceiling
// (reporting.MaxJoinsHardCeiling = 4) which acts as defence-in-depth
// against a misconfigured plan row; this function returns the user-
// facing limit so the UI can disable the "Add join" button at the
// right count.
//
//   - free        → 0 (no joins)
//   - starter     → 1
//   - business    → 2
//   - enterprise  → 4
func MaxJoinsForPlan(plan string) int {
	switch plan {
	case PlanStarter:
		return 1
	case PlanBusiness:
		return 2
	case PlanEnterprise:
		return 4
	default:
		return 0
	}
}

// ErrPlanNotFound is surfaced by PlanStore.Get when no row matches.
var ErrPlanNotFound = errors.New("tenant: plan not found")

// PlanStore reads plan_definitions. It intentionally exposes no
// mutators — plans are managed by migration so upgrading pricing
// ships as a schema change the platform team reviews rather than a
// runtime toggle.
type PlanStore struct {
	pool *pgxpool.Pool
}

// NewPlanStore binds a store to the shared pool.
func NewPlanStore(pool *pgxpool.Pool) *PlanStore {
	return &PlanStore{pool: pool}
}

// List returns every plan in display order (free → enterprise) so
// the UI can render the upgrade matrix without additional sorting.
func (s *PlanStore) List(ctx context.Context) ([]Plan, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, display_name, limits, features
		FROM plan_definitions
		ORDER BY
		  CASE name
		    WHEN 'free' THEN 0
		    WHEN 'starter' THEN 1
		    WHEN 'business' THEN 2
		    WHEN 'enterprise' THEN 3
		    ELSE 99
		  END`)
	if err != nil {
		return nil, fmt.Errorf("tenant: list plans: %w", err)
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns the named plan or ErrPlanNotFound.
func (s *PlanStore) Get(ctx context.Context, name string) (*Plan, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT name, display_name, limits, features
		FROM plan_definitions WHERE name = $1`, name)
	p, err := scanPlan(row)
	if err != nil {
		if errors.Is(err, errNoRowsSentinel) {
			return nil, ErrPlanNotFound
		}
		return nil, err
	}
	return &p, nil
}

// planRow is the subset of the pgx Row/Rows interface scanPlan
// needs. Defining it here lets the same helper serve both List (Rows)
// and Get (Row).
type planRow interface {
	Scan(dest ...any) error
}

var errNoRowsSentinel = errors.New("no rows")

func scanPlan(r planRow) (Plan, error) {
	var (
		p        Plan
		limits   []byte
		features []byte
	)
	if err := r.Scan(&p.Name, &p.DisplayName, &limits, &features); err != nil {
		// pgx.ErrNoRows is a distinct sentinel but this package is
		// chosen to avoid importing pgx here; translate by string.
		if err.Error() == "no rows in result set" {
			return Plan{}, errNoRowsSentinel
		}
		return Plan{}, fmt.Errorf("tenant: scan plan: %w", err)
	}
	if err := json.Unmarshal(limits, &p.Limits); err != nil {
		return Plan{}, fmt.Errorf("tenant: decode plan limits: %w", err)
	}
	if err := json.Unmarshal(features, &p.Features); err != nil {
		return Plan{}, fmt.Errorf("tenant: decode plan features: %w", err)
	}
	return p, nil
}

// DefaultFeaturesForPlan returns the feature flag map the setup
// wizard should seed for a tenant on the given plan. Used when the
// plan_definitions row is unreachable at wizard time (control-plane
// DB blip, offline seeding) so a new tenant still gets a sane
// default feature set.
func DefaultFeaturesForPlan(plan string) map[string]bool {
	switch plan {
	case PlanStarter:
		return map[string]bool{
			FeatureCRM:               true,
			FeatureFinance:           true,
			FeatureInventory:         true,
			FeatureHR:                false,
			FeatureLMS:               false,
			FeatureHelpdesk:          false,
			FeatureReporting:         false,
			FeatureWebhook:           false,
			FeaturePortal:            false,
			FeaturePrint:             true,
			FeatureImporter:          true,
			FeatureReportBuilder:     false,
			FeatureInsights:          false,
			FeatureInsightsExternal:  false,
			FeatureInsightsEmbed:     false,
			FeatureInsightsSQLEditor: false,
			FeaturePOS:               false,
			FeatureProjects:          true,
		}
	case PlanBusiness:
		return map[string]bool{
			FeatureCRM:               true,
			FeatureFinance:           true,
			FeatureInventory:         true,
			FeatureHR:                true,
			FeatureLMS:               true,
			FeatureHelpdesk:          true,
			FeatureReporting:         true,
			FeatureWebhook:           true,
			FeaturePortal:            true,
			FeaturePrint:             true,
			FeatureImporter:          true,
			FeatureReportBuilder:     true,
			FeatureInsights:          true,
			FeatureInsightsExternal:  true,
			FeatureInsightsEmbed:     false,
			FeatureInsightsSQLEditor: false,
			FeaturePOS:               true,
			FeatureProjects:          true,
		}
	case PlanEnterprise:
		return map[string]bool{
			FeatureCRM:               true,
			FeatureFinance:           true,
			FeatureInventory:         true,
			FeatureHR:                true,
			FeatureLMS:               true,
			FeatureHelpdesk:          true,
			FeatureReporting:         true,
			FeatureWebhook:           true,
			FeaturePortal:            true,
			FeaturePrint:             true,
			FeatureImporter:          true,
			FeatureReportBuilder:     true,
			FeatureInsights:          true,
			FeatureInsightsExternal:  true,
			FeatureInsightsEmbed:     true,
			FeatureInsightsSQLEditor: true,
			FeaturePOS:               true,
			FeatureProjects:          true,
		}
	default:
		// Free plan — CRM only. Also the fallback when the plan
		// name does not match any canonical identifier so a typo
		// fails closed rather than opening every feature.
		return map[string]bool{
			FeatureCRM:               true,
			FeatureFinance:           false,
			FeatureInventory:         false,
			FeatureHR:                false,
			FeatureLMS:               false,
			FeatureHelpdesk:          false,
			FeatureReporting:         false,
			FeatureWebhook:           false,
			FeaturePortal:            false,
			FeaturePrint:             false,
			FeatureImporter:          false,
			FeatureReportBuilder:     false,
			FeatureInsights:          false,
			FeatureInsightsExternal:  false,
			FeatureInsightsEmbed:     false,
			FeatureInsightsSQLEditor: false,
			FeaturePOS:               false,
			FeatureProjects:          false,
		}
	}
}
