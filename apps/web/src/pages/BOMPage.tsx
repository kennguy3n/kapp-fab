import { useMemo, useState } from "react";
import {
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import type { BOM, BOMComponent, InventoryItem } from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Select,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
  Textarea,
  toast,
  type BadgeProps,
} from "@kapp/ui";
import {
  AlertTriangle,
  Boxes,
  ChevronDown,
  ChevronRight,
  Download,
  Layers,
  Plus,
  Trash2,
} from "lucide-react";
import { api } from "../lib/api";
import { downloadCsv } from "../lib/csv";
import { useFormatter } from "../lib/i18n";

type BOMStatus = BOM["status"];
type Formatters = ReturnType<typeof useFormatter>;

// BOMComponentDraft is the in-form shape used while the user is
// authoring components, before any of them have been persisted.
// Deliberately omits bom_id (assigned server-side on insert) AND
// sort_order (the server derives it from array position — see
// CreateBOMInput in @kapp/client). Mirroring the server contract
// here means the UI never lets a user dial in a "sort_order" that
// would be silently overridden on POST.
type BOMComponentDraft = Omit<BOMComponent, "bom_id" | "sort_order">;

const MONEY_OPTS: Intl.NumberFormatOptions = {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
};

const STATUS_LABELS: Record<BOMStatus, string> = {
  draft: "Draft",
  active: "Active",
  obsolete: "Obsolete",
};

const STATUS_VARIANT: Record<BOMStatus, BadgeProps["variant"]> = {
  draft: "default",
  active: "success",
  obsolete: "neutral",
};

// Cap explosion depth so a mis-entered component cycle (A needs B,
// B needs A) that slips past the visited-set guard can never spin
// the recursion forever; SME recipes are rarely deeper than this.
const MAX_TREE_DEPTH = 8;

function StatusBadge({ status }: { status: BOMStatus }) {
  return <Badge variant={STATUS_VARIANT[status]}>{STATUS_LABELS[status]}</Badge>;
}

function scrapFraction(c: { scrap_percent?: string | null }): number {
  const pct = Number(c.scrap_percent ?? 0);
  return Number.isFinite(pct) && pct > 0 ? pct / 100 : 0;
}

/**
 * BOMPage renders the Phase N6 Bill of Materials builder. The model
 * is:
 *   - One BOM per (item, version). Status moves draft → active →
 *     obsolete. Only one row per item may be active at a time
 *     (enforced by the partial unique index on the boms table).
 *   - Each BOM has N components; a component item may itself be a
 *     manufactured good with its own active BOM, so a recipe is a
 *     tree, not a flat list.
 *
 * The page lists existing BOMs on the left and an authoring form on
 * the right so an SME can stand up a recipe end-to-end. Selecting a
 * BOM opens a detail panel that explodes the recipe into a readable,
 * indented component tree and rolls component costs (from the
 * inventory valuation) up to a make cost for the finished good.
 */
