package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// matcher.go implements the smart reconciliation engine layered on top
// of the conservative exact-match reconciler in bank.go. Where
// ReconcileTransaction only auto-pairs a single unambiguous candidate,
// the SmartMatcher scores every plausible journal entry on a 0..1
// confidence scale and records the candidates as bank_match_suggestions
// for the operator to accept/reject. Accepting a suggestion both
// reconciles the line and feeds the historical-pattern learner so the
// same counterparty is recognised next month.
//
// Confidence model (capped at 1.0):
//   exact amount        +0.40   (within tolerance scales down to +0.25)
//   date proximity      +0.20   (same day full, linearly to 0 at window edge)
//   description overlap  +0.20   (similarity * 0.20)
//   historical match    +0.20   (learner maps this counterparty to the
//                                 candidate's category account)

// Suggestion statuses, mirroring the bank_match_suggestions CHECK.
const (
	SuggestionSuggested = "suggested"
	SuggestionAccepted  = "accepted"
	SuggestionRejected  = "rejected"
)

// Suggestion-lifecycle sentinels let the HTTP/agent layers map a routine
// client condition to the right status code instead of a 500. A missing
// suggestion id is a 404; acting on a suggestion that has already been
// decided (or whose transaction is no longer unreconciled) is a 409
// conflict, not a server fault — so neither pollutes the error log.
var (
	// ErrSuggestionNotFound is returned when the suggestion id does not
	// exist under the tenant's RLS scope.
	ErrSuggestionNotFound = errors.New("ledger: suggestion not found")
	// ErrSuggestionConflict is returned when the suggestion exists but is
	// no longer actionable (already accepted/rejected, or its transaction
	// has since been reconciled by another path).
	ErrSuggestionConflict = errors.New("ledger: suggestion no longer actionable")
	// ErrSplitInvalid is returned when a split-reconcile request is
	// malformed in a way the operator can correct: no legs, duplicate
	// journal entries, a leg in a different currency than the bank line,
	// or legs that do not sum to the line's amount. It is a 422 (the
	// request is well-formed JSON but semantically unprocessable), never a
	// server fault — the ledger re-validates every split server-side and
	// refuses to persist an unbalanced reconciliation.
	ErrSplitInvalid = errors.New("ledger: split allocation invalid")
)

// SplitAmountTolerance is the absolute slack allowed between the sum of a
// split's legs and the bank line's amount. Half a cent absorbs the
// rounding an operator's per-line figures can carry while still rejecting
// a genuinely unbalanced split. It mirrors the web composer's gate so the
// client and server agree on what "balanced" means.
var SplitAmountTolerance = decimal.RequireFromString("0.005")

// SplitLeg is one allocation of a bank line to a journal entry. Amount is
// the signed partial figure in the bank line's currency; SuggestionID
// optionally ties the leg back to a ranked suggestion so it is marked
// accepted (and its siblings collapsed) just like a single accept.
type SplitLeg struct {
	SuggestionID   uuid.UUID
	JournalEntryID uuid.UUID
	Amount         decimal.Decimal
}

// DefaultMinConfidence is the floor below which a candidate is not worth
// surfacing as a suggestion. 0.5 keeps the queue signal-rich (an exact
// amount alone clears it; weak description-only coincidences do not).
const DefaultMinConfidence = 0.5

// MatchOptions tune the candidate search and scoring for one matcher
// run. Zero values fall back to safe defaults so callers can pass an
// empty struct.
type MatchOptions struct {
	// Window is the ± date window around the statement line in which a
	// journal entry is considered a candidate. Defaults to
	// DefaultMatchWindow (±7d).
	Window time.Duration
	// AmountTolerance is the per-account absolute amount slack (e.g.
	// 0.05 for ±5¢ to absorb FX rounding). Zero means exact-only.
	AmountTolerance decimal.Decimal
	// MinConfidence is the persistence threshold. Defaults to
	// DefaultMinConfidence.
	MinConfidence float64
}

func (o MatchOptions) window() time.Duration {
	if o.Window <= 0 {
		return DefaultMatchWindow
	}
	return o.Window
}

func (o MatchOptions) minConfidence() float64 {
	if o.MinConfidence <= 0 {
		return DefaultMinConfidence
	}
	return o.MinConfidence
}

