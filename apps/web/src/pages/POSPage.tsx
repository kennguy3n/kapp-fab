import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

const KTYPE_PROFILE = "sales.pos_profile";
const KTYPE_INVOICE = "sales.pos_invoice";
const KTYPE_ITEM = "inventory.item";

const QUEUE_STORAGE_KEY = "kapp.pos.offline-queue";

interface ItemData {
  name?: string;
  sku?: string;
  barcode?: string;
  default_price?: number | string;
  default_warehouse_id?: string;
}

interface ProfileData {
  name?: string;
  warehouse_id?: string;
  currency?: string;
  default_customer_id?: string;
}

interface CartLine {
  itemId: string;
  itemName: string;
  qty: number;
  unitPrice: number;
}

interface QueuedInvoice {
  /** stable client-side id used as the Idempotency-Key on the
   *  finalize POST so replays after reconnect collapse to the
   *  same server-side outcome. */
  idempotencyKey: string;
  posInvoiceId: string;
  total: number;
  queuedAt: string;
}

/**
 * POSPage is the Phase M Task 6 storefront UX. It renders a
 * touch-friendly item grid, a cart, a barcode/SKU input for fast
 * scan-and-ring, and a finalize button that posts the cart through
 * the /api/v1/pos/invoices/{id}/finalize endpoint.
 *
 * Offline behaviour:
 *  - All finalize calls go through `attemptFinalize` which catches
 *    network errors and persists the pending invoice into a
 *    localStorage-backed queue (`kapp.pos.offline-queue`).
 *  - On reconnect (or whenever the page mounts) the queue is
 *    drained sequentially. Each retry reuses the original
 *    idempotency_key so the server collapses duplicates.
 */
