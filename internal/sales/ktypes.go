// Package sales holds the canonical KType definitions for the sales
// pipeline that feeds finance — sales orders, purchase orders, and
// price lists. These are intentionally stored as KRecords (JSONB on
// `krecords`) rather than behind a typed table because they benefit
// from the metadata-driven UI, cards, and agent tools (ARCHITECTURE.md
// §6) and the authoritative double-entry impact lives on the posted
// AR invoice / AP bill, not on the order itself.
//
// The KTypes here refer to crm.organization for customer/supplier so
// existing CRM records drive the subledger, and to inventory.item
// through the `lines` array so stock and fulfilment flows can hook in
// without another source of truth for the catalog.
package sales

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KType identifiers and workflow names. Kept as constants so the
// router, agent tools, and tests all reference the same strings.
const (
	KTypeSalesOrder    = "sales.order"
	KTypePurchaseOrder = "procurement.purchase_order"
	KTypePriceList     = "sales.price_list"
)

const (
	WorkflowSalesOrder    = "sales.order.lifecycle"
	WorkflowPurchaseOrder = "procurement.purchase_order.lifecycle"
)

// salesOrderSchema — customer sales order. Mirrors the ERPNext Sales
// Order DocType shape: a customer ref, a currency, and a `lines`
// array of inventory.item + qty + price + discount. The workflow
// goes draft → confirmed → fulfilled so fulfilment and invoicing can
// branch off the confirmed state without permitting edits that
// contradict an already-committed order.
var salesOrderSchema = []byte(`{
  "name": "sales.order",
  "version": 1,
  "fields": [
    {"name": "order_number", "type": "string", "max_length": 64},
    {"name": "customer_id", "type": "ref", "ktype": "crm.organization", "required": true},
    {"name": "deal_id", "type": "ref", "ktype": "crm.deal"},
    {"name": "order_date", "type": "date", "required": true},
    {"name": "delivery_date", "type": "date"},
    {"name": "price_list_id", "type": "ref", "ktype": "sales.price_list"},
    {"name": "lines", "type": "array"},
    {"name": "subtotal", "type": "number", "min": 0},
    {"name": "discount_total", "type": "number", "min": 0},
    {"name": "tax_code", "type": "string", "max_length": 32},
    {"name": "tax_amount", "type": "number", "min": 0},
    {"name": "total", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["draft", "confirmed", "fulfilled", "cancelled"], "default": "draft"},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["order_number", "customer_id", "order_date", "total", "currency", "status"]},
    "form": {"sections": [
      {"title": "Order", "fields": ["order_number", "customer_id", "deal_id", "order_date", "delivery_date", "currency", "owner"]},
      {"title": "Pricing", "fields": ["price_list_id", "lines", "subtotal", "discount_total", "tax_code", "tax_amount", "total"]},
      {"title": "Lifecycle", "fields": ["status"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "order_number", "card_subtitle": "total"}
  },
  "cards": {"summary": "SO {{order_number}} — {{total}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["sales.admin", "tenant.admin"]},
  "workflow": {
    "name": "sales.order.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "confirmed", "fulfilled", "cancelled"],
    "transitions": [
      {"from": ["draft"], "to": "confirmed", "action": "confirm"},
      {"from": ["confirmed"], "to": "fulfilled", "action": "fulfil"},
      {"from": ["draft", "confirmed"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["sales.create_order", "sales.confirm_order"]
}`)

// purchaseOrderSchema — procurement counterpart. Supplier ref points
// at crm.organization (the same entity type stores both customers
// and suppliers). `expected_date` feeds the goods-receipt window.
var purchaseOrderSchema = []byte(`{
  "name": "procurement.purchase_order",
  "version": 1,
  "fields": [
    {"name": "po_number", "type": "string", "max_length": 64},
    {"name": "supplier_id", "type": "ref", "ktype": "crm.organization", "required": true},
    {"name": "order_date", "type": "date", "required": true},
    {"name": "expected_date", "type": "date"},
    {"name": "lines", "type": "array"},
    {"name": "subtotal", "type": "number", "min": 0},
    {"name": "tax_code", "type": "string", "max_length": 32},
    {"name": "tax_amount", "type": "number", "min": 0},
    {"name": "total", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["draft", "confirmed", "received", "cancelled"], "default": "draft"},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["po_number", "supplier_id", "order_date", "total", "currency", "status"]},
    "form": {"sections": [
      {"title": "Purchase Order", "fields": ["po_number", "supplier_id", "order_date", "expected_date", "currency", "owner"]},
      {"title": "Pricing", "fields": ["lines", "subtotal", "tax_code", "tax_amount", "total"]},
      {"title": "Lifecycle", "fields": ["status"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "po_number", "card_subtitle": "total"}
  },
  "cards": {"summary": "PO {{po_number}} — {{total}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["procurement.admin", "tenant.admin"]},
  "workflow": {
    "name": "procurement.purchase_order.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "confirmed", "received", "cancelled"],
    "transitions": [
      {"from": ["draft"], "to": "confirmed", "action": "confirm"},
      {"from": ["confirmed"], "to": "received", "action": "receive"},
      {"from": ["draft", "confirmed"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["procurement.create_po", "procurement.receive_po"]
}`)

// priceListSchema — per-customer or per-currency pricing matrix. The
// `items` array stores {item_id, price, discount_percent, min_qty}
// tuples so the quote → order → invoice pipeline can look up an
// effective unit price before applying line-level discounts.
var priceListSchema = []byte(`{
  "name": "sales.price_list",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "required": true},
    {"name": "customer_id", "type": "ref", "ktype": "crm.organization"},
    {"name": "valid_from", "type": "date"},
    {"name": "valid_until", "type": "date"},
    {"name": "items", "type": "array"},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["name", "currency", "customer_id", "valid_from", "valid_until", "active"]},
    "form": {"sections": [
      {"title": "Price List", "fields": ["name", "currency", "customer_id", "valid_from", "valid_until", "active"]},
      {"title": "Items", "fields": ["items"]}
    ]}
  },
  "cards": {"summary": "{{name}} ({{currency}})"},
  "permissions": {"read": ["tenant.member"], "write": ["sales.admin", "tenant.admin"]}
}`)

// All returns every sales/procurement KType as a freshly-constructed
// slice so callers can register them alongside the rest of the Phase C
// catalog.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeSalesOrder, Version: 1, Schema: salesOrderSchema},
		{Name: KTypePurchaseOrder, Version: 1, Schema: purchaseOrderSchema},
		{Name: KTypePriceList, Version: 1, Schema: priceListSchema},
	}
}

func init() {
	for _, kt := range All() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("sales: embedded schema %q is not valid JSON", kt.Name))
		}
	}
}

// RegisterKTypes registers every sales/procurement KType against the
// supplied registry. Idempotent — the underlying PGRegistry upserts on
// conflict.
func RegisterKTypes(ctx context.Context, registry ktype.Registry) error {
	for _, kt := range All() {
		if err := registry.Register(ctx, kt); err != nil {
			return fmt.Errorf("sales: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
