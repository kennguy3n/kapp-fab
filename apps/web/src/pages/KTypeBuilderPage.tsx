import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowUp,
  GripVertical,
  Plus,
  Sparkles,
  Trash2,
} from "lucide-react";
import type {
  FieldSpec,
  KType,
  KTypeSchema,
  TenantKType,
  TenantKTypeStatus,
  UpsertTenantKTypeInput,
} from "@kapp/client";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  EmptyState,
  Field,
  Input,
  Select,
  Skeleton,
  Textarea,
  Tooltip,
  TooltipContent,
  TooltipTrigger,
  type BadgeProps,
} from "@kapp/ui";
import { api } from "../lib/api";
import {
  humanizeLabel,
  humanizeToken,
  ktypeSingular,
  relationTargetKtype,
  resolveControl,
} from "../lib/ktypeView";
import {
  AdminErrorState,
  AdminPageHeader,
  Toggle,
} from "./adminKit";

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
type BadgeVariant = NonNullable<BadgeProps["variant"]>;

const STATUS_VARIANT: Record<TenantKTypeStatus, BadgeVariant> = {
  active: "success",
  archived: "neutral",
  draft: "warning",
};

const STATUS_LABEL: Record<TenantKTypeStatus, string> = {
  draft: "Draft",
  active: "Active",
  archived: "Archived",
};

const STATUS_HELP: Record<TenantKTypeStatus, string> = {
  draft: "Draft — editable, no records yet",
  active: "Active — can back new records",
  archived: "Archived — frozen, read-only",
};

const SAFE_TYPES: { value: string; label: string }[] = [
  { value: "string", label: "Short text" },
  { value: "text", label: "Long text" },
  { value: "number", label: "Number" },
  { value: "integer", label: "Integer" },
  { value: "float", label: "Float" },
  { value: "decimal", label: "Decimal" },
  { value: "boolean", label: "Yes / no" },
  { value: "date", label: "Date" },
  { value: "datetime", label: "Date & time" },
  { value: "enum", label: "Choice list" },
  { value: "ref", label: "Link to another record" },
  { value: "email", label: "Email" },
  { value: "phone", label: "Phone" },
  { value: "url", label: "URL" },
];

