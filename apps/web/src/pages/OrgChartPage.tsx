import { useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Avatar,
  AvatarFallback,
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Skeleton,
  initials,
} from "@kapp/ui";
import {
  AlertTriangle,
  ChevronDown,
  ChevronRight,
  Maximize2,
  RefreshCw,
  Search,
  Users,
  ZoomIn,
  ZoomOut,
} from "lucide-react";
import { api } from "../lib/api";
import { humanizeToken, statusVariant } from "../lib/ktypeView";

const MIN_SCALE = 0.5;
const MAX_SCALE = 2;
const SCALE_STEP = 0.1;

interface EmployeeNode {
  id: string;
  name: string;
  designation?: string;
  department?: string;
  email?: string;
  status?: string;
}

interface TreeShape {
  roots: EmployeeNode[];
  childrenByParent: Map<string, EmployeeNode[]>;
  parentById: Map<string, string>;
}

/**
 * OrgChartPage renders the reporting hierarchy as a zoomable, pannable
 * tree of employee cards. Each manager can be collapsed; a search box
 * filters the org and auto-reveals every match's reporting line. The
 * hierarchy is derived from each employee's manager — people with no
 * manager (or whose manager isn't on the team) appear at the top.
 */
export function OrgChartPage() {
  const employeesQ = useQuery<KRecord[]>({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });

  const tree = useMemo(
    () => buildTree(employeesQ.data ?? []),
    [employeesQ.data],
  );
  const total = employeesQ.data?.length ?? 0;

  const [query, setQuery] = useState("");
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const [scale, setScale] = useState(1);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const dragRef = useRef<{ x: number; y: number; panX: number; panY: number } | null>(
    null,
  );

  const q = query.trim().toLowerCase();

  // When searching, compute the matches plus every ancestor on their
  // reporting line so the path to each match stays visible even if the
  // manager was manually collapsed.
  const { matches, forcedOpen } = useMemo(() => {
    if (!q) return { matches: new Set<string>(), forcedOpen: new Set<string>() };
    const m = new Set<string>();
    const open = new Set<string>();
    const all = [...tree.childrenByParent.values()].flat();
    const search = (n: EmployeeNode) => {
      const hay = [n.name, n.designation, n.department, n.email]
        .filter(Boolean)
        .join(" ")
        .toLowerCase();
      if (hay.includes(q)) {
        m.add(n.id);
        let p = tree.parentById.get(n.id);
        while (p) {
          open.add(p);
          p = tree.parentById.get(p);
        }
      }
    };
    tree.roots.forEach(search);
    all.forEach(search);
    return { matches: m, forcedOpen: open };
  }, [q, tree]);

  function toggle(id: string) {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function isOpen(id: string): boolean {
    // While searching, expand only the reporting lines that lead to a
    // match (forcedOpen holds those ancestors) so irrelevant branches stay
    // collapsed and the result stays focused on large orgs.
    if (q) return forcedOpen.has(id);
    return !collapsed.has(id);
  }

  function zoomBy(delta: number) {
    setScale((s) => clamp(Math.round((s + delta) * 10) / 10, MIN_SCALE, MAX_SCALE));
  }
  function resetView() {
    setScale(1);
    setPan({ x: 0, y: 0 });
  }

  function onPointerDown(e: React.PointerEvent<HTMLDivElement>) {
    // Let clicks on cards, toggles and links through; only blank canvas pans.
    if ((e.target as HTMLElement).closest("button,a,input")) return;
    dragRef.current = { x: e.clientX, y: e.clientY, panX: pan.x, panY: pan.y };
    e.currentTarget.setPointerCapture(e.pointerId);
  }
  function onPointerMove(e: React.PointerEvent<HTMLDivElement>) {
    const d = dragRef.current;
    if (!d) return;
    setPan({ x: d.panX + (e.clientX - d.x), y: d.panY + (e.clientY - d.y) });
  }
  function onPointerUp(e: React.PointerEvent<HTMLDivElement>) {
    dragRef.current = null;
    if (e.currentTarget.hasPointerCapture(e.pointerId))
      e.currentTarget.releasePointerCapture(e.pointerId);
  }

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>Human Resources</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            Org Chart
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            Who reports to whom across your team. Search for anyone to jump
            to their place in the org.
          </p>
        </div>
        {total > 0 && (
          <div className="flex flex-wrap items-center gap-2">
            <Field label="Search people" hideLabel>
              <Input
                size="sm"
                className="w-56"
                type="search"
                placeholder="Search name, role, team…"
                leadingAddon={<Search className="h-4 w-4" />}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            </Field>
            <div className="flex items-center gap-1 rounded-pill border border-border bg-bg-elevated p-0.5">
              <Button
                size="icon"
                variant="ghost"
                aria-label="Zoom out"
                disabled={scale <= MIN_SCALE}
                onClick={() => zoomBy(-SCALE_STEP)}
              >
                <ZoomOut className="h-4 w-4" />
              </Button>
              <span className="w-12 text-center text-xs tabular-nums text-fg-muted">
                {Math.round(scale * 100)}%
              </span>
              <Button
                size="icon"
                variant="ghost"
                aria-label="Zoom in"
                disabled={scale >= MAX_SCALE}
                onClick={() => zoomBy(SCALE_STEP)}
              >
                <ZoomIn className="h-4 w-4" />
              </Button>
              <Button
                size="icon"
                variant="ghost"
                aria-label="Reset view"
                onClick={resetView}
              >
                <Maximize2 className="h-4 w-4" />
              </Button>
            </div>
          </div>
        )}
      </header>

      {employeesQ.isLoading && <OrgChartSkeleton />}

      {employeesQ.isError && (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-sm text-fg"
        >
          <AlertTriangle className="h-4 w-4 shrink-0 text-danger" aria-hidden />
          <span className="min-w-0 flex-1">
            We couldn't load your team.
          </span>
          <Button
            size="sm"
            variant="outline"
            leadingIcon={<RefreshCw className="h-4 w-4" />}
            onClick={() => employeesQ.refetch()}
          >
            Retry
          </Button>
        </div>
      )}

      {employeesQ.data && total === 0 && (
        <EmptyState
          icon={<Users />}
          title="No employees yet"
          description="Add people in the Employees area and set each person's manager — your org chart builds itself from there."
        />
      )}

      {employeesQ.data && total > 0 && (
        <div
          className="relative h-[34rem] cursor-grab touch-none overflow-hidden rounded-lg border border-border bg-bg-subtle active:cursor-grabbing"
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={onPointerUp}
          onPointerLeave={onPointerUp}
        >
          <div
            className="absolute left-0 top-0 origin-top-left p-5"
            style={{
              transform: `translate(${pan.x}px, ${pan.y}px) scale(${scale})`,
            }}
          >
            {q && matches.size === 0 ? (
              <p className="text-sm text-fg-muted">
                No one matches “{query}”.
              </p>
            ) : (
              <TreeList
                nodes={tree.roots}
                childrenByParent={tree.childrenByParent}
                isOpen={isOpen}
                onToggle={toggle}
                matches={matches}
                forcedOpen={forcedOpen}
                searching={Boolean(q)}
              />
            )}
          </div>
          <p className="pointer-events-none absolute bottom-2 right-3 text-xs text-fg-subtle">
            Drag to pan · use the controls to zoom
          </p>
        </div>
      )}
    </section>
  );
}