export function BOMPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();
  const [filter, setFilter] = useState<"" | BOMStatus>("");
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const bomsQ = useQuery({
    queryKey: ["mfg", "boms", filter],
    queryFn: () => api.listBOMs(filter || undefined),
  });
  const itemsQ = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });
  // Active BOMs identify which component items are themselves made
  // in-house (sub-assemblies) so the detail tree can drill into them.
  const activeBomsQ = useQuery({
    queryKey: ["mfg", "boms", "active"],
    queryFn: () => api.listBOMs("active"),
  });
  // The on-hand valuation gives a per-unit cost (value ÷ qty) used to
  // cost bought components; sub-assembly costs are rolled up from it.
  const valuationQ = useQuery({
    queryKey: ["inventory", "valuation"],
    queryFn: () => api.getInventoryValuation(),
  });

  const detailQ = useQuery({
    queryKey: ["mfg", "bom", selectedId],
    queryFn: () => api.getBOM(selectedId!),
    enabled: !!selectedId,
  });

  // Eagerly fetch every active BOM's components once a recipe is open
  // so the cost roll-up can fully explode multi-level sub-assemblies
  // client-side (SME catalogues are small; react-query dedupes/caches).
  const activeBoms = activeBomsQ.data ?? [];
  const activeDetailQs = useQueries({
    queries: activeBoms.map((b) => ({
      queryKey: ["mfg", "bom", b.id],
      queryFn: () => api.getBOM(b.id),
      enabled: !!selectedId,
    })),
  });

  const setStatus = useMutation({
    mutationFn: ({ id, status }: { id: string; status: BOMStatus }) =>
      api.setBOMStatus(id, status),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: ["mfg", "boms"] });
      toast.success(
        vars.status === "active" ? "BOM activated" : "BOM marked obsolete",
      );
    },
  });

  const itemLabel = useMemo(() => {
    const m = new Map<string, string>();
    (itemsQ.data ?? []).forEach((it) => m.set(it.id, `${it.sku} — ${it.name}`));
    return m;
  }, [itemsQ.data]);

  // item_id → per-unit on-hand cost, derived from the valuation report.
  const unitCostByItem = useMemo(() => {
    const m = new Map<string, number>();
    (valuationQ.data?.rows ?? []).forEach((r) => {
      const qty = Number(r.qty);
      const value = Number(r.value_cost);
      if (qty > 0 && Number.isFinite(value)) m.set(r.item_id, value / qty);
    });
    return m;
  }, [valuationQ.data]);

  // item_id → its active BOM (with components) for sub-assembly explosion.
  // useQueries returns a fresh array every render, so depend on a stable
  // key derived from the resolved data (mirrors StockLevelsPage) rather
  // than the array identity — otherwise the recursive cost roll-up below
  // would recompute on every render.
  const activeDetailData = activeDetailQs.map((q) => q.data);
  const activeDetailKey = activeDetailData
    .map((b) => (b ? b.id : ""))
    .join("|");
  const bomByItem = useMemo(() => {
    const m = new Map<string, BOM>();
    activeDetailData.forEach((b) => {
      if (b && b.components) m.set(b.item_id, b);
    });
    return m;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeDetailKey]);

  function handleExport() {
    const rows = (bomsQ.data ?? []).map((b) => [
      itemLabel.get(b.item_id) ?? b.item_id,
      b.version,
      STATUS_LABELS[b.status],
      `${fmt.number(Number(b.output_qty))} ${b.uom}`,
    ]);
    downloadCsv(
      "bills-of-materials.csv",
      ["Finished good", "Version", "Status", "Output"],
      rows,
    );
    toast.success("Export complete", {
      description: `bills-of-materials.csv · ${rows.length} row(s)`,
    });
  }

  return (
    <section className="flex flex-col gap-6">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Manufacturing</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Bills of Materials
            </h1>
            <p className="mt-1 max-w-2xl text-sm text-fg-muted">
              A bill of materials is the recipe for a finished good — the
              components and quantities needed to make it. Each item keeps one
              active recipe; select a recipe to see its full component tree and
              rolled-up make cost.
            </p>
          </div>
          <Button
            size="sm"
            variant="outline"
            leadingIcon={<Download className="size-4" />}
            onClick={handleExport}
            disabled={!bomsQ.data || bomsQ.data.length === 0}
          >
            Export CSV
          </Button>
        </div>
      </header>

      <div className="flex flex-col gap-6 xl:flex-row">
        <div className="flex flex-col gap-3 xl:w-[380px] xl:shrink-0">
          <Field label="Status" className="w-44">
            <Select
              size="sm"
              value={filter}
              onChange={(e) => setFilter(e.target.value as "" | BOMStatus)}
            >
              <option value="">All statuses</option>
              <option value="draft">Draft</option>
              <option value="active">Active</option>
              <option value="obsolete">Obsolete</option>
            </Select>
          </Field>

          {bomsQ.isLoading ? (
            <div className="flex flex-col gap-2">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </div>
          ) : bomsQ.isError ? (
            <EmptyState
              icon={<AlertTriangle />}
              title="Couldn't load recipes"
              description={(bomsQ.error as Error).message}
              action={
                <Button
                  variant="secondary"
                  onClick={() => void bomsQ.refetch()}
                  disabled={bomsQ.isFetching}
                >
                  Retry
                </Button>
              }
            />
          ) : (bomsQ.data ?? []).length === 0 ? (
            <EmptyState
              icon={<Boxes />}
              title={filter ? "No recipes with this status" : "No recipes yet"}
              description={
                filter
                  ? "Try a different status filter."
                  : "Author your first bill of materials with the form on the right."
              }
            />
          ) : (
            <BOMList
              boms={bomsQ.data ?? []}
              fmt={fmt}
              itemLabel={itemLabel}
              selectedId={selectedId}
              onSelect={setSelectedId}
              onActivate={(id) => setStatus.mutate({ id, status: "active" })}
              onObsolete={(id) => setStatus.mutate({ id, status: "obsolete" })}
              actionPending={setStatus.isPending}
            />
          )}
        </div>

        <div className="min-w-0 flex-1">
          <BOMAuthoringForm
            items={itemsQ.data ?? []}
            itemLabel={itemLabel}
            onCreated={(b) => {
              qc.invalidateQueries({ queryKey: ["mfg", "boms"] });
              setSelectedId(b.id);
            }}
          />
        </div>
      </div>

      {selectedId ? (
        detailQ.isLoading ? (
          <div className="flex flex-col gap-3">
            <Skeleton className="h-8 w-72" />
            <Skeleton className="h-48 w-full" />
          </div>
        ) : detailQ.isError ? (
          <EmptyState
            icon={<AlertTriangle />}
            title="Couldn't load this recipe"
            description={(detailQ.error as Error).message}
            action={
              <Button
                variant="secondary"
                onClick={() => void detailQ.refetch()}
                disabled={detailQ.isFetching}
              >
                Retry
              </Button>
            }
          />
        ) : detailQ.data ? (
          <BOMDetail
            key={selectedId}
            bom={detailQ.data}
            fmt={fmt}
            itemLabel={itemLabel}
            unitCostByItem={unitCostByItem}
            bomByItem={bomByItem}
            costReady={!!valuationQ.data}
            onClose={() => setSelectedId(null)}
          />
        ) : null
      ) : null}
    </section>
  );
}

