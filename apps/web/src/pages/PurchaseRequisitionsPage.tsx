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
  itemOptions as toItemOptions,
  linesFromData,
  orgOptions,
  type DocumentSubmitPayload,
} from "../components/lineitems";

const KTYPE = "procurement.purchase_requisition";
const config = DOCUMENT_CONFIGS.purchase_requisition;

// Workflow states from internal/sales/requisition.go.
const STAGES = ["requested", "approved", "ordered", "cancelled"];

type Verb = "approve" | "convert" | "cancel";

interface RequisitionData {
  requisition_number?: string;
  requested_by?: string;
  department?: string;
  cost_center?: string;
  supplier_id?: string;
  request_date?: string;
  needed_by?: string;
  justification?: string;
  subtotal?: number | string;
  currency?: string;
  status?: string;
  po_id?: string;
}

type DialogState = { mode: "create" } | { mode: "edit"; record: KRecord };

function dateInput(value: string | undefined): string {
  return value ? value.slice(0, 10) : "";
}

function today(): string {
  return new Date().toISOString().slice(0, 10);
}

export function PurchaseRequisitionsPage() {
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

  const supplierName = useMemo(() => buildNameResolver(orgsQ.data), [orgsQ.data]);
  const orgOpts = useMemo(() => orgOptions(orgsQ.data ?? []), [orgsQ.data]);
  const itemOpts = useMemo(() => toItemOptions(itemsQ.data ?? []), [itemsQ.data]);

  const moveMutation = useMutation({
    mutationFn: async ({ r, to }: { r: KRecord; to: string }) => {
      const current = (r.data as unknown as RequisitionData).status ?? "requested";
      if (current === to) return;
      // No raw-status fallback: `convert` allocates a purchase order
      // on the backend, so a bare status patch would orphan the
      // requisition from its PO. Defer entirely to the state machine.
      const verb = resolveVerb(current, to);
      if (!verb) {
        throw new Error(`A requisition can’t move straight to “${to}” from “${current}”.`);
      }
      await api.runRequisitionTransition(r.id, verb);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
    onError: (e: Error) => toast.error("Couldn’t move requisition", { description: e.message }),
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
      toast.success(dialog?.mode === "edit" ? "Requisition updated" : "Requisition created");
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
        eyebrow="Procurement"
        title="Purchase Requisitions"
        description="Internal requests to buy. Approve a requisition, then convert it into a purchase order in one step."
        newLabel="New requisition"
        onNew={openCreate}
        stages={STAGES}
        records={q.data}
        statusOf={(r) => (r.data as unknown as RequisitionData).status ?? "requested"}
        isLoading={q.isLoading}
        isError={q.isError}
        error={q.error}
        onRetry={() => q.refetch()}
        onMove={(r, to) => moveMutation.mutate({ r, to })}
        onCardClick={openEdit}
        emptyTitle="No requisitions yet"
        emptyDescription="Raise a requisition when someone needs to buy stock or services."
        renderCard={(r) => {
          const d = r.data as unknown as RequisitionData;
          return (
            <div className="flex flex-col gap-1.5">
              <div className="flex items-start justify-between gap-2">
                <span className="font-medium text-fg">
                  {d.requisition_number || "Untitled requisition"}
                </span>
                <StatusBadge status={d.status ?? "requested"} size="xs" />
              </div>
              <span className="text-xs text-fg-muted">
                {d.requested_by ? `Requested by ${d.requested_by}` : "Requester not set"}
              </span>
              {d.department && (
                <span className="text-xs text-fg-subtle">{d.department}</span>
              )}
              {d.supplier_id && (
                <span className="text-xs text-fg-subtle">
                  {supplierName(d.supplier_id) || "Suggested supplier"}
                </span>
              )}
              <div className="flex items-center justify-between text-xs">
                <span className="text-fg-muted">&nbsp;</span>
                <span className="font-medium tabular-nums text-fg">
                  {fmt.currency(Number(d.subtotal ?? 0), d.currency ?? "USD", {
                    currencyDisplay: "code",
                  })}
                </span>
              </div>
              {d.po_id && (
                <Badge variant="success" size="xs">
                  Purchase order created
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
          title={dialog.mode === "edit" ? "Edit requisition" : "New requisition"}
          initialHeader={{
            requested_by: editData ? String(editData.requested_by ?? "") : "",
            request_date: editData ? dateInput(editData.request_date as string) : today(),
            needed_by: editData ? dateInput(editData.needed_by as string) : "",
            department: editData ? String(editData.department ?? "") : "",
            cost_center: editData ? String(editData.cost_center ?? "") : "",
            supplier_id: editData ? String(editData.supplier_id ?? "") : "",
            requisition_number: editData ? String(editData.requisition_number ?? "") : "",
            justification: editData ? String(editData.justification ?? "") : "",
          }}
          initialLines={editData ? linesFromData("purchase_requisition", editData) : []}
          initialCurrency={editData ? String(editData.currency ?? "USD") : "USD"}
          itemOptions={itemOpts}
          selectOptions={{ supplier_id: orgOpts }}
          saving={saveMutation.isPending}
          error={saveError}
          onSubmit={(payload) => saveMutation.mutate(payload)}
        />
      )}
    </>
  );
}

// resolveVerb maps a (from, to) status pair to the RequisitionPoster
// verb. Any drop into Cancelled resolves to `cancel` so the backend
// can return its specific rejection (e.g. ordered requisitions must
// be cancelled via their PO) rather than a generic frontend message.
function resolveVerb(from: string, to: string): Verb | undefined {
  if (from === "requested" && to === "approved") return "approve";
  if (from === "approved" && to === "ordered") return "convert";
  if (to === "cancelled") return "cancel";
  return undefined;
}