function clamp(n: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, n));
}

function buildTree(records: KRecord[]): TreeShape {
  const nodes: EmployeeNode[] = records.map((r) => {
    const d = r.data as Record<string, unknown>;
    return {
      id: r.id,
      name: stringField(d.name) ?? "Unnamed",
      designation: stringField(d.designation),
      department: stringField(d.department),
      email: stringField(d.email),
      status: stringField(d.status),
    };
  });
  const byId = new Map<string, EmployeeNode>();
  nodes.forEach((n) => byId.set(n.id, n));
  const childrenByParent = new Map<string, EmployeeNode[]>();
  const parentById = new Map<string, string>();
  const roots: EmployeeNode[] = [];
  nodes.forEach((n) => {
    const raw = (records.find((r) => r.id === n.id)?.data ?? {}) as Record<
      string,
      unknown
    >;
    const managerId = stringField(raw.reporting_to);
    if (managerId && byId.has(managerId)) {
      const siblings = childrenByParent.get(managerId) ?? [];
      siblings.push(n);
      childrenByParent.set(managerId, siblings);
      parentById.set(n.id, managerId);
    } else {
      roots.push(n);
    }
  });
  const sortByName = (a: EmployeeNode, b: EmployeeNode) =>
    a.name.localeCompare(b.name);
  roots.sort(sortByName);
  childrenByParent.forEach((kids) => kids.sort(sortByName));
  return { roots, childrenByParent, parentById };
}

