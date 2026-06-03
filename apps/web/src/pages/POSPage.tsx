import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";
import { drainQueue, enqueue, listQueue } from "../lib/offlineQueue";

const KTYPE_PROFILE = "sales.pos_profile";
const KTYPE_INVOICE = "sales.pos_invoice";
const KTYPE_ITEM = "inventory.item";

// Mutation discriminator for the shared offline queue. POS finalize
// replays are stored alongside any other queued mutations in the same
// IndexedDB store but drained independently via this type tag.
const POS_MUTATION_TYPE = "pos.finalize";

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

/** Replay body for a queued POS finalize. The queue entry's `id` is
 *  the idempotency key reused on retry so duplicates collapse. */
interface POSFinalizePayload {
  posInvoiceId: string;
  total: number;
}

/**
 * POSPage is the Phase M Task 6 storefront UX. It renders a
 * touch-friendly item grid, a cart, a barcode/SKU input for fast
 * scan-and-ring, and a finalize button that posts the cart through
 * the /api/v1/pos/invoices/{id}/finalize endpoint.
 *
 * Offline behaviour:
 *  - A finalize call that fails on the network persists the pending
 *    invoice into the shared IndexedDB offline queue
 *    (src/lib/offlineQueue.ts), tagged with the `pos.finalize` type.
 *  - On reconnect (or whenever the page mounts) the queue is drained
 *    sequentially. Each retry reuses the original idempotency key (the
 *    queue entry id) so the server collapses duplicates.
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
  const [queueCount, setQueueCount] = useState(0);
  const [status, setStatus] = useState<string>("");

  const refreshQueueCount = useCallback(async () => {
    try {
      const pending = await listQueue(POS_MUTATION_TYPE);
      setQueueCount(pending.length);
    } catch {
      // IndexedDB unavailable (e.g. private mode) — leave count at 0.
    }
  }, []);

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
      try {
        // drainQueue removes each entry it successfully replays and
        // leaves failures in place; it reads the store fresh each call
        // so a stale 'online' listener racing a finalize still starts
        // from the canonical persisted slice.
        await drainQueue(async (mutation) => {
          const payload = mutation.payload as POSFinalizePayload;
          await api.finalizePOSInvoice(payload.posInvoiceId, mutation.id);
        }, POS_MUTATION_TYPE);
      } catch {
        // IndexedDB unavailable — nothing to drain.
      }
      if (!cancelled) await refreshQueueCount();
    };
    void drain();
    const onOnline = () => void drain();
    window.addEventListener("online", onOnline);
    return () => {
      cancelled = true;
      window.removeEventListener("online", onOnline);
    };
  }, [refreshQueueCount]);

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
        // Network or transient error — queue for replay. The entry id
        // is the idempotency key so the eventual replay collapses to
        // the same server-side outcome as this attempt.
        try {
          await enqueue({
            id: idempotencyKey,
            type: POS_MUTATION_TYPE,
            payload: { posInvoiceId: created.id, total } satisfies POSFinalizePayload,
            queuedAt: new Date().toISOString(),
          });
          await refreshQueueCount();
          setStatus(`Queued offline: ${(err as Error).message}`);
        } catch {
          setStatus(`Finalize failed and could not queue: ${(err as Error).message}`);
        }
      }
    } catch (err) {
      setStatus(`Cart save failed: ${(err as Error).message}`);
    }
  };

  return (
    <section className="grid grid-cols-1 gap-4 lg:grid-cols-[2fr_1fr]">
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

      <aside className="lg:border-l lg:border-border lg:pl-4">
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

        {queueCount > 0 && (
          <div style={{ marginTop: 16, padding: 8, background: "#fef3c7", borderRadius: 4 }}>
            <strong>Offline queue:</strong> {queueCount} pending
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


