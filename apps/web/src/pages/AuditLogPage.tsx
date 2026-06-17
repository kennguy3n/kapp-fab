import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { History, FilterX } from "lucide-react";
import type { AuditEntry } from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Field,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type BadgeProps,
} from "@kapp/ui";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeLabel, humanizeToken, ktypeSingular } from "../lib/ktypeView";
import {
  AdminErrorState,
  AdminPageHeader,
  AdminTableSkeleton,
  CopyableId,
} from "./adminKit";

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

const PAGE_SIZE = 50;

const ACTOR_LABEL: Record<AuditEntry["actor_kind"], string> = {
  user: "Person",
  agent: "Agent",
  system: "System",
};

/**
 * AuditLogPage renders the append-only audit log for the current
 * tenant. The record-type / record-ID filters map to the backend
 * query parameters; actor and date filters refine the loaded page
 * client-side (the API exposes no server-side filter for those).
 * Pagination is offset-based to match the backend.
 */
export function AuditLogPage() {
  const fmt = useFormatter();
  const [targetKType, setTargetKType] = useState("");
  const [targetID, setTargetID] = useState("");
  const [actorKind, setActorKind] = useState("");
  const [fromDate, setFromDate] = useState("");
  const [toDate, setToDate] = useState("");
  const [page, setPage] = useState(0);

  const params = useMemo(
    () => ({
      target_ktype: targetKType.trim() || undefined,
      target_id: targetID.trim() || undefined,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
    }),
    [targetKType, targetID, page],
  );

  const entries = useQuery({
    queryKey: ["audit", params],
    queryFn: () => api.listAuditLog(params),
  });

  const rows = entries.data ?? [];
  const visible = useMemo(() => {
    const fromMs = fromDate ? new Date(`${fromDate}T00:00:00`).getTime() : null;
    const toMs = toDate ? new Date(`${toDate}T23:59:59.999`).getTime() : null;
    return rows.filter((e) => {
      if (actorKind && e.actor_kind !== actorKind) return false;
      const ts = new Date(e.created_at).getTime();
      if (fromMs !== null && ts < fromMs) return false;
      if (toMs !== null && ts > toMs) return false;
      return true;
    });
  }, [rows, actorKind, fromDate, toDate]);

  const clientFiltered = actorKind !== "" || fromDate !== "" || toDate !== "";
  const anyFilter =
    clientFiltered || targetKType.trim() !== "" || targetID.trim() !== "";

  const clearFilters = () => {
    setTargetKType("");
    setTargetID("");
    setActorKind("");
    setFromDate("");
    setToDate("");
    setPage(0);
  };

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Audit log"
        description="An append-only record of who changed what in this workspace. Filtering only changes what's shown — the underlying trail is never altered."
        actions={
          rows.length > 0 ? (
            <Badge variant="neutral" size="md">
              {fmt.number(visible.length)}{" "}
              {visible.length === 1 ? "entry" : "entries"}
            </Badge>
          ) : undefined
        }
      />

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-5">
        <Field label="Record type">
          <Input
            value={targetKType}
            onChange={(e) => {
              setTargetKType(e.target.value);
              setPage(0);
            }}
            placeholder="e.g. crm.deal"
          />
        </Field>
        <Field label="Record ID">
          <Input
            value={targetID}
            onChange={(e) => {
              setTargetID(e.target.value);
              setPage(0);
            }}
            placeholder="Exact identifier"
          />
        </Field>
        <Field label="Actor">
          <Select
            value={actorKind}
            onChange={(e) => setActorKind(e.target.value)}
          >
            <option value="">All actors</option>
            <option value="user">People</option>
            <option value="agent">Agents</option>
            <option value="system">System</option>
          </Select>
        </Field>
        <Field label="From">
          <Input
            type="date"
            value={fromDate}
            onChange={(e) => setFromDate(e.target.value)}
          />
        </Field>
        <Field label="To">
          <Input
            type="date"
            value={toDate}
            onChange={(e) => setToDate(e.target.value)}
          />
        </Field>
      </div>

      {entries.isLoading ? (
        <AdminTableSkeleton
          columns={["When", "Actor", "Action", "Record", "Changes"]}
        />
      ) : entries.isError ? (
        <AdminErrorState
          title="Couldn't load the audit log"
          error={entries.error}
          onRetry={() => entries.refetch()}
        />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={<History />}
          title={anyFilter ? "No entries match these filters" : "No activity yet"}
          description={
            anyFilter
              ? "Try widening your filters or clearing them to see the full trail."
              : "Changes to records, settings, and access will appear here as your team works."
          }
          action={
            anyFilter ? (
              <Button variant="secondary" size="sm" onClick={clearFilters}>
                Clear filters
              </Button>
            ) : undefined
          }
        />
      ) : visible.length === 0 ? (
        <EmptyState
          icon={<FilterX />}
          title="No entries match these filters"
          description="This page of activity has no entries for the selected actor or date range."
          action={
            <Button variant="secondary" size="sm" onClick={clearFilters}>
              Clear filters
            </Button>
          }
        />
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-44">When</TableHead>
                <TableHead>Actor</TableHead>
                <TableHead>Action</TableHead>
                <TableHead>Record</TableHead>
                <TableHead>Changes</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.map((e) => (
                <AuditRow key={e.id} entry={e} fmt={fmt} />
              ))}
            </TableBody>
          </Table>
          <div className="flex items-center justify-between text-sm">
            <span className="text-fg-muted">Page {page + 1}</span>
            <div className="flex gap-2">
              <Button
                size="sm"
                variant="outline"
                disabled={page === 0}
                onClick={() => setPage((p) => Math.max(0, p - 1))}
              >
                Previous
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={rows.length < PAGE_SIZE}
                onClick={() => setPage((p) => p + 1)}
              >
                Next
              </Button>
            </div>
          </div>
        </>
      )}
    </section>
  );
}

