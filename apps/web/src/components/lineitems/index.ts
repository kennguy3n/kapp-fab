// Reusable line-item editor module — shared by Sales Orders,
// Purchase Orders, Sales Returns, and Purchase Requisitions. Lives
// under apps/web for now; designed to be lifted into @kapp/ui by the
// Foundation workstream later (it has no app-specific imports beyond
// the i18n formatter).

export { DOCUMENT_CONFIGS } from "./configs";
export { computeTotals, lineGross, lineNet, round2 } from "./compute";
export {
  buildDocumentData,
  buildLines,
  deriveTaxRate,
  linesFromData,
  priceKey,
} from "./mapping";
export { looksLikeId, titleCase } from "./format";
export {
  buildNameResolver,
  invoiceOptions,
  itemOptions,
  orgOptions,
  warehouseOptions,
} from "./options";
export { DocumentBoard, type DocumentBoardProps } from "./DocumentBoard";
export { LineItemsEditor, type LineItemsEditorProps } from "./LineItemsEditor";
export {
  DocumentDialog,
  type DocumentDialogProps,
  type DocumentSubmitPayload,
} from "./DocumentDialog";
export { RecordSelect, type RecordSelectProps } from "./RecordSelect";
export { StatusBadge, type StatusBadgeProps } from "./StatusBadge";
export type {
  DocumentConfig,
  DocumentKind,
  DocumentTotals,
  HeaderField,
  HeaderFieldType,
  ItemOption,
  LineColumns,
  LineItem,
  RecordOption,
} from "./types";