function stringField(v: unknown): string | undefined {
  if (typeof v !== "string") return undefined;
  const s = v.trim();
  return s ? s : undefined;
}

interface TreeProps {
  nodes: EmployeeNode[];
  childrenByParent: Map<string, EmployeeNode[]>;
  isOpen: (id: string) => boolean;
  onToggle: (id: string) => void;
  matches: Set<string>;
  forcedOpen: Set<string>;
  searching: boolean;
}

function TreeList({ nodes, ...rest }: TreeProps) {
  return (
    <ul className="flex list-none flex-col gap-2 pl-0">
      {nodes.map((n) => (
        <TreeNode key={n.id} node={n} {...rest} />
      ))}
    </ul>
  );
}

function TreeNode({
  node,
  childrenByParent,
  isOpen,
  onToggle,
  matches,
  forcedOpen,
  searching,
}: Omit<TreeProps, "nodes"> & { node: EmployeeNode }) {
  const kids = childrenByParent.get(node.id) ?? [];
  const hasKids = kids.length > 0;
  const open = isOpen(node.id);
  const isMatch = matches.has(node.id);
  // While searching, dim cards that are neither a match nor on a match's
  // reporting line so the eye lands on the relevant branch.
  const dimmed =
    searching && !isMatch && !forcedOpen.has(node.id);

  return (
    <li>
      <div className="flex items-center gap-2">
        <button
          type="button"
          aria-label={hasKids ? (open ? "Collapse" : "Expand") : undefined}
          aria-expanded={hasKids ? open : undefined}
          disabled={!hasKids}
          onClick={() => hasKids && onToggle(node.id)}
          className="flex h-5 w-5 shrink-0 items-center justify-center rounded-sm text-fg-muted transition-colors hover:bg-bg-muted disabled:opacity-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
        >
          {open ? (
            <ChevronDown className="h-4 w-4" />
          ) : (
            <ChevronRight className="h-4 w-4" />
          )}
        </button>
        <div
          className={[
            "flex min-w-[15rem] items-center gap-3 rounded-lg border bg-bg-elevated px-3 py-2 transition-colors",
            isMatch
              ? "border-accent ring-1 ring-accent"
              : "border-border",
            dimmed ? "opacity-40" : "opacity-100",
          ].join(" ")}
        >
          <Avatar size="sm">
            <AvatarFallback>{initials(node.name)}</AvatarFallback>
          </Avatar>
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <span className="truncate text-sm font-medium text-fg">
                {node.name}
              </span>
              {node.status && node.status !== "active" && (
                <Badge variant={statusVariant(node.status)} size="xs">
                  {humanizeToken(node.status)}
                </Badge>
              )}
            </div>
            <div className="flex flex-wrap items-center gap-x-2 text-xs text-fg-muted">
              {node.designation && <span className="truncate">{node.designation}</span>}
              {node.department && (
                <span className="truncate text-fg-subtle">· {node.department}</span>
              )}
            </div>
          </div>
          {hasKids && (
            <span className="ms-auto shrink-0 rounded-pill bg-bg-muted px-2 py-0.5 text-xs text-fg-muted">
              {kids.length}
            </span>
          )}
        </div>
      </div>
      {hasKids && open && (
        <ul className="ms-[1.4rem] mt-2 flex list-none flex-col gap-2 border-l border-border pl-4">
          {kids.map((c) => (
            <TreeNode
              key={c.id}
              node={c}
              childrenByParent={childrenByParent}
              isOpen={isOpen}
              onToggle={onToggle}
              matches={matches}
              forcedOpen={forcedOpen}
              searching={searching}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

function OrgChartSkeleton() {
  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border bg-bg-subtle p-5">
      {Array.from({ length: 5 }).map((_, i) => (
        <div
          key={i}
          className="flex items-center gap-3"
          style={{ marginInlineStart: `${(i % 3) * 1.5}rem` }}
        >
          <Skeleton variant="circle" className="h-7 w-7" />
          <Skeleton className="h-12 w-60" />
        </div>
      ))}
    </div>
  );
}
