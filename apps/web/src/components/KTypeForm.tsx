import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { FieldSpec, KType } from "@kapp/client";
import {
  Button,
  Field,
  Input,
  Select,
  Textarea,
  cn,
} from "@kapp/ui";
import { ChevronsUpDown } from "lucide-react";
import { api } from "../lib/api";
import {
  humanizeLabel,
  humanizeToken,
  isMoneyField,
  recordLabel,
  relationTargetKtype,
  resolveControl,
  schemaHasCurrency,
} from "../lib/ktypeView";

interface KTypeFormProps {
  ktype: KType;
  initialData?: Record<string, unknown>;
  onSubmit: (data: Record<string, unknown>) => void | Promise<void>;
  /** When set, renders a Cancel action (e.g. navigate back to the list). */
  onCancel?: () => void;
  /**
   * When set, renders a "Save & add another" action. The form submits
   * the payload and only resets itself for the next entry once the
   * save resolves — used by the create flow so operators can capture
   * several records in a row without losing input if a save fails.
   * Return a promise so the form can await the result before clearing.
   */
  onSubmitAndAddAnother?: (
    data: Record<string, unknown>,
  ) => void | Promise<void>;
  /** Disables the action bar while a save mutation is in flight. */
  submitting?: boolean;
}

type Errors = Record<string, string>;

const isEmpty = (value: unknown): boolean =>
  value == null ||
  value === "" ||
  (Array.isArray(value) && value.length === 0);

/**
 * KTypeForm is the shared, metadata-driven record editor. Every field
 * is rendered through the design-system `Field` wrapper with a control
 * resolved from the KType field spec (select / relation / date /
 * email / tel / currency / textarea / switch / number / text), a
 * humanized Title Case label, a required marker, and inline
 * validation. Fields are grouped into the schema's form sections when
 * present. The submit payload shape is unchanged: a flat
 * `Record<string, unknown>` of field name → value.
 */
