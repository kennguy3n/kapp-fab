import {
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { Plus, Trash2 } from "lucide-react";
import { useFormatter } from "../../lib/i18n/useFormatter";
import { lineNet } from "./compute";
import { RecordSelect } from "./RecordSelect";
import type { ItemOption, LineColumns, LineItem } from "./types";

export interface LineItemsEditorProps {
  lines: LineItem[];
  onChange: (lines: LineItem[]) => void;
  itemOptions: ItemOption[];
  columns: LineColumns;
  currency: string;
  disabled?: boolean;
}

const EMPTY_LINE: LineItem = {
  itemId: "",
  description: "",
  uom: "",
  qty: 1,
  unitPrice: 0,
  discount: 0,
};

/**
 * LineItemsEditor is a controlled, presentational table for editing a
 * document's lines. It is deliberately api-agnostic: callers pass the
 * `lines`, an `onChange` handler, and the `itemOptions` to populate
 * the per-row item picker, so the same editor backs orders, purchase
 * orders, returns, and requisitions. Money and quantities are
 * right-aligned with tabular figures; selecting an item pre-fills its
 * unit price and unit of measure.
 */
export function LineItemsEditor({
  lines,
  onChange,
  itemOptions,
  columns,
  currency,
  disabled = false,
}: LineItemsEditorProps) {
  const fmt = useFormatter();
  const money = (n: number) => fmt.currency(n, currency, { currencyDisplay: "code" });

  const colCount =
    6 + (columns.description ? 1 : 0) + (columns.uom ? 1 : 0) + (columns.discount ? 1 : 0);

  const update = (index: number, patch: Partial<LineItem>) => {
    onChange(lines.map((line, i) => (i === index ? { ...line, ...patch } : line)));
  };

  const onItemChange = (index: number, value: string) => {
    const opt = itemOptions.find((o) => o.value === value);
    update(index, {
      itemId: value,
      ...(opt?.price !== undefined ? { unitPrice: opt.price } : {}),
      ...(opt?.uom ? { uom: opt.uom } : {}),
    });
  };

  const addLine = () => onChange([...lines, { ...EMPTY_LINE }]);
  const removeLine = (index: number) => onChange(lines.filter((_, i) => i !== index));

  return (
    <div className="flex flex-col gap-2">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-10 text-end text-fg-subtle">#</TableHead>
            <TableHead>Item</TableHead>
            {columns.description && <TableHead>Description</TableHead>}
            {columns.uom && <TableHead className="w-20">Unit</TableHead>}
            <TableHead className="w-24 text-end">Qty</TableHead>
            <TableHead className="w-32 text-end">{columns.unitPriceLabel}</TableHead>
            {columns.discount && <TableHead className="w-28 text-end">Discount</TableHead>}
            <TableHead className="w-36 text-end">Amount</TableHead>
            <TableHead className="w-10">
              <span className="sr-only">Actions</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {lines.length === 0 ? (
            <TableRow>
              <TableCell colSpan={colCount} className="py-6 text-center text-fg-muted">
                No items yet. Add your first line below.
              </TableCell>
            </TableRow>
          ) : (
            lines.map((line, i) => (
              <TableRow key={i}>
                <TableCell className="text-end text-fg-subtle">{i + 1}</TableCell>
                <TableCell>
                  <RecordSelect
                    aria-label={`Item for line ${i + 1}`}
                    value={line.itemId}
                    onChange={(v) => onItemChange(i, v)}
                    options={itemOptions}
                    placeholder="Select an item"
                    disabled={disabled}
                  />
                </TableCell>
                {columns.description && (
                  <TableCell>
                    <Input
                      aria-label={`Description for line ${i + 1}`}
                      value={line.description}
                      onChange={(e) => update(i, { description: e.target.value })}
                      disabled={disabled}
                    />
                  </TableCell>
                )}
                {columns.uom && (
                  <TableCell>
                    <Input
                      aria-label={`Unit for line ${i + 1}`}
                      value={line.uom}
                      onChange={(e) => update(i, { uom: e.target.value })}
                      disabled={disabled}
                    />
                  </TableCell>
                )}
                <TableCell>
                  <Input
                    type="number"
                    min={0}
                    step="any"
                    aria-label={`Quantity for line ${i + 1}`}
                    className="text-end"
                    value={String(line.qty)}
                    onChange={(e) => update(i, { qty: Number(e.target.value) })}
                    disabled={disabled}
                  />
                </TableCell>
                <TableCell>
                  <Input
                    type="number"
                    min={0}
                    step="0.01"
                    aria-label={`${columns.unitPriceLabel} for line ${i + 1}`}
                    className="text-end"
                    value={String(line.unitPrice)}
                    onChange={(e) => update(i, { unitPrice: Number(e.target.value) })}
                    disabled={disabled}
                  />
                </TableCell>
                {columns.discount && (
                  <TableCell>
                    <Input
                      type="number"
                      min={0}
                      step="0.01"
                      aria-label={`Discount for line ${i + 1}`}
                      className="text-end"
                      value={String(line.discount)}
                      onChange={(e) => update(i, { discount: Number(e.target.value) })}
                      disabled={disabled}
                    />
                  </TableCell>
                )}
                <TableCell className="text-end tabular-nums">{money(lineNet(line))}</TableCell>
                <TableCell>
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    aria-label={`Remove line ${i + 1}`}
                    onClick={() => removeLine(i)}
                    disabled={disabled}
                  >
                    <Trash2 className="h-4 w-4" aria-hidden="true" />
                  </Button>
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
      <div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          leadingIcon={<Plus className="h-4 w-4" aria-hidden="true" />}
          onClick={addLine}
          disabled={disabled}
        >
          Add line
        </Button>
      </div>
    </div>
  );
}