function BOMList(props: {
  boms: BOM[];
  fmt: Formatters;
  itemLabel: Map<string, string>;
  selectedId: string | null;
  onSelect: (id: string) => void;
  onActivate: (id: string) => void;
  onObsolete: (id: string) => void;
  actionPending: boolean;
}) {
  const { fmt } = props;
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Finished good</TableHead>
          <TableHead>Version</TableHead>
          <TableHead>Output</TableHead>
          <TableHead>Status</TableHead>
          <TableHead className="w-0 text-right">Actions</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {props.boms.map((b) => {
          const selected = b.id === props.selectedId;
          return (
            <TableRow
              key={b.id}
              onClick={() => props.onSelect(b.id)}
              aria-selected={selected}
              className={
                selected ? "cursor-pointer bg-bg-muted" : "cursor-pointer"
              }
            >
              <TableCell className="font-medium text-fg">
                {props.itemLabel.get(b.item_id) ?? b.item_id}
              </TableCell>
              <TableCell className="text-fg-muted">{b.version}</TableCell>
              <TableCell className="whitespace-nowrap tabular-nums text-fg-muted">
                {fmt.number(Number(b.output_qty))} {b.uom}
              </TableCell>
              <TableCell>
                <StatusBadge status={b.status} />
              </TableCell>
              <TableCell className="text-right">
                {b.status === "draft" && (
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={(e) => {
                      e.stopPropagation();
                      props.onActivate(b.id);
                    }}
                    disabled={props.actionPending}
                  >
                    Activate
                  </Button>
                )}
                {b.status === "active" && (
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={(e) => {
                      e.stopPropagation();
                      props.onObsolete(b.id);
                    }}
                    disabled={props.actionPending}
                  >
                    Obsolete
                  </Button>
                )}
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

// ---------------------------------------------------------------------------
// Recipe detail — exploded component tree + cost roll-up
// ---------------------------------------------------------------------------

interface TreeNode {
  key: string;
  itemId: string;
  label: string;
  // Effective quantity this node contributes to one production batch of
  // the top-level finished good (component qty × scrap, scaled by every
  // ancestor sub-assembly's required quantity).
  qty: number;
  uom: string;
  scrapPercent: number | null;
  // Per-unit cost of this item (rolled up for sub-assemblies). Undefined
  // when no cost is known for it or any of its descendants.
  unitCost: number | undefined;
  // qty × unitCost — this node's contribution to the make cost.
  extended: number | undefined;
  isSubassembly: boolean;
  children: TreeNode[];
}

function BOMDetail(props: {
  bom: BOM;
  fmt: Formatters;
  itemLabel: Map<string, string>;
  unitCostByItem: Map<string, number>;
  bomByItem: Map<string, BOM>;
  costReady: boolean;
  onClose: () => void;
}) {
  const { bom, fmt, itemLabel, unitCostByItem, bomByItem } = props;

  // Rolled-up per-unit cost of producing one unit of `itemId`. Bought
  // items use their on-hand unit cost; made items recurse into their
  // active BOM. Returns undefined if any leaf cost is unknown so we
  // never show a confidently-wrong total.
  const rolledUnitCost = useMemo(() => {
    const cache = new Map<string, number | undefined>();
    function compute(itemId: string, visited: Set<string>): number | undefined {
      if (cache.has(itemId)) return cache.get(itemId);
      const sub = bomByItem.get(itemId);
      if (!sub || visited.has(itemId) || visited.size >= MAX_TREE_DEPTH) {
        return unitCostByItem.get(itemId);
      }
      const next = new Set(visited).add(itemId);
      const output = Number(sub.output_qty) || 1;
      let sum = 0;
      let known = true;
      for (const c of sub.components ?? []) {
        const cu = compute(c.component_item_id, next);
        if (cu === undefined) {
          known = false;
        } else {
          sum += Number(c.qty) * (1 + scrapFraction(c)) * cu;
        }
      }
      const result = known ? sum / output : undefined;
      cache.set(itemId, result);
      return result;
    }
    return (itemId: string) => compute(itemId, new Set());
  }, [bomByItem, unitCostByItem]);

  const tree = useMemo<TreeNode[]>(() => {
    function build(
      itemId: string,
      qty: number,
      uom: string,
      scrapPercent: number | null,
      keyPrefix: string,
      visited: Set<string>,
    ): TreeNode {
      const unitCost = rolledUnitCost(itemId);
      const sub = bomByItem.get(itemId);
      const isSubassembly = !!sub;
      const children: TreeNode[] = [];
      if (sub && !visited.has(itemId) && visited.size < MAX_TREE_DEPTH) {
        const next = new Set(visited).add(itemId);
        const output = Number(sub.output_qty) || 1;
        (sub.components ?? []).forEach((c, i) => {
          children.push(
            build(
              c.component_item_id,
              (qty / output) * Number(c.qty) * (1 + scrapFraction(c)),
              c.uom,
              c.scrap_percent != null ? Number(c.scrap_percent) : null,
              `${keyPrefix}.${i}`,
              next,
            ),
          );
        });
      }
      return {
        key: keyPrefix,
        itemId,
        label: itemLabel.get(itemId) ?? itemId,
        qty,
        uom,
        scrapPercent,
        unitCost,
        extended: unitCost === undefined ? undefined : qty * unitCost,
        isSubassembly,
        children,
      };
    }
    const visited = new Set<string>([bom.item_id]);
    return (bom.components ?? []).map((c, i) =>
      build(
        c.component_item_id,
        Number(c.qty) * (1 + scrapFraction(c)),
        c.uom,
        c.scrap_percent != null ? Number(c.scrap_percent) : null,
        `${i}`,
        visited,
      ),
    );
  }, [bom, bomByItem, itemLabel, rolledUnitCost]);

  const outputQty = Number(bom.output_qty) || 1;
  const totalCost = tree.reduce((acc, n) => acc + (n.extended ?? 0), 0);
  const hasUnknownCost = tree.some(
    (n) => n.extended === undefined || hasUndefinedDescendant(n),
  );
  const perUnitCost = totalCost / outputQty;
  const componentCount = tree.length;

  return (
    <div className="flex flex-col gap-4 rounded-xl border border-border bg-bg-subtle p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h2 className="m-0 truncate text-lg font-semibold text-fg">
              {itemLabel.get(bom.item_id) ?? bom.item_id}
            </h2>
            <StatusBadge status={bom.status} />
            <Badge variant="outline">Version {bom.version}</Badge>
          </div>
          <p className="mt-1 text-sm text-fg-muted">
            Makes {fmt.number(outputQty)} {bom.uom} ·{" "}
            {componentCount === 1
              ? "1 direct component"
              : `${componentCount} direct components`}
          </p>
          {bom.notes ? (
            <p className="mt-1 text-sm text-fg-muted">{bom.notes}</p>
          ) : null}
        </div>
        <Button size="sm" variant="ghost" onClick={props.onClose}>
          Close
        </Button>
      </div>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        <CostStat
          label={`Make cost (${fmt.number(outputQty)} ${bom.uom})`}
          value={
            props.costReady
              ? `${fmt.number(totalCost, MONEY_OPTS)}${hasUnknownCost ? "*" : ""}`
              : "…"
          }
        />
        <CostStat
          label="Cost per unit"
          value={
            props.costReady
              ? `${fmt.number(perUnitCost, MONEY_OPTS)}${hasUnknownCost ? "*" : ""}`
              : "…"
          }
        />
        <CostStat label="Components" value={fmt.number(componentCount)} />
      </div>

      {tree.length === 0 ? (
        <EmptyState
          icon={<Layers />}
          title="No components"
          description="This recipe has no components yet."
        />
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Component</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead>Unit</TableHead>
                <TableHead className="text-right">Scrap</TableHead>
                <TableHead className="text-right">Unit cost</TableHead>
                <TableHead className="text-right">Extended</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tree.map((node) => (
                <TreeRow key={node.key} node={node} depth={0} fmt={fmt} />
              ))}
            </TableBody>
            <TableFooter>
              <TableRow>
                <TableCell colSpan={5} className="font-medium text-fg">
                  Total make cost
                </TableCell>
                <TableCell className="text-right font-semibold tabular-nums text-fg">
                  {props.costReady ? fmt.number(totalCost, MONEY_OPTS) : "…"}
                </TableCell>
              </TableRow>
            </TableFooter>
          </Table>
          {hasUnknownCost ? (
            <p className="text-xs text-fg-subtle">
              * Some components have no on-hand cost yet, so this total covers
              only the priced components.
            </p>
          ) : null}
        </>
      )}
    </div>
  );
}

function hasUndefinedDescendant(node: TreeNode): boolean {
  return node.children.some(
    (c) => c.extended === undefined || hasUndefinedDescendant(c),
  );
}

function CostStat(props: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-border bg-bg p-3">
      <div className="text-xs font-medium text-fg-muted">{props.label}</div>
      <div className="mt-1 text-lg font-semibold tabular-nums text-fg">
        {props.value}
      </div>
    </div>
  );
}

function TreeRow(props: { node: TreeNode; depth: number; fmt: Formatters }) {
  const { node, depth, fmt } = props;
  const [open, setOpen] = useState(depth < 1);
  const hasChildren = node.children.length > 0;
  return (
    <>
      <TableRow>
        <TableCell>
          <div
            className="flex items-center gap-1.5"
            style={{ paddingInlineStart: `${depth * 1.25}rem` }}
          >
            {hasChildren ? (
              <button
                type="button"
                onClick={() => setOpen((v) => !v)}
                aria-expanded={open}
                aria-label={
                  open ? `Collapse ${node.label}` : `Expand ${node.label}`
                }
                className="inline-flex size-5 items-center justify-center rounded-sm text-fg-muted hover:bg-bg-muted hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
              >
                {open ? (
                  <ChevronDown className="size-4" />
                ) : (
                  <ChevronRight className="size-4" />
                )}
              </button>
            ) : (
              <span className="inline-block size-5" aria-hidden />
            )}
            <span className="truncate font-medium text-fg">{node.label}</span>
            {node.isSubassembly ? (
              <Badge variant="info" size="xs">
                Sub-assembly
              </Badge>
            ) : null}
          </div>
        </TableCell>
        <TableCell className="text-right tabular-nums text-fg">
          {fmt.number(node.qty, { maximumFractionDigits: 3 })}
        </TableCell>
        <TableCell className="text-fg-muted">{node.uom}</TableCell>
        <TableCell className="text-right tabular-nums text-fg-muted">
          {node.scrapPercent && node.scrapPercent > 0
            ? `${fmt.number(node.scrapPercent)}%`
            : "—"}
        </TableCell>
        <TableCell className="text-right tabular-nums text-fg-muted">
          {node.unitCost === undefined
            ? "—"
            : fmt.number(node.unitCost, MONEY_OPTS)}
        </TableCell>
        <TableCell className="text-right tabular-nums text-fg">
          {node.extended === undefined
            ? "—"
            : fmt.number(node.extended, MONEY_OPTS)}
        </TableCell>
      </TableRow>
      {open
        ? node.children.map((child) => (
            <TreeRow
              key={child.key}
              node={child}
              depth={depth + 1}
              fmt={fmt}
            />
          ))
        : null}
    </>
  );
}

// ---------------------------------------------------------------------------
// Authoring form
// ---------------------------------------------------------------------------

function BOMAuthoringForm(props: {
  items: InventoryItem[];
  itemLabel: Map<string, string>;
  onCreated: (b: BOM) => void;
}) {
  const { items, itemLabel } = props;
  const [itemID, setItemID] = useState("");
  const [version, setVersion] = useState("v1");
  const [outputQty, setOutputQty] = useState("1");
  const [uom, setUOM] = useState("each");
  const [notes, setNotes] = useState("");
  const [activate, setActivate] = useState(false);
  const [components, setComponents] = useState<BOMComponentDraft[]>([
    { component_item_id: "", qty: "1", uom: "each" },
  ]);

  const createMut = useMutation({
    mutationFn: () =>
      api.createBOM({
        item_id: itemID,
        version,
        output_qty: outputQty,
        uom,
        notes,
        activate,
        // Server assigns sort_order from array position, so we forward
        // the filtered draft array as-is; ordering is implicit in the
        // JSON. See CreateBOMInput.components in @kapp/client.
        components: components
          .filter((c) => c.component_item_id)
          .map((c) => ({
            component_item_id: c.component_item_id,
            qty: c.qty,
            uom: c.uom,
            scrap_percent: c.scrap_percent ?? undefined,
          })),
      }),
    onSuccess: (b) => {
      toast.success("Recipe created", {
        description: itemLabel.get(b.item_id) ?? undefined,
      });
      // Reset every field the form owns so a follow-up authoring
      // session starts from a clean slate. The `activate` checkbox is
      // the load-bearing one — leaving it checked silently promotes the
      // next BOM to active on creation, which then auto-demotes any
      // currently-active BOM for that item to obsolete. Losing the
      // previously-active BOM without an explicit action is unsafe; the
      // user must opt in each time.
      setItemID("");
      setVersion("v1");
      setOutputQty("1");
      setUOM("each");
      setNotes("");
      setActivate(false);
      setComponents([{ component_item_id: "", qty: "1", uom: "each" }]);
      props.onCreated(b);
    },
  });

  const updateComponent = (idx: number, patch: Partial<BOMComponentDraft>) => {
    setComponents((prev) =>
      prev.map((c, i) => (i === idx ? { ...c, ...patch } : c)),
    );
  };

  const namedComponents = components.filter((c) => c.component_item_id).length;

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        createMut.mutate();
      }}
      className="flex flex-col gap-4 rounded-xl border border-border p-4"
    >
      <div>
        <h2 className="m-0 text-base font-semibold text-fg">
          Author a recipe
        </h2>
        <p className="mt-1 text-sm text-fg-muted">
          Choose the finished good, then list what it's made from.
        </p>
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Field label="Finished good" required>
          <Select
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
            required
          >
            <option value="">Select an item…</option>
            {items.map((it) => (
              <option key={it.id} value={it.id}>
                {it.sku} — {it.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Version" required help="e.g. v1, v2, 2024-rev-A.">
          <Input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            required
          />
        </Field>
        <Field label="Output quantity" required help="How many this recipe makes.">
          <Input
            type="number"
            step="0.01"
            min="0"
            inputMode="decimal"
            value={outputQty}
            onChange={(e) => setOutputQty(e.target.value)}
            required
          />
        </Field>
        <Field label="Unit" required help="Unit of measure for the output.">
          <Input
            value={uom}
            onChange={(e) => setUOM(e.target.value)}
            required
          />
        </Field>
      </div>

      <Field label="Notes" help="Optional — anything an operator should know.">
        <Textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          rows={2}
        />
      </Field>

      <div className="flex flex-col gap-2">
        <div className="flex items-center justify-between">
          <h3 className="m-0 text-sm font-semibold text-fg">
            Components{" "}
            <span className="font-normal text-fg-muted">
              ({namedComponents} selected)
            </span>
          </h3>
        </div>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Item</TableHead>
              <TableHead className="text-right">Qty</TableHead>
              <TableHead>Unit</TableHead>
              <TableHead className="text-right">Scrap %</TableHead>
              <TableHead className="w-0 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {components.map((c, i) => (
              <TableRow key={i}>
                <TableCell>
                  <Select
                    size="sm"
                    aria-label={`Component ${i + 1}`}
                    value={c.component_item_id}
                    onChange={(e) =>
                      updateComponent(i, { component_item_id: e.target.value })
                    }
                    required
                  >
                    <option value="">Select…</option>
                    {items
                      .filter((it) => it.id !== itemID)
                      .map((it) => (
                        <option key={it.id} value={it.id}>
                          {itemLabel.get(it.id) ?? it.sku}
                        </option>
                      ))}
                  </Select>
                </TableCell>
                <TableCell>
                  <Input
                    size="sm"
                    type="number"
                    step="0.001"
                    min="0"
                    inputMode="decimal"
                    aria-label={`Quantity for component ${i + 1}`}
                    className="w-24 text-right tabular-nums"
                    value={c.qty}
                    onChange={(e) => updateComponent(i, { qty: e.target.value })}
                    required
                  />
                </TableCell>
                <TableCell>
                  <Input
                    size="sm"
                    aria-label={`Unit for component ${i + 1}`}
                    className="w-20"
                    value={c.uom}
                    onChange={(e) => updateComponent(i, { uom: e.target.value })}
                    required
                  />
                </TableCell>
                <TableCell>
                  <Input
                    size="sm"
                    type="number"
                    step="0.01"
                    min="0"
                    inputMode="decimal"
                    aria-label={`Scrap percent for component ${i + 1}`}
                    className="w-24 text-right tabular-nums"
                    value={c.scrap_percent ?? ""}
                    onChange={(e) =>
                      updateComponent(i, {
                        scrap_percent: e.target.value || undefined,
                      })
                    }
                  />
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    onClick={() =>
                      setComponents((prev) => prev.filter((_, j) => j !== i))
                    }
                    disabled={components.length === 1}
                    aria-label={`Remove component ${i + 1}`}
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            leadingIcon={<Plus className="size-4" />}
            onClick={() =>
              setComponents((prev) => [
                ...prev,
                { component_item_id: "", qty: "1", uom: "each" },
              ])
            }
          >
            Add component
          </Button>
        </div>
      </div>

      <label className="flex items-start gap-2 text-sm text-fg">
        <input
          type="checkbox"
          className="mt-0.5 size-4 rounded-sm border-border text-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
          checked={activate}
          onChange={(e) => setActivate(e.target.checked)}
        />
        <span>
          Make this the active recipe now
          <span className="block text-xs text-fg-muted">
            Replaces any other active recipe for this item.
          </span>
        </span>
      </label>

      <div className="flex flex-wrap items-center gap-3">
        <Button type="submit" disabled={createMut.isPending || !itemID}>
          {createMut.isPending ? "Creating…" : "Create BOM"}
        </Button>
        {createMut.isError ? (
          <span className="text-sm text-danger">
            Couldn't create recipe: {(createMut.error as Error).message}
          </span>
        ) : null}
      </div>
    </form>
  );
}