export function POSPage() {
  const profilesQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_PROFILE],
    queryFn: () => api.listRecords(KTYPE_PROFILE),
  });
  const itemsQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_ITEM],
    queryFn: () => api.listRecords(KTYPE_ITEM),
  });

  const [profileId, setProfileId] = useState<string>("");
  const [cart, setCart] = useState<CartLine[]>([]);
  const [barcode, setBarcode] = useState("");
  const [tendered, setTendered] = useState("0");
  const [queue, setQueue] = useState<QueuedInvoice[]>(() => loadQueue());
  const [status, setStatus] = useState<string>("");

  const profile = useMemo(() => {
    if (!profilesQ.data) return null;
    if (profileId) return profilesQ.data.find((p) => p.id === profileId) ?? null;
    return profilesQ.data.find((p) => (p.data as ProfileData)?.name) ?? profilesQ.data[0] ?? null;
  }, [profileId, profilesQ.data]);

  const currency = (profile?.data as ProfileData)?.currency ?? "USD";

  const subtotal = cart.reduce((s, l) => s + l.qty * l.unitPrice, 0);
  const total = subtotal; // tax stub — real tax pack runs server-side

  // Drain the offline queue once on mount and whenever the network
  // flips back to online. Drains are best-effort; failures stay in
  // the queue and surface in the status strip so the cashier knows
  // there's pending work.
  useEffect(() => {
    let cancelled = false;
    const drain = async () => {
      // loadQueue() reads from localStorage so concurrent drains
      // (e.g. a stale 'online' listener firing while finalize is
      // also racing) all start from the same source-of-truth slice.
      const pending = loadQueue();
      if (pending.length === 0) return;
      const remaining: QueuedInvoice[] = [];
      for (const q of pending) {
        try {
          await api.finalizePOSInvoice(q.posInvoiceId, q.idempotencyKey);
          if (cancelled) return;
        } catch {
          remaining.push(q);
        }
      }
      if (cancelled) return;
      // Functional setQueue avoids stomping a sibling finalize that
      // appended to the queue between loadQueue() and now: keep any
      // ids in `prev` that aren't in the current `pending` slice and
      // merge them with `remaining`.
      setQueue((prev) => {
        const pendingIds = new Set(pending.map((p) => p.idempotencyKey));
        const appendedDuringDrain = prev.filter((p) => !pendingIds.has(p.idempotencyKey));
        const merged = [...remaining, ...appendedDuringDrain];
        saveQueue(merged);
        return merged;
      });
    };
    void drain();
    const onOnline = () => void drain();
    window.addEventListener("online", onOnline);
    return () => {
      cancelled = true;
      window.removeEventListener("online", onOnline);
    };
  }, []);

  const addByBarcode = () => {
    const code = barcode.trim();
    if (!code || !itemsQ.data) return;
    const match = itemsQ.data.find((i) => {
      const d = (i.data as ItemData) ?? {};
      return d.barcode === code || d.sku === code;
    });
    if (!match) {
      setStatus(`No item matching "${code}"`);
      return;
    }
    addToCart(match);
    setBarcode("");
  };

  const addToCart = (rec: KRecord) => {
    const data = (rec.data as ItemData) ?? {};
    const price = Number(data.default_price ?? 0);
    setCart((prev) => {
      const idx = prev.findIndex((l) => l.itemId === rec.id);
      if (idx >= 0) {
        const next = [...prev];
        next[idx] = { ...next[idx], qty: next[idx].qty + 1 };
        return next;
      }
      return [
        ...prev,
        {
          itemId: rec.id,
          itemName: data.name ?? data.sku ?? rec.id,
          qty: 1,
          unitPrice: price,
        },
      ];
    });
  };

  const finalize = async () => {
    if (!profile) {
      setStatus("Pick a POS profile first");
      return;
    }
    if (cart.length === 0) {
      setStatus("Cart is empty");
      return;
    }
    const idempotencyKey = crypto.randomUUID();
    const lines = cart.map((l) => ({
      item_id: l.itemId,
      qty: l.qty,
      unit_price: l.unitPrice,
      warehouse_id: (profile.data as ProfileData)?.warehouse_id,
    }));
    const tend = Number(tendered) || total;
    const draftBody = {
      profile_id: profile.id,
      lines,
      subtotal,
      total,
      tendered: tend,
      change_due: tend - total,
      currency,
      status: "draft",
      idempotency_key: idempotencyKey,
    };
    try {
      const created = await api.createRecord(KTYPE_INVOICE, draftBody);
      try {
        await api.finalizePOSInvoice(created.id, idempotencyKey);
        setStatus(`Finalized ${created.id}`);
        setCart([]);
        setTendered("0");
      } catch (err) {
        // Network or transient error — queue for replay. Functional
        // setQueue updater so a concurrent drain that ran between
        // this render and this catch can't overwrite the appended
        // entry with its stale closure value.
        const queued: QueuedInvoice = {
          idempotencyKey,
          posInvoiceId: created.id,
          total,
          queuedAt: new Date().toISOString(),
        };
        setQueue((prev) => {
          const next = [...prev, queued];
          saveQueue(next);
          return next;
        });
        setStatus(`Queued offline: ${(err as Error).message}`);
      }
    } catch (err) {
      setStatus(`Cart save failed: ${(err as Error).message}`);
    }
  };

  return (
    <section style={{ display: "grid", gridTemplateColumns: "2fr 1fr", gap: 16 }}>
      <div>
        <h1>Point of Sale</h1>
        <div style={{ marginBottom: 12 }}>
          <label>
            Profile:&nbsp;
            <select
              value={profileId || profile?.id || ""}
              onChange={(e) => setProfileId(e.target.value)}
            >
              {(profilesQ.data ?? []).map((p) => (
                <option key={p.id} value={p.id}>
                  {(p.data as ProfileData)?.name ?? p.id}
                </option>
              ))}
            </select>
          </label>
        </div>

        <div style={{ display: "flex", gap: 8, marginBottom: 12 }}>
          <input
            placeholder="Scan or type barcode/SKU…"
            value={barcode}
            onChange={(e) => setBarcode(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") addByBarcode();
            }}
            style={{ flex: 1, padding: 8, fontSize: 16 }}
          />
          <button onClick={addByBarcode} style={btnPrimary()}>
            Add
          </button>
        </div>

        <div style={itemGrid()}>
          {(itemsQ.data ?? []).slice(0, 24).map((rec) => {
            const data = (rec.data as ItemData) ?? {};
            return (
              <button
                key={rec.id}
                onClick={() => addToCart(rec)}
                style={itemTile()}
              >
                <div style={{ fontWeight: 600 }}>{data.name ?? rec.id}</div>
                <div style={{ fontSize: 12, color: "#6b7280" }}>{data.sku}</div>
                <div style={{ marginTop: 4 }}>
                  {currency} {Number(data.default_price ?? 0).toFixed(2)}
                </div>
              </button>
            );
          })}
        </div>
      </div>

      <aside style={{ borderLeft: "1px solid #e5e7eb", paddingLeft: 16 }}>
        <h2>Cart</h2>
        {cart.length === 0 ? (
          <p style={{ color: "#6b7280" }}>Empty.</p>
        ) : (
          <table style={{ width: "100%", fontSize: 14 }}>
            <thead>
              <tr>
                <th style={{ textAlign: "left" }}>Item</th>
                <th>Qty</th>
                <th>Price</th>
              </tr>
            </thead>
            <tbody>
              {cart.map((l) => (
                <tr key={l.itemId}>
                  <td>{l.itemName}</td>
                  <td style={{ textAlign: "center" }}>{l.qty}</td>
                  <td style={{ textAlign: "right" }}>
                    {(l.qty * l.unitPrice).toFixed(2)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <div style={{ marginTop: 12, fontSize: 18 }}>
          Total: {currency} {total.toFixed(2)}
        </div>
        <div style={{ marginTop: 8 }}>
          <label>
            Tendered:&nbsp;
            <input
              value={tendered}
              onChange={(e) => setTendered(e.target.value)}
              style={{ width: 100 }}
            />
          </label>
        </div>
        <button onClick={finalize} style={{ ...btnPrimary(), marginTop: 12, width: "100%", padding: "12px 16px", fontSize: 16 }}>
          Finalize
        </button>

        {queue.length > 0 && (
          <div style={{ marginTop: 16, padding: 8, background: "#fef3c7", borderRadius: 4 }}>
            <strong>Offline queue:</strong> {queue.length} pending
          </div>
        )}
        {status && (
          <div style={{ marginTop: 16, fontSize: 13, color: "#6b7280" }}>{status}</div>
        )}
      </aside>
    </section>
  );
}

function btnPrimary(): React.CSSProperties {
  return {
    padding: "8px 16px",
    background: "#2563eb",
    color: "white",
    border: "none",
    borderRadius: 4,
    cursor: "pointer",
    fontSize: 14,
  };
}

function itemGrid(): React.CSSProperties {
  return {
    display: "grid",
    gridTemplateColumns: "repeat(auto-fill, minmax(140px, 1fr))",
    gap: 8,
  };
}

function itemTile(): React.CSSProperties {
  return {
    padding: 12,
    border: "1px solid #e5e7eb",
    borderRadius: 6,
    background: "white",
    cursor: "pointer",
    textAlign: "left",
    minHeight: 80,
  };
}

function loadQueue(): QueuedInvoice[] {
  try {
    const raw = localStorage.getItem(QUEUE_STORAGE_KEY);
    if (!raw) return [];
    return JSON.parse(raw) as QueuedInvoice[];
  } catch {
    return [];
  }
}

function saveQueue(q: QueuedInvoice[]): void {
  try {
    localStorage.setItem(QUEUE_STORAGE_KEY, JSON.stringify(q));
  } catch {
    // best-effort — quota exceeded or storage disabled in private mode.
  }
}
