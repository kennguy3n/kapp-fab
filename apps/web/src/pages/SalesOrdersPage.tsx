import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { toast } from "@kapp/ui";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import {
  DOCUMENT_CONFIGS,
  DocumentBoard,
  DocumentDialog,
  StatusBadge,
  buildNameResolver,
  deriveTaxRate,
  itemOptions as toItemOptions,
  lineCount,
  linesFromData,
  orgOptions,
  type DocumentSubmitPayload,
} from "../components/lineitems";

const KTYPE = "sales.order";
const config = DOCUMENT_CONFIGS.sales_order;

// Workflow states from internal/sales/ktypes.go — kept local so the
// board doesn't round-trip to the registry for a stable constant.
const STAGES = ["draft", "confirmed", "fulfilled", "cancelled"];

interface SalesOrderData {
  order_number?: string;
  customer_id?: string;
  order_date?: string;
  total?: number | string;
  currency?: string;
  status?: string;
}

type DialogState = { mode: "create" } | { mode: "edit"; record: KRecord };

function dateInput(value: string | undefined): string {
  return value ? value.slice(0, 10) : "";
}

function today(): string {
  return new Date().toISOString().slice(0, 10);
}

export function SalesOrdersPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();
  const [dialog, setDialog] = useState<DialogState | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);

  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });
  const orgsQ = useQuery<KRecord[]>({
    queryKey: ["records", "crm.organization"],
    queryFn: () => api.listRecords("crm.organization"),
  });
  const itemsQ = useQuery<KRecord[]>({
    queryKey: ["records", "inventory.item"],
    queryFn: () => api.listRecords("inventory.item"),
  });

  const customerName = useMemo(() => buildNameResolver(orgsQ.data), [orgsQ.data]);
  const orgOpts = useMemo(() => orgOptions(orgsQ.data ?? []), [orgsQ.data]);
  const itemOpts = useMemo(() => toItemOptions(itemsQ.data ?? []), [itemsQ.data]);

  const fmtDate = (value?: string) => {
    if (!value) return "";
    const d = new Date(value.length === 10 ? `${value}T00:00:00` : value);
    return Number.isNaN(d.getTime()) ? "" : fmt.date(d);
  };

  const moveMutation = useMutation({
    mutationFn: async ({ r, to }: { r: KRecord; to: string }) => {
      const current = (r.data as unknown as SalesOrderData).status ?? "draft";
      if (current === to) return;
      // Always drive moves through the workflow action so backend
      // side-effects run; an edge the state machine doesn't expose is
      // rejected rather than patched straight onto status.
      const action = resolveAction(current, to);
      if (!action) {
        throw new Error(`An order can’t move straight to “${to}” from “${current}”.`);
      }
      await api.runAction(KTYPE, r.id, action);
    },
    // Resync to the server's truth whether the transition succeeded or
    // was rejected, so a refused drop snaps the card back.
    onSettled: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
    onError: (e: Error) => toast.error("Couldn’t move order", { description: e.message }),
  });

  const saveMutation = useMutation({
    mutationFn: async (payload: DocumentSubmitPayload) => {
      if (dialog?.mode === "edit") {
        return api.updateRecord(KTYPE, dialog.record.id, {
          ...dialog.record.data,
          ...payload.data,
        });
      }
      return api.createRecord(KTYPE, { status: "draft", ...payload.data });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE] });
      toast.success(dialog?.mode === "edit" ? "Order updated" : "Order created");
      setDialog(null);
      setSaveError(null);
    },
    onError: (e: Error) => setSaveError(e.message),
  });

  const openCreate = () => {
    setSaveError(null);
    setDialog({ mode: "create" });
  };
  const openEdit = (record: KRecord) => {
    setSaveError(null);
    setDialog({ mode: "edit", record });
  };

  const editData = dialog?.mode === "edit" ? (dialog.record.data as Record<string, unknown>) : null;

  return (
    <>
      <DocumentBoard
        eyebrow="Sales"
        title="Sales Orders"
        description="Turn confirmed deals into orders, then move them across the board as they’re confirmed and fulfilled."
        newLabel="New order"
        onNew={openCreate}
        stages={STAGES}
        records={q.data}
        statusOf={(r) => (r.data as unknown as SalesOrderData).status ?? "draft"}
        isLoading={q.isLoading}
        isError={q.isError}
        error={q.error}
        onRetry={() => q.refetch()}
        onMove={(r, to) => moveMutation.mutate({ r, to })}
        onCardClick={openEdit}
        emptyTitle="No sales orders yet"
        emptyDescription="Create your first order to start tracking fulfilment."
        renderCard={(r) => {
          const d = r.data as unknown as SalesOrderData;
          const count = lineCount(r.data);
          return (
            <div className="flex flex-col gap-1.5">
              <div className="flex items-start justify-between gap-2">
                <span className="font-medium text-fg">
                  {d.order_number || "Untitled order"}
                </span>
                <StatusBadge status={d.status ?? "draft"} size="xs" />
              </div>
              <span className="text-xs text-fg-muted">
                {customerName(d.customer_id) || "Unknown customer"}
              </span>
              <div className="flex items-center justify-between text-xs">
                <span className="text-fg-muted">{fmtDate(d.order_date)}</span>
                <span className="font-medium tabular-nums text-fg">
                  {fmt.currency(Number(d.total ?? 0), d.currency ?? "USD", {
                    currencyDisplay: "code",
                  })}
                </span>
              </div>
              {count > 0 && (
                <span className="text-xs text-fg-subtle">
                  {count} line{count === 1 ? "" : "s"}
                </span>
              )}
            </div>
          );
        }}
      />

      {dialog && (
        <DocumentDialog
          open
          onClose={() => setDialog(null)}
          mode={dialog.mode}
          config={config}
          title={dialog.mode === "edit" ? "Edit sales order" : "New sales order"}
          initialHeader={{
            customer_id: editData ? String(editData.customer_id ?? "") : "",
            order_date: editData ? dateInput(editData.order_date as string) : today(),
            delivery_date: editData ? dateInput(editData.delivery_date as string) : "",
            order_number: editData ? String(editData.order_number ?? "") : "",
          }}
          initialLines={editData ? linesFromData("sales_order", editData) : []}
          initialCurrency={editData ? String(editData.currency ?? "USD") : "USD"}
          initialTaxRate={editData ? deriveTaxRate(editData) : 0}
          itemOptions={itemOpts}
          selectOptions={{ customer_id: orgOpts }}
          saving={saveMutation.isPending}
          error={saveError}
          onSubmit={(payload) => saveMutation.mutate(payload)}
        />
      )}
    </>
  );
}

// resolveAction maps (from, to) stage pairs to the workflow action
// names declared in internal/sales/ktypes.go. Edges the state
// machine doesn't expose return undefined, and the caller rejects
// the move rather than patching status directly.
function resolveAction(from: string, to: string): string | undefined {
  if (from === "draft" && to === "confirmed") return "confirm";
  if (from === "confirmed" && to === "fulfilled") return "fulfil";
  if ((from === "draft" || from === "confirmed") && to === "cancelled") return "cancel";
  return undefined;
}
