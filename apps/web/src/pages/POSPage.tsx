import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Badge,
  Button,
  Eyebrow,
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
} from "@kapp/ui";
import { Minus, Plus, ScanLine, ShoppingCart, Trash2 } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";

const KTYPE_PROFILE = "sales.pos_profile";
const KTYPE_INVOICE = "sales.pos_invoice";
const KTYPE_ITEM = "inventory.item";

const QUEUE_STORAGE_KEY = "kapp.pos.offline-queue";

// Quick-cash denominations offered as one-tap tender chips.
const QUICK_CASH = [5, 10, 20, 50, 100];

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
 * POSPage is the storefront register UX. It renders a touch-friendly
 * item grid, a cart with quantity steppers, a barcode/SKU input for
 * fast scan-and-ring, quick-cash tender chips with change due, and a
 * finalize button that posts the cart through the
 * /api/v1/pos/invoices/{id}/finalize endpoint.
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
  const fmt = useFormatter();
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
  const money = (n: number) => fmt.currency(n, currency, { currencyDisplay: "code" });

  const subtotal = cart.reduce((s, l) => s + l.qty * l.unitPrice, 0);
  const total = subtotal; // tax stub — real tax pack runs server-side
  const tenderedNum = Number(tendered) || 0;
  const changeDue = tenderedNum - total;

  // Drain the offline queue once on mount and whenever the network
  // flips back to online. Drains are best-effort; failures stay in
  // the queue and surface in the status strip so the cashier knows
  // there's pending work.
  useEffect(() => {
    let cancelled = false;
    const drain = async () => {
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
    setStatus("");
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

  const changeQty = (itemId: string, delta: number) => {
    setCart((prev) =>
      prev
        .map((l) => (l.itemId === itemId ? { ...l, qty: l.qty + delta } : l))
        .filter((l) => l.qty > 0),
    );
  };

  const removeLine = (itemId: string) =>
    setCart((prev) => prev.filter((l) => l.itemId !== itemId));

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
    <section className="grid grid-cols-1 gap-6 lg:grid-cols-[2fr_1fr]">
      <div className="flex flex-col gap-4">
        <header className="flex flex-col gap-1">
          <Eyebrow>Point of sale</Eyebrow>
          <h1 className="text-2xl font-semibold tracking-tight text-fg">Point of Sale</h1>
          <p className="text-sm text-fg-muted">
            Scan or tap products to build the sale, take payment, and finalize the receipt.
          </p>
        </header>

        <div className="flex flex-wrap items-end gap-3">
          <Field label="Register" className="w-56">
            <Select
              value={profileId || profile?.id || ""}
              onChange={(e) => setProfileId(e.target.value)}
            >
              {(profilesQ.data ?? []).map((p) => (
                <option key={p.id} value={p.id}>
                  {(p.data as ProfileData)?.name ?? p.id}
                </option>
              ))}
            </Select>
          </Field>

          <Field label="Scan barcode or SKU" className="flex-1 min-w-[220px]">
            <Input
              placeholder="Scan or type barcode/SKU…"
              value={barcode}
              onChange={(e) => setBarcode(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") addByBarcode();
              }}
              leadingAddon={<ScanLine className="h-4 w-4" aria-hidden="true" />}
            />
          </Field>
          <Button onClick={addByBarcode}>Add</Button>
        </div>

        {itemsQ.isLoading ? (
          <div className="grid grid-cols-[repeat(auto-fill,minmax(150px,1fr))] gap-3">
            {Array.from({ length: 8 }).map((_, i) => (
              <div
                key={i}
                className="h-24 animate-pulse rounded-xl border border-border bg-bg-muted"
              />
            ))}
          </div>
        ) : (
          <div className="grid grid-cols-[repeat(auto-fill,minmax(150px,1fr))] gap-3">
            {(itemsQ.data ?? []).slice(0, 24).map((rec) => {
              const data = (rec.data as ItemData) ?? {};
              return (
                <button
                  key={rec.id}
                  type="button"
                  onClick={() => addToCart(rec)}
                  className="flex min-h-24 flex-col justify-between rounded-xl border border-border bg-bg-elevated p-3 text-left shadow-sm transition-all hover:-translate-y-0.5 hover:border-accent hover:shadow-md active:translate-y-0 active:scale-[0.98] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
                >
                  <div className="flex flex-col">
                    <span className="font-semibold leading-snug text-fg">
                      {data.name ?? rec.id}
                    </span>
                    {data.sku && (
                      <span className="text-xs text-fg-subtle">{data.sku}</span>
                    )}
                  </div>
                  <span className="mt-2 font-medium tabular-nums text-fg">
                    {money(Number(data.default_price ?? 0))}
                  </span>
                </button>
              );
            })}
          </div>
        )}
      </div>

      <aside className="flex flex-col gap-3 rounded-xl border border-border bg-bg-subtle p-4">
        <div className="flex items-center justify-between">
          <h2 className="text-lg font-semibold text-fg">Cart</h2>
          {cart.length > 0 && (
            <Button variant="ghost" size="sm" onClick={() => setCart([])}>
              Clear
            </Button>
          )}
        </div>

        {cart.length === 0 ? (
          <EmptyState
            icon={<ShoppingCart aria-hidden="true" />}
            title="Your cart is empty"
            description="Scan a barcode or tap a product to start a sale."
          />
        ) : (
          <div className="overflow-hidden rounded-lg border border-border bg-bg-elevated">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Item</TableHead>
                  <TableHead className="text-center">Qty</TableHead>
                  <TableHead className="text-end">Amount</TableHead>
                  <TableHead className="w-10">
                    <span className="sr-only">Remove</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {cart.map((l) => (
                  <TableRow key={l.itemId}>
                    <TableCell>
                      <span className="font-medium text-fg">{l.itemName}</span>
                      <span className="block text-xs text-fg-subtle tabular-nums">
                        {money(l.unitPrice)} each
                      </span>
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center justify-center gap-1">
                        <Button
                          variant="outline"
                          size="sm"
                          aria-label={`Decrease ${l.itemName}`}
                          onClick={() => changeQty(l.itemId, -1)}
                        >
                          <Minus className="h-3 w-3" aria-hidden="true" />
                        </Button>
                        <span className="w-6 text-center tabular-nums text-fg">{l.qty}</span>
                        <Button
                          variant="outline"
                          size="sm"
                          aria-label={`Increase ${l.itemName}`}
                          onClick={() => changeQty(l.itemId, 1)}
                        >
                          <Plus className="h-3 w-3" aria-hidden="true" />
                        </Button>
                      </div>
                    </TableCell>
                    <TableCell className="text-end font-medium tabular-nums text-fg">
                      {money(l.qty * l.unitPrice)}
                    </TableCell>
                    <TableCell className="text-end">
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-label={`Remove ${l.itemName}`}
                        onClick={() => removeLine(l.itemId)}
                      >
                        <Trash2 className="h-4 w-4" aria-hidden="true" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}

        <dl className="flex flex-col gap-1 border-t border-border pt-3">
          <div className="flex items-center justify-between text-sm">
            <dt className="text-fg-muted">Subtotal</dt>
            <dd className="tabular-nums text-fg">{money(subtotal)}</dd>
          </div>
          <p className="flex items-center justify-between text-lg font-semibold text-fg">
            Total: {money(total)}
          </p>
        </dl>

        <Field label="Cash tendered">
          <Input
            inputMode="decimal"
            value={tendered}
            onChange={(e) => setTendered(e.target.value)}
            className="text-end tabular-nums"
          />
        </Field>

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => setTendered(total.toFixed(2))}>
            Exact
          </Button>
          {QUICK_CASH.map((amount) => (
            <Button
              key={amount}
              variant="outline"
              size="sm"
              onClick={() => setTendered(String(amount))}
            >
              {money(amount)}
            </Button>
          ))}
        </div>

        {cart.length > 0 && (
          <div
            className={`flex items-center justify-between rounded-lg px-3 py-2 text-sm font-medium ${
              changeDue >= 0 ? "bg-success/15 text-fg" : "bg-warning/15 text-fg"
            }`}
          >
            <span>{changeDue >= 0 ? "Change due" : "Balance due"}</span>
            <span className="tabular-nums">{money(Math.abs(changeDue))}</span>
          </div>
        )}

        <Button onClick={finalize} className="w-full" size="lg">
          Finalize sale
        </Button>

        {queue.length > 0 && (
          <div className="flex items-center gap-2 rounded-md bg-warning/15 p-2 text-sm text-fg">
            <Badge variant="warning" size="xs">
              {queue.length}
            </Badge>
            <span>
              {queue.length} pending {queue.length === 1 ? "sale" : "sales"} will sync when
              you’re back online
            </span>
          </div>
        )}
        {status && <p className="text-sm text-fg-muted" role="status">{status}</p>}
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
