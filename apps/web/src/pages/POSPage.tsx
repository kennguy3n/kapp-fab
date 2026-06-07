import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
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
    <section className="grid grid-cols-[2fr_1fr] gap-4">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Point of Sale
        </h1>
        <div className="mt-3 mb-3">
          <label className="flex items-center gap-2 text-sm text-fg">
            Profile:
            <Select
              className="w-auto"
              value={profileId || profile?.id || ""}
              onChange={(e) => setProfileId(e.target.value)}
            >
              {(profilesQ.data ?? []).map((p) => (
                <option key={p.id} value={p.id}>
                  {(p.data as ProfileData)?.name ?? p.id}
                </option>
              ))}
            </Select>
          </label>
        </div>

        <div className="mb-3 flex gap-2">
          <Input
            placeholder="Scan or type barcode/SKU…"
            value={barcode}
            onChange={(e) => setBarcode(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") addByBarcode();
            }}
            className="flex-1"
          />
          <Button onClick={addByBarcode}>Add</Button>
        </div>

        <div className="grid grid-cols-[repeat(auto-fill,minmax(140px,1fr))] gap-2">
          {(itemsQ.data ?? []).slice(0, 24).map((rec) => {
            const data = (rec.data as ItemData) ?? {};
            return (
              <button
                key={rec.id}
                onClick={() => addToCart(rec)}
                className="flex min-h-20 flex-col rounded-md border border-border bg-bg-elevated p-3 text-left transition-colors hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
              >
                <div className="font-semibold text-fg">
                  {data.name ?? rec.id}
                </div>
                <div className="text-xs text-fg-muted">{data.sku}</div>
                <div className="mt-1 text-fg">
                  {currency} {Number(data.default_price ?? 0).toFixed(2)}
                </div>
              </button>
            );
          })}
        </div>
      </div>

      <aside className="border-l border-border pl-4">
        <h2 className="text-lg font-semibold text-fg">Cart</h2>
        {cart.length === 0 ? (
          <p className="text-fg-muted">Empty.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Item</TableHead>
                <TableHead className="text-center">Qty</TableHead>
                <TableHead className="text-right">Price</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {cart.map((l) => (
                <TableRow key={l.itemId}>
                  <TableCell>{l.itemName}</TableCell>
                  <TableCell className="text-center">{l.qty}</TableCell>
                  <TableCell className="text-right">
                    {(l.qty * l.unitPrice).toFixed(2)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <div className="mt-3 text-lg text-fg">
          Total: {currency} {total.toFixed(2)}
        </div>
        <div className="mt-2">
          <label className="flex items-center gap-2 text-sm text-fg">
            Tendered:
            <Input
              value={tendered}
              onChange={(e) => setTendered(e.target.value)}
              className="w-28"
            />
          </label>
        </div>
        <Button onClick={finalize} className="mt-3 w-full" size="lg">
          Finalize
        </Button>

        {queue.length > 0 && (
          <div className="mt-4 rounded-md bg-warning/15 p-2 text-sm text-fg">
            <strong>Offline queue:</strong> {queue.length} pending
          </div>
        )}
        {status && (
          <div className="mt-4 text-sm text-fg-muted">{status}</div>
        )}
      </aside>
    </section>
  );
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
