import { Select } from "@kapp/ui";
import type { ComponentPropsWithoutRef } from "react";
import type { RecordOption } from "./types";

type SelectBaseProps = Omit<
  ComponentPropsWithoutRef<typeof Select>,
  "value" | "onChange" | "children"
>;

export interface RecordSelectProps extends SelectBaseProps {
  value: string;
  onChange: (value: string) => void;
  options: RecordOption[];
  /** Disabled leading option shown when nothing is selected. */
  placeholder?: string;
}

/**
 * RecordSelect is the generic picker used for every reference field
 * in the line-item editor and document dialog — customer, supplier,
 * warehouse, invoice, and item. It renders the design-system `Select`
 * with a disabled placeholder option so the empty state reads as a
 * prompt rather than an accidental first choice, and forwards the
 * `id` / `aria-*` / `invalid` props a `Field` injects.
 */
export function RecordSelect({
  value,
  onChange,
  options,
  placeholder = "Select…",
  ...rest
}: RecordSelectProps) {
  return (
    <Select value={value} onChange={(e) => onChange(e.target.value)} {...rest}>
      <option value="" disabled>
        {placeholder}
      </option>
      {options.map((opt) => (
        <option key={opt.value} value={opt.value}>
          {opt.hint ? `${opt.label} — ${opt.hint}` : opt.label}
        </option>
      ))}
    </Select>
  );
}