// Suggestion is one scored match candidate.
type Suggestion struct {
	ID             uuid.UUID `json:"id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	TransactionID  uuid.UUID `json:"transaction_id"`
	JournalEntryID uuid.UUID `json:"journal_entry_id"`
	Confidence     float64   `json:"confidence"`
	MatchReason    string    `json:"match_reason"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

// SmartMatcher owns the suggestion lifecycle and learned-pattern store.
// It reuses the parent PGStore's pool and auditor so it shares the same
// tenant-scoped transaction discipline.
type SmartMatcher struct {
	store *PGStore
	now   func() time.Time
}

// NewSmartMatcher builds a matcher bound to a ledger store.
func NewSmartMatcher(store *PGStore) *SmartMatcher {
	return &SmartMatcher{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock pins the clock for deterministic tests.
func (m *SmartMatcher) WithClock(now func() time.Time) *SmartMatcher {
	if now != nil {
		m.now = now
	}
	return m
}

// candidate is an in-flight match candidate loaded from the ledger.
type candidate struct {
	entryID     uuid.UUID
	postedAt    time.Time
	memo        string
	lineAmount  decimal.Decimal
	accountCode string
}

// SuggestMatches scores journal-entry candidates for one unreconciled
// statement line and upserts them as bank_match_suggestions. It returns
// the persisted suggestions ordered by confidence (desc). Lines that are
// already matched/ignored return nil with no error so a re-sync is a
// cheap no-op.
func (m *SmartMatcher) SuggestMatches(ctx context.Context, tenantID, txnID uuid.UUID, opts MatchOptions) ([]Suggestion, error) {
	if tenantID == uuid.Nil || txnID == uuid.Nil {
		return nil, errors.New("ledger: tenant_id and txn_id required")
	}
	var out []Suggestion
	err := dbutil.WithTenantTx(ctx, m.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			amount        decimal.Decimal
			valueDate     time.Time
			currency      string
			status        string
			description   string
			bankAccountID uuid.UUID
		)
		if err := tx.QueryRow(ctx,
			`SELECT amount, value_date, currency, status, COALESCE(description,''), bank_account_id
			   FROM bank_transactions WHERE tenant_id = $1 AND id = $2`,
			tenantID, txnID,
		).Scan(&amount, &valueDate, &currency, &status, &description, &bankAccountID); err != nil {
			return fmt.Errorf("ledger: load bank_transaction: %w", err)
		}
		if status != BankTxnUnreconciled {
			return nil // nothing to do
		}
		cands, err := m.loadCandidates(ctx, tx, tenantID, amount, valueDate, currency, opts)
		if err != nil {
			return err
		}
		learned, err := m.loadLearned(ctx, tx, tenantID, bankAccountID, description)
		if err != nil {
			return err
		}
		scored := scoreCandidates(amount, valueDate, description, cands, learned, opts)
		for _, sg := range scored {
			if sg.Confidence < opts.minConfidence() {
				continue
			}
			persisted, err := m.upsertSuggestionTx(ctx, tx, tenantID, txnID, sg.entryID, sg.Confidence, sg.reason)
			if err != nil {
				return err
			}
			out = append(out, *persisted)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// loadCandidates finds journal entries within the date window whose line
// amount is within tolerance of the (absolute) statement amount, in the
// same currency. It returns one candidate per (entry, matching line) so
// the scorer sees the category account_code directly.
func (m *SmartMatcher) loadCandidates(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, amount decimal.Decimal, valueDate time.Time, currency string, opts MatchOptions) ([]candidate, error) {
	abs := amount.Abs()
	lo := abs.Sub(opts.AmountTolerance.Abs())
	// Clamp at zero: journal line amounts are non-negative, so a negative
	// lower bound (tolerance exceeding a tiny statement amount) would widen
	// the BETWEEN to match every small debit/credit rather than the intended
	// near-amount band. Zero keeps the band correct without spurious hits.
	if lo.IsNegative() {
		lo = decimal.Zero
	}
	hi := abs.Add(opts.AmountTolerance.Abs())
	w := opts.window()
	rows, err := tx.Query(ctx,
		`SELECT je.id, je.posted_at, COALESCE(je.memo,''), jl.account_code,
		        CASE WHEN jl.debit > 0 THEN jl.debit ELSE jl.credit END AS line_amount
		   FROM journal_entries je
		   JOIN journal_lines jl ON jl.tenant_id = je.tenant_id AND jl.entry_id = je.id
		  WHERE je.tenant_id = $1
		    AND je.posted_at BETWEEN $2 AND $3
		    AND jl.currency = $4
		    AND (jl.debit BETWEEN $5 AND $6 OR jl.credit BETWEEN $5 AND $6)`,
		tenantID, valueDate.Add(-w), valueDate.Add(w), currency, lo, hi,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: scan candidates: %w", err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.entryID, &c.postedAt, &c.memo, &c.accountCode, &c.lineAmount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scoredCandidate carries a candidate plus its computed confidence and
// human-readable reason.
type scoredCandidate struct {
	entryID    uuid.UUID
	Confidence float64
	reason     string
}

// scoreCandidates ranks candidates by the confidence model. When several
// lines of the same entry qualify, the entry keeps its highest-scoring
// line so an entry is never double-suggested. The learned map is
// account_code -> hit_count for the statement description.
func scoreCandidates(amount decimal.Decimal, valueDate time.Time, description string, cands []candidate, learned map[string]int, opts MatchOptions) []scoredCandidate {
	best := map[uuid.UUID]scoredCandidate{}
	abs := amount.Abs()
	for _, c := range cands {
		conf, reason := scoreOne(abs, valueDate, description, c, learned, opts)
		if cur, ok := best[c.entryID]; !ok || conf > cur.Confidence {
			best[c.entryID] = scoredCandidate{entryID: c.entryID, Confidence: conf, reason: reason}
		}
	}
	out := make([]scoredCandidate, 0, len(best))
	for _, sc := range best {
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].entryID.String() < out[j].entryID.String()
	})
	return out
}

// scoreOne computes the confidence for a single candidate line.
func scoreOne(abs decimal.Decimal, valueDate time.Time, description string, c candidate, learned map[string]int, opts MatchOptions) (score float64, reason string) {
	var reasons []string

	diff := c.lineAmount.Sub(abs).Abs()
	switch {
	case diff.IsZero():
		score += 0.40
		reasons = append(reasons, "exact amount")
	case diff.LessThanOrEqual(opts.AmountTolerance.Abs()) && opts.AmountTolerance.IsPositive():
		score += 0.25
		reasons = append(reasons, "amount within tolerance")
	}

	dayDiff := mathAbs(valueDate.Sub(c.postedAt).Hours()) / 24.0
	windowDays := opts.window().Hours() / 24.0
	if windowDays > 0 && dayDiff <= windowDays {
		proximity := 0.20 * (1.0 - dayDiff/windowDays)
		score += proximity
		if dayDiff < 1 {
			reasons = append(reasons, "same-day")
		} else {
			reasons = append(reasons, fmt.Sprintf("%.0f days apart", dayDiff))
		}
	}

	sim := DescriptionSimilarity(description, c.memo)
	if sim > 0 {
		score += 0.20 * sim
		if sim >= 0.6 {
			reasons = append(reasons, "description match")
		}
	}

	if learned[c.accountCode] > 0 {
		score += 0.20
		reasons = append(reasons, "learned counterparty")
	}

	if score > 1.0 {
		score = 1.0
	}
	return score, strings.Join(reasons, ", ")
}

// mathAbs avoids importing math just for Abs on a float64.
func mathAbs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// loadLearned returns account_code -> hit_count for the normalized
// description of the statement line, scoped to the bank account.
func (m *SmartMatcher) loadLearned(ctx context.Context, tx pgx.Tx, tenantID, bankAccountID uuid.UUID, description string) (map[string]int, error) {
	key := DescriptionKey(description)
	if key == "" {
		return map[string]int{}, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT account_code, hit_count FROM bank_learned_matches
		  WHERE tenant_id = $1 AND bank_account_id = $2 AND description_key = $3`,
		tenantID, bankAccountID, key)
	if err != nil {
		return nil, fmt.Errorf("ledger: load learned matches: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var code string
		var hits int
		if err := rows.Scan(&code, &hits); err != nil {
			return nil, err
		}
		out[code] = hits
	}
	return out, rows.Err()
}

// upsertSuggestionTx writes (or re-ranks) one open suggestion inside the
// caller's transaction.
func (m *SmartMatcher) upsertSuggestionTx(ctx context.Context, tx pgx.Tx, tenantID, txnID, entryID uuid.UUID, confidence float64, reason string) (*Suggestion, error) {
	id := uuid.New()
	now := m.now()
	var outID uuid.UUID
	var createdAt time.Time
	err := tx.QueryRow(ctx,
		`INSERT INTO bank_match_suggestions
		     (tenant_id, id, transaction_id, journal_entry_id, confidence, match_reason, status, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'suggested',$7,$7)
		 ON CONFLICT (tenant_id, transaction_id, journal_entry_id) WHERE status = 'suggested'
		 DO UPDATE SET confidence = EXCLUDED.confidence, match_reason = EXCLUDED.match_reason, updated_at = EXCLUDED.updated_at
		 RETURNING id, created_at`,
		tenantID, id, txnID, entryID, decimal.NewFromFloat(confidence), nullIfEmpty(reason), now,
	).Scan(&outID, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("ledger: upsert suggestion: %w", err)
	}
	return &Suggestion{
		ID:             outID,
		TenantID:       tenantID,
		TransactionID:  txnID,
		JournalEntryID: entryID,
		Confidence:     confidence,
		MatchReason:    reason,
		Status:         SuggestionSuggested,
		CreatedAt:      createdAt,
	}, nil
}

// ListSuggestions returns open suggestions for a bank account's
// transactions, highest confidence first. Used by the suggestions API
// and the bulk-accept UI.
func (m *SmartMatcher) ListSuggestions(ctx context.Context, tenantID, bankAccountID uuid.UUID) ([]Suggestion, error) {
	var out []Suggestion
	err := dbutil.WithTenantTx(ctx, m.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT s.id, s.tenant_id, s.transaction_id, s.journal_entry_id, s.confidence,
			        COALESCE(s.match_reason,''), s.status, s.created_at
			   FROM bank_match_suggestions s
			   JOIN bank_transactions t ON t.tenant_id = s.tenant_id AND t.id = s.transaction_id
			  WHERE s.tenant_id = $1 AND t.bank_account_id = $2 AND s.status = 'suggested'
			  ORDER BY s.confidence DESC, s.created_at`,
			tenantID, bankAccountID)
		if err != nil {
			return fmt.Errorf("ledger: list suggestions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var s Suggestion
			var conf decimal.Decimal
			if err := rows.Scan(&s.ID, &s.TenantID, &s.TransactionID, &s.JournalEntryID, &conf,
				&s.MatchReason, &s.Status, &s.CreatedAt); err != nil {
				return err
			}
			s.Confidence, _ = conf.Float64()
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AcceptSuggestion reconciles the transaction against the suggested
// journal entry, marks the suggestion accepted, rejects the transaction's
// other open suggestions, and feeds the learner. Everything happens in
// one tenant-scoped transaction with an audit entry. actor attributes the
// decision in the audit trail.
func (m *SmartMatcher) AcceptSuggestion(ctx context.Context, tenantID, suggestionID, actor uuid.UUID) (*Suggestion, error) {
	if tenantID == uuid.Nil || suggestionID == uuid.Nil {
		return nil, errors.New("ledger: tenant_id and suggestion_id required")
	}
	var accepted *Suggestion
	err := dbutil.WithTenantTx(ctx, m.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var s Suggestion
		var conf decimal.Decimal
		if err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, transaction_id, journal_entry_id, confidence, COALESCE(match_reason,''), status, created_at
			   FROM bank_match_suggestions WHERE tenant_id = $1 AND id = $2`,
			tenantID, suggestionID,
		).Scan(&s.ID, &s.TenantID, &s.TransactionID, &s.JournalEntryID, &conf, &s.MatchReason, &s.Status, &s.CreatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("ledger: suggestion %s: %w", suggestionID, ErrSuggestionNotFound)
			}
			return fmt.Errorf("ledger: load suggestion: %w", err)
		}
		if s.Status != SuggestionSuggested {
			return fmt.Errorf("ledger: suggestion already %s: %w", s.Status, ErrSuggestionConflict)
		}
		s.Confidence, _ = conf.Float64()

		now := m.now()
		// Reconcile the line.
		if ct, err := tx.Exec(ctx,
			`UPDATE bank_transactions SET status = $3, matched_entry_id = $4
			  WHERE tenant_id = $1 AND id = $2 AND status = $5`,
			tenantID, s.TransactionID, BankTxnMatched, s.JournalEntryID, BankTxnUnreconciled,
		); err != nil {
			return fmt.Errorf("ledger: reconcile via suggestion: %w", err)
		} else if ct.RowsAffected() == 0 {
			return fmt.Errorf("ledger: transaction no longer unreconciled: %w", ErrSuggestionConflict)
		}
		// Mark this suggestion accepted and any sibling open suggestions
		// rejected so the queue collapses to the decided state.
		if _, err := tx.Exec(ctx,
			`UPDATE bank_match_suggestions SET status = $3, updated_at = $4
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, suggestionID, SuggestionAccepted, now); err != nil {
			return fmt.Errorf("ledger: accept suggestion: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE bank_match_suggestions SET status = $4, updated_at = $5
			  WHERE tenant_id = $1 AND transaction_id = $2 AND id <> $3 AND status = $6`,
			tenantID, s.TransactionID, suggestionID, SuggestionRejected, now, SuggestionSuggested); err != nil {
			return fmt.Errorf("ledger: reject siblings: %w", err)
		}
		if err := m.learnFromMatchTx(ctx, tx, tenantID, s.TransactionID, s.JournalEntryID, now); err != nil {
			return err
		}
		s.Status = SuggestionAccepted
		accepted = &s
		return m.auditSuggestion(ctx, tx, tenantID, &actor, s, "finance.bank_feed.suggestion.accept")
	})
	if err != nil {
		return nil, err
	}
	return accepted, nil
}

// AcceptSplit reconciles one bank line against several journal entries in a
// single tenant-scoped transaction, recording each leg's partial amount in
// bank_transaction_allocations. It is the multi-entry counterpart to
// AcceptSuggestion: the operator allocates the line across >1 entries and
// the split is persisted only when it nets to the line's amount.
//
// Every invariant the web composer surfaces is RE-VALIDATED here — the
// server never trusts a client-sent balance for a financial mutation:
//   - at least one leg, with distinct journal entries
//   - every journal entry exists under the tenant's RLS scope
//   - sum(leg.Amount) == line.amount within SplitAmountTolerance
//   - (currency is implicitly the line's; legs carry no separate currency,
//     so a split cannot silently mix currencies)
//
// On success the line moves to 'matched' with matched_entry_id left NULL
// (the allocations table is the source of truth for a split); any chosen
// suggestion is marked accepted and the line's other open suggestions are
// collapsed to rejected, mirroring AcceptSuggestion. The learner is fed
// once per entry and an audit row is written.
func (m *SmartMatcher) AcceptSplit(ctx context.Context, tenantID, txnID uuid.UUID, legs []SplitLeg, actor uuid.UUID) (*BankTransaction, error) {
	if tenantID == uuid.Nil || txnID == uuid.Nil {
		return nil, errors.New("ledger: tenant_id and transaction_id required")
	}
	if len(legs) == 0 {
		return nil, fmt.Errorf("ledger: split needs at least one allocation: %w", ErrSplitInvalid)
	}
	seen := make(map[uuid.UUID]struct{}, len(legs))
	sum := decimal.Zero
	for _, leg := range legs {
		if leg.JournalEntryID == uuid.Nil {
			return nil, fmt.Errorf("ledger: split leg missing journal_entry_id: %w", ErrSplitInvalid)
		}
		if _, dup := seen[leg.JournalEntryID]; dup {
			return nil, fmt.Errorf("ledger: split entry %s allocated twice: %w", leg.JournalEntryID, ErrSplitInvalid)
		}
		seen[leg.JournalEntryID] = struct{}{}
		sum = sum.Add(leg.Amount)
	}

	var out *BankTransaction
	err := dbutil.WithTenantTx(ctx, m.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			line    BankTransaction
			amount  decimal.Decimal
			matched *uuid.UUID
			extern  *string
			status  string
		)
		if err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, bank_account_id, value_date, COALESCE(description,''),
			        amount, currency, status, matched_entry_id, external_ref
			   FROM bank_transactions WHERE tenant_id = $1 AND id = $2`,
			tenantID, txnID,
		).Scan(&line.ID, &line.TenantID, &line.BankAccountID, &line.ValueDate, &line.Description,
			&amount, &line.Currency, &status, &matched, &extern); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("ledger: transaction %s: %w", txnID, ErrSuggestionNotFound)
			}
			return fmt.Errorf("ledger: load transaction: %w", err)
		}
		if status != BankTxnUnreconciled {
			return fmt.Errorf("ledger: transaction already %s: %w", status, ErrSuggestionConflict)
		}
		// Net-zero check: the legs must reconstruct the line's amount. A
		// split that leaves a residual is exactly what we must refuse.
		if sum.Sub(amount).Abs().GreaterThan(SplitAmountTolerance) {
			return fmt.Errorf("ledger: split sums to %s, expected %s: %w", sum, amount, ErrSplitInvalid)
		}
		// Every entry must exist under the tenant. The RLS policy already
		// scopes the read, so a cross-tenant id simply isn't visible here.
		for _, leg := range legs {
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM journal_entries WHERE tenant_id = $1 AND id = $2)`,
				tenantID, leg.JournalEntryID).Scan(&exists); err != nil {
				return fmt.Errorf("ledger: verify entry: %w", err)
			}
			if !exists {
				return fmt.Errorf("ledger: journal entry %s: %w", leg.JournalEntryID, ErrSuggestionNotFound)
			}
		}

		now := m.now()
		// Reconcile the line. Guarded on status so two concurrent splits (or
		// a split racing a single accept) cannot both win.
		if ct, err := tx.Exec(ctx,
			`UPDATE bank_transactions SET status = $3, matched_entry_id = NULL
			  WHERE tenant_id = $1 AND id = $2 AND status = $4`,
			tenantID, txnID, BankTxnMatched, BankTxnUnreconciled,
		); err != nil {
			return fmt.Errorf("ledger: reconcile via split: %w", err)
		} else if ct.RowsAffected() == 0 {
			return fmt.Errorf("ledger: transaction no longer unreconciled: %w", ErrSuggestionConflict)
		}

		var actorID *uuid.UUID
		if actor != uuid.Nil {
			a := actor
			actorID = &a
		}
		for _, leg := range legs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO bank_transaction_allocations
				    (tenant_id, id, transaction_id, journal_entry_id, amount, created_by, created_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)
				 ON CONFLICT (tenant_id, transaction_id, journal_entry_id)
				 DO UPDATE SET amount = EXCLUDED.amount, created_by = EXCLUDED.created_by, created_at = EXCLUDED.created_at`,
				tenantID, uuid.New(), txnID, leg.JournalEntryID, leg.Amount, actorID, now,
			); err != nil {
				return fmt.Errorf("ledger: insert allocation: %w", err)
			}
			// Accept the leg's suggestion if one was cited; ignore a stale
			// id (already decided) rather than fail the whole split. Scope to
			// transaction_id so a cited suggestion belonging to a *different*
			// bank line can never be flipped to accepted by this split — that
			// would strand the other line unreconciled with an accepted
			// suggestion no longer in its review queue.
			if leg.SuggestionID != uuid.Nil {
				if _, err := tx.Exec(ctx,
					`UPDATE bank_match_suggestions SET status = $3, updated_at = $4
					  WHERE tenant_id = $1 AND id = $2 AND status = $5 AND transaction_id = $6`,
					tenantID, leg.SuggestionID, SuggestionAccepted, now, SuggestionSuggested, txnID); err != nil {
					return fmt.Errorf("ledger: accept split suggestion: %w", err)
				}
			}
			if err := m.learnFromMatchTx(ctx, tx, tenantID, txnID, leg.JournalEntryID, now); err != nil {
				return err
			}
		}
		// Collapse any remaining open suggestions for this line so the queue
		// reflects the decided state.
		if _, err := tx.Exec(ctx,
			`UPDATE bank_match_suggestions SET status = $3, updated_at = $4
			  WHERE tenant_id = $1 AND transaction_id = $2 AND status = $5`,
			tenantID, txnID, SuggestionRejected, now, SuggestionSuggested); err != nil {
			return fmt.Errorf("ledger: collapse suggestions: %w", err)
		}

		line.Amount = amount
		line.Status = BankTxnMatched
		line.MatchedEntryID = nil
		if extern != nil {
			line.ExternalRef = *extern
		}
		out = &line
		return m.auditSplit(ctx, tx, tenantID, actorID, txnID, len(legs))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// auditSplit records a single audit entry for a split reconciliation,
// keyed on the bank transaction (not a suggestion, since a split may span
// several or none).
func (m *SmartMatcher) auditSplit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, txnID uuid.UUID, legs int) error {
	if m.store.auditor == nil {
		return nil
	}
	id := txnID
	actorKind := audit.ActorUser
	if actor == nil || *actor == uuid.Nil {
		actorKind = audit.ActorSystem
		actor = nil
	}
	// Record how many legs the split cleared so a forensic review can tell
	// a 2-way split from a 5-way one without re-reading the allocations.
	auditCtx, _ := json.Marshal(map[string]any{"legs": legs})
	return m.store.auditor.LogTx(ctx, tx, audit.Entry{
		TenantID:    tenantID,
		ActorID:     actor,
		ActorKind:   actorKind,
		Action:      "finance.bank_feed.transaction.split",
		TargetKType: "finance.bank_transaction",
		TargetID:    &id,
		Context:     auditCtx,
	})
}

// RejectSuggestion marks a single suggestion rejected without touching
// the transaction. Emits an audit entry.
func (m *SmartMatcher) RejectSuggestion(ctx context.Context, tenantID, suggestionID, actor uuid.UUID) error {
	if tenantID == uuid.Nil || suggestionID == uuid.Nil {
		return errors.New("ledger: tenant_id and suggestion_id required")
	}
	return dbutil.WithTenantTx(ctx, m.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE bank_match_suggestions SET status = $3, updated_at = $4
			  WHERE tenant_id = $1 AND id = $2 AND status = $5`,
			tenantID, suggestionID, SuggestionRejected, m.now(), SuggestionSuggested)
		if err != nil {
			return fmt.Errorf("ledger: reject suggestion: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// The status-guarded UPDATE matched nothing: either the id is
			// absent (→ 404) or it exists but is already decided (→ 409).
			// A cheap existence probe disambiguates so the caller gets the
			// correct status instead of a generic error.
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM bank_match_suggestions WHERE tenant_id = $1 AND id = $2)`,
				tenantID, suggestionID).Scan(&exists); err != nil {
				return fmt.Errorf("ledger: reject suggestion probe: %w", err)
			}
			if exists {
				return fmt.Errorf("ledger: suggestion %s already decided: %w", suggestionID, ErrSuggestionConflict)
			}
			return fmt.Errorf("ledger: suggestion %s: %w", suggestionID, ErrSuggestionNotFound)
		}
		id := suggestionID
		return m.auditSuggestion(ctx, tx, tenantID, &actor,
			Suggestion{ID: id, TenantID: tenantID}, "finance.bank_feed.suggestion.reject")
	})
}

// learnFromMatchTx records the (description -> category account)
// association for a reconciled transaction so future lines with the same
// counterparty are nudged toward the same account. The category account
// is the entry's largest-magnitude line that is NOT the bank account's
// own GL code; if every line is the bank code, learning is skipped.
func (m *SmartMatcher) learnFromMatchTx(ctx context.Context, tx pgx.Tx, tenantID, txnID, entryID uuid.UUID, now time.Time) error {
	var (
		description   string
		bankAccountID uuid.UUID
		bankCode      string
	)
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(t.description,''), t.bank_account_id, a.account_code
		   FROM bank_transactions t
		   JOIN bank_accounts a ON a.tenant_id = t.tenant_id AND a.id = t.bank_account_id
		  WHERE t.tenant_id = $1 AND t.id = $2`,
		tenantID, txnID,
	).Scan(&description, &bankAccountID, &bankCode); err != nil {
		return fmt.Errorf("ledger: load txn for learning: %w", err)
	}
	key := DescriptionKey(description)
	if key == "" {
		return nil
	}
	var category string
	if err := tx.QueryRow(ctx,
		`SELECT account_code FROM journal_lines
		  WHERE tenant_id = $1 AND entry_id = $2 AND account_code <> $3
		  ORDER BY GREATEST(debit, credit) DESC LIMIT 1`,
		tenantID, entryID, bankCode,
	).Scan(&category); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing meaningful to learn
		}
		return fmt.Errorf("ledger: pick category account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO bank_learned_matches (tenant_id, bank_account_id, description_key, account_code, hit_count, last_seen_at)
		 VALUES ($1,$2,$3,$4,1,$5)
		 ON CONFLICT (tenant_id, bank_account_id, description_key, account_code)
		 DO UPDATE SET hit_count = bank_learned_matches.hit_count + 1, last_seen_at = EXCLUDED.last_seen_at`,
		tenantID, bankAccountID, key, category, now,
	); err != nil {
		return fmt.Errorf("ledger: upsert learned match: %w", err)
	}
	return nil
}

func (m *SmartMatcher) auditSuggestion(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, s Suggestion, action string) error {
	if m.store.auditor == nil {
		return nil
	}
	id := s.ID
	actorKind := audit.ActorUser
	if actor == nil || *actor == uuid.Nil {
		actorKind = audit.ActorSystem
		actor = nil
	}
	return m.store.auditor.LogTx(ctx, tx, audit.Entry{
		TenantID:    tenantID,
		ActorID:     actor,
		ActorKind:   actorKind,
		Action:      action,
		TargetKType: "finance.bank_match_suggestion",
		TargetID:    &id,
	})
}

// ---------------------------------------------------------------------------
// Recurring detection
// ---------------------------------------------------------------------------

// RecurringGroup is a set of transactions that share a normalized
// description and approximately equal amount on a regular cadence.
type RecurringGroup struct {
	DescriptionKey string          `json:"description_key"`
	SampleLabel    string          `json:"sample_label"`
	Cadence        string          `json:"cadence"` // weekly | monthly | irregular
	Count          int             `json:"count"`
	AverageAmount  decimal.Decimal `json:"average_amount"`
	TransactionIDs []uuid.UUID     `json:"transaction_ids"`
}

// DetectRecurring groups a bank account's transactions by normalized
// description and classifies each group's cadence from the median gap
// between consecutive value dates. Groups of fewer than minOccurrences
// are dropped. The result drives the "recurring" UI section's bulk-match
// action.
func (m *SmartMatcher) DetectRecurring(ctx context.Context, tenantID, bankAccountID uuid.UUID, minOccurrences int) ([]RecurringGroup, error) {
	if minOccurrences < 2 {
		minOccurrences = 2
	}
	type row struct {
		id   uuid.UUID
		date time.Time
		desc string
		amt  decimal.Decimal
	}
	var rows []row
	err := dbutil.WithTenantTx(ctx, m.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rs, err := tx.Query(ctx,
			`SELECT id, value_date, COALESCE(description,''), amount
			   FROM bank_transactions
			  WHERE tenant_id = $1 AND bank_account_id = $2
			  ORDER BY value_date`,
			tenantID, bankAccountID)
		if err != nil {
			return fmt.Errorf("ledger: load txns for recurring: %w", err)
		}
		defer rs.Close()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.id, &r.date, &r.desc, &r.amt); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	})
	if err != nil {
		return nil, err
	}

	type bucket struct {
		label string
		dates []time.Time
		ids   []uuid.UUID
		sum   decimal.Decimal
	}
	buckets := map[string]*bucket{}
	for _, r := range rows {
		key := DescriptionKey(r.desc)
		if key == "" {
			continue
		}
		b := buckets[key]
		if b == nil {
			b = &bucket{label: r.desc}
			buckets[key] = b
		}
		b.dates = append(b.dates, r.date)
		b.ids = append(b.ids, r.id)
		b.sum = b.sum.Add(r.amt)
	}

	out := make([]RecurringGroup, 0, len(buckets))
	for key, b := range buckets {
		if len(b.ids) < minOccurrences {
			continue
		}
		out = append(out, RecurringGroup{
			DescriptionKey: key,
			SampleLabel:    b.label,
			Cadence:        classifyCadence(b.dates),
			Count:          len(b.ids),
			AverageAmount:  b.sum.Div(decimal.NewFromInt(int64(len(b.ids)))),
			TransactionIDs: b.ids,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].DescriptionKey < out[j].DescriptionKey
	})
	return out, nil
}

