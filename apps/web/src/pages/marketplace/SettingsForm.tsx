import { useEffect, useMemo, useState } from "react";
import { Input, Select } from "@kapp/ui";

/**
 * SettingsForm renders the install / update-settings document
 * editor. When the version manifest declares a settings_schema
 * (JSON Schema draft-07 subset that the engine validates with
 * gojsonschema), this component renders a typed field-per-row UI
 * that the user can fill in without ever touching JSON. When the
 * manifest declares no schema (a permitted shape — settings are
 * then free-form), it falls back to a raw JSON textarea so the
 * user can still pass an arbitrary key/value bag.
 *
 * Validation is best-effort client-side. The engine re-validates
 * with the canonical schema before writing the row, so a client
 * that's out of date with a recent manifest update still gets a
 * correct 400 from the server.
 */
export type SettingsSchema = {
  type?: "object";
  properties?: Record<string, SettingsSchemaProperty>;
  required?: string[];
  additionalProperties?: boolean;
  // Best-effort top-level passthrough. Anything we don't model
  // explicitly we forward to the textarea fallback.
  [key: string]: unknown;
};

export type SettingsSchemaProperty = {
  type?: "string" | "number" | "integer" | "boolean" | "array" | "object";
  title?: string;
  description?: string;
  enum?: Array<string | number>;
  default?: unknown;
  format?: string;
  pattern?: string;
  minLength?: number;
  maxLength?: number;
  minimum?: number;
  maximum?: number;
  items?: { type?: "string" | "number" | "integer" | "boolean" };
};

export function SettingsForm({
  schema,
  value,
  onChange,
  disabled,
}: {
  schema: SettingsSchema | null;
  value: Record<string, unknown>;
  onChange: (next: Record<string, unknown>) => void;
  disabled?: boolean;
}) {
  if (!schema || !schema.properties || Object.keys(schema.properties).length === 0) {
    return (
      <FreeformJsonEditor
        value={value}
        onChange={onChange}
        disabled={disabled}
      />
    );
  }
  const required = new Set(schema.required ?? []);
  const props = schema.properties;
  return (
    <div style={{ display: "grid", gap: 12 }}>
      {Object.entries(props).map(([key, prop]) => (
        <SettingsField
          key={key}
          name={key}
          required={required.has(key)}
          schema={prop}
          value={value[key]}
          onChange={(next) => {
            const merged = { ...value };
            if (next === undefined) delete merged[key];
            else merged[key] = next;
            onChange(merged);
          }}
          disabled={disabled}
        />
      ))}
    </div>
  );
}

function SettingsField({
  name,
  required,
  schema,
  value,
  onChange,
  disabled,
}: {
  name: string;
  required: boolean;
  schema: SettingsSchemaProperty;
  value: unknown;
  onChange: (next: unknown) => void;
  disabled?: boolean;
}) {
  const label = schema.title ?? name;
  const description = schema.description;
  const id = `setting-${name}`;
  const control = renderControl({
    id,
    name,
    schema,
    value,
    onChange,
    disabled,
  });
  return (
    <div>
      <label
        htmlFor={id}
        style={{
          display: "block",
          fontSize: 13,
          fontWeight: 500,
          marginBottom: 4,
        }}
      >
        {label}
        {required && (
          <span style={{ color: "#dc2626", marginLeft: 4 }}>*</span>
        )}
      </label>
      {control}
      {description && (
        <p style={{ margin: "4px 0 0", fontSize: 12, color: "#6b7280" }}>
          {description}
        </p>
      )}
    </div>
  );
}

