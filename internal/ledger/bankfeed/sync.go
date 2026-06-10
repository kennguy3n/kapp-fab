package bankfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// ActionTypeBankFeedSync is the scheduled_actions.action_type the bank
// feed sync registers under. The tenant onboarding seeds one hourly row
// of this type; the handler fans out across that tenant's active
// connections each tick.
const ActionTypeBankFeedSync = "bankfeed_sync"

// DefaultSyncLookback bounds the first sync of a fresh connection (one
// that has never synced) so we don't pull a provider's entire history.
// Subsequent syncs use the connection's persisted cursor / last_sync_at.
const DefaultSyncLookback = 90 * 24 * time.Hour

// AutoAcceptThreshold is the confidence at or above which an auto_approve
// rule lets the sync accept the matcher's top suggestion without human
// review. Below it, even an auto_approve rule only leaves a suggestion.
const AutoAcceptThreshold = 0.80

// The sync handler depends on its collaborators through narrow
// interfaces (rather than the concrete *ConnectionStore / *RuleStore /
// *ledger.SmartMatcher) so the full pipeline is unit-testable with
// in-memory fakes and no database. The concrete production types satisfy
// these by construction.

// txStore is the subset of *ledger.PGStore the sync handler needs to
// ingest feed lines.
type txStore interface {
	SyncBankTransactions(ctx context.Context, tenantID, bankAccountID uuid.UUID, lines []ledger.BankTransaction) ([]ledger.BankTransaction, error)
}

// connStore is the connection-lifecycle subset the handler drives.
type connStore interface {
	ListActiveConnections(ctx context.Context, tenantID uuid.UUID) ([]Connection, error)
	AdvanceCursor(ctx context.Context, tenantID, id uuid.UUID, cursor string, syncedAt time.Time) error
	MarkError(ctx context.Context, tenantID, id uuid.UUID, msg string) error
}

// ruleLister loads the priority-ordered rules applicable to an account.
type ruleLister interface {
	ListRules(ctx context.Context, tenantID uuid.UUID, bankAccountID *uuid.UUID) ([]Rule, error)
}

// suggester is the matcher subset used during sync.
type suggester interface {
	SuggestMatches(ctx context.Context, tenantID, txnID uuid.UUID, opts ledger.MatchOptions) ([]ledger.Suggestion, error)
	AcceptSuggestion(ctx context.Context, tenantID, suggestionID, actor uuid.UUID) (*ledger.Suggestion, error)
}

// SyncHandler is the scheduler.ActionHandler that pulls new transactions
// for every active connection of a tenant, ingests them idempotently,
// runs the auto-categorization rules, asks the smart matcher for
// suggestions, and auto-accepts confident matches that an auto_approve
// rule has green-lit.
//
// The handler is stateless and per-connection failures are
// log-and-continue: one revoked Plaid item must not stall the other
// feeds. A connection whose provider call fails has its last_error
// stamped and is skipped until the next tick.
type SyncHandler struct {
	conns    connStore
	rules    ruleLister
	registry *Registry
	store    txStore
	matcher  suggester
	now      func() time.Time
	lookback time.Duration
}

// NewSyncHandler wires the handler. All collaborators are required
// except matcher (nil disables suggestion generation, e.g. in a feed-
// only deployment).
func NewSyncHandler(conns *ConnectionStore, rules *RuleStore, registry *Registry, store txStore, matcher *ledger.SmartMatcher) *SyncHandler {
	// matcher is passed as a concrete *SmartMatcher; normalize a typed
	// nil to an untyped nil so the `h.matcher != nil` guard works.
	var m suggester
	if matcher != nil {
		m = matcher
	}
	var r ruleLister
	if rules != nil {
		r = rules
	}
	var c connStore
	if conns != nil {
		c = conns
	}
	return &SyncHandler{
		conns:    c,
		rules:    r,
		registry: registry,
		store:    store,
		matcher:  m,
		now:      func() time.Time { return time.Now().UTC() },
		lookback: DefaultSyncLookback,
	}
}

// newSyncHandlerForTest wires a handler directly from interfaces so unit
// tests can inject in-memory fakes. Production code uses NewSyncHandler.
func newSyncHandlerForTest(conns connStore, rules ruleLister, registry *Registry, store txStore, matcher suggester) *SyncHandler {
	return &SyncHandler{
		conns:    conns,
		rules:    rules,
		registry: registry,
		store:    store,
		matcher:  matcher,
		now:      func() time.Time { return time.Now().UTC() },
		lookback: DefaultSyncLookback,
	}
}

