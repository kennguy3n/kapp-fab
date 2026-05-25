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

function emptyField(): FieldSpec {
  return { name: "", type: "string" };
}

function isCustomName(name: string): boolean {
  return /^custom\.[a-z][a-z0-9_]*$/.test(name);
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
  const [fields, setFields] = useState<FieldSpec[]>([emptyField()]);
  const [status, setLocalStatus] = useState<TenantKTypeStatus>("draft");

  const items = list.data?.items ?? [];
  const fieldLimit = list.data?.field_limit ?? 50;

  // Live preview: project the editor state into the wire shape
  // the API expects, so the user sees exactly what they're about
  // to POST. The mock store / openapi-typescript can validate
  // against this shape without an extra schema layer.
  const preview = useMemo<UpsertTenantKTypeInput>(() => {
    const schema: KTypeSchema = {
      name,
      version: 1,
      fields,
    };
    return { name, title, description, schema, status };
  }, [name, title, description, fields, status]);

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
    fields.forEach((f, i) => {
      if (!f.name.trim()) errs.push(`Field #${i + 1}: name required`);
      if (
        f.type === "enum" &&
        (!f.values || f.values.filter((v) => v.trim()).length === 0)
      )
        errs.push(`Field "${f.name || `#${i + 1}`}": enum needs values`);
      if (f.type === "ref" && !(f.ref || f.ktype))
        errs.push(`Field "${f.name || `#${i + 1}`}": ref needs target KType`);
    });
    return errs;
  }, [name, title, fields, fieldLimit]);

  const canSave = validationErrors.length === 0 && !upsert.isPending;

  function moveField(i: number, dir: -1 | 1) {
    const j = i + dir;
    if (j < 0 || j >= fields.length) return;
    const next = [...fields];
    const tmp = next[i];
    next[i] = next[j];
    next[j] = tmp;
    setFields(next);
  }

  function updateField(i: number, patch: Partial<FieldSpec>) {
    const next = [...fields];
    next[i] = { ...next[i], ...patch };
    setFields(next);
  }

  function loadInto(kt: TenantKType) {
    setName(kt.name);
    setTitle(kt.title);
    setDescription(kt.description);
    setFields(kt.schema.fields ?? []);
    setLocalStatus(kt.status);
  }

  function reset() {
    setName("custom.");
    setTitle("");
    setDescription("");
    setFields([emptyField()]);
    setLocalStatus("draft");
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
                    .filter((s) => s !== it.status)
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
            {fields.map((f, i) => (
              <FieldEditor
                key={i}
                field={f}
                onChange={(patch) => updateField(i, patch)}
                onMoveUp={() => moveField(i, -1)}
                onMoveDown={() => moveField(i, 1)}
                onRemove={() => setFields(fields.filter((_, j) => j !== i))}
              />
            ))}
            <button
              type="button"
              onClick={() => setFields([...fields, emptyField()])}
              disabled={fields.length >= fieldLimit}
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
              <option value="draft">draft (editable, no records yet)</option>
              <option value="active">active (back record creates)</option>
              <option value="archived">archived (frozen)</option>
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