export function KTypeForm({
  ktype,
  initialData,
  onSubmit,
  onCancel,
  onSubmitAndAddAnother,
  submitting,
}: KTypeFormProps) {
  const fields: FieldSpec[] = useMemo(
    () => ktype.schema?.fields ?? [],
    [ktype],
  );
  // Only show a currency affordance on monetary inputs when the schema
  // actually models a currency, so a plain numeric field never picks
  // up a spurious `$`.
  const moneyContext = useMemo(() => schemaHasCurrency(fields), [fields]);
  const [data, setData] = useState<Record<string, unknown>>(initialData ?? {});
  const [errors, setErrors] = useState<Errors>({});
  const [dirty, setDirty] = useState(false);

  // Warn before a hard navigation / tab close while the form has
  // unsaved edits — the SPA Cancel path is guarded separately below.
  useEffect(() => {
    if (!dirty) return;
    const handler = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, [dirty]);

  const update = (name: string, value: unknown) => {
    setData((d) => ({ ...d, [name]: value }));
    setDirty(true);
    setErrors((e) => {
      if (!e[name]) return e;
      const next = { ...e };
      delete next[name];
      return next;
    });
  };

  // Group fields into the schema's declared form sections; everything
  // not referenced lands in a trailing catch-all so a partial section
  // config never hides a field.
  const sections = useMemo(() => {
    const declared = ktype.schema?.views?.form?.sections;
    const byName = new Map(fields.map((f) => [f.name, f]));
    if (declared && declared.length > 0) {
      const used = new Set<string>();
      const groups = declared.map((section) => {
        const groupFields = section.fields
          .map((name) => byName.get(name))
          .filter((f): f is FieldSpec => {
            if (!f) return false;
            used.add(f.name);
            return true;
          });
        return { title: section.title, fields: groupFields };
      });
      const leftover = fields.filter((f) => !used.has(f.name));
      if (leftover.length)
        groups.push({ title: "More details", fields: leftover });
      return groups.filter((g) => g.fields.length > 0);
    }
    return [{ title: undefined as string | undefined, fields }];
  }, [ktype, fields]);

  const validate = (): Errors => {
    const next: Errors = {};
    for (const field of fields) {
      const value = data[field.name];
      if (field.required && isEmpty(value)) {
        next[field.name] = `${humanizeLabel(field.name)} is required.`;
        continue;
      }
      if (isEmpty(value)) continue;
      const control = resolveControl(field);
      if (
        control === "email" &&
        typeof value === "string" &&
        !/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(value)
      ) {
        next[field.name] = "Enter a valid email address.";
      }
      if (field.pattern && typeof value === "string") {
        try {
          if (!new RegExp(field.pattern).test(value))
            next[field.name] = "This value doesn't match the expected format.";
        } catch {
          // An invalid pattern in the schema should never block save.
        }
      }
    }
    return next;
  };

  const submit = async (after: "close" | "again") => {
    const found = validate();
    setErrors(found);
    if (Object.keys(found).length > 0) return;
    if (after === "again" && onSubmitAndAddAnother) {
      // Wait for the save to resolve before clearing the form so a
      // failed create never silently discards what the operator typed.
      try {
        await onSubmitAndAddAnother(data);
        setData({});
        setErrors({});
        setDirty(false);
      } catch {
        // The parent surfaces the failure (toast); keep the form
        // populated so the entry can be retried without retyping.
      }
      return;
    }
    // Mirror the "again" path: only clear the dirty flag once the save
    // resolves so a failed save keeps the unsaved-changes guard armed and
    // the operator can't navigate away and silently lose their input.
    try {
      await onSubmit(data);
      setDirty(false);
    } catch {
      // The parent surfaces the failure (toast); keep the form dirty.
    }
  };

  const errorCount = Object.keys(errors).length;

  const handleCancel = () => {
    if (!onCancel) return;
    if (dirty && !window.confirm("Discard your unsaved changes?")) return;
    onCancel();
  };

  return (
    <form
      className="space-y-6"
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        void submit("close");
      }}
    >
      {errorCount > 0 && (
        <div
          role="alert"
          className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger"
        >
          Please fix {errorCount} {errorCount === 1 ? "field" : "fields"} below.
        </div>
      )}

      {sections.map((section, i) => (
        <section key={section.title ?? `section-${i}`} className="space-y-4">
          {section.title && (
            <h2 className="text-sm font-semibold tracking-tight text-fg">
              {section.title}
            </h2>
          )}
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            {section.fields.map((field) => (
              <FieldRow
                key={field.name}
                field={field}
                value={data[field.name]}
                error={errors[field.name]}
                moneyContext={moneyContext}
                onChange={(v) => update(field.name, v)}
              />
            ))}
          </div>
        </section>
      ))}

      <div className="sticky bottom-0 -mx-1 flex flex-wrap items-center justify-end gap-2 border-t border-border bg-bg/80 px-1 py-3 backdrop-blur">
        {onCancel && (
          <Button
            type="button"
            variant="ghost"
            onClick={handleCancel}
            disabled={submitting}
          >
            Cancel
          </Button>
        )}
        {onSubmitAndAddAnother && (
          <Button
            type="button"
            variant="secondary"
            onClick={() => void submit("again")}
            disabled={submitting}
          >
            Save &amp; add another
          </Button>
        )}
        <Button type="submit" disabled={submitting}>
          {submitting ? "Saving…" : "Save"}
        </Button>
      </div>
    </form>
  );
}

interface FieldRowProps {
  field: FieldSpec;
  value: unknown;
  error?: string;
  /** Whether the owning schema models a currency (gates the `$`). */
  moneyContext: boolean;
  onChange: (value: unknown) => void;
}

