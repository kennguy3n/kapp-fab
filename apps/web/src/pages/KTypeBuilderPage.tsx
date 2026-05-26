import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  FieldSpec,
  KTypeSchema,
  TenantKType,
  TenantKTypeStatus,
  UpsertTenantKTypeInput,
} from "@kapp/client";
import { api } from "../lib/api";

/**
 * KTypeBuilderPage is the Phase N8b low-code visual editor for
 * tenant-authored KTypes. Power users author a custom business
 * object (asset register, compliance checklist, approval form,
 * etc.) without writing Go.
 *
 * Constraints enforced by the backend and surfaced here:
 *
 *   - Name must match `custom.<slug>` (lowercase + underscore).
 *   - Field type must be one of the safe subset (no object/array,
 *     no posting hooks, no computed fields, no custom agent
 *     tools — those still require developer-authored KTypes).
 *   - Field count is capped (default 50; the API reports the
 *     active limit in the list response).
 *   - Status transitions: draft → active → archived. Only `active`
 *     KTypes back record creates.
 */
const SAFE_TYPES: { value: string; label: string; help?: string }[] = [
  { value: "string", label: "Short text" },
  { value: "text", label: "Long text" },
  { value: "number", label: "Number" },
  { value: "integer", label: "Integer" },
  { value: "float", label: "Float" },
  { value: "decimal", label: "Decimal" },
  { value: "boolean", label: "Boolean (yes / no)" },
  { value: "date", label: "Date" },
  { value: "datetime", label: "Date & time" },
  { value: "enum", label: "Choice list (enum)" },
  { value: "ref", label: "Reference to another record" },
  { value: "email", label: "Email" },
  { value: "phone", label: "Phone" },
  { value: "url", label: "URL" },
];

// FieldRow couples a FieldSpec with a row-local React identity so
// the field list can stay reorderable without using the array
// index as a key. React keys must be stable across renders for
// each conceptual row; using `i` would force every row above the
// move to remount, dropping focus inside <input> and tearing any
// transient component state (e.g. a half-typed enum value). The
// rowID is allocated when the row is created (Add field, loadInto,
// reset) and survives reorders.
type FieldRow = { spec: FieldSpec; rowID: string };

function emptyFieldRow(): FieldRow {
  return { spec: { name: "", type: "string" }, rowID: newRowID() };
}

function toFieldRows(fields: FieldSpec[]): FieldRow[] {
  return fields.map((f) => ({ spec: f, rowID: newRowID() }));
}

