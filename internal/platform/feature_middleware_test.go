package platform

import (
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestFeatureFromPathRecordsKType pins the per-domain mapping for
// the /api/v1/records/{ktype} route so the fix landed in this
// patch (extracting the domain from the KType prefix instead of
// the literal "records" segment) does not regress.
func TestFeatureFromPathRecordsKType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want string
	}{
		// Domain KTypes resolve via prefix.
		{"/api/v1/records/crm.deal", tenant.FeatureCRM},
		{"/api/v1/records/crm.contact/abc", tenant.FeatureCRM},
		{"/api/v1/records/finance.ar_invoice", tenant.FeatureFinance},
		{"/api/v1/records/ledger.journal", tenant.FeatureFinance},
		{"/api/v1/records/inventory.item", tenant.FeatureInventory},
		{"/api/v1/records/procurement.purchase_order", tenant.FeatureInventory},
		{"/api/v1/records/sales.order", tenant.FeatureInventory},
		{"/api/v1/records/hr.employee", tenant.FeatureHR},
		{"/api/v1/records/payroll.run", tenant.FeatureHR},
		{"/api/v1/records/lms.course", tenant.FeatureLMS},
		{"/api/v1/records/helpdesk.ticket", tenant.FeatureHelpdesk},
		// Core platform KTypes are not plan-gated.
		{"/api/v1/records/platform.audit", ""},
		// Top-level domains map directly.
		{"/api/v1/finance/accounts", tenant.FeatureFinance},
		{"/api/v1/inventory/stock-levels", tenant.FeatureInventory},
		{"/api/v1/imports/run", tenant.FeatureImporter},
		{"/api/v1/report-builder/queries", tenant.FeatureReportBuilder},
		// Out-of-scope paths are permissive.
		{"/api/v1/tenants/me/features", ""},
		{"/api/v1/auth/login", ""},
		{"/health", ""},
	}
	for _, tc := range cases {
		got := FeatureFromPath(tc.path)
		if got != tc.want {
			t.Errorf("FeatureFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
