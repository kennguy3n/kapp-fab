import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { Approval } from "@kapp/client";
import { api } from "../lib/api";

/**
 * ApprovalsPage lists pending approvals for the current user. Each row
 * shows the target record, the current step, and Approve / Reject
 * buttons that call POST /api/v1/approvals/{id}/decide. The underlying
 * mutation invalidates the ["approvals"] query so the table refreshes
 * automatically after each decision — if a decision satisfies step
 * quorum and advances the chain, the row disappears from the actor's
 * pending list on the next fetch.
 */
export function ApprovalsPage() {
  const qc = useQueryClient();
  const approvals = useQuery({
    queryKey: ["approvals"],
    queryFn: () => api.listApprovals(),
  });

  const decide = useMutation({
    mutationFn: ({ id, decision }: { id: string; decision: "approve" | "reject" }) =>
      api.decideApproval(id, decision),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["approvals"] });
    },
  });

  return (
    <section>
      <h1>Approvals</h1>
      <p style={{ color: "#6b7280" }}>
        Pending approvals for the current user appear here. Use the Approve /
        Reject buttons or run <code>/approve &lt;id&gt; approve</code> in KChat.
      </p>

      {approvals.isLoading && <p>Loading…</p>}
      {approvals.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load approvals: {(approvals.error as Error).message}
        </p>
      )}

      {approvals.data && approvals.data.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No pending approvals. You're all caught up.
        </p>
      )}

      {approvals.data && approvals.data.length > 0 && (
        <table
          style={{
            width: "100%",
            borderCollapse: "collapse",
            marginTop: 12,
            fontSize: 14,
          }}
        >
          <thead>
            <tr style={{ textAlign: "left", color: "#6b7280" }}>
              <Th>Record</Th>
              <Th>Record ID</Th>
              <Th>State</Th>
              <Th>Step</Th>
              <Th>Requested</Th>
              <Th>Actions</Th>
            </tr>
          </thead>
          <tbody>
            {approvals.data.map((a) => (
              <ApprovalRow
                key={a.id}
                approval={a}
                pending={decide.isPending}
                onDecide={(decision) => decide.mutate({ id: a.id, decision })}
              />
            ))}
          </tbody>
        </table>
      )}

      {decide.isError && (
        <p style={{ color: "#b91c1c" }}>
          Decision failed: {(decide.error as Error).message}
        </p>
      )}
    </section>
  );
}

function ApprovalRow({
  approval,
  pending,
  onDecide,
}: {
  approval: Approval;
  pending: boolean;
  onDecide: (decision: "approve" | "reject") => void;
}) {
  const stepLabel = `${approval.chain.current_step + 1} / ${approval.chain.steps.length}`;
  return (
    <tr style={{ borderTop: "1px solid #e5e7eb" }}>
      <Td>{approval.record_ktype}</Td>
      <Td>
        <code>{approval.record_id.slice(0, 8)}</code>
      </Td>
      <Td>{approval.state}</Td>
      <Td>{stepLabel}</Td>
      <Td>{new Date(approval.created_at).toLocaleString()}</Td>
      <Td>
        <div style={{ display: "flex", gap: 8 }}>
          <button disabled={pending} onClick={() => onDecide("approve")}>
            Approve
          </button>
          <button disabled={pending} onClick={() => onDecide("reject")}>
            Reject
          </button>
        </div>
      </Td>
    </tr>
  );
}

function Th({ children }: { children: React.ReactNode }) {
  return (
    <th style={{ padding: "6px 8px", fontWeight: 500, fontSize: 12 }}>{children}</th>
  );
}

function Td({ children }: { children: React.ReactNode }) {
  return <td style={{ padding: "8px" }}>{children}</td>;
}
