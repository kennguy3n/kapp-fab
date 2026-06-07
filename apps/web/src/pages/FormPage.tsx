import { useState } from "react";
import { useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { FieldSpec } from "@kapp/client";
import { Button, Input, Select } from "@kapp/ui";
import { api } from "../lib/api";

/**
 * FormPage is the public, tenant-less form renderer. It fetches the
 * form config + its KType schema from `GET /api/v1/forms/{id}` and
 * builds a simple HTML form. Submission POSTs to the tenant-scoped
 * submit endpoint; the backend infers tenant from the form id so no
 * auth/header is required from the visitor.
 */
export function FormPage() {
  const { formId } = useParams<{ formId: string }>();
  const [values, setValues] = useState<Record<string, unknown>>({});
  const [status, setStatus] = useState<"idle" | "submitting" | "submitted" | "error">(
    "idle",
  );
  const [error, setError] = useState<string | null>(null);

  const formQuery = useQuery({
    queryKey: ["public-form", formId],
    queryFn: () => api.getPublicForm(formId!),
    enabled: !!formId,
  });

  if (!formId) return null;
  if (formQuery.isLoading) return <div>Loading form…</div>;
  if (formQuery.error || !formQuery.data) return <div>Form not available.</div>;

  const { form, schema } = formQuery.data;
  const fields: FieldSpec[] = schema.fields ?? [];

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
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
      <div className="mx-auto my-12 max-w-[540px] p-6">
        <h2>Thanks — your submission was received.</h2>
      </div>
    );
  }

  return (
    <form onSubmit={onSubmit} className="mx-auto my-12 max-w-[540px] p-6">
      <h1>{form.config?.title || schema.name}</h1>
      {form.config?.description && (
        <p className="text-fg-muted">{form.config.description}</p>
      )}
      <div className="flex flex-col gap-3">
        {fields.map((f) => (
          <FormField
            key={f.name}
            field={f}
            value={values[f.name]}
            onChange={(v) => setValues((prev) => ({ ...prev, [f.name]: v }))}
          />
        ))}
      </div>
      {error && <div className="mt-3 text-danger">{error}</div>}
      <Button type="submit" disabled={status === "submitting"} className="mt-4">
        {status === "submitting" ? "Submitting…" : "Submit"}
      </Button>
    </form>
  );
}

interface FormFieldProps {
  field: FieldSpec;
  value: unknown;
  onChange: (value: unknown) => void;
}

function FormField({ field, value, onChange }: FormFieldProps) {
  const label = (
    <label className="mb-1 block font-medium">
      {field.name}
      {field.required ? " *" : ""}
    </label>
  );
  if (field.type === "enum" && field.values) {
    return (
      <div>
        {label}
        <Select
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
          required={field.required}
        >
          <option value="">—</option>
          {field.values.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </Select>
      </div>
    );
  }
  if (field.type === "text") {
    return (
      <div>
        {label}
        <textarea
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
          required={field.required}
          rows={4}
          className="w-full rounded-md border border-border bg-bg px-3 py-2 text-sm"
        />
      </div>
    );
  }
  const inputType = field.type === "number" || field.type === "integer"
    ? "number"
    : field.type === "date"
      ? "date"
      : "text";
  return (
    <div>
      {label}
      <Input
        type={inputType}
        value={(value as string) ?? ""}
        onChange={(e) =>
          onChange(inputType === "number" ? Number(e.target.value) : e.target.value)
        }
        required={field.required}
        maxLength={field.max_length}
        className="w-full"
      />
    </div>
  );
}
