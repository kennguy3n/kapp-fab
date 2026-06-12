import type { BankFeedRule } from "@kapp/client";
import {
  Badge,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";

// describeCondition renders a rule's matcher in operator language. Newer
// rules carry a compound `conditions` array; legacy rules carry the
// single condition_type/condition_value pair. Both are projected by the
// API, so handle whichever is populated.
function describeCondition(rule: BankFeedRule): string {
  if (rule.condition_type || rule.condition_value) {
    return `${rule.condition_type || "?"}: ${rule.condition_value || ""}`.trim();
  }
  return "—";
}

/**
 * ReconciliationRulesPanel makes the auto-categorization rules visible to
 * the operator so they understand which rules drive auto-matching: a line
 * an enabled, auto-approve rule fires on is reconciled without review,
 * while the rest land in the queue. Read-only here — rules are authored on
 * the Bank Feeds page.
 */
export function ReconciliationRulesPanel({
  rules,
  isLoading,
  isError,
  error,
}: {
  rules: BankFeedRule[];
  isLoading: boolean;
  isError: boolean;
  error?: unknown;
}) {
  const sorted = [...rules].sort((a, b) => a.priority - b.priority);
  return (
    <section className="flex flex-col gap-2" aria-label="Reconciliation rules">
      <h2 className="text-base font-semibold text-fg">Reconciliation rules</h2>
      <p className="text-sm text-fg-muted">
        Rules are evaluated in priority order; an enabled auto-approve rule
        reconciles a matching line automatically.
      </p>
      {isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {isError && (
        <p className="text-sm text-danger">{String(error ?? "Failed to load rules")}</p>
      )}
      {!isLoading && !isError && sorted.length === 0 && (
        <p className="text-sm text-fg-muted">
          No reconciliation rules configured.
        </p>
      )}
      {sorted.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-16">Priority</TableHead>
              <TableHead>Condition</TableHead>
              <TableHead>Target account</TableHead>
              <TableHead>Auto-approve</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sorted.map((rule) => (
              <TableRow key={rule.id}>
                <TableCell className="tabular-nums">{rule.priority}</TableCell>
                <TableCell>{describeCondition(rule)}</TableCell>
                <TableCell>{rule.target_account_code || "—"}</TableCell>
                <TableCell>
                  {rule.auto_approve ? (
                    <Badge variant="accent" size="xs">
                      Auto
                    </Badge>
                  ) : (
                    <span className="text-xs text-fg-muted">Review</span>
                  )}
                </TableCell>
                <TableCell>
                  <Badge
                    variant={rule.enabled ? "success" : "default"}
                    size="xs"
                  >
                    {rule.enabled ? "Enabled" : "Disabled"}
                  </Badge>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}