function FieldRow({ field, value, error, moneyContext, onChange }: FieldRowProps) {
  const control = resolveControl(field);
  const label = humanizeLabel(field.name);
  const required = !!field.required;

  // Long free-text reads better spanning both grid columns; every
  // other control (including the single-line relation picker) sits in
  // one column.
  const fullWidth = control === "textarea";

  if (control === "boolean") {
    return (
      <div className={cn("flex flex-col gap-1.5", fullWidth && "sm:col-span-2")}>
        <label className="inline-flex items-center gap-2 text-sm font-medium text-fg">
          <input
            type="checkbox"
            className="size-4 rounded border-border accent-accent"
            checked={!!value}
            onChange={(e) => onChange(e.target.checked)}
          />
          {label}
          {required && (
            <span aria-hidden="true" className="text-danger">
              *
            </span>
          )}
        </label>
        {error && <p className="text-xs text-danger">{error}</p>}
      </div>
    );
  }

  return (
    <Field
      label={label}
      required={required}
      error={error}
      help={fieldHelp(field, control)}
      className={cn(fullWidth && "sm:col-span-2")}
    >
      <FieldControl
        field={field}
        control={control}
        value={value}
        moneyContext={moneyContext}
        onChange={onChange}
      />
    </Field>
  );
}

function fieldHelp(field: FieldSpec, control: ReturnType<typeof resolveControl>):
  | string
  | undefined {
  if (control === "email") return "We'll use this to get in touch.";
  if (control === "tel") return undefined;
  if (control === "url") return undefined;
  if (field.max_length && control === "textarea")
    return `Up to ${field.max_length} characters.`;
  return undefined;
}

interface FieldControlProps {
  field: FieldSpec;
  control: ReturnType<typeof resolveControl>;
  value: unknown;
  moneyContext?: boolean;
  onChange: (value: unknown) => void;
  id?: string;
  invalid?: boolean;
  "aria-describedby"?: string;
  "aria-required"?: boolean;
}