// WithClock pins the clock for deterministic tests.
func (h *SyncHandler) WithClock(now func() time.Time) *SyncHandler {
	if now != nil {
		h.now = now
	}
	return h
}

// WithLookback overrides the first-sync lookback window.
func (h *SyncHandler) WithLookback(d time.Duration) *SyncHandler {
	if d > 0 {
		h.lookback = d
	}
	return h
}

// Handle implements scheduler.ActionHandler.
func (h *SyncHandler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if h == nil || h.conns == nil || h.registry == nil || h.store == nil {
		return errors.New("bankfeed: sync handler not wired")
	}
	conns, err := h.conns.ListActiveConnections(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("bankfeed: list active connections: %w", err)
	}
	for i := range conns {
		conn := conns[i]
		if err := h.syncConnection(ctx, tenantID, &conn); err != nil {
			// Sanitize: provider errors may embed URLs but never tokens
			// (httpjson bounds bodies and we never log credentials).
			log.Printf("bankfeed: sync tenant=%s conn=%s provider=%s: %v",
				tenantID, conn.ID, conn.Provider, err)
			if mErr := h.conns.MarkError(ctx, tenantID, conn.ID, sanitizeErr(err)); mErr != nil {
				log.Printf("bankfeed: mark error tenant=%s conn=%s: %v", tenantID, conn.ID, mErr)
			}
			continue
		}
	}
	return nil
}

// SyncResult summarizes one connection's sync for callers that drive it
// directly (the manual "Sync now" route reuses this).
type SyncResult struct {
	Fetched     int
	Skipped     int // pending/unsettled lines deferred to a later sync
	Inserted    int
	Suggested   int
	AutoMatched int
	Cursor      string
}

// syncConnection runs the full pipeline for one connection.
func (h *SyncHandler) syncConnection(ctx context.Context, tenantID uuid.UUID, conn *Connection) error {
	_, err := h.SyncOne(ctx, tenantID, conn)
	return err
}

