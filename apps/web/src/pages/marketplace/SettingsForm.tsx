import { useEffect, useRef, useState } from "react";
import { Input, Select } from "@kapp/ui";

// Stable validity key for the no-schema fallback editor. There is
// at most one FreeformJsonEditor in the tree at any time (it's
// the no-schema branch in SettingsForm), so a constant key is
// sufficient — the parent's per-key validity map (see
// InstallationDetailPage.settingsInvalidKeys) tracks freeform
// independently of any schema-driven NestedJsonEditor instances
// (which use their own `id` as the validity key, one per object-
// typed field). Exported so tests can refer to it by symbol.
export const FREEFORM_VALIDITY_KEY = "__settings_freeform__";

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
  onValidityChange,
  disabled,
}: {
  schema: SettingsSchema | null;
  value: Record<string, unknown>;
  onChange: (next: Record<string, unknown>) => void;
  // onValidityChange propagates the JSON-textarea editors'
  // parse-error state up to the parent so the Save button can
  // be disabled while the textarea contents are unparseable.
  // Without this, the parent's settingsDraft retains the LAST
  // valid parsed value while the user is mid-edit on invalid
  // JSON — the textarea shows the error message, but Save is
  // still enabled and would send the stale-but-valid draft
  // instead of the text currently visible. The server
  // re-validates so data is safe, but the UX is confusing: a
  // successful save with data that does not match what the user
  // last typed. Schema-driven controls (Input, Select) are by
  // construction always parsable, so the typed-field path does
  // not fire this callback — it stays implicitly valid.
  //
  // The signature carries a per-editor `key` so multiple
  // editors mounted at the same time don't race — each signal
  // identifies which editor it came from, and the parent
  // maintains a per-key validity map. Today only one editor
  // (the no-schema FreeformJsonEditor) is mounted at a time, but
  // B6.2 will wire settings_schema with potentially multiple
  // object-typed fields each rendering a NestedJsonEditor; the
  // map-based parent state is the correct shape for that.
  onValidityChange?: (key: string, valid: boolean) => void;
  disabled?: boolean;
}) {
  if (!schema || !schema.properties || Object.keys(schema.properties).length === 0) {
    return (
      <FreeformJsonEditor
        value={value}
        onChange={onChange}
        onValidityChange={onValidityChange}
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
          onValidityChange={onValidityChange}
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
  onValidityChange,
  disabled,
}: {
  name: string;
  required: boolean;
  schema: SettingsSchemaProperty;
  value: unknown;
  onChange: (next: unknown) => void;
  onValidityChange?: (key: string, valid: boolean) => void;
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
    onValidityChange,
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
  onValidityChange,
  disabled,
}: {
  id: string;
  name: string;
  schema: SettingsSchemaProperty;
  value: unknown;
  onChange: (next: unknown) => void;
  onValidityChange?: (key: string, valid: boolean) => void;
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
          onValidityChange={onValidityChange}
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
  onValidityChange,
  disabled,
}: {
  id: string;
  value: unknown;
  onChange: (next: unknown) => void;
  onValidityChange?: (key: string, valid: boolean) => void;
  disabled?: boolean;
}) {
  // Uncontrolled-with-buffer pattern: the textarea owns its own
  // text state so the user can keep typing through an in-flight
  // invalid-JSON moment (e.g. `{"foo":` mid-keystroke) without
  // the parent's re-render resetting the cursor to the previous
  // valid value. The trade-off is that a parent-driven reset
  // (Discard changes, save success, cross-tab refetch, switch
  // to a different installation) won't propagate into the
  // textarea on its own.
  //
  // We resolve that by requiring callers to remount the editor
  // when they want a reset, via React's standard `key` prop on
  // the enclosing SettingsForm. The init state below seeds from
  // the value prop on mount only — re-seeding on a subsequent
  // render would race with mid-typing edits. See the resetKey
  // pattern in InstallationDetailPage for the parent contract.
  const [text, setText] = useState(() =>
    value === undefined ? "" : JSON.stringify(value, null, 2),
  );
  const [error, setError] = useState<string | null>(null);
  // Propagate validity up so the parent can disable Save when
  // the textarea contents are unparseable. Effect-on-change so
  // we only fire when the local error transitions, and on
  // unmount we restore valid — if a remount drops this editor
  // from the tree we don't want a stale "invalid" sticking the
  // parent's Save in disabled. Without this, the user could see
  // an invalid-JSON error in the editor while Save is enabled
  // and would silently send the last valid parsed draft instead
  // of the text on screen — the textarea "loses" to the cache.
  //
  // We capture onValidityChange in a ref so the unmount cleanup
  // path below always reaches the LATEST callback identity,
  // never the one captured on initial mount. In practice the
  // parent passes a useCallback-stable closure today, but the
  // ref pattern makes the editor robust to identity churn — a
  // future caller can inline an arrow function without
  // accidentally stranding a stale closure on the unmount path.
  const onValidityChangeRef = useRef(onValidityChange);
  useEffect(() => {
    onValidityChangeRef.current = onValidityChange;
  }, [onValidityChange]);
  const lastSignalled = useRef<boolean | null>(null);
  useEffect(() => {
    const valid = error === null;
    if (lastSignalled.current === valid) return;
    lastSignalled.current = valid;
    onValidityChangeRef.current?.(id, valid);
  }, [error, id]);
  useEffect(() => {
    return () => {
      if (lastSignalled.current === false) {
        onValidityChangeRef.current?.(id, true);
      }
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
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
//
// Reset semantics: uncontrolled-with-buffer (same as
// NestedJsonEditor). The textarea seeds from the value prop on
// mount and never re-seeds — this preserves cursor position +
// mid-typing invalid-JSON state through unrelated parent
// re-renders (e.g. a sibling mutation rewriting cache). Parent-
// driven resets (Discard, save success, cross-tab refetch,
// switch installation) MUST go through React's `key` prop on
// SettingsForm — incrementing the key remounts this component
// and re-seeds from the fresh value prop. See resetKey in
// InstallationDetailPage for the contract.
//
// A previous implementation tried to auto-detect resets by
// watching `value` in a useEffect and only resyncing when
// Object.keys(value).length === 0. That broke the common case
// (settings already populated, user types over them, then
// clicks Discard — Discard sets value back to the previous
// non-empty object, which the useEffect couldn't distinguish
// from the textarea's own emit, so the textarea kept the
// pre-discard edits and the user could accidentally save
// stale data on the next keystroke).
function FreeformJsonEditor({
  value,
  onChange,
  onValidityChange,
  disabled,
}: {
  value: Record<string, unknown>;
  onChange: (next: Record<string, unknown>) => void;
  onValidityChange?: (key: string, valid: boolean) => void;
  disabled?: boolean;
}) {
  const [text, setText] = useState(() =>
    Object.keys(value ?? {}).length === 0
      ? ""
      : JSON.stringify(value, null, 2),
  );
  const [error, setError] = useState<string | null>(null);
  // See NestedJsonEditor for the validity-propagation contract.
  // Identical pattern: signal on transition, restore-to-valid
  // on unmount so a remount can't leave the parent's Save stuck
  // in disabled. The ref-based latest-callback capture protects
  // the unmount cleanup against onValidityChange identity churn.
  // We pass FREEFORM_VALIDITY_KEY as the key because at most one
  // FreeformJsonEditor exists in the tree at a time (no-schema
  // branch); the parent's per-key map disambiguates this from
  // any schema-driven NestedJsonEditor signals.
  const onValidityChangeRef = useRef(onValidityChange);
  useEffect(() => {
    onValidityChangeRef.current = onValidityChange;
  }, [onValidityChange]);
  const lastSignalled = useRef<boolean | null>(null);
  useEffect(() => {
    const valid = error === null;
    if (lastSignalled.current === valid) return;
    lastSignalled.current = valid;
    onValidityChangeRef.current?.(FREEFORM_VALIDITY_KEY, valid);
  }, [error]);
  useEffect(() => {
    return () => {
      if (lastSignalled.current === false) {
        onValidityChangeRef.current?.(FREEFORM_VALIDITY_KEY, true);
      }
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
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
