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
 * useValiditySignal centralises the validity-propagation contract
 * shared by every uncontrolled-buffer JSON editor in this file
 * (FreeformJsonEditor today, NestedJsonEditor today, and any
 * future schema-driven editors B6.2 lights up — for example a
 * dedicated "array of objects" editor that round-trips through a
 * textarea instead of typed inputs).
 *
 * The hook encodes four invariants the editors had previously
 * duplicated (round-13 ANALYSIS_0005 — Devin Review observed the
 * two copies sitting side-by-side were ~25 lines of identical
 * boilerplate). All four are load-bearing for correctness:
 *
 *   1. Latest-callback capture via ref. The parent passes a
 *      useCallback-stable closure today, but the contract should
 *      remain robust to callback identity churn — a future caller
 *      that inlines an arrow function must not strand a stale
 *      closure on the unmount cleanup path. We mirror the
 *      onValidityChange identity into a ref synchronously on
 *      every render (via the effect), then always invoke through
 *      the ref so signal-on-error and signal-on-unmount both
 *      reach the latest callback.
 *
 *   2. Dedup on the signal effect. error → boolean → "signal
 *      only on transition" prevents re-signalling on every render
 *      that doesn't change validity. Without this, the parent's
 *      setSettingsInvalidKeys would receive false-positive churn
 *      on every keystroke that doesn't move the error state,
 *      which (with the parent's Set identity check) is benign
 *      today but couples performance to React's batching.
 *
 *   3. Restore-to-valid on unmount. If a remount drops the editor
 *      from the tree (parent triggers a SettingsForm key change
 *      on version swap or Discard), we don't want a stale invalid
 *      flag sticking the parent's Save in disabled. The cleanup
 *      checks lastSignalled === false and only signals "valid"
 *      if we'd previously signalled "invalid" — avoiding a
 *      spurious signal in the steady-state-valid case.
 *
 *   4. Empty dep array on the unmount cleanup. The cleanup MUST
 *      fire exactly once at component teardown, never on every
 *      render and never on dep change. Adding `key` to the deps
 *      would be actively wrong: the cleanup would fire on key
 *      change, signal (oldKey, true) against the parent's
 *      invalidKeys set (clearing the wrong entry, since the OLD
 *      key is still sitting in the set), and then the rebuilt
 *      effect would never signal (newKey, false) — only the
 *      keystroke effect does that, and only on a transition.
 *      The eslint disable below is intentional and load-bearing.
 *
 * Inputs:
 *   - key: the validity-map key under which the parent tracks
 *     this editor's validity. For NestedJsonEditor, this is the
 *     stable `id` = `setting-${name}`; for FreeformJsonEditor,
 *     this is the module-level FREEFORM_VALIDITY_KEY constant.
 *     The key must be structurally stable across the editor
 *     instance's lifetime (parent reconciles a new instance via
 *     React key on schema property change, so within one
 *     instance the key never changes).
 *   - error: the editor's local parse-error state. null →
 *     valid; non-null → invalid. Dependency drives the signal
 *     effect.
 *   - onValidityChange: optional parent callback (the dialog or
 *     detail page may opt out of validity tracking, in which
 *     case the editor renders without gating Save).
 */