function newRowID(): string {
  // crypto.randomUUID is available in every modern evergreen
  // browser the rest of the app targets (we already use it
  // elsewhere in apps/web). The fallback is a defence-in-depth
  // measure for non-secure contexts (e.g. some test environments)
  // — collisions across a single editing session are astronomically
  // unlikely with 26+ random digits.
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `row-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function isCustomName(name: string): boolean {
  return /^custom\.[a-z][a-z0-9_]*$/.test(name);
}

// canTransitionStatus pins the same forward-only lifecycle gate the
// backend ktype.SetStatus / Upsert enforce. Keeping it in lock-step
// here is what lets the builder UI hide buttons that would surface
// a 409 instead of disabling them after the click. See
// internal/ktype/tenant_store.go#isForwardTransition.
function canTransitionStatus(
  from: TenantKTypeStatus,
  to: TenantKTypeStatus,
): boolean {
  const rank: Record<TenantKTypeStatus, number> = {
    draft: 0,
    active: 1,
    archived: 2,
  };
  return rank[to] >= rank[from];
}

export function KTypeBuilderPage() {
  const qc = useQueryClient();
  const list = useQuery<{ items: TenantKType[]; field_limit: number }>({
    queryKey: ["tenant-ktypes"],
    queryFn: () => api.listTenantKTypes(),
  });

  const upsert = useMutation({
    mutationFn: (input: UpsertTenantKTypeInput) =>
      api.upsertTenantKType(input),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tenant-ktypes"] }),
  });
  const setStatus = useMutation({
    mutationFn: (args: {
      name: string;
      version: number;
      status: TenantKTypeStatus;
    }) => api.setTenantKTypeStatus(args.name, args.version, args.status),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tenant-ktypes"] }),
  });

  // Editor state — separate from the read-side cache so unsaved
  // changes survive a list refetch.
  const [name, setName] = useState("custom.");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  // The KType version of the row currently loaded in the editor. A
  // freshly-reset editor defaults to 1; loadInto pulls the loaded
  // row's version so re-saving from an existing record updates the
  // correct version in tenant_ktypes rather than silently writing
  // back into v1 and stranding any v2+ rows that may have been
  // shipped by a developer-authored migration.
  const [version, setVersion] = useState<number>(1);
  const [rows, setRows] = useState<FieldRow[]>(() => [emptyFieldRow()]);
  const [status, setLocalStatus] = useState<TenantKTypeStatus>("draft");
  // The loaded row's status, used to gate which lifecycle
  // transition buttons the sidebar offers. "" means "no row
  // loaded yet" (i.e. we're authoring a brand-new KType) so the
  // "Status on save" picker shows every option.
  const [loadedStatus, setLoadedStatus] = useState<TenantKTypeStatus | "">("");

  // FieldSpec[] is the wire-shape the API expects; rows hold extra
  // React identity. Derive once per render so downstream readers
  // (preview memo, validation memo) don't re-pull rowIDs out.
  const fields = useMemo<FieldSpec[]>(
    () => rows.map((r) => r.spec),
    [rows],
  );

  const items = list.data?.items ?? [];
  const fieldLimit = list.data?.field_limit ?? 50;

  // Live preview: project the editor state into the wire shape
  // the API expects, so the user sees exactly what they're about
  // to POST. The mock store / openapi-typescript can validate
  // against this shape without an extra schema layer.
  //
  // Both the schema's `version` and the top-level `version` track
  // the editor-state `version` so re-saving an existing custom
  // KType writes back into the row the user loaded instead of
  // silently targeting v1.
  const preview = useMemo<UpsertTenantKTypeInput>(() => {
    const schema: KTypeSchema = {
      name,
      version,
      fields,
    };
    return { name, title, description, schema, status, version };
  }, [name, title, description, fields, status, version]);

  const validationErrors = useMemo(() => {
    const errs: string[] = [];
    if (!isCustomName(name))
      errs.push(
        "Name must look like custom.<slug> (lowercase letters, digits, underscores)",
      );
    if (!title.trim()) errs.push("Title is required");
    if (fields.length === 0) errs.push("Add at least one field");
    if (fields.length > fieldLimit)
      errs.push(`Maximum ${fieldLimit} fields per custom KType`);
    // Duplicate field-name guard — mirrors the backend
    // ErrDuplicateField check in validateCustomSchema. Surfacing
    // here turns a confusing 400-with-server-error into a precise
    // inline error pointing at the duplicated slot.
    const seen = new Set<string>();
    fields.forEach((f, i) => {
      if (!f.name.trim()) errs.push(`Field #${i + 1}: name required`);
      else if (seen.has(f.name))
        errs.push(`Field "${f.name}" is duplicated`);
      else seen.add(f.name);
      if (
        f.type === "enum" &&
        (!f.values || f.values.filter((v) => v.trim()).length === 0)
      )
        errs.push(`Field "${f.name || `#${i + 1}`}": enum needs values`);
      if (f.type === "ref" && !(f.ref || f.ktype))
        errs.push(`Field "${f.name || `#${i + 1}`}": ref needs target KType`);
    });
    // Forward-only lifecycle guard — mirrors the backend
    // ErrInvalidTransition check. Surfaced inline so the user
    // sees the rejection before clicking Save.
    if (loadedStatus && !canTransitionStatus(loadedStatus, status))
      errs.push(
        `Cannot move ${loadedStatus} → ${status}: status lifecycle is forward-only (draft → active → archived)`,
      );
    return errs;
  }, [name, title, fields, fieldLimit, loadedStatus, status]);

  const canSave = validationErrors.length === 0 && !upsert.isPending;

  function moveField(i: number, dir: -1 | 1) {
    const j = i + dir;
    if (j < 0 || j >= rows.length) return;
    const next = [...rows];
    const tmp = next[i];
    next[i] = next[j];
    next[j] = tmp;
    setRows(next);
  }

  function updateField(i: number, patch: Partial<FieldSpec>) {
    const next = [...rows];
    next[i] = { ...next[i], spec: { ...next[i].spec, ...patch } };
    setRows(next);
  }

  function loadInto(kt: TenantKType) {
    setName(kt.name);
    setTitle(kt.title);
    setDescription(kt.description);
    setVersion(kt.version);
    setRows(toFieldRows(kt.schema.fields ?? []));
    setLocalStatus(kt.status);
    setLoadedStatus(kt.status);
  }

  function reset() {
    setName("custom.");
    setTitle("");
    setDescription("");
    setVersion(1);
    setRows([emptyFieldRow()]);
    setLocalStatus("draft");
    setLoadedStatus("");
  }

  function submit(e: React.FormEvent) {
    e.preventDefault();
    upsert.mutate(preview);
  }

  return (
    <section style={{ maxWidth: 1100 }}>
      <h1>Low-code KType Builder</h1>
      <p style={{ color: "#6b7280" }}>
        Define a custom business object for your tenant — an asset register,
        compliance checklist, custom approval form. The generated KType is
        scoped to your tenant only and lives in the <code>custom.*</code>{" "}
        namespace; record CRUD, list views, and agent tools auto-generate from
        the definition.
      </p>

      <div style={{ display: "grid", gridTemplateColumns: "320px 1fr", gap: 16, alignItems: "start" }}>
        {/* Sidebar: existing custom KTypes ------------------------- */}
        <aside style={{ border: "1px solid #e5e7eb", borderRadius: 6, padding: 12 }}>
          <header
            style={{
              display: "flex",
              alignItems: "center",
              justifyContent: "space-between",
            }}
          >
            <h2 style={{ fontSize: 14, margin: 0 }}>Your custom KTypes</h2>
            <button type="button" onClick={reset} style={{ fontSize: 12 }}>
              + New
            </button>
          </header>
          {list.isLoading && <p style={{ fontSize: 12 }}>Loading…</p>}
          {items.length === 0 && !list.isLoading && (
            <p style={{ fontSize: 12, color: "#6b7280" }}>
              No custom KTypes yet. Define one on the right.
            </p>
          )}
          <ul style={{ listStyle: "none", padding: 0, fontSize: 13 }}>
            {items.map((it) => (
              <li
                key={`${it.name}@${it.version}`}
                style={{ padding: "6px 0", borderTop: "1px solid #f3f4f6" }}
              >
                <div style={{ display: "flex", justifyContent: "space-between" }}>
                  <button
                    type="button"
                    onClick={() => loadInto(it)}
                    style={{
                      border: "none",
                      background: "none",
                      padding: 0,
                      cursor: "pointer",
                      textAlign: "start",
                      color: "#1f2937",
                    }}
                  >
                    <strong>{it.title}</strong>
                    <br />
                    <code style={{ fontSize: 11, color: "#6b7280" }}>{it.name}</code>
                  </button>
                  <span
                    style={{
                      fontSize: 11,
                      padding: "2px 6px",
                      borderRadius: 4,
                      background:
                        it.status === "active"
                          ? "#dcfce7"
                          : it.status === "archived"
                            ? "#f3f4f6"
                            : "#fef3c7",
                      color: "#374151",
                    }}
                  >
                    {it.status}
                  </span>
                </div>
                <div style={{ marginTop: 4, display: "flex", gap: 4, flexWrap: "wrap" }}>
                  {(["draft", "active", "archived"] as TenantKTypeStatus[])
                    .filter(
                      (s) =>
                        s !== it.status &&
                        // Only offer forward transitions — see
                        // canTransitionStatus for the matrix that
                        // matches the backend’s ErrInvalidTransition
                        // gate. Hiding the buttons (rather than
                        // disabling them) keeps the sidebar quiet
                        // for archived rows, which otherwise advertise
                        // “→ draft” and “→ active” only to surface 409
                        // on click.
                        canTransitionStatus(it.status, s),
                    )
                    .map((s) => (
                      <button
                        key={s}
                        type="button"
                        onClick={() =>
                          setStatus.mutate({
                            name: it.name,
                            version: it.version,
                            status: s,
                          })
                        }
                        style={{ fontSize: 11 }}
                        disabled={setStatus.isPending}
                      >
                        → {s}
                      </button>
                    ))}
                </div>
              </li>
            ))}
          </ul>
        </aside>

        {/* Editor ---------------------------------------------- */}
        <form onSubmit={submit} style={{ display: "grid", gap: 12 }}>
          <label style={{ display: "grid", gap: 4, fontSize: 13 }}>
            <span>Machine name</span>
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="custom.asset_register"
              style={{ padding: 6 }}
            />
            <span style={{ fontSize: 11, color: "#6b7280" }}>
              Must start with <code>custom.</code> and use lowercase letters,
              digits, or underscores.
            </span>
          </label>

          <label style={{ display: "grid", gap: 4, fontSize: 13 }}>
            <span>Title</span>
            <input
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="Asset Register"
              style={{ padding: 6 }}
            />
          </label>

          <label style={{ display: "grid", gap: 4, fontSize: 13 }}>
            <span>Description</span>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={2}
              style={{ padding: 6 }}
            />
          </label>

          <fieldset style={{ border: "1px solid #e5e7eb", padding: 12 }}>
            <legend style={{ fontSize: 13 }}>
              Fields ({fields.length} / {fieldLimit})
            </legend>
            {rows.map((r, i) => (
              <FieldEditor
                key={r.rowID}
                field={r.spec}
                onChange={(patch) => updateField(i, patch)}
                onMoveUp={() => moveField(i, -1)}
                onMoveDown={() => moveField(i, 1)}
                onRemove={() =>
                  setRows(rows.filter((_, j) => j !== i))
                }
              />
            ))}
            <button
              type="button"
              onClick={() => setRows([...rows, emptyFieldRow()])}
              disabled={rows.length >= fieldLimit}
              style={{ marginTop: 8 }}
            >
              + Add field
            </button>
          </fieldset>

          <label style={{ display: "grid", gap: 4, fontSize: 13 }}>
            <span>Status on save</span>
            <select
              value={status}
              onChange={(e) =>
                setLocalStatus(e.target.value as TenantKTypeStatus)
              }
              style={{ padding: 6 }}
            >
              {/* Only show statuses the loaded row can transition
                  to (or every status when authoring a brand-new
                  KType). Mirrors the backend lifecycle gate. */}
              {(["draft", "active", "archived"] as TenantKTypeStatus[])
                .filter(
                  (s) =>
                    !loadedStatus || canTransitionStatus(loadedStatus, s),
                )
                .map((s) => (
                  <option key={s} value={s}>
                    {s === "draft"
                      ? "draft (editable, no records yet)"
                      : s === "active"
                        ? "active (back record creates)"
                        : "archived (frozen)"}
                  </option>
                ))}
            </select>
          </label>

          {validationErrors.length > 0 && (
            <ul style={{ color: "#b91c1c", fontSize: 12, paddingLeft: 16 }}>
              {validationErrors.map((e) => (
                <li key={e}>{e}</li>
              ))}
            </ul>
          )}
          {upsert.isError && (
            <p style={{ color: "#b91c1c", fontSize: 12 }}>
              {(upsert.error as Error).message}
            </p>
          )}
          <div style={{ display: "flex", gap: 8 }}>
            <button type="submit" disabled={!canSave}>
              {upsert.isPending ? "Saving…" : "Save KType"}
            </button>
            <button type="button" onClick={reset}>
              Reset
            </button>
          </div>

          <details>
            <summary style={{ fontSize: 12, color: "#6b7280" }}>
              Live preview (JSON sent to API)
            </summary>
            <pre
              style={{
                background: "#f3f4f6",
                padding: 8,
                fontSize: 11,
                overflow: "auto",
              }}
            >
              {JSON.stringify(preview, null, 2)}
            </pre>
          </details>
        </form>
      </div>
    </section>
  );
}

