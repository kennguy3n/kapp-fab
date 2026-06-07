import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  FieldSpec,
  KTypeSchema,
  TenantKType,
  TenantKTypeStatus,
  UpsertTenantKTypeInput,
} from "@kapp/client";
import { Badge, Button, Input, Select } from "@kapp/ui";
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
const STATUS_VARIANT: Record<TenantKTypeStatus, "success" | "default" | "warning"> = {
  active: "success",
  archived: "default",
  draft: "warning",
};

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
    // The save just persisted `status`, so the editor's notion of the
    // loaded row's status is now stale. Bring it back in lock-step
    // before the next render so the forward-only transition gate
    // (canTransitionStatus / validationErrors) keeps reading the
    // correct from-state — otherwise the user could pick a value
    // that round-trips through canTransitionStatus(loadedStatus,
    // requestedStatus) but is rejected by the backend with 409.
    // input.status is typed optional on UpsertTenantKTypeInput, but
    // the builder's `preview` memo always sets it from the editor's
    // status state — fall back to that state for type-safety.
    onSuccess: (_saved, input) => {
      setLoadedStatus(input.status ?? status);
      qc.invalidateQueries({ queryKey: ["tenant-ktypes"] });
    },
  });
  const setStatus = useMutation({
    mutationFn: (args: {
      name: string;
      version: number;
      status: TenantKTypeStatus;
    }) => api.setTenantKTypeStatus(args.name, args.version, args.status),
    // The sidebar status buttons operate on rows in the list, not the
    // editor — but if the affected row is the one currently loaded
    // in the editor, loadedStatus would otherwise stay stale and
    // open the same backward-transition desync as the upsert path.
    onSuccess: (_data, args) => {
      if (args.name === name && args.version === version) {
        setLoadedStatus(args.status);
        // Keep the editor's "Status on save" picker pointed at the
        // newly-persisted status by default so the next Save isn't
        // a backward transition.
        setLocalStatus(args.status);
      }
      qc.invalidateQueries({ queryKey: ["tenant-ktypes"] });
    },
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
    <section className="max-w-[1100px]">
      <h1>Low-code KType Builder</h1>
      <p className="text-fg-muted">
        Define a custom business object for your tenant — an asset register,
        compliance checklist, custom approval form. The generated KType is
        scoped to your tenant only and lives in the <code>custom.*</code>{" "}
        namespace; record CRUD, list views, and agent tools auto-generate from
        the definition.
      </p>

      <div className="grid grid-cols-[320px_1fr] items-start gap-4">
        {/* Sidebar: existing custom KTypes ------------------------- */}
        <aside className="rounded-md border border-border p-3">
          <header className="flex items-center justify-between">
            <h2 className="m-0 text-sm">Your custom KTypes</h2>
            <Button type="button" variant="ghost" size="sm" onClick={reset}>
              + New
            </Button>
          </header>
          {list.isLoading && <p className="text-xs">Loading…</p>}
          {items.length === 0 && !list.isLoading && (
            <p className="text-xs text-fg-muted">
              No custom KTypes yet. Define one on the right.
            </p>
          )}
          <ul className="m-0 list-none p-0 text-[13px]">
            {items.map((it) => (
              <li
                key={`${it.name}@${it.version}`}
                className="border-t border-border py-1.5"
              >
                <div className="flex justify-between">
                  <button
                    type="button"
                    onClick={() => loadInto(it)}
                    className="cursor-pointer border-none bg-transparent p-0 text-start text-fg"
                  >
                    <strong>{it.title}</strong>
                    <br />
                    <code className="text-[11px] text-fg-muted">{it.name}</code>
                  </button>
                  <Badge variant={STATUS_VARIANT[it.status]} size="xs">
                    {it.status}
                  </Badge>
                </div>
                <div className="mt-1 flex flex-wrap gap-1">
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
                      <Button
                        key={s}
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() =>
                          setStatus.mutate({
                            name: it.name,
                            version: it.version,
                            status: s,
                          })
                        }
                        disabled={setStatus.isPending}
                      >
                        → {s}
                      </Button>
                    ))}
                </div>
              </li>
            ))}
          </ul>
        </aside>

        {/* Editor ---------------------------------------------- */}
        <form onSubmit={submit} className="grid gap-3">
          <label className="grid gap-1 text-[13px]">
            <span>Machine name</span>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="custom.asset_register"
            />
            <span className="text-[11px] text-fg-muted">
              Must start with <code>custom.</code> and use lowercase letters,
              digits, or underscores.
            </span>
          </label>

          <label className="grid gap-1 text-[13px]">
            <span>Title</span>
            <Input
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="Asset Register"
            />
          </label>

          <label className="grid gap-1 text-[13px]">
            <span>Description</span>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={2}
              className="rounded-md border border-border bg-bg px-2 py-1.5 text-sm text-fg outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
            />
          </label>

          <fieldset className="border border-border p-3">
            <legend className="text-[13px]">
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
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setRows([...rows, emptyFieldRow()])}
              disabled={rows.length >= fieldLimit}
              className="mt-2"
            >
              + Add field
            </Button>
          </fieldset>

          <label className="grid gap-1 text-[13px]">
            <span>Status on save</span>
            <Select
              value={status}
              onChange={(e) =>
                setLocalStatus(e.target.value as TenantKTypeStatus)
              }
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
            </Select>
          </label>

          {validationErrors.length > 0 && (
            <ul className="pl-4 text-xs text-danger">
              {validationErrors.map((e) => (
                <li key={e}>{e}</li>
              ))}
            </ul>
          )}
          {upsert.isError && (
            <p className="text-xs text-danger">
              {(upsert.error as Error).message}
            </p>
          )}
          <div className="flex gap-2">
            <Button type="submit" disabled={!canSave}>
              {upsert.isPending ? "Saving…" : "Save KType"}
            </Button>
            <Button type="button" variant="outline" onClick={reset}>
              Reset
            </Button>
          </div>

          <details>
            <summary className="text-xs text-fg-muted">
              Live preview (JSON sent to API)
            </summary>
            <pre className="overflow-auto bg-bg-muted p-2 text-[11px]">
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
    <div className="my-2 grid grid-cols-[1fr_1fr_auto_auto_auto_auto] items-center gap-1.5 text-[13px]">
      <Input
        value={field.name}
        onChange={(e) => onChange({ name: e.target.value })}
        placeholder="field name"
      />
      <Select
        value={field.type}
        onChange={(e) => onChange({ type: e.target.value })}
      >
        {SAFE_TYPES.map((t) => (
          <option key={t.value} value={t.value}>
            {t.label}
          </option>
        ))}
      </Select>
      <label className="flex items-center gap-1 text-[11px]">
        <input
          type="checkbox"
          checked={field.required ?? false}
          onChange={(e) => onChange({ required: e.target.checked })}
        />
        required
      </label>
      <Button type="button" variant="outline" size="sm" onClick={onMoveUp}>
        ↑
      </Button>
      <Button type="button" variant="outline" size="sm" onClick={onMoveDown}>
        ↓
      </Button>
      <Button type="button" variant="outline" size="sm" onClick={onRemove}>
        ✕
      </Button>
      {field.type === "enum" && (
        <Input
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
          className="col-span-6"
        />
      )}
      {field.type === "ref" && (
        <Input
          value={field.ref ?? field.ktype ?? ""}
          onChange={(e) => onChange({ ref: e.target.value })}
          placeholder="target ktype (e.g. crm.account)"
          className="col-span-6"
        />
      )}
    </div>
  );
}