const TYPE_LABEL = new Map(SAFE_TYPES.map((t) => [t.value, t.label]));

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
  const ktypesQuery = useQuery<KType[]>({
    queryKey: ["ktypes"],
    queryFn: () => api.listKTypes(),
    staleTime: 60_000,
  });

  const upsert = useMutation({
    mutationFn: (input: UpsertTenantKTypeInput) => api.upsertTenantKType(input),
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
    onSuccess: (_data, args) => {
      if (args.name === name && args.version === version) {
        setLoadedStatus(args.status);
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
  const [version, setVersion] = useState<number>(1);
  const [rows, setRows] = useState<FieldRow[]>(() => [emptyFieldRow()]);
  const [status, setLocalStatus] = useState<TenantKTypeStatus>("draft");
  const [loadedStatus, setLoadedStatus] = useState<TenantKTypeStatus | "">("");
  const [submitted, setSubmitted] = useState(false);
  const [dragIndex, setDragIndex] = useState<number | null>(null);

  const fields = useMemo<FieldSpec[]>(() => rows.map((r) => r.spec), [rows]);

  const items = list.data?.items ?? [];
  const fieldLimit = list.data?.field_limit ?? 50;

  // Candidate targets for "link to another record" fields: the
  // platform KTypes plus the tenant's own custom objects, labelled
  // in plain language rather than raw machine names.
  const refOptions = useMemo(() => {
    const map = new Map<string, string>();
    for (const k of ktypesQuery.data ?? []) map.set(k.name, ktypeSingular(k.name));
    for (const it of items) map.set(it.name, it.title || ktypeSingular(it.name));
    return Array.from(map.entries())
      .map(([value, label]) => ({ value, label }))
      .sort((a, b) => a.label.localeCompare(b.label));
  }, [ktypesQuery.data, items]);

  const preview = useMemo<UpsertTenantKTypeInput>(() => {
    const schema: KTypeSchema = { name, version, fields };
    return { name, title, description, schema, status, version };
  }, [name, title, description, fields, status, version]);

  const validationErrors = useMemo(() => {
    const errs: string[] = [];
    if (!isCustomName(name))
      errs.push(
        "Name must look like custom.<slug> (lowercase letters, digits, underscores).",
      );
    if (!title.trim()) errs.push("Give your object a title.");
    if (fields.length === 0) errs.push("Add at least one field.");
    if (fields.length > fieldLimit)
      errs.push(`You can add at most ${fieldLimit} fields.`);
    const seen = new Set<string>();
    fields.forEach((f, i) => {
      if (!f.name.trim()) errs.push(`Field ${i + 1}: enter a field name.`);
      else if (seen.has(f.name))
        errs.push(`The field name "${f.name}" is used more than once.`);
      else seen.add(f.name);
      if (
        f.type === "enum" &&
        (!f.values || f.values.filter((v) => v.trim()).length === 0)
      )
        errs.push(
          `Field "${f.name || i + 1}": add at least one choice for the choice list.`,
        );
      if (f.type === "ref" && !(f.ref || f.ktype))
        errs.push(`Field "${f.name || i + 1}": pick a record type to link to.`);
    });
    if (loadedStatus && !canTransitionStatus(loadedStatus, status))
      errs.push(
        `Can't move ${STATUS_LABEL[loadedStatus]} → ${STATUS_LABEL[status]}: status only moves forward (Draft → Active → Archived).`,
      );
    return errs;
  }, [name, title, fields, fieldLimit, loadedStatus, status]);

  const canSave = validationErrors.length === 0 && !upsert.isPending;

  function moveField(i: number, dir: -1 | 1) {
    const j = i + dir;
    if (j < 0 || j >= rows.length) return;
    const next = [...rows];
    [next[i], next[j]] = [next[j], next[i]];
    setRows(next);
  }

  function reorderField(from: number, to: number) {
    if (from === to) return;
    setRows((rs) => {
      const next = [...rs];
      const [moved] = next.splice(from, 1);
      next.splice(to, 0, moved);
      return next;
    });
  }

  function updateField(i: number, patch: Partial<FieldSpec>) {
    const next = [...rows];
    next[i] = { ...next[i], spec: { ...next[i].spec, ...patch } };
    setRows(next);
  }

  function changeFieldType(i: number, type: string) {
    // Switching type makes the previous type's extras meaningless,
    // so clear them to avoid posting a stale enum list / ref target.
    updateField(i, {
      type,
      values: type === "enum" ? rows[i].spec.values : undefined,
      ref: type === "ref" ? rows[i].spec.ref : undefined,
      ktype: type === "ref" ? rows[i].spec.ktype : undefined,
    });
  }

  function loadInto(kt: TenantKType) {
    setName(kt.name);
    setTitle(kt.title);
    setDescription(kt.description);
    setVersion(kt.version);
    setRows(toFieldRows(kt.schema.fields ?? []));
    setLocalStatus(kt.status);
    setLoadedStatus(kt.status);
    setSubmitted(false);
  }

  function reset() {
    setName("custom.");
    setTitle("");
    setDescription("");
    setVersion(1);
    setRows([emptyFieldRow()]);
    setLocalStatus("draft");
    setLoadedStatus("");
    setSubmitted(false);
  }

  function submit(e: React.FormEvent) {
    e.preventDefault();
    setSubmitted(true);
    if (!canSave) return;
    upsert.mutate(preview);
  }

  const editingExisting = loadedStatus !== "";
  const statusOptions = (["draft", "active", "archived"] as TenantKTypeStatus[]).filter(
    (s) => !loadedStatus || canTransitionStatus(loadedStatus, s),
  );

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Data model"
        title="Custom objects"
        description="Design your own business objects — an asset register, a compliance checklist, an approval form — without writing code. Records, lists, and forms generate automatically from what you define here."
        actions={
          <Button leadingIcon={<Plus />} variant="secondary" onClick={reset}>
            New object
          </Button>
        }
      />

      <div className="grid items-start gap-6 lg:grid-cols-[18rem_1fr]">
        {/* Sidebar: existing custom KTypes ------------------------- */}
        <aside className="flex flex-col gap-2">
          <h2 className="text-xs font-semibold uppercase tracking-wide text-fg-muted">
            Your custom objects
          </h2>
          {list.isLoading ? (
            <div className="flex flex-col gap-2">
              <Skeleton variant="rect" className="h-16 w-full" />
              <Skeleton variant="rect" className="h-16 w-full" />
            </div>
          ) : list.isError ? (
            <AdminErrorState
              title="Couldn't load your objects"
              error={list.error}
              onRetry={() => list.refetch()}
            />
          ) : items.length === 0 ? (
            <p className="rounded-lg border border-dashed border-border p-4 text-sm text-fg-muted">
              No custom objects yet. Define one on the right and save it as a
              draft to get started.
            </p>
          ) : (
            <ul className="flex list-none flex-col gap-2 p-0">
              {items.map((it) => {
                const selected = it.name === name && it.version === version;
                return (
                  <li key={`${it.name}@${it.version}`}>
                    <div
                      className={`rounded-lg border p-3 transition-colors ${
                        selected
                          ? "border-accent bg-bg-subtle"
                          : "border-border bg-bg-elevated hover:bg-bg-subtle"
                      }`}
                    >
                      <button
                        type="button"
                        onClick={() => loadInto(it)}
                        className="flex w-full items-start justify-between gap-2 text-start focus-visible:outline-none"
                      >
                        <span className="min-w-0">
                          <span className="block truncate font-medium text-fg">
                            {it.title || ktypeSingular(it.name)}
                          </span>
                          <span className="block truncate text-xs text-fg-muted">
                            {it.schema.fields?.length ?? 0}{" "}
                            {(it.schema.fields?.length ?? 0) === 1
                              ? "field"
                              : "fields"}
                          </span>
                        </span>
                        <Badge variant={STATUS_VARIANT[it.status]} size="xs">
                          {STATUS_LABEL[it.status]}
                        </Badge>
                      </button>
                      <div className="mt-2 flex flex-wrap gap-1">
                        {(["draft", "active", "archived"] as TenantKTypeStatus[])
                          .filter(
                            (s) => s !== it.status && canTransitionStatus(it.status, s),
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
                              Mark {STATUS_LABEL[s].toLowerCase()}
                            </Button>
                          ))}
                      </div>
                    </div>
                  </li>
                );
              })}
            </ul>
          )}
        </aside>

        {/* Editor + live preview ------------------------------------ */}
        <div className="grid items-start gap-6 2xl:grid-cols-2">
          <form onSubmit={submit} className="flex flex-col gap-4">
            <Card>
              <CardHeader>
                <CardTitle>{editingExisting ? "Edit object" : "New object"}</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col gap-4">
                <Field
                  label="Title"
                  required
                  error={submitted && !title.trim() ? "Give your object a title." : undefined}
                  help="The friendly name people see, e.g. Asset Register."
                >
                  <Input
                    value={title}
                    onChange={(e) => setTitle(e.target.value)}
                    placeholder="Asset Register"
                  />
                </Field>
                <Field
                  label="Machine name"
                  required
                  error={
                    submitted && !isCustomName(name)
                      ? "Use custom.<slug> — lowercase letters, digits, underscores."
                      : undefined
                  }
                  help="A stable identifier used behind the scenes. Starts with custom."
                >
                  <Input
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder="custom.asset_register"
                    className="font-mono"
                    disabled={editingExisting}
                  />
                </Field>
                <Field
                  label="Description"
                  help="Optional. Explain what this object is for."
                >
                  <Textarea
                    value={description}
                    onChange={(e) => setDescription(e.target.value)}
                    rows={2}
                    placeholder="Tracks every physical asset the company owns."
                  />
                </Field>
              </CardContent>
            </Card>

            <Card>
              <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
                <CardTitle>Fields</CardTitle>
                <Badge variant="neutral">
                  {fields.length} of {fieldLimit}
                </Badge>
              </CardHeader>
              <CardContent className="flex flex-col gap-3">
                {rows.map((r, i) => (
                  <div
                    key={r.rowID}
                    onDragOver={(e) => {
                      if (dragIndex !== null && dragIndex !== i) e.preventDefault();
                    }}
                    onDrop={(e) => {
                      e.preventDefault();
                      if (dragIndex !== null) {
                        reorderField(dragIndex, i);
                        setDragIndex(null);
                      }
                    }}
                    className={
                      dragIndex === i ? "opacity-60" : undefined
                    }
                  >
                    <FieldEditor
                      field={r.spec}
                      index={i}
                      count={rows.length}
                      refOptions={refOptions}
                      onChange={(patch) => updateField(i, patch)}
                      onChangeType={(t) => changeFieldType(i, t)}
                      onMoveUp={() => moveField(i, -1)}
                      onMoveDown={() => moveField(i, 1)}
                      onRemove={() => setRows(rows.filter((_, j) => j !== i))}
                      onDragStart={() => setDragIndex(i)}
                      onDragEnd={() => setDragIndex(null)}
                    />
                  </div>
                ))}
                <div>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    leadingIcon={<Plus />}
                    onClick={() => setRows([...rows, emptyFieldRow()])}
                    disabled={rows.length >= fieldLimit}
                  >
                    Add field
                  </Button>
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Lifecycle</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col gap-4">
                <Field
                  label="Status on save"
                  help="Records can only be created against Active objects. Status moves forward only."
                >
                  <Select
                    value={status}
                    onChange={(e) =>
                      setLocalStatus(e.target.value as TenantKTypeStatus)
                    }
                  >
                    {statusOptions.map((s) => (
                      <option key={s} value={s}>
                        {STATUS_HELP[s]}
                      </option>
                    ))}
                  </Select>
                </Field>

                {submitted && validationErrors.length > 0 && (
                  <div className="rounded-md border border-danger/40 bg-danger/10 p-3">
                    <p className="text-sm font-medium text-danger">
                      Fix these before saving:
                    </p>
                    <ul className="mt-1 list-disc pl-5 text-sm text-danger">
                      {validationErrors.map((e) => (
                        <li key={e}>{e}</li>
                      ))}
                    </ul>
                  </div>
                )}
                {upsert.isError && (
                  <p className="text-sm text-danger">
                    {(upsert.error as Error).message}
                  </p>
                )}

                <div className="flex flex-wrap gap-2">
                  <Button type="submit" disabled={!canSave}>
                    {upsert.isPending ? "Saving…" : "Save object"}
                  </Button>
                  <Button type="button" variant="ghost" onClick={reset}>
                    Reset
                  </Button>
                </div>
              </CardContent>
            </Card>
          </form>

          <SchemaPreview
            title={title}
            description={description}
            status={status}
            fields={fields}
            refOptions={refOptions}
          />
        </div>
      </div>
    </section>
  );
}

interface FieldEditorProps {
  field: FieldSpec;
  index: number;
  count: number;
  refOptions: { value: string; label: string }[];
  onChange: (patch: Partial<FieldSpec>) => void;
  onChangeType: (type: string) => void;
  onMoveUp: () => void;
  onMoveDown: () => void;
  onRemove: () => void;
  onDragStart: () => void;
  onDragEnd: () => void;
}

function FieldEditor({
  field,
  index,
  count,
  refOptions,
  onChange,
  onChangeType,
  onMoveUp,
  onMoveDown,
  onRemove,
  onDragStart,
  onDragEnd,
}: FieldEditorProps) {
  const label = field.name.trim() ? humanizeLabel(field.name) : `Field ${index + 1}`;
  const refValue = field.ref ?? field.ktype ?? "";
  const refMissing = refValue !== "" && !refOptions.some((o) => o.value === refValue);

  return (
    <div className="rounded-lg border border-border bg-bg-subtle p-3">
      <div className="flex flex-wrap items-end gap-2">
        <span
          draggable
          onDragStart={onDragStart}
          onDragEnd={onDragEnd}
          aria-hidden="true"
          className="mb-1.5 flex h-9 cursor-grab items-center text-fg-subtle active:cursor-grabbing"
          title="Drag to reorder"
        >
          <GripVertical className="h-4 w-4" />
        </span>
        <Field label="Field name" className="min-w-[10rem] flex-1">
          <Input
            value={field.name}
            onChange={(e) => onChange({ name: e.target.value })}
            placeholder="serial_number"
            className="font-mono"
          />
        </Field>
        <Field label="Type" className="min-w-[9rem]">
          <Select value={field.type} onChange={(e) => onChangeType(e.target.value)}>
            {SAFE_TYPES.map((t) => (
              <option key={t.value} value={t.value}>
                {t.label}
              </option>
            ))}
          </Select>
        </Field>
        <div className="mb-1.5 flex items-center gap-2">
          <Toggle
            checked={field.required ?? false}
            onChange={(v) => onChange({ required: v })}
            label={`Make ${label} required`}
          />
          <span className="text-sm text-fg-muted">Required</span>
        </div>
        <div className="mb-1 flex items-center gap-0.5">
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={`Move ${label} up`}
            disabled={index === 0}
            onClick={onMoveUp}
          >
            <ArrowUp className="h-4 w-4" aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={`Move ${label} down`}
            disabled={index === count - 1}
            onClick={onMoveDown}
          >
            <ArrowDown className="h-4 w-4" aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={`Remove ${label}`}
            onClick={onRemove}
          >
            <Trash2 className="h-4 w-4" aria-hidden="true" />
          </Button>
        </div>
      </div>

      {field.type === "enum" && (
        <div className="mt-2">
          <Field
            label="Choices"
            help="Comma-separated options people can pick from."
          >
            <Input
              value={(field.values ?? []).join(", ")}
              onChange={(e) =>
                onChange({
                  values: e.target.value
                    .split(",")
                    .map((s) => s.trim())
                    .filter(Boolean),
                })
              }
              placeholder="Active, In repair, Retired"
            />
          </Field>
        </div>
      )}
      {field.type === "ref" && (
        <div className="mt-2">
          <Field label="Links to" help="Which record type this field points at.">
            <Select
              value={refValue}
              onChange={(e) => onChange({ ref: e.target.value, ktype: undefined })}
            >
              <option value="">Select a record type…</option>
              {refMissing && <option value={refValue}>{refValue}</option>}
              {refOptions.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </Select>
          </Field>
        </div>
      )}
    </div>
  );
}

/**
 * SchemaPreview renders the editor state the way an end user will
 * actually experience it — a record form with real labels and the
 * right control per field type — so authors get instant, jargon-free
 * feedback instead of reading raw JSON.
 */
function SchemaPreview({
  title,
  description,
  status,
  fields,
  refOptions,
}: {
  title: string;
  description: string;
  status: TenantKTypeStatus;
  fields: FieldSpec[];
  refOptions: { value: string; label: string }[];
}) {
  const named = fields.filter((f) => f.name.trim());
  return (
    <Card className="2xl:sticky 2xl:top-4">
      <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle className="flex items-center gap-2">
          <Sparkles className="h-4 w-4 text-accent" aria-hidden="true" />
          Live preview
        </CardTitle>
        <Badge variant={STATUS_VARIANT[status]} size="xs">
          {STATUS_LABEL[status]}
        </Badge>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div>
          <h3 className="text-base font-semibold text-fg">
            {title.trim() || "Untitled object"}
          </h3>
          {description.trim() && (
            <p className="mt-1 text-sm text-fg-muted">{description}</p>
          )}
        </div>
        {named.length === 0 ? (
          <EmptyState
            icon={<Sparkles />}
            title="Add a field to preview"
            description="As you add fields, the record form people will fill in appears here."
          />
        ) : (
          <div className="flex flex-col gap-3">
            {named.map((f, i) => (
              <PreviewControl key={`${f.name}-${i}`} field={f} refOptions={refOptions} />
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function PreviewControl({
  field,
  refOptions,
}: {
  field: FieldSpec;
  refOptions: { value: string; label: string }[];
}) {
  const label = humanizeLabel(field.name);
  const control = resolveControl(field);
  const required = field.required ?? false;

  const labelNode = (
    <span className="flex items-center gap-1 text-sm font-medium text-fg">
      {label}
      {required && (
        <span className="text-danger" aria-hidden="true">
          *
        </span>
      )}
      <Badge variant="outline" size="xs" className="ml-1 font-normal">
        {TYPE_LABEL.get(field.type) ?? humanizeToken(field.type)}
      </Badge>
    </span>
  );

  let body: React.ReactNode;
  if (control === "boolean") {
    body = (
      <span className="flex items-center gap-2 text-sm text-fg-muted">
        <Toggle checked={false} disabled onChange={() => {}} label={`${label} preview`} />
        No
      </span>
    );
  } else if (control === "select") {
    body = (
      <span className="flex flex-wrap gap-1">
        {(field.values ?? []).length === 0 ? (
          <span className="text-sm text-fg-subtle">No choices yet</span>
        ) : (
          field.values!.map((v) => (
            <Badge key={v} variant="neutral" size="xs">
              {humanizeToken(v)}
            </Badge>
          ))
        )}
      </span>
    );
  } else if (control === "relation") {
    const target = relationTargetKtype(field);
    const targetLabel =
      refOptions.find((o) => o.value === target)?.label ??
      (target ? ktypeSingular(target) : "record");
    body = (
      <span className="block rounded-md border border-border bg-bg px-3 py-2 text-sm text-fg-subtle">
        Search a {targetLabel}…
      </span>
    );
  } else if (control === "textarea") {
    body = (
      <span className="block h-16 rounded-md border border-border bg-bg px-3 py-2 text-sm text-fg-subtle">
        Long text…
      </span>
    );
  } else {
    const placeholder: Record<string, string> = {
      number: "0",
      date: "Pick a date",
      datetime: "Pick a date & time",
      email: "name@example.com",
      tel: "+1 555 0100",
      url: "https://…",
      text: "Text…",
    };
    body = (
      <span className="block rounded-md border border-border bg-bg px-3 py-2 text-sm text-fg-subtle">
        {placeholder[control] ?? "Text…"}
      </span>
    );
  }

  return (
    <div className="flex flex-col gap-1.5">
      {field.type === "ref" && !field.name.trim() ? (
        <Tooltip>
          <TooltipTrigger asChild>{labelNode}</TooltipTrigger>
          <TooltipContent>Name this field to finish it.</TooltipContent>
        </Tooltip>
      ) : (
        labelNode
      )}
      {body}
    </div>
  );
}
