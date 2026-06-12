import { useState } from "react";
import type { BankFeedSuggestion, KRecord } from "@kapp/client";
import { Badge, Button } from "@kapp/ui";
import {
  confidenceBand,
  formatAmount,
  formatConfidence,
  parseReasons,
  shortId,
  txnData,
  type SuggestionGroup,
} from "./reconciliation";

function ConfidenceBadge({ confidence }: { confidence: number }) {
  const band = confidenceBand(confidence);
  const variant =
    band === "high" ? "success" : band === "medium" ? "warning" : "default";
  return (
    <Badge variant={variant} title={`${band} confidence`}>
      {formatConfidence(confidence)}
    </Badge>
  );
}

// ReasonChips renders the matcher's "why" — the comma-joined reason
// string ("exact amount, same-day, learned counterparty") as discrete
// chips so the operator can see at a glance which signals fired.
function ReasonChips({ reason }: { reason: string }) {
  const reasons = parseReasons(reason);
  if (reasons.length === 0) {
    return <span className="text-xs text-fg-muted">No reason recorded</span>;
  }
  return (
    <div className="flex flex-wrap gap-1">
      {reasons.map((r) => (
        <Badge key={r} variant="outline" size="xs">
          {r}
        </Badge>
      ))}
    </div>
  );
}

function CandidateRow({
  suggestion,
  primary,
  pending,
  onAccept,
  onReject,
}: {
  suggestion: BankFeedSuggestion;
  primary: boolean;
  pending: boolean;
  onAccept: (s: BankFeedSuggestion) => void;
  onReject: (s: BankFeedSuggestion) => void;
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-bg px-3 py-2">
      <div className="min-w-0 flex flex-col gap-1">
        <div className="flex items-center gap-2">
          <ConfidenceBadge confidence={suggestion.confidence} />
          <span
            className="font-mono text-xs text-fg-muted"
            title={suggestion.journal_entry_id}
          >
            Journal entry {shortId(suggestion.journal_entry_id)}
          </span>
          {primary && (
            <Badge variant="accent" size="xs">
              Best match
            </Badge>
          )}
        </div>
        <ReasonChips reason={suggestion.match_reason} />
      </div>
      <div className="flex gap-1">
        <Button
          size="sm"
          disabled={pending}
          onClick={() => onAccept(suggestion)}
        >
          Accept
        </Button>
        <Button
          size="sm"
          variant="ghost"
          disabled={pending}
          onClick={() => onReject(suggestion)}
        >
          Reject
        </Button>
      </div>
    </div>
  );
}

function GroupCard({
  group,
  txn,
  pendingIds,
  onAccept,
  onReject,
}: {
  group: SuggestionGroup;
  txn: KRecord | undefined;
  pendingIds: Set<string>;
  onAccept: (s: BankFeedSuggestion) => void;
  onReject: (s: BankFeedSuggestion) => void;
}) {
  const [showAlternatives, setShowAlternatives] = useState(false);
  const [best, ...alternatives] = group.suggestions;
  const d = txn ? txnData(txn) : undefined;

  return (
    <li className="rounded-lg border border-border bg-bg-subtle p-3">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="min-w-0">
          <div className="font-medium text-fg">
            {d?.description ?? (
              <span
                className="font-mono text-sm"
                title={group.transactionId}
              >
                Transaction {shortId(group.transactionId)}
              </span>
            )}
          </div>
          <div className="text-xs text-fg-muted">{d?.value_date ?? ""}</div>
        </div>
        {d && (
          <div className="text-right font-semibold tabular-nums text-fg">
            {formatAmount(
              typeof d.amount === "number" ? d.amount : Number(d.amount ?? 0),
              d.currency,
            )}
          </div>
        )}
      </div>

      <div className="mt-2 flex flex-col gap-2">
        {best && (
          <CandidateRow
            suggestion={best}
            primary
            pending={pendingIds.has(best.id)}
            onAccept={onAccept}
            onReject={onReject}
          />
        )}

        {alternatives.length > 0 && (
          <>
            <Button
              size="sm"
              variant="outline"
              className="self-start"
              aria-expanded={showAlternatives}
              onClick={() => setShowAlternatives((v) => !v)}
            >
              {showAlternatives
                ? "Hide alternatives"
                : `Find alternative (${alternatives.length})`}
            </Button>
            {showAlternatives &&
              alternatives.map((s) => (
                <CandidateRow
                  key={s.id}
                  suggestion={s}
                  primary={false}
                  pending={pendingIds.has(s.id)}
                  onAccept={onAccept}
                  onReject={onReject}
                />
              ))}
          </>
        )}
      </div>
    </li>
  );
}

/**
 * ReconciliationMatchQueue is the smart-matcher review inbox: one card per
 * unmatched bank line carrying its candidate journal entries (highest
 * confidence first), the reasons each was suggested, and per-candidate
 * accept / reject. Lines with more than one candidate fold the rest behind
 * a "find alternative" toggle. The header carries the accept-all-high-
 * confidence bulk action.
 */
export function ReconciliationMatchQueue({
  groups,
  txnById,
  pendingIds,
  highConfidenceCount,
  bulkPending,
  onAccept,
  onReject,
  onAcceptAllHighConfidence,
}: {
  groups: SuggestionGroup[];
  txnById: Map<string, KRecord>;
  pendingIds: Set<string>;
  highConfidenceCount: number;
  bulkPending: boolean;
  onAccept: (s: BankFeedSuggestion) => void;
  onReject: (s: BankFeedSuggestion) => void;
  onAcceptAllHighConfidence: () => void;
}) {
  return (
    <section className="flex flex-col gap-3" aria-label="Match review queue">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold text-fg">Match review queue</h2>
        <Button
          size="sm"
          disabled={highConfidenceCount === 0 || bulkPending}
          onClick={onAcceptAllHighConfidence}
        >
          Accept all high-confidence ({highConfidenceCount})
        </Button>
      </header>
      {groups.length === 0 ? (
        <p className="text-sm text-fg-muted">
          No match suggestions to review.
        </p>
      ) : (
        <ul className="flex list-none flex-col gap-2 p-0">
          {groups.map((g) => (
            <GroupCard
              key={g.transactionId}
              group={g}
              txn={txnById.get(g.transactionId)}
              pendingIds={pendingIds}
              onAccept={onAccept}
              onReject={onReject}
            />
          ))}
        </ul>
      )}
    </section>
  );
}
