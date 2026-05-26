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
		// POS KTypes share the "sales" prefix but ride their own
		// FeaturePOS gate so a Starter tenant (inventory=true,
		// pos=false) cannot CRUD them through /records.
		{"/api/v1/records/sales.pos_profile", tenant.FeaturePOS},
		{"/api/v1/records/sales.pos_invoice", tenant.FeaturePOS},
		{"/api/v1/records/sales.pos_invoice/abc-123", tenant.FeaturePOS},
		{"/api/v1/records/hr.employee", tenant.FeatureHR},
		{"/api/v1/records/payroll.run", tenant.FeatureHR},
		{"/api/v1/records/lms.course", tenant.FeatureLMS},
		{"/api/v1/records/helpdesk.ticket", tenant.FeatureHelpdesk},
		// Manufacturing KTypes — BOMs and work orders ride the
		// FeatureManufacturing gate on both the dedicated
		// /api/v1/manufacturing/* surface and the generic
		// /api/v1/records/{ktype} surface, so a Starter tenant
		// without manufacturing on their plan cannot reach
		// either path.
		{"/api/v1/records/manufacturing.bom", tenant.FeatureManufacturing},
		{"/api/v1/records/manufacturing.bom_component/abc-123", tenant.FeatureManufacturing},
		{"/api/v1/records/manufacturing.work_order", tenant.FeatureManufacturing},
		// Core platform KTypes are not plan-gated.
		{"/api/v1/records/platform.audit", ""},
		// Top-level domains map directly.
		{"/api/v1/finance/accounts", tenant.FeatureFinance},
		{"/api/v1/inventory/stock-levels", tenant.FeatureInventory},
		// /api/v1/sales/returns/{id}/{verb} — the sales sub-domain
		// rides the inventory feature gate to match the
		// sales.* KType prefix in featureFromKType. A tenant
		// without inventory in their plan must not be able to
		// invoke the return-lifecycle transitions.
		{"/api/v1/sales/returns/abc/approve", tenant.FeatureInventory},
		{"/api/v1/sales/returns/abc/receive", tenant.FeatureInventory},
		{"/api/v1/sales/returns/abc/refund", tenant.FeatureInventory},
		{"/api/v1/sales/returns/abc/cancel", tenant.FeatureInventory},
		{"/api/v1/imports/run", tenant.FeatureImporter},
		{"/api/v1/report-builder/queries", tenant.FeatureReportBuilder},
		// Manufacturing is gated on its own feature key so a
		// tenant with inventory but no manufacturing on their
		// plan can't reach /api/v1/manufacturing/*.
		{"/api/v1/manufacturing/boms", tenant.FeatureManufacturing},
		{"/api/v1/manufacturing/work-orders/abc/release", tenant.FeatureManufacturing},
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
