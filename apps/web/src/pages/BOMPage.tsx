import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { BOM, BOMComponent, InventoryItem } from "@kapp/client";
import {
  Badge,
  Button,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

// BOMComponentDraft is the in-form shape used while the user is
// authoring components, before any of them have been persisted.
// Deliberately omits bom_id (assigned server-side on insert) AND
// sort_order (the server derives it from array position — see
// CreateBOMInput in @kapp/client). Mirroring the server contract
// here means the UI never lets a user dial in a "sort_order" that
// would be silently overridden on POST.
type BOMComponentDraft = Omit<BOMComponent, "bom_id" | "sort_order">;

/**
 * BOMPage renders the Phase N6 Bill of Materials builder. The model
 * is:
 *   - One BOM per (item, version). Status moves draft → active →
 *     obsolete. Only one row per item may be active at a time
 *     (enforced by the partial unique index on the boms table).
 *   - Each BOM has N components; components are stored in their
 *     own row keyed by (bom_id, component_item_id, sort_order).
 *
 * The page lists existing BOMs on the left and exposes an
 * authoring form on the right so an SME can stand up a recipe
 * end-to-end without round-tripping through KChat. Activate flips
 * status=active, automatically demoting any previously-active row
 * for the same item to obsolete (server-side).
 */
export function BOMPage() {
  const qc = useQueryClient();
  const [filter, setFilter] = useState<"" | "draft" | "active" | "obsolete">("");
  const bomsQ = useQuery({
    queryKey: ["mfg", "boms", filter],
    queryFn: () => api.listBOMs(filter || undefined),
  });
  const itemsQ = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });

  const setStatus = useMutation({
    mutationFn: ({ id, status }: { id: string; status: string }) =>
      api.setBOMStatus(id, status),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["mfg", "boms"] }),
  });

  const itemLabel = useMemo(() => {
    const m = new Map<string, string>();
    (itemsQ.data ?? []).forEach((it: InventoryItem) =>
      m.set(it.id, `${it.sku} — ${it.name}`),
    );
    return m;
  }, [itemsQ.data]);

  return (
    <section className="grid grid-cols-[2fr_3fr] gap-6">
      <div>
        <h1>Bills of Materials</h1>
        <p className="text-fg-muted">
          One row per (item, version). Only one BOM per item may be active.
        </p>
        <div className="mb-2 flex items-center gap-2">
          <label htmlFor="bom-filter">Status:</label>
          <Select
            id="bom-filter"
            value={filter}
            onChange={(e) =>
              setFilter(e.target.value as "" | "draft" | "active" | "obsolete")
            }
          >
            <option value="">All</option>
            <option value="draft">Draft</option>
            <option value="active">Active</option>
            <option value="obsolete">Obsolete</option>
          </Select>
        </div>
        {bomsQ.isLoading && <p>Loading…</p>}
        {bomsQ.isError && (
          <p className="text-danger">{String(bomsQ.error)}</p>
        )}
        {bomsQ.data && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Item</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Output</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {bomsQ.data.map((b: BOM) => (
                <TableRow key={b.id}>
                  <TableCell>{itemLabel.get(b.item_id) ?? b.item_id}</TableCell>
                  <TableCell>{b.version}</TableCell>
                  <TableCell>
                    <Badge
                      variant={
                        b.status === "active"
                          ? "success"
                          : b.status === "obsolete"
                            ? "danger"
                            : "default"
                      }
                    >
                      {b.status}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    {b.output_qty} {b.uom}
                  </TableCell>
                  <TableCell>
                    {b.status === "draft" && (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() =>
                          setStatus.mutate({ id: b.id, status: "active" })
                        }
                        disabled={setStatus.isPending}
                      >
                        Activate
                      </Button>
                    )}
                    {b.status === "active" && (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() =>
                          setStatus.mutate({ id: b.id, status: "obsolete" })
                        }
                        disabled={setStatus.isPending}
                      >
                        Obsolete
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>
      <BOMAuthoringForm items={itemsQ.data ?? []} itemLabel={itemLabel} />
    </section>
  );
}

interface BOMAuthoringFormProps {
  items: InventoryItem[];
  itemLabel: Map<string, string>;
}

function BOMAuthoringForm({ items, itemLabel }: BOMAuthoringFormProps) {
  const qc = useQueryClient();
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
        // Server assigns sort_order from array position, so we
        // forward the filtered draft array as-is; ordering is
        // implicit in the JSON. See CreateBOMInput.components in
        // @kapp/client.
        components: components
          .filter((c) => c.component_item_id)
          .map((c) => ({
            component_item_id: c.component_item_id,
            qty: c.qty,
            uom: c.uom,
            scrap_percent: c.scrap_percent ?? undefined,
          })),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mfg", "boms"] });
      // Reset every field the form owns so a follow-up authoring
      // session starts from a clean slate. The `activate` checkbox
      // is the load-bearing one — leaving it checked silently
      // promotes the next BOM to active on creation, which then
      // auto-demotes any currently-active BOM for that item to
      // obsolete. Losing the previously-active BOM without an
      // explicit user action is unsafe; the user must opt in each
      // time.
      setItemID("");
      setVersion("v1");
      setOutputQty("1");
      setUOM("each");
      setNotes("");
      setActivate(false);
      setComponents([{ component_item_id: "", qty: "1", uom: "each" }]);
    },
  });

  const updateComponent = (idx: number, patch: Partial<BOMComponentDraft>) => {
    setComponents((prev) =>
      prev.map((c, i) => (i === idx ? { ...c, ...patch } : c)),
    );
  };

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        createMut.mutate();
      }}
      className="rounded-lg border border-border p-4"
    >
      <h2 className="mt-0">Author BOM</h2>
      <div className="grid grid-cols-2 gap-2">
        <label>
          Finished good
          <Select
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
            required
            className="w-full"
          >
            <option value="">Select item…</option>
            {items.map((it) => (
              <option key={it.id} value={it.id}>
                {it.sku} — {it.name}
              </option>
            ))}
          </Select>
        </label>
        <label>
          Version
          <Input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            required
            className="w-full"
          />
        </label>
        <label>
          Output qty
          <Input
            type="number"
            step="0.01"
            value={outputQty}
            onChange={(e) => setOutputQty(e.target.value)}
            required
            className="w-full"
          />
        </label>
        <label>
          UOM
          <Input
            value={uom}
            onChange={(e) => setUOM(e.target.value)}
            required
            className="w-full"
          />
        </label>
      </div>
      <label className="mt-2 block">
        Notes
        <textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          className="w-full rounded-md border border-border bg-bg px-2 py-1.5 text-sm text-fg outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
        />
      </label>

      <h3>Components</h3>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Item</TableHead>
            <TableHead>Qty</TableHead>
            <TableHead>UOM</TableHead>
            <TableHead>Scrap %</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {components.map((c, i) => (
            <TableRow key={i}>
              <TableCell>
                <Select
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
                  type="number"
                  step="0.001"
                  value={c.qty}
                  onChange={(e) => updateComponent(i, { qty: e.target.value })}
                  required
                  className="w-20"
                />
              </TableCell>
              <TableCell>
                <Input
                  value={c.uom}
                  onChange={(e) => updateComponent(i, { uom: e.target.value })}
                  required
                  className="w-16"
                />
              </TableCell>
              <TableCell>
                <Input
                  type="number"
                  step="0.01"
                  value={c.scrap_percent ?? ""}
                  onChange={(e) =>
                    updateComponent(i, {
                      scrap_percent: e.target.value || undefined,
                    })
                  }
                  className="w-20"
                />
              </TableCell>
              <TableCell>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() =>
                    setComponents((prev) => prev.filter((_, j) => j !== i))
                  }
                  disabled={components.length === 1}
                  aria-label="remove component"
                >
                  ×
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="mt-2"
        onClick={() =>
          setComponents((prev) => [
            ...prev,
            { component_item_id: "", qty: "1", uom: "each" },
          ])
        }
      >
        + Add component
      </Button>

      <div className="mt-3">
        <label>
          <input
            type="checkbox"
            checked={activate}
            onChange={(e) => setActivate(e.target.checked)}
          />{" "}
          Activate immediately (demotes any other active BOM for this item)
        </label>
      </div>

      <div className="mt-3 flex items-center">
        <Button type="submit" disabled={createMut.isPending || !itemID}>
          {createMut.isPending ? "Creating…" : "Create BOM"}
        </Button>
        {createMut.isError && (
          <span className="ml-3 text-danger">
            {String(createMut.error)}
          </span>
        )}
      </div>
    </form>
  );
}