function FieldControl({
  field,
  control,
  value,
  moneyContext,
  onChange,
  ...injected
}: FieldControlProps) {
  switch (control) {
    case "textarea":
      return (
        <Textarea
          {...injected}
          rows={4}
          maxLength={field.max_length}
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "select":
      return (
        <Select
          {...injected}
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="">Select…</option>
          {(field.values ?? []).map((v) => (
            <option key={v} value={v}>
              {humanizeToken(v)}
            </option>
          ))}
        </Select>
      );
    case "relation":
      return (
        <RelationCombobox
          {...injected}
          field={field}
          value={value}
          onChange={onChange}
        />
      );
    case "email":
      return (
        <Input
          {...injected}
          type="email"
          inputMode="email"
          autoComplete="email"
          placeholder="name@company.com"
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "tel":
      return (
        <Input
          {...injected}
          type="tel"
          inputMode="tel"
          autoComplete="tel"
          placeholder="+1 (555) 123-4567"
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "url":
      return (
        <Input
          {...injected}
          type="url"
          inputMode="url"
          placeholder="https://example.com"
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "date":
      return (
        <Input
          {...injected}
          type="date"
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "datetime":
      return (
        <Input
          {...injected}
          type="datetime-local"
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "number": {
      const money = isMoneyField(field) && !!moneyContext;
      const stepDecimal = /^(decimal|float|double|money|currency)$/i.test(
        field.type,
      );
      return (
        <Input
          {...injected}
          type="number"
          inputMode={stepDecimal || money ? "decimal" : "numeric"}
          step={stepDecimal || money ? "0.01" : undefined}
          min={field.min}
          max={field.max}
          leadingAddon={money ? <span aria-hidden>$</span> : undefined}
          value={(value as number | "") ?? ""}
          onChange={(e) => {
            const n = e.target.valueAsNumber;
            // Empty input -> NaN. Send null (not undefined) so a cleared
            // field is preserved by JSON.stringify and reaches the PATCH
            // body as an explicit clear; undefined would be dropped and the
            // backend would read the omitted key as "leave unchanged".
            onChange(Number.isNaN(n) ? null : n);
          }}
        />
      );
    }
    default:
      return (
        <Input
          {...injected}
          maxLength={field.max_length}
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
  }
}

interface RelationComboboxProps {
  field: FieldSpec;
  value: unknown;
  onChange: (value: unknown) => void;
  id?: string;
  invalid?: boolean;
  "aria-describedby"?: string;
  "aria-required"?: boolean;
}

/**
 * Searchable relation picker. Loads the candidate records for the
 * field's target ktype via the shared data layer and lets the operator
 * search by the record's human label while persisting the underlying
 * id. Only mounts for fields that resolve to a known relation target.
 */
function RelationCombobox({
  field,
  value,
  onChange,
  id,
  invalid,
  ...aria
}: RelationComboboxProps) {
  const target = relationTargetKtype(field);
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const containerRef = useRef<HTMLDivElement>(null);

  // Close the dropdown when the operator clicks anywhere outside it.
  useEffect(() => {
    if (!open) return;
    const onDocMouseDown = (e: MouseEvent) => {
      if (
        containerRef.current &&
        !containerRef.current.contains(e.target as Node)
      ) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onDocMouseDown);
    return () => document.removeEventListener("mousedown", onDocMouseDown);
  }, [open]);

  const recordsQuery = useQuery({
    queryKey: ["records", target],
    queryFn: () => api.listRecords(target as string),
    enabled: !!target,
    staleTime: 60_000,
  });

  const records = recordsQuery.data ?? [];
  const selected = records.find((r) => r.id === value);
  const filtered = query
    ? records.filter((r) =>
        recordLabel(r).toLowerCase().includes(query.toLowerCase()),
      )
    : records;

  const targetLabel = target
    ? humanizeToken(target.split(".").pop() ?? target)
    : "record";

  return (
    <div className="relative" ref={containerRef}>
      <Input
        {...aria}
        id={id}
        invalid={invalid}
        readOnly
        role="combobox"
        aria-expanded={open}
        placeholder={
          recordsQuery.isLoading
            ? "Loading…"
            : `Select a ${targetLabel.toLowerCase()}`
        }
        value={selected ? recordLabel(selected) : ""}
        trailingAddon={<ChevronsUpDown className="size-4" aria-hidden />}
        onClick={() => setOpen((o) => !o)}
        onKeyDown={(e) => {
          if (e.key === "Escape") setOpen(false);
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            setOpen((o) => !o);
          }
        }}
      />
      {open && (
        <div className="absolute z-20 mt-1 w-full overflow-hidden rounded-md border border-border bg-bg-elevated shadow-lg">
          <div className="p-2">
            <Input
              size="sm"
              autoFocus
              placeholder={`Search ${targetLabel.toLowerCase()}s`}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                // The combobox lives inside the record <form>; Enter in
                // this search box must filter, not submit the whole form.
                if (e.key === "Enter") e.preventDefault();
              }}
            />
          </div>
          <ul role="listbox" className="max-h-56 overflow-auto pb-1">
            {recordsQuery.isLoading && (
              <li className="px-3 py-2 text-sm text-fg-muted">Loading…</li>
            )}
            {recordsQuery.isError && (
              <li className="px-3 py-2 text-sm text-danger">
                Couldn't load options.
              </li>
            )}
            {!recordsQuery.isLoading && filtered.length === 0 && (
              <li className="px-3 py-2 text-sm text-fg-muted">No matches.</li>
            )}
            {filtered.map((r) => (
              <li key={r.id}>
                <button
                  type="button"
                  role="option"
                  aria-selected={r.id === value}
                  className={cn(
                    "flex w-full items-center px-3 py-2 text-start text-sm hover:bg-bg-subtle",
                    r.id === value && "bg-bg-subtle font-medium",
                  )}
                  onClick={() => {
                    onChange(r.id);
                    setOpen(false);
                    setQuery("");
                  }}
                >
                  {recordLabel(r)}
                </button>
              </li>
            ))}
          </ul>
          {value != null && value !== "" && (
            <button
              type="button"
              className="w-full border-t border-border px-3 py-2 text-start text-xs text-fg-muted hover:bg-bg-subtle"
              onClick={() => {
                onChange(null);
                setOpen(false);
              }}
            >
              Clear selection
            </button>
          )}
        </div>
      )}
    </div>
  );
}
