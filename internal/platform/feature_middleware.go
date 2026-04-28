package platform

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// FeatureMiddleware gates an HTTP route group on the presence of a
// tenant_features row with enabled=true for the given feature key.
// When the feature is disabled the request is rejected with a 403
// JSON error envelope so the web UI can render a "feature not
// available on your plan" banner instead of a raw 403 page.
func FeatureMiddleware(store *tenant.FeatureStore, featureKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := TenantFromContext(r.Context())
			if t == nil {
				http.Error(w, "tenant context missing", http.StatusInternalServerError)
				return
			}
			if store == nil || featureKey == "" {
				next.ServeHTTP(w, r)
				return
			}
			enabled, err := store.IsEnabled(r.Context(), t.ID, featureKey)
			if err != nil {
				http.Error(w, "feature lookup failed", http.StatusInternalServerError)
				return
			}
			if !enabled {
				writeFeatureDisabled(w, featureKey)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// FeatureFromPath extracts the KApp domain slug out of the URL path
// and returns the canonical feature key it gates on. Paths outside
// the KApp domain surface (e.g. /api/v1/tenants, /api/v1/auth) map
// to an empty string so the caller can skip the gate.
//
// Exposed so the router in services/api/main.go can wire one
// dynamic middleware instance that introspects the path rather than
// duplicating the list of domain → feature_key mappings.
func FeatureFromPath(p string) string {
	// /api/v1/<domain>/...
	const prefix = "/api/v1/"
	if !strings.HasPrefix(p, prefix) {
		return ""
	}
	rest := p[len(prefix):]
	slash := strings.IndexByte(rest, '/')
	if slash == -1 {
		slash = len(rest)
	}
	domain := rest[:slash]
	// /api/v1/records/{ktype}/... — derive the feature key from
	// the KType domain prefix (e.g. "crm.deal" → FeatureCRM,
	// "finance.ar_invoice" → FeatureFinance) instead of the
	// generic "records" segment, which has no per-feature
	// mapping. This is the same prefix-routing rule the search
	// + bulk endpoints follow.
	if domain == "records" && slash != len(rest) {
		ktypeRest := rest[slash+1:]
		ktSlash := strings.IndexByte(ktypeRest, '/')
		if ktSlash == -1 {
			ktSlash = len(ktypeRest)
		}
		return featureFromKType(ktypeRest[:ktSlash])
	}
	switch domain {
	case "finance":
		return tenant.FeatureFinance
	case "inventory":
		return tenant.FeatureInventory
	case "hr":
		return tenant.FeatureHR
	case "helpdesk":
		return tenant.FeatureHelpdesk
	case "reports":
		return tenant.FeatureReporting
	case "lms":
		return tenant.FeatureLMS
	case "crm":
		return tenant.FeatureCRM
	case "webhooks":
		return tenant.FeatureWebhook
	case "portal":
		return tenant.FeaturePortal
	case "imports", "importer":
		return tenant.FeatureImporter
	case "report-builder":
		return tenant.FeatureReportBuilder
	case "insights":
		return tenant.FeatureInsights
	case "pos":
		return tenant.FeaturePOS
	default:
		return ""
	}
}

// featureFromKType maps a KType name (e.g. "crm.deal",
// "finance.ar_invoice") to the canonical feature key the tenant's
// plan must enable. Unknown prefixes (e.g. core platform KTypes
// like "platform.audit") return "" so the gate is permissive — only
// domain KTypes are plan-gated.
func featureFromKType(ktype string) string {
	dot := strings.IndexByte(ktype, '.')
	if dot == -1 {
		return ""
	}
	switch ktype[:dot] {
	case "crm":
		return tenant.FeatureCRM
	case "finance", "ar", "ap", "ledger":
		return tenant.FeatureFinance
	case "inventory", "procurement", "warehouse", "sales":
		return tenant.FeatureInventory
	case "hr", "payroll":
		return tenant.FeatureHR
	case "lms":
		return tenant.FeatureLMS
	case "helpdesk":
		return tenant.FeatureHelpdesk
	default:
		return ""
	}
}

// DynamicFeatureMiddleware derives the feature key from the request
// path via FeatureFromPath and delegates to the static gate.
// Preferred wiring style when one middleware instance needs to
// cover many routes.
func DynamicFeatureMiddleware(store *tenant.FeatureStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := FeatureFromPath(r.URL.Path)
			if key == "" || store == nil {
				next.ServeHTTP(w, r)
				return
			}
			t := TenantFromContext(r.Context())
			if t == nil {
				http.Error(w, "tenant context missing", http.StatusInternalServerError)
				return
			}
			enabled, err := store.IsEnabled(r.Context(), t.ID, key)
			if err != nil {
				http.Error(w, "feature lookup failed", http.StatusInternalServerError)
				return
			}
			if !enabled {
				writeFeatureDisabled(w, key)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteFeatureDisabled emits the canonical 403 envelope used by the
// feature middleware. Exported so handlers that need to gate a
// nested code path (e.g. only when the request body opts into a
// premium mode) can return the same shape rather than reinventing
// it. Keeping the envelope shape in one place keeps the React shell
// agnostic about *where* the gate fired.
func WriteFeatureDisabled(w http.ResponseWriter, featureKey string) {
	writeFeatureDisabled(w, featureKey)
}

func writeFeatureDisabled(w http.ResponseWriter, featureKey string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   "feature disabled",
		"feature": featureKey,
		"message": "this feature is not enabled on your current plan",
	})
}
