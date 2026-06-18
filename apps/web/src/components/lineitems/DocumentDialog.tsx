import { useMemo, useState } from "react";
import {
  Button,
  ControlledModal,
  Field,
  Input,
  Select,
  Textarea,
} from "@kapp/ui";
import { useFormatter } from "../../lib/i18n/useFormatter";
import { computeTotals } from "./compute";
import { buildDocumentData } from "./mapping";
import { LineItemsEditor } from "./LineItemsEditor";
import { RecordSelect } from "./RecordSelect";
import type {
  DocumentConfig,
  HeaderField,
  ItemOption,
  LineItem,
  RecordOption,
} from "./types";

const DEFAULT_CURRENCIES = ["USD", "EUR", "GBP", "CAD", "AUD"];

export interface DocumentSubmitPayload {
  header: Record<string, string>;
  lines: LineItem[];
  currency: string;
  taxRate: number;
  data: Record<string, unknown>;
}

export interface DocumentDialogProps {
  open: boolean;
  onClose: () => void;
  mode: "create" | "edit";
  config: DocumentConfig;
  title: string;
  initialHeader: Record<string, string>;
  initialLines: LineItem[];
  initialCurrency?: string;
  initialTaxRate?: number;
  itemOptions: ItemOption[];
  /** Option lists for header select fields, keyed by field name. */
  selectOptions: Record<string, RecordOption[]>;
  currencyOptions?: string[];
  saving?: boolean;
  /** Server-side error to surface above the actions. */
  error?: string | null;
  onSubmit: (payload: DocumentSubmitPayload) => void;
}

function headerFromInitial(config: DocumentConfig, initial: Record<string, string>) {
  const out: Record<string, string> = {};
  for (const field of config.header) out[field.name] = initial[field.name] ?? "";
  return out;
}

/**
 * DocumentDialog is the in-page create/edit surface for the four
 * line-item documents. It composes the configurable header `Field`s,
 * the shared `LineItemsEditor`, a live totals panel, and Save/Cancel
 * actions inside a `ControlledModal` — so users never leave the
 * workflow board to edit a document. Required fields are validated
 * inline before the parent's `onSubmit` (which performs the actual
 * create/update) is invoked.
 */
export function DocumentDialog({
  open,
  onClose,
  mode,
  config,
  title,
  initialHeader,
  initialLines,
  initialCurrency = "USD",
  initialTaxRate = 0,
  itemOptions,
  selectOptions,
  currencyOptions = DEFAULT_CURRENCIES,
  saving = false,
  error = null,
  onSubmit,
}: DocumentDialogProps) {
  const fmt = useFormatter();
  const [header, setHeader] = useState<Record<string, string>>(() =>
    headerFromInitial(config, initialHeader),
  );
  const [lines, setLines] = useState<LineItem[]>(initialLines);
  const [currency, setCurrency] = useState(initialCurrency);
  const [taxRateInput, setTaxRateInput] = useState(String(initialTaxRate));
  const [attempted, setAttempted] = useState(false);

  const taxRate = Number(taxRateInput) || 0;
  const totals = useMemo(
    () => computeTotals(lines, { taxRate: config.taxable ? taxRate : 0 }),
    [lines, taxRate, config.taxable],
  );
  const money = (n: number) => fmt.currency(n, currency, { currencyDisplay: "code" });

  const validLines = lines.filter((l) => l.itemId && l.qty > 0);
  const missing = (field: HeaderField) =>
    field.required ? header[field.name]?.trim().length === 0 : false;
  const hasLineError = validLines.length === 0;

  const setField = (name: string, value: string) =>
    setHeader((prev) => ({ ...prev, [name]: value }));

  const handleSubmit = () => {
    setAttempted(true);
    const headerInvalid = config.header.some((f) => missing(f));
    if (headerInvalid || hasLineError) return;
    const data = buildDocumentData(config, header, validLines, currency, taxRate, mode);
    onSubmit({ header, lines: validLines, currency, taxRate, data });
  };

  const renderControl = (field: HeaderField) => {
    const value = header[field.name] ?? "";
    if (field.type === "select") {
      return (
        <RecordSelect
          value={value}
          onChange={(v) => setField(field.name, v)}
          options={selectOptions[field.name] ?? []}
          placeholder={field.placeholder}
        />
      );
    }
    if (field.type === "textarea") {
      return (
        <Textarea
          value={value}
          placeholder={field.placeholder}
          rows={2}
          onChange={(e) => setField(field.name, e.target.value)}
        />
      );
    }
    return (
      <Input
        type={field.type === "date" ? "date" : "text"}
        value={value}
        placeholder={field.placeholder}
        onChange={(e) => setField(field.name, e.target.value)}
      />
    );
  };

  return (
    <ControlledModal open={open} onClose={onClose} title={title} className="max-w-3xl">
      <div className="flex flex-col gap-5">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          {config.header.map((field) => (
            <Field
              key={field.name}
              label={field.label}
              required={field.required}
              help={field.help}
              error={attempted && missing(field) ? "Required" : undefined}
              className={field.fullWidth ? "sm:col-span-2" : undefined}
            >
              {renderControl(field)}
            </Field>
          ))}
          <Field label="Currency">
            <Select value={currency} onChange={(e) => setCurrency(e.target.value)}>
              {currencyOptions.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </Select>
          </Field>
          {config.taxable && (
            <Field label="Tax %" help="Applied to the discounted subtotal.">
              <Input
                type="number"
                min={0}
                step="0.01"
                className="text-end"
                value={taxRateInput}
                onChange={(e) => setTaxRateInput(e.target.value)}
              />
            </Field>
          )}
        </div>

        <div className="flex flex-col gap-2">
          <h3 className="text-sm font-semibold text-fg">Line items</h3>
          <LineItemsEditor
            lines={lines}
            onChange={setLines}
            itemOptions={itemOptions}
            columns={config.columns}
            currency={currency}
          />
          {attempted && hasLineError && (
            <p className="text-xs text-danger" role="alert">
              Add at least one line item with a quantity.
            </p>
          )}
        </div>

        <dl className="ms-auto w-full max-w-xs space-y-1 text-sm">
          <div className="flex justify-between">
            <dt className="text-fg-muted">Subtotal</dt>
            <dd className="tabular-nums">{money(totals.subtotal)}</dd>
          </div>
          {config.kind === "sales_order" && (
            <div className="flex justify-between">
              <dt className="text-fg-muted">Discount</dt>
              <dd className="tabular-nums">−{money(totals.discountTotal)}</dd>
            </div>
          )}
          {config.taxable && (
            <div className="flex justify-between">
              <dt className="text-fg-muted">Tax ({taxRate || 0}%)</dt>
              <dd className="tabular-nums">{money(totals.taxAmount)}</dd>
            </div>
          )}
          <div className="flex justify-between border-t border-border pt-1 text-base font-semibold">
            <dt>Total</dt>
            <dd className="tabular-nums">
              {money(config.kind === "purchase_requisition" ? totals.subtotal : totals.total)}
            </dd>
          </div>
        </dl>

        {error && (
          <p className="text-sm text-danger" role="alert">
            {error}
          </p>
        )}

        <div className="flex justify-end gap-2 border-t border-border pt-4">
          <Button type="button" variant="outline" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button type="button" onClick={handleSubmit} disabled={saving}>
            {saving ? "Saving…" : mode === "create" ? `Create ${config.noun}` : "Save changes"}
          </Button>
        </div>
      </div>
    </ControlledModal>
  );
}