// SyncOne fetches, ingests, categorizes and matches a single
// connection's transactions and advances its cursor. Exposed so the
// manual sync route can call it and surface counts to the operator.
func (h *SyncHandler) SyncOne(ctx context.Context, tenantID uuid.UUID, conn *Connection) (*SyncResult, error) {
	provider, err := h.registry.Get(conn.Provider)
	if err != nil {
		return nil, fmt.Errorf("bankfeed: provider %q not registered: %w", conn.Provider, err)
	}
	since := h.now().Add(-h.lookback)
	if conn.LastSyncAt != nil && conn.LastSyncAt.After(since) {
		since = *conn.LastSyncAt
	}
	raw, cursor, err := provider.FetchTransactions(ctx, conn, since)
	if err != nil {
		return nil, fmt.Errorf("bankfeed: fetch transactions: %w", err)
	}
	res := &SyncResult{Fetched: len(raw), Cursor: cursor}

	lines := make([]ledger.BankTransaction, 0, len(raw))
	// Keep each settled line's original RawTransaction keyed by its resolved
	// external ref. bank_transactions has no Counterparty column, so rule
	// evaluation below must read it from here rather than reconstructing it
	// from the stored row (which would drop Counterparty and make
	// counterparty rules fall back to coarse Description matching).
	byRef := make(map[string]RawTransaction, len(raw))
	for i := range raw {
		rt := raw[i]
		// Skip not-yet-settled lines: a pending authorization carries a
		// provisional amount and a later sync delivers the settled version.
		// Ingesting the pending row would also burn its ExternalID in the
		// dedup index and block the settled line from ever landing.
		if rt.Pending {
			res.Skipped++
			continue
		}
		ref := rt.ExternalID
		if ref == "" {
			// Provider supplies no stable id (per RawTransaction.ExternalID):
			// fall back to a deterministic content hash so the same row
			// re-fetched on a later sync dedupes via the unique external_ref
			// index instead of duplicating.
			ref = contentHashRef(conn.BankAccountID, rt)
		}
		rt.ExternalID = ref
		byRef[ref] = rt
		lines = append(lines, ledger.BankTransaction{
			BankAccountID: conn.BankAccountID,
			ValueDate:     rt.ValueDate,
			Description:   rt.Description,
			Amount:        rt.Amount,
			Currency:      rt.Currency,
			ExternalRef:   ref,
		})
	}
	inserted, err := h.store.SyncBankTransactions(ctx, tenantID, conn.BankAccountID, lines)
	if err != nil {
		return nil, fmt.Errorf("bankfeed: ingest transactions: %w", err)
	}
	res.Inserted = len(inserted)

	// Load applicable rules once per connection (account-scoped + tenant-
	// wide), already priority-ordered.
	var rules []Rule
	if h.rules != nil {
		rules, err = h.rules.ListRules(ctx, tenantID, &conn.BankAccountID)
		if err != nil {
			return nil, fmt.Errorf("bankfeed: load rules: %w", err)
		}
	}

	if h.matcher != nil {
		for i := range inserted {
			ln := &inserted[i]
			// Prefer the original fetched line so counterparty rules see the
			// provider-supplied Counterparty (not persisted on the row). Fall
			// back to reconstructing from the stored line only if the ref is
			// somehow absent from the map.
			rawTxn, ok := byRef[ln.ExternalRef]
			if !ok {
				rawTxn = RawTransaction{
					ExternalID:  ln.ExternalRef,
					ValueDate:   ln.ValueDate,
					Description: ln.Description,
					Amount:      ln.Amount,
					Currency:    ln.Currency,
				}
			}
			match, hasRule := Evaluate(rules, rawTxn)
			suggestions, err := h.matcher.SuggestMatches(ctx, tenantID, ln.ID, ledger.MatchOptions{})
			if err != nil {
				log.Printf("bankfeed: suggest tenant=%s txn=%s: %v", tenantID, ln.ID, err)
				continue
			}
			res.Suggested += len(suggestions)
			// Only match.AutoApprove is consumed here by design. The rule's
			// TargetAccountCode / TargetCostCenter are categorization targets
			// for auto-*posting* a journal entry against an as-yet-unmatched
			// line — a separate, financially-sensitive capability (tax
			// treatment, posting idempotency, reversal) that is deliberately
			// out of scope for this reconciliation pipeline, which only pairs
			// lines with journal entries that already exist and never
			// fabricates one. The target fields remain first-class, validated,
			// audited rule configuration consumed by that follow-up poster.
			//
			// An auto_approve rule auto-accepts the top suggestion when it
			// clears the confidence bar, collapsing the common "known
			// payee" case to zero clicks while still gating on a real
			// ledger match.
			if hasRule && match.AutoApprove && len(suggestions) > 0 &&
				suggestions[0].Confidence >= AutoAcceptThreshold {
				if _, err := h.matcher.AcceptSuggestion(ctx, tenantID, suggestions[0].ID, uuid.Nil); err != nil {
					log.Printf("bankfeed: auto-accept tenant=%s sug=%s: %v", tenantID, suggestions[0].ID, err)
					continue
				}
				res.AutoMatched++
			}
		}
	}

	if err := h.conns.AdvanceCursor(ctx, tenantID, conn.ID, cursor, h.now()); err != nil {
		return nil, fmt.Errorf("bankfeed: advance cursor: %w", err)
	}
	return res, nil
}

// contentHashRef derives a stable dedup key for a provider line that
// carries no native id (RawTransaction.ExternalID empty). It hashes the
// line's natural fields so the same statement row re-fetched on a later
// sync collapses via the unique external_ref index rather than
// duplicating. The "ch:" prefix keeps it from colliding with a real
// provider id namespace.
func contentHashRef(bankAccountID uuid.UUID, rt RawTransaction) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		bankAccountID.String(),
		rt.ValueDate.UTC().Format(time.RFC3339),
		rt.Amount.String(),
		rt.Currency,
		rt.Description,
	}, "\x1f")))
	return "ch:" + hex.EncodeToString(sum[:])
}

// sanitizeErr trims an error to a bounded, credential-free message for
// persistence in last_error. The provider layer already keeps tokens out
// of error strings; this caps length so a verbose upstream body cannot
// bloat the column.
func sanitizeErr(err error) string {
	const maxLen = 500
	// Rune-safe truncation: the error string can embed a snippet of a
	// non-ASCII provider response, and this value is persisted to the
	// last_error TEXT column. Splitting a multi-byte rune would produce
	// invalid UTF-8 that Postgres rejects, dropping the MarkError write and
	// hiding the sync failure from operators.
	return truncateRunes(err.Error(), maxLen)
}
