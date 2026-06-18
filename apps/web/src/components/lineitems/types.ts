// Shared types for the reusable line-item editor used by Sales
// Orders, Purchase Orders, Sales Returns, and Purchase Requisitions.
//
// Each of those documents stores its lines under different field
// names (e.g. `unit_price` vs `estimated_unit_price`) but the editor
// works against this single normalised model; the `mapping` module
// translates to/from the KType-specific shapes on load and save.

/** Document families that share the line-item editor. */
export type DocumentKind =
  | "sales_order"
  | "purchase_order"
  | "sales_return"
  | "purchase_requisition";

/** Normalised, UI-facing representation of one document line. */
export interface LineItem {
  itemId: string;
  description: string;
  uom: string;
  qty: number;
  unitPrice: number;
  discount: number;
}

/** Computed money figures shown in the totals panel and persisted. */
export interface DocumentTotals {
  subtotal: number;
  discountTotal: number;
  taxAmount: number;
  total: number;
}

/** An option in a record picker (customer, supplier, warehouse…). */
export interface RecordOption {
  value: string;
  label: string;
  /** Optional secondary line shown muted next to the label. */
  hint?: string;
}

/** Item picker option carries pricing so selecting an item can
 *  pre-fill the unit price and unit of measure. */
export interface ItemOption extends RecordOption {
  price?: number;
  uom?: string;
}

/** Controls which columns the editor renders and the price label. */
export interface LineColumns {
  description: boolean;
  uom: boolean;
  discount: boolean;
  unitPriceLabel: string;
}

export type HeaderFieldType = "select" | "text" | "date" | "textarea";

/** A configurable header control. `name` is the literal KType data
 *  key, so values map straight onto the record payload. Select
 *  options are resolved at runtime and keyed by `name`. */
export interface HeaderField {
  name: string;
  label: string;
  type: HeaderFieldType;
  required?: boolean;
  help?: string;
  placeholder?: string;
  /** Span the full dialog width (used for textareas). */
  fullWidth?: boolean;
}

/** Static description of a document family. */
export interface DocumentConfig {
  kind: DocumentKind;
  ktype: string;
  /** Singular noun used in copy, e.g. "order", "return". */
  noun: string;
  /** Whether the document carries tax (drives the Tax % control). */
  taxable: boolean;
  columns: LineColumns;
  header: HeaderField[];
}