function actionVariant(action: string): BadgeVariant {
  const verb = action.toLowerCase();
  if (/(create|add|insert)/.test(verb)) return "success";
  if (/(delete|remove|destroy)/.test(verb)) return "danger";
  if (/(suspend|disable|revoke|archive)/.test(verb)) return "warning";
  if (/(update|edit|change|set)/.test(verb)) return "info";
  return "neutral";
}

function actionLabel(action: string): string {
  return action
    .split(/[._]/)
    .filter(Boolean)
    .map((part) => humanizeToken(part))
    .join(" ");
}

function AuditRow({
  entry,
  fmt,
}: {
  entry: AuditEntry;
  fmt: ReturnType<typeof useFormatter>;
}) {
  return (
    <TableRow className="align-top">
      <TableCell className="whitespace-nowrap font-tabular text-fg-muted">
        {fmt.dateTime(new Date(entry.created_at))}
      </TableCell>
      <TableCell>
        <div className="font-medium text-fg">{ACTOR_LABEL[entry.actor_kind]}</div>
        {entry.actor_id ? (
          <CopyableId value={entry.actor_id} label="actor" />
        ) : null}
      </TableCell>
      <TableCell>
        <Badge variant={actionVariant(entry.action)}>
          {actionLabel(entry.action)}
        </Badge>
      </TableCell>
      <TableCell>
        {entry.target_ktype ? (
          <div className="font-medium text-fg">
            {ktypeSingular(entry.target_ktype)}
          </div>
        ) : (
          <span className="text-fg-subtle">—</span>
        )}
        {entry.target_id ? (
          <CopyableId value={entry.target_id} label="record" />
        ) : null}
      </TableCell>
      <TableCell className="min-w-[16rem]">
        <DiffView before={entry.before} after={entry.after} fmt={fmt} />
      </TableCell>
    </TableRow>
  );
}

function toRecord(v: unknown): Record<string, unknown> | null {
  return v && typeof v === "object" && !Array.isArray(v)
    ? (v as Record<string, unknown>)
    : null;
}

const ISO_DATE_RE = /^\d{4}-\d{2}-\d{2}T/;
const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function formatScalar(
  value: unknown,
  fmt: ReturnType<typeof useFormatter>,
): string {
  if (value == null || value === "") return "empty";
  if (typeof value === "boolean") return value ? "Yes" : "No";
  if (typeof value === "number") return fmt.number(value);
  if (typeof value === "string") {
    if (ISO_DATE_RE.test(value)) {
      const d = new Date(value);
      if (!Number.isNaN(d.getTime())) return fmt.dateTime(d);
    }
    if (UUID_RE.test(value)) return `${value.slice(0, 8)}…`;
    return value;
  }
  const json = JSON.stringify(value);
  return json.length > 60 ? `${json.slice(0, 57)}…` : json;
}

interface FieldChange {
  key: string;
  before: unknown;
  after: unknown;
}

function diffRecords(
  before: Record<string, unknown> | null,
  after: Record<string, unknown> | null,
): FieldChange[] {
  const keys = new Set<string>([
    ...Object.keys(before ?? {}),
    ...Object.keys(after ?? {}),
  ]);
  const changes: FieldChange[] = [];
  for (const key of keys) {
    const b = before?.[key];
    const a = after?.[key];
    if (JSON.stringify(b) !== JSON.stringify(a)) {
      changes.push({ key, before: b, after: a });
    }
  }
  return changes.sort((x, y) => x.key.localeCompare(y.key));
}

// DiffView turns the raw before/after JSON blobs into a readable,
// field-level change list — the canonical "no raw JSON" treatment for
// the audit trail. Creation shows only new values; deletion is flagged;
// updates render "old → new" per changed field.
function DiffView({
  before,
  after,
  fmt,
}: {
  before: unknown;
  after: unknown;
  fmt: ReturnType<typeof useFormatter>;
}) {
  const beforeObj = toRecord(before);
  const afterObj = toRecord(after);

  if (before == null && after == null) {
    return <span className="text-fg-subtle">No field changes</span>;
  }

  const created = before == null && after != null;
  const deleted = before != null && after == null;
  const changes = diffRecords(beforeObj, afterObj);

  // A deletion is always flagged, even when it carries field data
  // (before populated, after null). Without this, a deletion fell
  // through to the generic "old → empty" path and lost its "removed"
  // indicator.
  if (!created && !deleted && changes.length === 0) {
    return <span className="text-fg-subtle">No field changes</span>;
  }

  return (
    <div className="flex flex-col gap-1">
      {created && (
        <Badge variant="success" className="self-start">
          Record created
        </Badge>
      )}
      {deleted && (
        <Badge variant="danger" className="self-start">
          Record removed
        </Badge>
      )}
      {changes.length > 0 && (
        <ul className="flex flex-col gap-1">
          {changes.map((c) => (
            <li key={c.key} className="flex flex-wrap items-baseline gap-1.5">
              <span className="font-medium text-fg">
                {humanizeLabel(c.key)}
              </span>
              {created ? (
                <span className="text-success">
                  {formatScalar(c.after, fmt)}
                </span>
              ) : deleted ? (
                <span className="text-fg-muted line-through">
                  {formatScalar(c.before, fmt)}
                </span>
              ) : (
                <>
                  <span className="text-fg-muted line-through">
                    {formatScalar(c.before, fmt)}
                  </span>
                  <span aria-hidden="true" className="text-fg-subtle">
                    →
                  </span>
                  <span className="text-fg">{formatScalar(c.after, fmt)}</span>
                </>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