// classifyCadence labels the median inter-arrival gap between sorted
// dates as weekly (~7d), monthly (~28-31d) or irregular.
func classifyCadence(dates []time.Time) string {
	if len(dates) < 2 {
		return "irregular"
	}
	sorted := append([]time.Time(nil), dates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	gaps := make([]float64, 0, len(sorted)-1)
	for i := 1; i < len(sorted); i++ {
		gaps = append(gaps, sorted[i].Sub(sorted[i-1]).Hours()/24.0)
	}
	sort.Float64s(gaps)
	median := gaps[len(gaps)/2]
	switch {
	case median >= 5 && median <= 9:
		return "weekly"
	case median >= 25 && median <= 35:
		return "monthly"
	case median >= 13 && median <= 16:
		return "biweekly"
	default:
		return "irregular"
	}
}

// ---------------------------------------------------------------------------
// Pure string-similarity helpers (exported for reuse + unit testing)
// ---------------------------------------------------------------------------

// DescriptionSimilarity blends normalized Levenshtein distance with token
// (word) overlap into a 0..1 score. The blend means a reordered or
// partially-abbreviated counterparty ("ACME CORP" vs "CORP, ACME LTD")
// still scores well via token overlap even when raw edit distance is
// large.
func DescriptionSimilarity(a, b string) float64 {
	na, nb := normalizeForMatch(a), normalizeForMatch(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	lev := levenshteinRatio(na, nb)
	tok := tokenOverlap(na, nb)
	// Weight token overlap slightly higher — it is more robust to the
	// reference noise common in bank descriptions.
	return 0.4*lev + 0.6*tok
}

// levenshteinRatio is 1 - distance/maxLen, clamped to [0,1].
func levenshteinRatio(a, b string) float64 {
	d := levenshtein(a, b)
	// Measure the denominator in runes, not bytes: levenshtein counts edits
	// over []rune, so a byte length would inflate the ratio for non-ASCII
	// descriptions (e.g. EU/UK counterparties like "Société Générale"),
	// spuriously boosting description similarity in the confidence score.
	maxLen := utf8.RuneCountInString(a)
	if rb := utf8.RuneCountInString(b); rb > maxLen {
		maxLen = rb
	}
	if maxLen == 0 {
		return 1
	}
	r := 1.0 - float64(d)/float64(maxLen)
	if r < 0 {
		return 0
	}
	return r
}

// levenshtein computes the edit distance between two strings with the
// standard two-row dynamic-programming algorithm (O(len(a)*len(b)) time,
// O(min) space).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// tokenOverlap is the Jaccard similarity of the two strings' word sets.
func tokenOverlap(a, b string) float64 {
	ta := tokenSet(a)
	tb := tokenSet(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if _, ok := tb[t]; ok {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, tok := range strings.Fields(s) {
		if len(tok) < 2 {
			continue // drop single-char noise
		}
		out[tok] = struct{}{}
	}
	return out
}

// normalizeForMatch canonicalizes a description for similarity scoring:
// lower-case, digits/punctuation collapsed to spaces, whitespace
// squeezed. This is the single source of truth for description
// normalization — the bankfeed package defers to the matcher for all
// keying so the two never drift.
func normalizeForMatch(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			b.WriteRune(r)
			prevSpace = false
		default:
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// DescriptionKey hashes a normalized description to the stable key used
// by bank_learned_matches. It is the canonical keying used by both the
// matcher and the sync pipeline.
func DescriptionKey(description string) string {
	norm := normalizeForMatch(description)
	if norm == "" {
		return ""
	}
	return shortHash(norm)
}

// shortHash returns the first 16 hex chars of the SHA-256 of s — enough
// entropy to key learned matches without storing the raw (potentially
// PII-bearing) counterparty description.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// ---------------------------------------------------------------------------
// Inter-account transfer auto-pairing + duplicate detection
// ---------------------------------------------------------------------------

// BankTxnTransfer marks a statement line that the transfer detector has
// paired with the opposite leg in another of the tenant's own accounts:
// a resolved line that is internal money movement, not P&L activity, so
// it drops out of the income/expense reconciliation surface. The value
// matches the bank_transactions status CHECK widened in migration 000091.
// Defined here (not bank.go) so the detector that owns the state also
// owns its constant.
const BankTxnTransfer = "transfer"

// DefaultTransferWindow is the ± date window in which a debit in one
// account and the equal-and-opposite credit in another are considered
// the two legs of one transfer. Inter-account transfers (especially
// cross-bank Faster Payments / SEPA) usually clear within a couple of
// days; ±4d absorbs a weekend without pairing coincidentally-equal
// amounts weeks apart.
const DefaultTransferWindow = 4 * 24 * time.Hour

// DefaultDuplicateWindow is the ± date window in which two lines in the
// SAME account with identical signed amount and currency are considered
// for duplicate flagging (the same real transaction ingested via two
// overlapping feeds, e.g. a CSV upload plus a live Plaid feed). Tight by
// design: a genuine recurring charge of the same amount is normally days
// apart, so ±2d keeps precision high — and a false positive only ever
// *flags* a line, never hides or deletes it.
const DefaultDuplicateWindow = 2 * 24 * time.Hour

// duplicateSimilarityFloor is the minimum description similarity for a
// duplicate flag. Two feeds format the same transaction differently
// (e.g. "TFL TRAVEL CH" vs "TFL.GOV.UK/CP TFL TRAVEL"), so this is a
// floor, not equality — but high enough that two distinct same-amount
// charges on the same day are not mistaken for one.
const duplicateSimilarityFloor = 0.5

// TransferPair is one detected inter-account transfer: a money-out line
// (the debit leg, negative amount) in one account paired with the
// money-in line (the credit leg, positive amount) in another.
type TransferPair struct {
	ID          uuid.UUID       `json:"id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	DebitTxnID  uuid.UUID       `json:"debit_txn_id"`
	CreditTxnID uuid.UUID       `json:"credit_txn_id"`
	Amount      decimal.Decimal `json:"amount"`
	Currency    string          `json:"currency"`
	Confidence  float64         `json:"confidence"`
	DetectedAt  time.Time       `json:"detected_at"`
}

// DetectTransfer tries to pair a freshly-ingested line with the equal-
// and-opposite leg in a different account of the same tenant, recording
// the pair and marking both legs status='transfer'. It is conservative
// and idempotent: it only ever pairs two currently-unreconciled,
// non-duplicate lines of exactly opposite signed amount and equal
// currency within DefaultTransferWindow, and the partial unique indexes
// on bank_transfer_pairs make a concurrent or repeated attempt a no-op
// rather than double-pairing a line. Returns the created pair, or nil
// when no confident counter-leg exists (the common case).
func (m *SmartMatcher) DetectTransfer(ctx context.Context, tenantID, txnID uuid.UUID) (*TransferPair, error) {
	if tenantID == uuid.Nil || txnID == uuid.Nil {
		return nil, errors.New("ledger: tenant_id and txn_id required")
	}
	var pair *TransferPair
	err := dbutil.WithTenantTx(ctx, m.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			amount        decimal.Decimal
			valueDate     time.Time
			currency      string
			status        string
			description   string
			bankAccountID uuid.UUID
			dupOf         *uuid.UUID
		)
		if err := tx.QueryRow(ctx,
			`SELECT amount, value_date, currency, status, COALESCE(description,''), bank_account_id, duplicate_of
			   FROM bank_transactions WHERE tenant_id = $1 AND id = $2`,
			tenantID, txnID,
		).Scan(&amount, &valueDate, &currency, &status, &description, &bankAccountID, &dupOf); err != nil {
			return fmt.Errorf("ledger: load bank_transaction: %w", err)
		}
		// Only pair a live, non-duplicate line. A zero amount cannot be a
		// meaningful transfer and would pair spuriously with any other zero
		// line, so skip it.
		if status != BankTxnUnreconciled || dupOf != nil || amount.IsZero() {
			return nil
		}

		// The counter-leg is the exact negation in a *different* account:
		// same currency, opposite signed amount, unreconciled, not itself a
		// duplicate, within the window, and not already half of a pair
		// (guarded by the NOT EXISTS against both legs). Closest value_date
		// first, breaking ties on id for determinism.
		var (
			otherID     uuid.UUID
			otherDate   time.Time
			otherDesc   string
			otherAcctID uuid.UUID
		)
		err := tx.QueryRow(ctx,
			`SELECT t.id, t.value_date, COALESCE(t.description,''), t.bank_account_id
			   FROM bank_transactions t
			  WHERE t.tenant_id = $1
			    AND t.bank_account_id <> $2
			    AND t.currency = $3
			    AND t.amount = $4
			    AND t.status = $5
			    AND t.duplicate_of IS NULL
			    AND t.value_date BETWEEN $6 AND $7
			    AND NOT EXISTS (
			        SELECT 1 FROM bank_transfer_pairs p
			         WHERE p.tenant_id = t.tenant_id
			           AND (p.debit_txn_id = t.id OR p.credit_txn_id = t.id))
			  ORDER BY abs(t.value_date - $8::date) ASC, t.id ASC
			  LIMIT 1`,
			tenantID, bankAccountID, currency, amount.Neg(), BankTxnUnreconciled,
			valueDate.Add(-DefaultTransferWindow), valueDate.Add(DefaultTransferWindow), valueDate,
		).Scan(&otherID, &otherDate, &otherDesc, &otherAcctID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ledger: find transfer counter-leg: %w", err)
		}

		// Orient the legs by sign: the negative amount is the debit (money
		// out), the positive is the credit (money in).
		debitID, creditID := txnID, otherID
		if amount.IsPositive() {
			debitID, creditID = otherID, txnID
		}
		magnitude := amount.Abs()
		confidence := transferConfidence(valueDate, otherDate, description, otherDesc)

		// Insert the pair. The partial unique indexes on (tenant_id,
		// debit_txn_id) and (tenant_id, credit_txn_id) collapse a racing or
		// repeated attempt to a no-op; a 0-row result means the line was
		// paired concurrently, so leave both legs untouched.
		pairID := uuid.New()
		now := m.now()
		ct, err := tx.Exec(ctx,
			`INSERT INTO bank_transfer_pairs
			        (tenant_id, id, debit_txn_id, credit_txn_id, amount, currency, confidence, detected_at, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
			 ON CONFLICT DO NOTHING`,
			tenantID, pairID, debitID, creditID, magnitude, currency, confidence, now)
		if err != nil {
			return fmt.Errorf("ledger: insert transfer pair: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return nil
		}

		// Mark both legs as transfers, but only while still unreconciled so
		// a leg matched to a journal entry by a concurrent path is never
		// clobbered. If either guard misses, unwind the pair so we never
		// leave a half-applied transfer.
		ct1, err := tx.Exec(ctx,
			`UPDATE bank_transactions SET status = $3
			  WHERE tenant_id = $1 AND id = $2 AND status = $4`,
			tenantID, debitID, BankTxnTransfer, BankTxnUnreconciled)
		if err != nil {
			return fmt.Errorf("ledger: mark debit leg transfer: %w", err)
		}
		ct2, err := tx.Exec(ctx,
			`UPDATE bank_transactions SET status = $3
			  WHERE tenant_id = $1 AND id = $2 AND status = $4`,
			tenantID, creditID, BankTxnTransfer, BankTxnUnreconciled)
		if err != nil {
			return fmt.Errorf("ledger: mark credit leg transfer: %w", err)
		}
		if ct1.RowsAffected() == 0 || ct2.RowsAffected() == 0 {
			return fmt.Errorf("ledger: transfer leg no longer unreconciled: %w", ErrSuggestionConflict)
		}

		pair = &TransferPair{
			ID:          pairID,
			TenantID:    tenantID,
			DebitTxnID:  debitID,
			CreditTxnID: creditID,
			Amount:      magnitude,
			Currency:    currency,
			Confidence:  confidence,
			DetectedAt:  now,
		}
		return m.auditTransfer(ctx, tx, tenantID, pairID, "finance.bank_feed.transfer.detect")
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// transferConfidence scores a detected pair on date proximity (same day
// is strongest, decaying linearly to the window edge) with a small bump
// when either leg's description hints at a transfer. Amount/currency are
// already exact by construction, so they are not re-scored.
func transferConfidence(aDate, bDate time.Time, aDesc, bDesc string) float64 {
	gap := aDate.Sub(bDate)
	if gap < 0 {
		gap = -gap
	}
	proximity := 1.0 - float64(gap)/float64(DefaultTransferWindow)
	if proximity < 0 {
		proximity = 0
	}
	// Base 0.6..0.9 from proximity so even a window-edge pair clears a
	// review bar, with same-day pairs near-certain.
	score := 0.6 + 0.3*proximity
	if mentionsTransfer(aDesc) || mentionsTransfer(bDesc) {
		score += 0.1
	}
	if score > 1 {
		score = 1
	}
	return score
}

// mentionsTransfer reports whether a description carries a common
// transfer cue. Lower-cased substring match keeps it cheap and is only a
// confidence nudge, never a gate.
func mentionsTransfer(desc string) bool {
	d := strings.ToLower(desc)
	for _, kw := range []string{"transfer", "xfer", "trf", "to savings", "from savings", "internal"} {
		if strings.Contains(d, kw) {
			return true
		}
	}
	return false
}

// DetectDuplicate flags a freshly-ingested line as a suspected duplicate
// of an earlier line in the SAME account when they share currency and
// exact signed amount, fall within DefaultDuplicateWindow, and their
// descriptions are similar enough — the signature of one real
// transaction arriving via two overlapping feeds. It is a conservative
// *flag* only (sets duplicate_of); the line is never hidden or deleted,
// so a false positive can never drop a genuine statement line off the
// books. Returns the id of the earlier line it was flagged against, or
// nil when the line is not a duplicate. Idempotent: a line already
// flagged returns its existing pointer.
func (m *SmartMatcher) DetectDuplicate(ctx context.Context, tenantID, txnID uuid.UUID) (*uuid.UUID, error) {
	if tenantID == uuid.Nil || txnID == uuid.Nil {
		return nil, errors.New("ledger: tenant_id and txn_id required")
	}
	var flagged *uuid.UUID
	err := dbutil.WithTenantTx(ctx, m.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			amount        decimal.Decimal
			valueDate     time.Time
			currency      string
			status        string
			description   string
			externalRef   string
			bankAccountID uuid.UUID
			dupOf         *uuid.UUID
		)
		if err := tx.QueryRow(ctx,
			`SELECT amount, value_date, currency, status, COALESCE(description,''),
			        COALESCE(external_ref,''), bank_account_id, duplicate_of
			   FROM bank_transactions WHERE tenant_id = $1 AND id = $2`,
			tenantID, txnID,
		).Scan(&amount, &valueDate, &currency, &status, &description, &externalRef, &bankAccountID, &dupOf); err != nil {
			return fmt.Errorf("ledger: load bank_transaction: %w", err)
		}
		// Already flagged: idempotent return.
		if dupOf != nil {
			flagged = dupOf
			return nil
		}
		// Only a live, unreconciled line is flaggable: a line already
		// matched, voided, or resolved as a transfer leg must not be newly
		// marked a duplicate (e.g. when the second leg of a just-paired
		// transfer is revisited later in the same sync batch).
		if status != BankTxnUnreconciled {
			return nil
		}

		// Earlier lines in the same account with identical signed amount and
		// currency, inside the window, that are not themselves a duplicate or
		// voided, and carry a different external_ref (a same-ref re-fetch
		// already dedupes via the unique index — that is not a duplicate, it
		// is the same row). Oldest first: we flag the newer line at the
		// canonical earlier one.
		// Collect candidates fully before scoring: the rows cursor and the
		// flagging UPDATE share one pooled connection, so the cursor must be
		// drained and closed before any Exec on the same tx (otherwise pgx
		// reports the connection busy).
		type candidate struct {
			id   uuid.UUID
			desc string
		}
		rows, err := tx.Query(ctx,
			`SELECT id, COALESCE(description,'')
			   FROM bank_transactions
			  WHERE tenant_id = $1
			    AND bank_account_id = $2
			    AND id <> $3
			    AND currency = $4
			    AND amount = $5
			    AND status <> $6
			    AND duplicate_of IS NULL
			    AND external_ref IS DISTINCT FROM $7
			    AND value_date BETWEEN $8 AND $9
			  ORDER BY value_date ASC, created_at ASC, id ASC`,
			tenantID, bankAccountID, txnID, currency, amount, BankTxnVoided, externalRef,
			valueDate.Add(-DefaultDuplicateWindow), valueDate.Add(DefaultDuplicateWindow))
		if err != nil {
			return fmt.Errorf("ledger: scan duplicate candidates: %w", err)
		}
		candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
			var c candidate
			return c, row.Scan(&c.id, &c.desc)
		})
		if err != nil {
			return fmt.Errorf("ledger: scan duplicate candidates: %w", err)
		}

		// First candidate (already oldest-first) whose description is
		// similar enough is the canonical line we flag against.
		var canonical *uuid.UUID
		for i := range candidates {
			if DescriptionSimilarity(description, candidates[i].desc) >= duplicateSimilarityFloor {
				id := candidates[i].id
				canonical = &id
				break
			}
		}
		if canonical == nil {
			return nil
		}

		if ct, err := tx.Exec(ctx,
			`UPDATE bank_transactions SET duplicate_of = $3
			  WHERE tenant_id = $1 AND id = $2 AND duplicate_of IS NULL`,
			tenantID, txnID, *canonical); err != nil {
			return fmt.Errorf("ledger: flag duplicate: %w", err)
		} else if ct.RowsAffected() == 0 {
			return nil
		}
		flagged = canonical
		return m.auditTransfer(ctx, tx, tenantID, txnID, "finance.bank_feed.duplicate.flag")
	})
	if err != nil {
		return nil, err
	}
	return flagged, nil
}

// auditTransfer emits an audit entry for a transfer-pair or duplicate
// detection. targetID is the pair id (transfer) or the flagged line id
// (duplicate); both are system-actor events from the sync pipeline.
func (m *SmartMatcher) auditTransfer(ctx context.Context, tx pgx.Tx, tenantID, targetID uuid.UUID, action string) error {
	if m.store.auditor == nil {
		return nil
	}
	id := targetID
	return m.store.auditor.LogTx(ctx, tx, audit.Entry{
		TenantID:    tenantID,
		ActorKind:   audit.ActorSystem,
		Action:      action,
		TargetKType: "finance.bank_transaction",
		TargetID:    &id,
	})
}
