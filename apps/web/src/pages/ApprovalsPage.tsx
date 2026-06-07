import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { Approval } from "@kapp/client";
import {
  Button,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
      <p className="text-fg-muted">
        Pending approvals for the current user appear here. Use the Approve /
        Reject buttons or run <code>/approve &lt;id&gt; approve</code> in KChat.
      </p>

      {approvals.isLoading && <p>Loading…</p>}
      {approvals.isError && (
        <p className="text-danger">
          Failed to load approvals: {(approvals.error as Error).message}
        </p>
      )}

      {approvals.data && approvals.data.length === 0 && (
        <p className="italic text-fg-subtle">
          No pending approvals. You're all caught up.
        </p>
      )}

      {approvals.data && approvals.data.length > 0 && (
        <Table className="mt-3 text-sm">
          <TableHeader>
            <TableRow>
              <TableHead>Record</TableHead>
              <TableHead>Record ID</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Step</TableHead>
              <TableHead>Requested</TableHead>
              <TableHead>Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {approvals.data.map((a) => (
              <ApprovalRow
                key={a.id}
                approval={a}
                pending={decide.isPending}
                onDecide={(decision) => decide.mutate({ id: a.id, decision })}
              />
            ))}
          </TableBody>
        </Table>
      )}

      {decide.isError && (
        <p className="text-danger">
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
    <TableRow>
      <TableCell>{approval.record_ktype}</TableCell>
      <TableCell>
        <code>{approval.record_id.slice(0, 8)}</code>
      </TableCell>
      <TableCell>{approval.state}</TableCell>
      <TableCell>{stepLabel}</TableCell>
      <TableCell>{new Date(approval.created_at).toLocaleString()}</TableCell>
      <TableCell>
        <div className="flex gap-2">
          <Button size="sm" disabled={pending} onClick={() => onDecide("approve")}>
            Approve
          </Button>
          <Button size="sm" variant="destructive" disabled={pending} onClick={() => onDecide("reject")}>
            Reject
          </Button>
        </div>
      </TableCell>
    </TableRow>
  );
}
