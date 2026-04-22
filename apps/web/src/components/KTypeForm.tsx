import { useState } from "react";
import type { KType, FieldSpec } from "@kapp/client";
import { Button, Input, Select } from "@kapp/ui";

interface KTypeFormProps {
  ktype: KType;
  initialData?: Record<string, unknown>;
  onSubmit: (data: Record<string, unknown>) => void;
}

export function KTypeForm({ ktype, initialData, onSubmit }: KTypeFormProps) {
  const [data, setData] = useState<Record<string, unknown>>(initialData ?? {});
  const fields: FieldSpec[] = ktype.schema?.fields ?? [];

  const update = (name: string, value: unknown) =>
    setData((d) => ({ ...d, [name]: value }));

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit(data);
      }}
    >
      {fields.map((field) => (
        <div key={field.name} style={{ marginBottom: 12 }}>
          <label>
            {field.name}
            {field.required ? " *" : ""}
          </label>
          <FieldInput
            field={field}
            value={data[field.name]}
            onChange={(v) => update(field.name, v)}
          />
        </div>
      ))}
      <Button type="submit">Save</Button>
    </form>
  );
}

interface FieldInputProps {
  field: FieldSpec;
  value: unknown;
  onChange: (value: unknown) => void;
}

function FieldInput({ field, value, onChange }: FieldInputProps) {
  switch (field.type) {
    case "string":
    case "text":
      return (
        <Input
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
          maxLength={field.max_length}
        />
      );
    case "integer":
    case "number":
    case "float":
    case "decimal":
      return (
        <Input
          type="number"
          value={(value as number | "") ?? ""}
          onChange={(e) => onChange(e.target.valueAsNumber)}
        />
      );
    case "boolean":
      return (
        <input
          type="checkbox"
          checked={!!value}
          onChange={(e) => onChange(e.target.checked)}
        />
      );
    case "date":
      return (
        <Input
          type="date"
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "datetime":
      return (
        <Input
          type="datetime-local"
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "enum":
      return (
        <Select
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="">—</option>
          {(field.values ?? []).map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </Select>
      );
    default:
      return (
        <Input
          value={(value as string) ?? ""}
          onChange={(e) => onChange(e.target.value)}
        />
      );
  }
}
