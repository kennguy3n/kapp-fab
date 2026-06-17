import { useState } from "react";
import { useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { FieldSpec } from "@kapp/client";
import {
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Select,
  Skeleton,
  Textarea,
  cn,
} from "@kapp/ui";
import { AlertTriangle, CheckCircle2 } from "lucide-react";
import { api } from "../lib/api";
import {
  humanizeLabel,
  humanizeToken,
  isMoneyField,
  ktypeSingular,
  resolveControl,
} from "../lib/ktypeView";

type FormErrors = Record<string, string>;

const isEmpty = (value: unknown): boolean =>
  value == null ||
  value === "" ||
  (Array.isArray(value) && value.length === 0);

/**
 * FormPage is the public, tenant-less form renderer. It fetches the
 * form config + its KType schema from `GET /api/v1/forms/{id}` and
 * builds a typed, accessible form from the schema metadata — the same
 * humanized labels and per-type controls the authenticated record
 * editor uses. Submission POSTs to the tenant-scoped submit endpoint;
 * the backend infers tenant from the form id so no auth/header is
 * required from the visitor.
 */
export function FormPage() {
  const { formId } = useParams<{ formId: string }>();
  const [values, setValues] = useState<Record<string, unknown>>({});
  const [errors, setErrors] = useState<FormErrors>({});
  const [status, setStatus] = useState<
    "idle" | "submitting" | "submitted" | "error"
  >("idle");
  const [error, setError] = useState<string | null>(null);

  const formQuery = useQuery({
    queryKey: ["public-form", formId],
    queryFn: () => api.getPublicForm(formId!),
    enabled: !!formId,
  });

  if (!formId) return null;

  if (formQuery.isLoading) {
    return (
      <Shell>
        <div className="space-y-4">
          <Skeleton className="h-8 w-2/3" />
          <Skeleton className="h-4 w-full" />
          <div className="space-y-4 pt-2">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        </div>
      </Shell>
    );
  }

  if (formQuery.error || !formQuery.data) {
    return (
      <Shell>
        <EmptyState
          icon={<AlertTriangle className="size-6" aria-hidden />}
          title="This form isn't available"
          description="The link may be incorrect or the form is no longer accepting responses."
          action={
            <Button variant="secondary" onClick={() => formQuery.refetch()}>
              Try again
            </Button>
          }
        />
      </Shell>
    );
  }

  const { form, schema } = formQuery.data;
  const fields: FieldSpec[] = schema.fields ?? [];
  const title = form.config?.title || ktypeSingular(schema.name);

  const validate = (): FormErrors => {
    const next: FormErrors = {};
    for (const field of fields) {
      const value = values[field.name];
      if (field.required && isEmpty(value)) {
        next[field.name] = `${humanizeLabel(field.name)} is required.`;
        continue;
      }
      if (isEmpty(value)) continue;
      if (
        resolveControl(field) === "email" &&
        typeof value === "string" &&
        !/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(value)
      ) {
        next[field.name] = "Enter a valid email address.";
      }
    }
    return next;
  };

  const update = (name: string, value: unknown) => {
    setValues((prev) => ({ ...prev, [name]: value }));
    setErrors((e) => {
      if (!e[name]) return e;
      const copy = { ...e };
      delete copy[name];
      return copy;
    });
  };

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const found = validate();
    setErrors(found);
    if (Object.keys(found).length > 0) return;
    setStatus("submitting");
    setError(null);
    try {
      await api.submitPublicForm(formId, values);
      setStatus("submitted");
      if (form.config?.redirect_url) {
        window.location.href = form.config.redirect_url;
      }
    } catch (err) {
      setStatus("error");
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  if (status === "submitted") {
    return (
      <Shell>
        <EmptyState
          icon={<CheckCircle2 className="size-6 text-success" aria-hidden />}
          title="Thanks — your response was received"
          description="You can safely close this page. We'll be in touch if we need anything else."
        />
      </Shell>
    );
  }

  const errorCount = Object.keys(errors).length;

  return (
    <Shell>
      <form onSubmit={onSubmit} noValidate className="space-y-6">
        <header className="space-y-1">
          <Eyebrow>Form</Eyebrow>
          <h1 className="text-2xl font-semibold tracking-tight text-fg">
            {title}
          </h1>
          {form.config?.description && (
            <p className="text-sm text-fg-muted">{form.config.description}</p>
          )}
        </header>

        {errorCount > 0 && (
          <div
            role="alert"
            className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger"
          >
            Please fix {errorCount} {errorCount === 1 ? "field" : "fields"}{" "}
            below.
          </div>
        )}

        <div className="space-y-4">
          {fields.map((f) => (
            <PublicField
              key={f.name}
              field={f}
              value={values[f.name]}
              error={errors[f.name]}
              onChange={(v) => update(f.name, v)}
            />
          ))}
        </div>

        {error && (
          <div className="text-sm text-danger" role="alert">
            {error}
          </div>
        )}

        <div className="flex justify-end border-t border-border pt-4">
          <Button type="submit" disabled={status === "submitting"}>
            {status === "submitting" ? "Submitting…" : "Submit"}
          </Button>
        </div>
      </form>
    </Shell>
  );
}

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div className="mx-auto my-12 w-full max-w-[600px] px-4">
      <div className="rounded-2xl border border-border bg-bg-elevated p-6 shadow-sm sm:p-8">
        {children}
      </div>
    </div>
  );
}

interface PublicFieldProps {
  field: FieldSpec;
  value: unknown;
  error?: string;
  onChange: (value: unknown) => void;
}

function PublicField({ field, value, error, onChange }: PublicFieldProps) {
  const control = resolveControl(field);
  const label = humanizeLabel(field.name);
  const required = !!field.required;

  if (control === "boolean") {
    return (
      <div className="flex flex-col gap-1.5">
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
      help={field.max_length && control === "textarea"
        ? `Up to ${field.max_length} characters.`
        : undefined}
      className={cn(control === "textarea" && "[&_textarea]:min-h-24")}
    >
      <PublicControl
        field={field}
        control={control}
        value={value}
        onChange={onChange}
      />
    </Field>
  );
}

interface PublicControlProps {
  field: FieldSpec;
  control: ReturnType<typeof resolveControl>;
  value: unknown;
  onChange: (value: unknown) => void;
  id?: string;
  invalid?: boolean;
  "aria-describedby"?: string;
  "aria-required"?: boolean;
}

function PublicControl({
  field,
  control,
  value,
  onChange,
  ...injected
}: PublicControlProps) {
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
      const money = isMoneyField(field);
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
