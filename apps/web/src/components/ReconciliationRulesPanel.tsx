import type { BankFeedRule } from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { rt, ruleConditionLabel } from "./ReconciliationStrings";

// describeCondition renders a rule's matcher in plain operator language,
// humanising the stored condition_type token (e.g. "description_contains"
// → "Description contains") so the panel never surfaces a raw enum value.
function describeCondition(rule: BankFeedRule): string {
  if (!rule.condition_type && !rule.condition_value) return "—";
  const label = ruleConditionLabel(rule.condition_type);
  if (!label) return rule.condition_value;
  return rule.condition_value ? `${label}: ${rule.condition_value}` : label;
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
  onRetry,
}: {
  rules: BankFeedRule[];
  isLoading: boolean;
  isError: boolean;
  onRetry?: () => void;
}) {
  const sorted = [...rules].sort((a, b) => a.priority - b.priority);
  return (
    <section className="flex flex-col gap-2" aria-label="Reconciliation rules">
      <div className="flex items-center gap-2">
        <h2 className="text-base font-semibold text-fg">Reconciliation rules</h2>
        {sorted.length > 0 && (
          <Badge variant="neutral" size="xs">
            {sorted.length}
          </Badge>
        )}
      </div>
      <p className="text-sm text-fg-muted">
        Rules are evaluated in priority order; an enabled auto-approve rule
        reconciles a matching line automatically.
      </p>

      {isLoading && (
        <div className="flex flex-col gap-2" aria-hidden="true">
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-3/4" />
        </div>
      )}

      {isError && (
        <div className="flex flex-col items-start gap-2 rounded-lg border border-border bg-bg-subtle p-4">
          <p className="text-sm text-danger">
            We couldn't load your reconciliation rules.
          </p>
          {onRetry && (
            <Button variant="outline" size="sm" onClick={onRetry}>
              {rt("reconciliation.retry")}
            </Button>
          )}
        </div>
      )}

      {!isLoading && !isError && sorted.length === 0 && (
        <EmptyState
          title="No reconciliation rules yet"
          description="Create rules on the Bank Feeds page to auto-categorize and reconcile recurring lines without review."
        />
      )}

      {!isLoading && !isError && sorted.length > 0 && (
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
                <TableCell>
                  {rule.target_account_code ? (
                    <code className="rounded bg-bg-muted px-1.5 py-0.5 text-xs">
                      {rule.target_account_code}
                    </code>
                  ) : (
                    <span className="text-fg-subtle">—</span>
                  )}
                </TableCell>
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
                    variant={rule.enabled ? "success" : "neutral"}
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
