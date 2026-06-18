import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { Badge, toast } from "@kapp/ui";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import {
  DOCUMENT_CONFIGS,
  DocumentBoard,
  DocumentDialog,
  StatusBadge,
  buildNameResolver,
  deriveTaxRate,
  invoiceOptions,
  itemOptions as toItemOptions,
  linesFromData,
  orgOptions,
  warehouseOptions,
  type DocumentSubmitPayload,
} from "../components/lineitems";

const KTYPE = "sales.return";
const config = DOCUMENT_CONFIGS.sales_return;

// Workflow states from internal/sales/returns.go.
const STAGES = ["requested", "approved", "received", "refunded", "cancelled"];

type Verb = "approve" | "receive" | "refund" | "cancel";

interface SalesReturnData {
  return_number?: string;
  customer_id?: string;
  original_invoice_id?: string;
  warehouse_id?: string;
  return_date?: string;
  reason?: string;
  total?: number | string;
  currency?: string;
  status?: string;
  credit_note_id?: string;
}

type DialogState = { mode: "create" } | { mode: "edit"; record: KRecord };

function dateInput(value: string | undefined): string {
  return value ? value.slice(0, 10) : "";
}

function today(): string {
  return new Date().toISOString().slice(0, 10);
}

export function SalesReturnsPage() {
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
  const invoicesQ = useQuery<KRecord[]>({
    queryKey: ["records", "finance.ar_invoice"],
    queryFn: () => api.listRecords("finance.ar_invoice"),
  });
  const warehousesQ = useQuery({
    queryKey: ["inventory", "warehouses"],
    queryFn: () => api.listInventoryWarehouses(),
  });

  const customerName = useMemo(() => buildNameResolver(orgsQ.data), [orgsQ.data]);
  const invoiceName = useMemo(
    () => buildNameResolver(invoicesQ.data, "invoice_number"),
    [invoicesQ.data],
  );
  const orgOpts = useMemo(() => orgOptions(orgsQ.data ?? []), [orgsQ.data]);
  const itemOpts = useMemo(() => toItemOptions(itemsQ.data ?? []), [itemsQ.data]);
  const invoiceOpts = useMemo(() => invoiceOptions(invoicesQ.data ?? []), [invoicesQ.data]);
  const warehouseOpts = useMemo(
    () => warehouseOptions(warehousesQ.data ?? []),
    [warehousesQ.data],
  );

  const fmtDate = (value?: string) => {
    if (!value) return "";
    const d = new Date(value.length === 10 ? `${value}T00:00:00` : value);
    return Number.isNaN(d.getTime()) ? "" : fmt.date(d);
  };

  const moveMutation = useMutation({
    mutationFn: async ({ r, to }: { r: KRecord; to: string }) => {
      const current = (r.data as unknown as SalesReturnData).status ?? "requested";
      if (current === to) return;
      const verb = resolveVerb(current, to);
      if (!verb) throw new Error(`A return can’t move straight to “${to}” from “${current}”.`);
      await api.runSalesReturnTransition(r.id, verb);
    },
    // Resync to the server's truth whether the transition succeeded
    // or was rejected, so a refused drop snaps the card back.
    onSettled: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
    onError: (e: Error) => toast.error("Couldn’t move return", { description: e.message }),
  });

  const saveMutation = useMutation({
    mutationFn: async (payload: DocumentSubmitPayload) => {
      if (dialog?.mode === "edit") {
        return api.updateRecord(KTYPE, dialog.record.id, {
          ...dialog.record.data,
          ...payload.data,
        });
      }
      return api.createRecord(KTYPE, { status: "requested", ...payload.data });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE] });
      toast.success(dialog?.mode === "edit" ? "Return updated" : "Return created");
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
        title="Sales Returns"
        description="Accept returned goods against an invoice, receive them back into stock, and issue the customer’s refund."
        newLabel="New return"
        onNew={openCreate}
        stages={STAGES}
        records={q.data}
        statusOf={(r) => (r.data as unknown as SalesReturnData).status ?? "requested"}
        isLoading={q.isLoading}
        isError={q.isError}
        error={q.error}
        onRetry={() => q.refetch()}
        onMove={(r, to) => moveMutation.mutate({ r, to })}
        onCardClick={openEdit}
        emptyTitle="No returns yet"
        emptyDescription="Log a return when a customer sends goods back against an invoice."
        renderCard={(r) => {
          const d = r.data as unknown as SalesReturnData;
          return (
            <div className="flex flex-col gap-1.5">
              <div className="flex items-start justify-between gap-2">
                <span className="font-medium text-fg">
                  {d.return_number || "Untitled return"}
                </span>
                <StatusBadge status={d.status ?? "requested"} size="xs" />
              </div>
              <span className="text-xs text-fg-muted">
                {customerName(d.customer_id) || "Unknown customer"}
              </span>
              {d.original_invoice_id && (
                <span className="text-xs text-fg-subtle">
                  Invoice {invoiceName(d.original_invoice_id) || "—"}
                </span>
              )}
              <div className="flex items-center justify-between text-xs">
                <span className="text-fg-muted">{fmtDate(d.return_date)}</span>
                <span className="font-medium tabular-nums text-fg">
                  {fmt.currency(Number(d.total ?? 0), d.currency ?? "USD", {
                    currencyDisplay: "code",
                  })}
                </span>
              </div>
              {d.credit_note_id && (
                <Badge variant="success" size="xs">
                  Credit note issued
                </Badge>
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
          title={dialog.mode === "edit" ? "Edit return" : "New return"}
          initialHeader={{
            original_invoice_id: editData ? String(editData.original_invoice_id ?? "") : "",
            customer_id: editData ? String(editData.customer_id ?? "") : "",
            warehouse_id: editData ? String(editData.warehouse_id ?? "") : "",
            return_date: editData ? dateInput(editData.return_date as string) : today(),
            return_number: editData ? String(editData.return_number ?? "") : "",
            reason: editData ? String(editData.reason ?? "") : "",
          }}
          initialLines={editData ? linesFromData("sales_return", editData) : []}
          initialCurrency={editData ? String(editData.currency ?? "USD") : "USD"}
          initialTaxRate={editData ? deriveTaxRate(editData) : 0}
          itemOptions={itemOpts}
          selectOptions={{
            original_invoice_id: invoiceOpts,
            customer_id: orgOpts,
            warehouse_id: warehouseOpts,
          }}
          saving={saveMutation.isPending}
          error={saveError}
          onSubmit={(payload) => saveMutation.mutate(payload)}
        />
      )}
    </>
  );
}

// resolveVerb maps a (from, to) status pair to the lifecycle verb
// that drives the ReturnPoster transition. Returns undefined for any
// pair the state machine doesn't permit.
function resolveVerb(from: string, to: string): Verb | undefined {
  if (from === "requested" && to === "approved") return "approve";
  if (from === "approved" && to === "received") return "receive";
  if (from === "received" && to === "refunded") return "refund";
  if (
    (from === "requested" || from === "approved" || from === "received") &&
    to === "cancelled"
  )
    return "cancel";
  return undefined;
}