function renderControl({
  id,
  name,
  schema,
  value,
  onChange,
  disabled,
}: {
  id: string;
  name: string;
  schema: SettingsSchemaProperty;
  value: unknown;
  onChange: (next: unknown) => void;
  disabled?: boolean;
}) {
  // Enum first — always renders as a Select regardless of base
  // type (the engine treats enum membership as a hard gate).
  if (schema.enum && schema.enum.length > 0) {
    const current = value === undefined ? "" : String(value);
    return (
      <Select
        id={id}
        name={name}
        value={current}
        disabled={disabled}
        onChange={(e) => {
          const raw = e.target.value;
          if (raw === "") {
            onChange(undefined);
            return;
          }
          // Round-trip number-typed enums back to numbers; the
          // engine's validator rejects "1" when the schema says
          // type: integer + enum: [1, 2, 3].
          if (schema.type === "number" || schema.type === "integer") {
            onChange(Number(raw));
            return;
          }
          onChange(raw);
        }}
      >
        <option value="">— Select —</option>
        {schema.enum.map((opt) => (
          <option key={String(opt)} value={String(opt)}>
            {String(opt)}
          </option>
        ))}
      </Select>
    );
  }
  switch (schema.type) {
    case "boolean": {
      const checked = value === true;
      return (
        <input
          id={id}
          name={name}
          type="checkbox"
          checked={checked}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
        />
      );
    }
    case "integer":
    case "number": {
      const current = value === undefined || value === null ? "" : String(value);
      return (
        <Input
          id={id}
          name={name}
          type="number"
          step={schema.type === "integer" ? 1 : "any"}
          min={schema.minimum}
          max={schema.maximum}
          value={current}
          disabled={disabled}
          onChange={(e) => {
            const raw = e.target.value;
            if (raw === "") {
              onChange(undefined);
              return;
            }
            const n = Number(raw);
            if (Number.isNaN(n)) return;
            onChange(schema.type === "integer" ? Math.trunc(n) : n);
          }}
        />
      );
    }
    case "array": {
      // Render arrays of primitives as a comma-separated text
      // input — adequate for the common case (allowed regions,
      // webhook URLs, etc.) without inventing a per-row UI.
      const current = Array.isArray(value)
        ? value.map((v) => String(v)).join(", ")
        : "";
      const itemType = schema.items?.type ?? "string";
      return (
        <Input
          id={id}
          name={name}
          type="text"
          value={current}
          disabled={disabled}
          placeholder="comma,separated,values"
          onChange={(e) => {
            const parts = e.target.value
              .split(",")
              .map((s) => s.trim())
              .filter((s) => s.length > 0);
            if (parts.length === 0) {
              onChange(undefined);
              return;
            }
            const converted = parts.map((p) => {
              if (itemType === "integer" || itemType === "number") {
                const n = Number(p);
                return Number.isNaN(n) ? p : n;
              }
              if (itemType === "boolean") return p === "true";
              return p;
            });
            onChange(converted);
          }}
        />
      );
    }
    case "object": {
      // Nested object — render as a JSON textarea. A first-class
      // recursive renderer is possible but unjustified at this
      // point (the engine's schemas are typically flat).
      return (
        <NestedJsonEditor
          id={id}
          value={value}
          onChange={onChange}
          disabled={disabled}
        />
      );
    }
    case "string":
    default: {
      const current = value === undefined || value === null ? "" : String(value);
      return (
        <Input
          id={id}
          name={name}
          type={schema.format === "email" ? "email" : schema.format === "uri" ? "url" : "text"}
          value={current}
          disabled={disabled}
          minLength={schema.minLength}
          maxLength={schema.maxLength}
          pattern={schema.pattern}
          onChange={(e) => {
            const raw = e.target.value;
            if (raw === "") {
              onChange(undefined);
              return;
            }
            onChange(raw);
          }}
        />
      );
    }
  }
}

function NestedJsonEditor({
  id,
  value,
  onChange,
  disabled,
}: {
  id: string;
  value: unknown;
  onChange: (next: unknown) => void;
  disabled?: boolean;
}) {
  const [text, setText] = useState(() =>
    value === undefined ? "" : JSON.stringify(value, null, 2),
  );
  const [error, setError] = useState<string | null>(null);
  return (
    <div>
      <textarea
        id={id}
        rows={5}
        value={text}
        disabled={disabled}
        onChange={(e) => {
          setText(e.target.value);
          if (e.target.value.trim() === "") {
            setError(null);
            onChange(undefined);
            return;
          }
          try {
            const parsed = JSON.parse(e.target.value);
            setError(null);
            onChange(parsed);
          } catch (err) {
            setError((err as Error).message);
          }
        }}
        style={{
          width: "100%",
          fontFamily: "monospace",
          fontSize: 13,
          padding: 8,
          border: "1px solid #d1d5db",
          borderRadius: 6,
        }}
      />
      {error && (
        <p style={{ color: "#b91c1c", fontSize: 12, margin: "4px 0 0" }}>
          {error}
        </p>
      )}
    </div>
  );
}