interface FieldEditorProps {
  field: FieldSpec;
  onChange: (patch: Partial<FieldSpec>) => void;
  onMoveUp: () => void;
  onMoveDown: () => void;
  onRemove: () => void;
}

function FieldEditor({
  field,
  onChange,
  onMoveUp,
  onMoveDown,
  onRemove,
}: FieldEditorProps) {
  return (
    <div
      style={{
        display: "grid",
        gridTemplateColumns: "1fr 1fr auto auto auto auto",
        gap: 6,
        margin: "8px 0",
        alignItems: "center",
        fontSize: 13,
      }}
    >
      <input
        value={field.name}
        onChange={(e) => onChange({ name: e.target.value })}
        placeholder="field name"
        style={{ padding: 4 }}
      />
      <select
        value={field.type}
        onChange={(e) => onChange({ type: e.target.value })}
        style={{ padding: 4 }}
      >
        {SAFE_TYPES.map((t) => (
          <option key={t.value} value={t.value}>
            {t.label}
          </option>
        ))}
      </select>
      <label style={{ fontSize: 11, display: "flex", alignItems: "center", gap: 4 }}>
        <input
          type="checkbox"
          checked={field.required ?? false}
          onChange={(e) => onChange({ required: e.target.checked })}
        />
        required
      </label>
      <button type="button" onClick={onMoveUp} style={{ fontSize: 11 }}>
        ↑
      </button>
      <button type="button" onClick={onMoveDown} style={{ fontSize: 11 }}>
        ↓
      </button>
      <button type="button" onClick={onRemove} style={{ fontSize: 11 }}>
        ✕
      </button>
      {field.type === "enum" && (
        <input
          value={(field.values ?? []).join(",")}
          onChange={(e) =>
            onChange({
              values: e.target.value
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean),
            })
          }
          placeholder="comma,separated,values"
          style={{ gridColumn: "1 / span 6", padding: 4 }}
        />
      )}
      {field.type === "ref" && (
        <input
          value={field.ref ?? field.ktype ?? ""}
          onChange={(e) => onChange({ ref: e.target.value })}
          placeholder="target ktype (e.g. crm.account)"
          style={{ gridColumn: "1 / span 6", padding: 4 }}
        />
      )}
    </div>
  );
}
