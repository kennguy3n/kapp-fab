import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { BOM, BOMComponent, InventoryItem } from "@kapp/client";
import { api } from "../lib/api";

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
    <section style={{ display: "grid", gridTemplateColumns: "2fr 3fr", gap: 24 }}>
      <div>
        <h1>Bills of Materials</h1>
        <p style={{ color: "#6b7280" }}>
          One row per (item, version). Only one BOM per item may be active.
        </p>
        <div style={{ marginBottom: 8 }}>
          <label htmlFor="bom-filter" style={{ marginRight: 8 }}>
            Status:
          </label>
          <select
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
          </select>
        </div>
        {bomsQ.isLoading && <p>Loading…</p>}
        {bomsQ.isError && (
          <p style={{ color: "#dc2626" }}>{String(bomsQ.error)}</p>
        )}
        {bomsQ.data && (
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
                <th>Item</th>
                <th>Version</th>
                <th>Status</th>
                <th>Output</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {bomsQ.data.map((b: BOM) => (
                <tr key={b.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                  <td>{itemLabel.get(b.item_id) ?? b.item_id}</td>
                  <td>{b.version}</td>
                  <td>
                    <span
                      style={{
                        padding: "2px 8px",
                        borderRadius: 12,
                        background:
                          b.status === "active"
                            ? "#dcfce7"
                            : b.status === "obsolete"
                              ? "#fee2e2"
                              : "#e5e7eb",
                      }}
                    >
                      {b.status}
                    </span>
                  </td>
                  <td>
                    {b.output_qty} {b.uom}
                  </td>
                  <td>
                    {b.status === "draft" && (
                      <button
                        onClick={() =>
                          setStatus.mutate({ id: b.id, status: "active" })
                        }
                        disabled={setStatus.isPending}
                      >
                        Activate
                      </button>
                    )}
                    {b.status === "active" && (
                      <button
                        onClick={() =>
                          setStatus.mutate({ id: b.id, status: "obsolete" })
                        }
                        disabled={setStatus.isPending}
                      >
                        Obsolete
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
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
  const [components, setComponents] = useState<
    Array<Omit<BOMComponent, "bom_id">>
  >([
    {
      component_item_id: "",
      qty: "1",
      uom: "each",
      sort_order: 0,
    } as Omit<BOMComponent, "bom_id">,
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
        components: components
          .filter((c) => c.component_item_id)
          .map((c, i) => ({
            component_item_id: c.component_item_id,
            qty: c.qty,
            uom: c.uom,
            scrap_percent: c.scrap_percent ?? undefined,
            sort_order: i,
          })),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mfg", "boms"] });
      setItemID("");
      setVersion("v1");
      setComponents([
        {
          component_item_id: "",
          qty: "1",
          uom: "each",
          sort_order: 0,
        } as Omit<BOMComponent, "bom_id">,
      ]);
    },
  });

  const updateComponent = (
    idx: number,
    patch: Partial<Omit<BOMComponent, "bom_id">>,
  ) => {
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
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 8,
        padding: 16,
      }}
    >
      <h2 style={{ marginTop: 0 }}>Author BOM</h2>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 8 }}>
        <label>
          Finished good
          <select
            value={itemID}
            onChange={(e) => setItemID(e.target.value)}
            required
            style={{ width: "100%" }}
          >
            <option value="">Select item…</option>
            {items.map((it) => (
              <option key={it.id} value={it.id}>
                {it.sku} — {it.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          Version
          <input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            required
            style={{ width: "100%" }}
          />
        </label>
        <label>
          Output qty
          <input
            type="number"
            step="0.01"
            value={outputQty}
            onChange={(e) => setOutputQty(e.target.value)}
            required
            style={{ width: "100%" }}
          />
        </label>
        <label>
          UOM
          <input
            value={uom}
            onChange={(e) => setUOM(e.target.value)}
            required
            style={{ width: "100%" }}
          />
        </label>
      </div>
      <label style={{ display: "block", marginTop: 8 }}>
        Notes
        <textarea
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          style={{ width: "100%" }}
        />
      </label>

      <h3>Components</h3>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th>Item</th>
            <th>Qty</th>
            <th>UOM</th>
            <th>Scrap %</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {components.map((c, i) => (
            <tr key={i}>
              <td>
                <select
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
                </select>
              </td>
              <td>
                <input
                  type="number"
                  step="0.001"
                  value={c.qty}
                  onChange={(e) => updateComponent(i, { qty: e.target.value })}
                  required
                  style={{ width: 80 }}
                />
              </td>
              <td>
                <input
                  value={c.uom}
                  onChange={(e) => updateComponent(i, { uom: e.target.value })}
                  required
                  style={{ width: 60 }}
                />
              </td>
              <td>
                <input
                  type="number"
                  step="0.01"
                  value={c.scrap_percent ?? ""}
                  onChange={(e) =>
                    updateComponent(i, {
                      scrap_percent: e.target.value || undefined,
                    })
                  }
                  style={{ width: 80 }}
                />
              </td>
              <td>
                <button
                  type="button"
                  onClick={() =>
                    setComponents((prev) => prev.filter((_, j) => j !== i))
                  }
                  disabled={components.length === 1}
                  aria-label="remove component"
                >
                  ×
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <button
        type="button"
        onClick={() =>
          setComponents((prev) => [
            ...prev,
            {
              component_item_id: "",
              qty: "1",
              uom: "each",
              sort_order: prev.length,
            } as Omit<BOMComponent, "bom_id">,
          ])
        }
      >
        + Add component
      </button>

      <div style={{ marginTop: 12 }}>
        <label>
          <input
            type="checkbox"
            checked={activate}
            onChange={(e) => setActivate(e.target.checked)}
          />{" "}
          Activate immediately (demotes any other active BOM for this item)
        </label>
      </div>

      <div style={{ marginTop: 12 }}>
        <button type="submit" disabled={createMut.isPending || !itemID}>
          {createMut.isPending ? "Creating…" : "Create BOM"}
        </button>
        {createMut.isError && (
          <span style={{ color: "#dc2626", marginLeft: 12 }}>
            {String(createMut.error)}
          </span>
        )}
      </div>
    </form>
  );
}