function useValiditySignal(
  key: string,
  error: string | null,
  onValidityChange?: (key: string, valid: boolean) => void,
) {
  // Round-14 ANALYSIS_0002: assign the ref synchronously DURING
  // render, not in a useEffect. The effect-based variant would
  // be correct today (React guarantees commit-phase effects fire
  // in declaration order, so the ref-update effect would always
  // commit before the signal effect that reads through it), but
  // it introduces a theoretical one-frame gap that widens once
  // a caller wraps the editor in a Suspense boundary or a
  // useTransition that interrupts/resumes renders — the render
  // would suspend with the OLD callback captured into the
  // closure that schedules the signal effect, and on resume the
  // effect would still fire through the stale callback because
  // its dep array (`error, key`) didn't change. The synchronous
  // in-render assignment is the modern React idiom (see
  // react.dev "Avoiding recreating the ref contents") and
  // closes the theoretical gap. The trade-off is that the
  // assignment runs on every render rather than only when the
  // identity changes, but assignment to a ref slot is
  // effectively free (no allocation, no scheduling) — strictly
  // safer for no measurable cost.
  const onValidityChangeRef = useRef(onValidityChange);
  onValidityChangeRef.current = onValidityChange;
  const lastSignalled = useRef<boolean | null>(null);
  useEffect(() => {
    const valid = error === null;
    if (lastSignalled.current === valid) return;
    lastSignalled.current = valid;
    onValidityChangeRef.current?.(key, valid);
  }, [error, key]);
  useEffect(() => {
    return () => {
      if (lastSignalled.current === false) {
        onValidityChangeRef.current?.(key, true);
      }
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
}

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
  // Round-7 ANALYSIS_0003: type-check the value prop at mount,
  // not just on keystroke. Pre-fix, the lazy initialiser
  // stringified ANY value — including null, arrays, and
  // primitives — and the keystroke-time check at line ~462
  // only fired on user input. So if the server returned a
  // mismatched value for a `type: "object"` field (most
  // commonly `{"config": null}`, since a missing Go map
  // marshals as JSON null and a future endpoint that omits
  // the handler-side coercion would surface it), the textarea
  // would display `"null"` with no error, the validity signal
  // would be valid, and Save would be enabled — until the
  // user typed a single character. That meant a user who
  // simply opened the editor and clicked Save would
  // round-trip the wrong-type value back to the server, which
  // would 400 with no obvious local diagnostic. Mirroring the
  // keystroke check at the initial state closes the gap and
  // means the user sees the diagnostic as soon as the editor
  // mounts.
  //
  // The text buffer continues to show the raw stringified
  // value (including literal `null`, `[1,2]`, `42`) so the
  // user can see exactly what the server returned and make
  // an informed decision about replacing it. We do NOT
  // collapse the buffer to empty — that would hide the
  // server's payload and leave the user wondering "what was
  // actually stored?". `undefined` is the only case that
  // legitimately yields an empty buffer (the field is unset).
  const [text, setText] = useState(() =>
    value === undefined ? "" : JSON.stringify(value, null, 2),
  );
  const [error, setError] = useState<string | null>(() => {
    // undefined → field is unset; empty buffer + valid signal.
    // The engine's server-side gojsonschema will reject the
    // empty payload if the field is required; that's the
    // correct surface for required-field errors.
    if (value === undefined) return null;
    if (!isPlainObject(value)) {
      return "Expected an object (got " + describeJsonType(value) + ")";
    }
    return null;
  });
  // Round-13 ANALYSIS_0005: the four-invariant validity
  // contract (latest-callback ref, dedup-on-transition, restore-
  // to-valid on unmount, empty-dep cleanup) lives in
  // `useValiditySignal` so this editor and FreeformJsonEditor
  // can't drift, and any future schema-driven editor B6.2 adds
  // inherits the correct behaviour by composition. The hook's
  // docstring (top of file) carries the full rationale,
  // including why an empty dep array on the unmount cleanup is
  // load-bearing here: `id` = `setting-${name}` is structurally
  // stable across this editor instance's lifetime (parent uses
  // `name` as React's key on <SettingsField>, so any change to
  // the property identity triggers a remount with a fresh
  // mount-time id capture rather than a mid-lifetime id swap).
  useValiditySignal(id, error, onValidityChange);
  return (
    <div>
      <textarea
        id={id}
        rows={5}
        value={text}
        disabled={disabled}
        onChange={(e) => {
          const raw = e.target.value;
          setText(raw);
          // Round-10 ANALYSIS_0002: mirror the
          // FreeformJsonEditor whitespace handling (round-8
          // ANALYSIS_0005). Pre-fix, NestedJsonEditor collapsed
          // both `raw === ""` and `raw === "   "` into the same
          // `onChange(undefined)` (unsetting the optional
          // object-typed field). That's wrong by the same UX
          // argument that motivated the FreeformJsonEditor fix:
          // a user who had a populated nested object, cleared
          // it, then accidentally typed a few spaces would have
          // their field silently unset — the textarea would
          // show whitespace while the parent's settings draft
          // dropped the key. Today the no-schema fallback is
          // the only mounted editor, so this is latent rather
          // than user-reachable; once B6.2 wires settings_schema
          // with `type: "object"` properties, NestedJsonEditor
          // will be the active surface and the asymmetry would
          // matter. Closing now keeps the two editors honest:
          //   * raw === ""        → unset (UI promise: empty
          //                         textarea reverts the field
          //                         to its undefined/default
          //                         state, which the schema's
          //                         `required` arr handles
          //                         server-side).
          //   * trimmed === ""    → parse error + invalid
          //                         validity signal, gating Save
          //                         the same way an unparseable
          //                         JSON body would.
          //   * trimmed === "{}"  → unchanged: explicit empty
          //                         object.
          if (raw === "") {
            setError(null);
            onChange(undefined);
            return;
          }
          const trimmed = raw.trim();
          if (trimmed === "") {
            setError(
              "Whitespace-only input is not valid JSON. Clear the textarea to unset this field, or type `{}` to send an empty object explicitly.",
            );
            return;
          }
          try {
            const parsed = JSON.parse(trimmed);
            // Round-6 ANALYSIS_0005: NestedJsonEditor is mounted
            // for SCHEMA properties declared `type: "object"`, so
            // an array or a primitive (number, string, boolean,
            // null) is a schema-type mismatch we must surface
            // before forwarding to the parent. Pre-fix, the
            // editor accepted any valid JSON and the parent's
            // `settings[key] = parsed` would propagate the bad
            // type forward; the engine's server-side
            // gojsonschema check would then 400 on save, leaving
            // the user wondering why their textarea, which
            // parsed cleanly, "got rejected" by the server.
            //
            // Architecturally correct: reject the type at the
            // editor boundary so the validity signal goes red
            // (Save disables) the moment the user types `42` or
            // `[1,2]`. We also suppress the onChange so the
            // parent's settings draft doesn't briefly carry a
            // value of the wrong type that would then have to
            // be re-emitted on the next valid keystroke. Mirrors
            // the FreeformJsonEditor pattern of error-on-parse
            // + suppress-onChange; the editor's text buffer
            // still holds the bad bytes so the user can fix the
            // error without losing their place.
            //
            // null is a JSON-valid value but not an object;
            // Array.isArray catches `[]`; typeof catches
            // primitives. The remaining case (plain object) is
            // the only one we forward.
            if (!isPlainObject(parsed)) {
              setError("Expected an object (got " + describeJsonType(parsed) + ")");
              return;
            }
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

// describeJsonType returns a short, user-facing name for the
// JSON type of a parsed value. Used by NestedJsonEditor's
// type-mismatch error to tell the user WHAT they typed instead
// of "object" — e.g. "Expected an object (got array)" is more
// useful than just "Expected an object", because the user can
// see the bytes on screen and the mismatch is immediately
// actionable ("oh, I have brackets instead of braces").
function describeJsonType(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v;
}

// isPlainObject is the type-guard for "JSON object" — non-null,
// typeof object, not an array. Module-scope (rather than inlined
// inside NestedJsonEditor) for parity with describeJsonType
// above: both are stateless predicates the editor consults at
// mount AND on every keystroke, and hoisting avoids redeclaring
// the closure on every render. Round-11 ANALYSIS_0004 — pre-fix
// the helper was declared inside NestedJsonEditor's body, which
// was functionally correct (no captured component state) but
// inconsistent with the existing module-scope helper sibling.
function isPlainObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
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
  // Round-13 ANALYSIS_0005: shares the validity-propagation
  // contract with NestedJsonEditor via useValiditySignal (see
  // the hook's docstring for the four invariants). We pass
  // FREEFORM_VALIDITY_KEY as the key because at most one
  // FreeformJsonEditor exists in the tree at a time (no-schema
  // branch); the parent's per-key map disambiguates this from
  // any schema-driven NestedJsonEditor signals.
  useValiditySignal(FREEFORM_VALIDITY_KEY, error, onValidityChange);
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
          // Round-8 ANALYSIS_0005: distinguish truly-empty
          // (raw === "") from whitespace-only (raw.trim() === ""
          // but raw !== ""). Pre-fix, both were collapsed into
          // `onChange({})`, which means a user who had populated
          // settings, cleared them, then accidentally typed a
          // few spaces (or a stray IME deadkey resolving to
          // whitespace) and hit Save would have silently wiped
          // their config — the textarea would show " " while
          // the mutation payload was {}. The "leave empty" UI
          // promise (see hint text above) was meant for the
          // genuinely-empty case, not for whitespace. Now:
          //   * raw === ""        → reset to {} (UI promise).
          //   * trimmed === ""    → surface a parse error so
          //                         the validity signal flips
          //                         to invalid and Save is
          //                         disabled until the user
          //                         either clears the box or
          //                         types `{}` explicitly.
          //   * trimmed === "{}"  → unchanged: explicit reset.
          if (raw === "") {
            setError(null);
            onChange({});
            return;
          }
          const trimmed = raw.trim();
          if (trimmed === "") {
            setError(
              "Whitespace-only input is not valid JSON. Clear the textarea to reset settings, or type `{}` to clear all keys explicitly.",
            );
            return;
          }
          if (trimmed === "{}") {
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
