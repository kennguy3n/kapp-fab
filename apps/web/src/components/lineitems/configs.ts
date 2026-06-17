import type { DocumentConfig, DocumentKind } from "./types";

// Static configuration per document family. `header[].name` values
// are the literal KType field names (see internal/sales/*.go) so the
// dialog's header model serialises straight onto the record payload.
// Required markers mirror the `"required": true` flags in those
// schemas.

export const DOCUMENT_CONFIGS: Record<DocumentKind, DocumentConfig> = {
  sales_order: {
    kind: "sales_order",
    ktype: "sales.order",
    noun: "order",
    taxable: true,
    columns: { description: false, uom: false, discount: true, unitPriceLabel: "Unit price" },
    header: [
      { name: "customer_id", label: "Customer", type: "select", required: true, placeholder: "Select a customer" },
      { name: "order_date", label: "Order date", type: "date", required: true },
      { name: "delivery_date", label: "Delivery date", type: "date" },
      { name: "order_number", label: "Order number", type: "text", placeholder: "Assigned on save if left blank" },
    ],
  },
  purchase_order: {
    kind: "purchase_order",
    ktype: "procurement.purchase_order",
    noun: "purchase order",
    taxable: true,
    columns: { description: true, uom: true, discount: false, unitPriceLabel: "Unit price" },
    header: [
      { name: "supplier_id", label: "Supplier", type: "select", required: true, placeholder: "Select a supplier" },
      { name: "order_date", label: "Order date", type: "date", required: true },
      { name: "expected_date", label: "Expected date", type: "date" },
      { name: "po_number", label: "PO number", type: "text", placeholder: "Assigned on save if left blank" },
    ],
  },
  sales_return: {
    kind: "sales_return",
    ktype: "sales.return",
    noun: "return",
    taxable: true,
    columns: { description: false, uom: false, discount: false, unitPriceLabel: "Unit price" },
    header: [
      { name: "original_invoice_id", label: "Original invoice", type: "select", required: true, placeholder: "Select the invoice being returned", help: "Credit-note posting reads accounts from this invoice." },
      { name: "customer_id", label: "Customer", type: "select", required: true, placeholder: "Select a customer" },
      { name: "warehouse_id", label: "Return to warehouse", type: "select", required: true, placeholder: "Select a warehouse", help: "Returned stock is received back into this warehouse." },
      { name: "return_date", label: "Return date", type: "date", required: true },
      { name: "return_number", label: "Return number", type: "text", placeholder: "Assigned on save if left blank" },
      { name: "reason", label: "Reason", type: "textarea", placeholder: "Why is this being returned?", fullWidth: true },
    ],
  },
  purchase_requisition: {
    kind: "purchase_requisition",
    ktype: "procurement.purchase_requisition",
    noun: "requisition",
    taxable: false,
    columns: { description: true, uom: true, discount: false, unitPriceLabel: "Est. unit price" },
    header: [
      { name: "requested_by", label: "Requested by", type: "text", required: true, placeholder: "Name of the requester", help: "Who is raising this request — used for approval routing." },
      { name: "request_date", label: "Request date", type: "date", required: true },
      { name: "needed_by", label: "Needed by", type: "date" },
      { name: "department", label: "Department", type: "text", placeholder: "e.g. Operations" },
      { name: "cost_center", label: "Cost center", type: "text", placeholder: "e.g. CC-100" },
      { name: "supplier_id", label: "Preferred supplier", type: "select", placeholder: "Optional — chosen at PO time if blank" },
      { name: "requisition_number", label: "Requisition number", type: "text", placeholder: "Assigned on save if left blank" },
      { name: "justification", label: "Justification", type: "textarea", placeholder: "Why is this purchase needed?", fullWidth: true },
    ],
  },
};