// FreeformJsonEditor is the fallback for installs whose manifest
// declared no settings_schema. Settings is then an unrestricted
// key/value bag; the engine accepts the document as-is. We still
// validate JSON parsability client-side so a syntax error doesn't
// surface as a 400 from the server.
function FreeformJsonEditor({
  value,
  onChange,
  disabled,
}: {
  value: Record<string, unknown>;
  onChange: (next: Record<string, unknown>) => void;
  disabled?: boolean;
}) {
  const initial = useMemo(
    () => JSON.stringify(value ?? {}, null, 2),
    // Intentional dependency-list omission of `value`: this is
    // an uncontrolled-textarea pattern — we want the seed text
    // only on first mount, not on every parent re-render. The
    // textarea's own `onChange` is the source of truth from
    // then on.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );
  const [text, setText] = useState(initial);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    // Reset only when the parent legitimately resets the value
    // to a fresh object (e.g. user switched extensions). We
    // compare to the stable empty representation to avoid
    // round-tripping the text on every keystroke.
    if (Object.keys(value).length === 0 && text !== "{}" && text !== "") {
      setText("");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value]);
  return (
    <div>
      <p style={{ margin: "0 0 6px", fontSize: 12, color: "#6b7280" }}>
        This extension does not declare a settings schema. Provide a JSON
        object with any keys the extension expects, or leave empty.
      </p>
      <textarea
        rows={6}
        value={text}
        disabled={disabled}
        placeholder='{"api_key":"…"}'
        onChange={(e) => {
          const raw = e.target.value;
          setText(raw);
          const trimmed = raw.trim();
          if (trimmed === "" || trimmed === "{}") {
            setError(null);
            onChange({});
            return;
          }
          try {
            const parsed = JSON.parse(trimmed);
            if (
              typeof parsed !== "object" ||
              parsed === null ||
              Array.isArray(parsed)
            ) {
              setError("Settings must be a JSON object.");
              return;
            }
            setError(null);
            onChange(parsed as Record<string, unknown>);
          } catch (err) {
            setError((err as Error).message);
          }
        }}
        style={{
          width: "100%",
          fontFamily: "monospace",
          fontSize: 13,
          padding: 8,
          border: "1px solid #d1d5db",
          borderRadius: 6,
        }}
      />
      {error && (
        <p style={{ color: "#b91c1c", fontSize: 12, margin: "4px 0 0" }}>
          {error}
        </p>
      )}
    </div>
  );
}

// validateAgainstSchema is the client-side fast-fail validator.
// Implements the subset of JSON Schema draft-07 the engine
// commonly uses (required, type, enum, minLength, maxLength,
// minimum, maximum, pattern). Returns the first error message
// found or null if the document is valid. The server runs the
// canonical check; we only block round-trips that we're sure
// would 400.
export function validateAgainstSchema(
  schema: SettingsSchema,
  value: Record<string, unknown>,
): string | null {
  const required = schema.required ?? [];
  for (const key of required) {
    const v = value[key];
    if (v === undefined || v === null || v === "") {
      const prop = schema.properties?.[key];
      const label = prop?.title ?? key;
      return `${label} is required.`;
    }
  }
  const props = schema.properties ?? {};
  for (const [key, prop] of Object.entries(props)) {
    if (!(key in value) || value[key] === undefined) continue;
    const err = checkProperty(prop, key, value[key]);
    if (err) return err;
  }
  return null;
}

function checkProperty(
  schema: SettingsSchemaProperty,
  key: string,
  value: unknown,
): string | null {
  const label = schema.title ?? key;
  if (schema.enum && schema.enum.length > 0) {
    const ok = schema.enum.some((opt) => opt === value);
    if (!ok) {
      return `${label} must be one of: ${schema.enum.join(", ")}.`;
    }
  }
  switch (schema.type) {
    case "string": {
      if (typeof value !== "string") return `${label} must be a string.`;
      if (schema.minLength !== undefined && value.length < schema.minLength) {
        return `${label} must be at least ${schema.minLength} characters.`;
      }
      if (schema.maxLength !== undefined && value.length > schema.maxLength) {
        return `${label} must be at most ${schema.maxLength} characters.`;
      }
      if (schema.pattern) {
        try {
          const re = new RegExp(schema.pattern);
          if (!re.test(value)) {
            return `${label} does not match the required pattern.`;
          }
        } catch {
          // Invalid regex in the schema — surface to the
          // server which will return a 400 with a richer
          // message than we can synthesise.
        }
      }
      return null;
    }
    case "integer":
    case "number": {
      if (typeof value !== "number" || Number.isNaN(value)) {
        return `${label} must be a number.`;
      }
      if (schema.type === "integer" && !Number.isInteger(value)) {
        return `${label} must be an integer.`;
      }
      if (schema.minimum !== undefined && value < schema.minimum) {
        return `${label} must be ≥ ${schema.minimum}.`;
      }
      if (schema.maximum !== undefined && value > schema.maximum) {
        return `${label} must be ≤ ${schema.maximum}.`;
      }
      return null;
    }
    case "boolean":
      if (typeof value !== "boolean") return `${label} must be true or false.`;
      return null;
    case "array":
      if (!Array.isArray(value)) return `${label} must be an array.`;
      return null;
    case "object":
      if (typeof value !== "object" || value === null || Array.isArray(value)) {
        return `${label} must be an object.`;
      }
      return null;
    default:
      return null;
  }
}
