import { useCallback, useEffect, useRef, useState } from "react";
import type { BankFeedSuggestion, KRecord } from "@kapp/client";
import { Badge, Button } from "@kapp/ui";
import {
  confidenceBand,
  formatConfidence,
  parseReasons,
  shortId,
  txnAmount,
  txnData,
  type RateMap,
  type SuggestionGroup,
} from "./reconciliation";
import { ReconciliationFxCell } from "./ReconciliationFxCell";
import { rt } from "./ReconciliationStrings";

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
  active,
  baseCurrency,
  rates,
  pendingIds,
  bulkPending,
  onAccept,
  onReject,
  cardRef,
}: {
  group: SuggestionGroup;
  txn: KRecord | undefined;
  active: boolean;
  baseCurrency?: string;
  rates: RateMap;
  pendingIds: Set<string>;
  bulkPending: boolean;
  onAccept: (s: BankFeedSuggestion) => void;
  onReject: (s: BankFeedSuggestion) => void;
  cardRef?: (el: HTMLLIElement | null) => void;
}) {
  const [showAlternatives, setShowAlternatives] = useState(false);
  const [best, ...alternatives] = group.suggestions;
  const d = txn ? txnData(txn) : undefined;

  return (
    <li
      ref={cardRef}
      aria-current={active ? "true" : undefined}
      className={`rounded-lg border bg-bg-subtle p-3 transition-colors ${
        active ? "border-accent ring-2 ring-(--focus-ring)" : "border-border"
      }`}
    >
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="min-w-0">
          <div className="font-medium text-fg">
            {d?.description || (
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
        {d && txn && (
          <div className="text-right">
            <ReconciliationFxCell
              amount={txnAmount(txn)}
              lineCurrency={d.currency}
              baseCurrency={baseCurrency}
              rates={rates}
            />
          </div>
        )}
      </div>

      <div className="mt-2 flex flex-col gap-2">
        {best && (
          <CandidateRow
            suggestion={best}
            primary
            pending={bulkPending || pendingIds.has(best.id)}
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
                  pending={bulkPending || pendingIds.has(s.id)}
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
 *
 * For throughput on a large queue the list is keyboard-drivable: the
 * container is focusable and ↑/↓ move the active card, A accepts the
 * active line's best candidate, R rejects it, and S skips to the next
 * line — so a bookkeeper can clear a statement without leaving the
 * keyboard.
 */
export function ReconciliationMatchQueue({
  groups,
  txnById,
  baseCurrency,
  rates,
  pendingIds,
  highConfidenceCount,
  bulkPending,
  onAccept,
  onReject,
  onAcceptAllHighConfidence,
}: {
  groups: SuggestionGroup[];
  txnById: Map<string, KRecord>;
  baseCurrency?: string;
  rates: RateMap;
  pendingIds: Set<string>;
  highConfidenceCount: number;
  bulkPending: boolean;
  onAccept: (s: BankFeedSuggestion) => void;
  onReject: (s: BankFeedSuggestion) => void;
  onAcceptAllHighConfidence: () => void;
}) {
  const [activeIndex, setActiveIndex] = useState(0);
  const cardRefs = useRef<(HTMLLIElement | null)[]>([]);

  // Keep the active index in range as the queue shrinks (lines get
  // matched / rejected out from under it).
  useEffect(() => {
    setActiveIndex((i) => {
      if (groups.length === 0) return 0;
      return Math.min(i, groups.length - 1);
    });
  }, [groups.length]);

  const move = useCallback(
    (delta: number) => {
      setActiveIndex((i) => {
        const next = Math.max(0, Math.min(groups.length - 1, i + delta));
        cardRefs.current[next]?.scrollIntoView({
          block: "nearest",
          behavior: "smooth",
        });
        return next;
      });
    },
    [groups.length],
  );

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLUListElement>) => {
      if (groups.length === 0) return;
      const group = groups[activeIndex];
      const best = group?.suggestions[0];
      switch (e.key) {
        case "ArrowDown":
          e.preventDefault();
          move(1);
          break;
        case "ArrowUp":
          e.preventDefault();
          move(-1);
          break;
        case "a":
        case "A":
          if (best && !bulkPending && !pendingIds.has(best.id)) {
            e.preventDefault();
            onAccept(best);
          }
          break;
        case "r":
        case "R":
          if (best && !bulkPending && !pendingIds.has(best.id)) {
            e.preventDefault();
            onReject(best);
          }
          break;
        case "s":
        case "S":
          e.preventDefault();
          move(1);
          break;
        default:
          break;
      }
    },
    [activeIndex, groups, move, onAccept, onReject, bulkPending, pendingIds],
  );

  return (
    <section className="flex flex-col gap-3" aria-label="Match review queue">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold text-fg">Match review queue</h2>
        <div className="flex items-center gap-2">
          {groups.length > 0 && (
            <span className="hidden text-xs text-fg-muted sm:inline">
              {rt("reconciliation.kbd.hint")}
            </span>
          )}
          <Button
            size="sm"
            disabled={highConfidenceCount === 0 || bulkPending}
            onClick={onAcceptAllHighConfidence}
          >
            Accept all high-confidence ({highConfidenceCount})
          </Button>
        </div>
      </header>
      {groups.length === 0 ? (
        <p className="text-sm text-fg-muted">
          {rt("reconciliation.suggestions.empty")}
        </p>
      ) : (
        <ul
          className="flex list-none flex-col gap-2 p-0 focus-visible:outline-none"
          tabIndex={0}
          role="listbox"
          aria-label="Match review queue"
          aria-activedescendant={
            groups[activeIndex]
              ? `match-group-${groups[activeIndex].transactionId}`
              : undefined
          }
          onKeyDown={onKeyDown}
        >
          {groups.map((g, idx) => (
            <GroupCard
              key={g.transactionId}
              group={g}
              txn={txnById.get(g.transactionId)}
              active={idx === activeIndex}
              baseCurrency={baseCurrency}
              rates={rates}
              pendingIds={pendingIds}
              bulkPending={bulkPending}
              onAccept={onAccept}
              onReject={onReject}
              cardRef={(el) => {
                cardRefs.current[idx] = el;
              }}
            />
          ))}
        </ul>
      )}
    </section>
  );
}
