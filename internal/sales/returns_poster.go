package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// ReturnPoster drives the sales.return state machine. It mirrors the
// POSPoster shape (records + invoice poster + auxiliary engine) and
// is wired into the API the same way:
//
//	NewReturnPoster(records, invoicePoster, inventoryStore).
//	    Approve(...) / Receive(...) / Refund(...).
//
// Every transition is idempotent in the strict sense:
//
//   - Approve patches the record from "requested" to "approved" with
//     no posting side-effects. A retry against a record already in
//     "approved" returns the current snapshot without re-emitting the
//     transition event.
//
//   - Receive appends one signed-positive inventory move per
//     aggregated (item_id, warehouse_id) bucket on the return. The
//     `inventory_moves_source_uniq` partial unique index keyed on
//     (tenant_id, source_ktype, source_id, item_id, warehouse_id)
//     guarantees that a replay never double-stocks the warehouse —
//     the second insert collides on 23505 and the inventory store
//     surfaces ErrDuplicateSourceMove, which Receive treats as
//     success.
//
//   - Refund materialises a finance.credit_note KRecord (idempotent:
//     the return stamps `credit_note_id` immediately after Create so
//     a retry reuses the prior credit note rather than allocating a
//     fresh one and double-reversing AR / revenue), then delegates
//     the contra-JE post to InvoicePoster.PostCreditNote. The poster
//     handles the (source_ktype, source_id) JE uniqueness check
//     internally; we treat the ErrCreditNoteAlreadyPosted sentinel as
//     success so the resume case is invisible to callers.
type ReturnPoster struct {
	records   *record.PGStore
	invoice   *ledger.InvoicePoster
	inventory *inventory.PGStore
	ledger    *ledger.PGStore
	now       func() time.Time
}

// NewReturnPoster wires the poster against existing stores. All
// three engines are required at runtime; tests can stub them via
// interfaces in a future refactor if needed, but the production
// surface keeps the call site terse and matches the POSPoster shape.
func NewReturnPoster(records *record.PGStore, invoicePoster *ledger.InvoicePoster, inventoryStore *inventory.PGStore, ledgerStore *ledger.PGStore) *ReturnPoster {
	return &ReturnPoster{
		records:   records,
		invoice:   invoicePoster,
		inventory: inventoryStore,
		ledger:    ledgerStore,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// WithNow lets tests pin the clock so the stamped timestamps
// (`received_at`, `refunded_at`) are deterministic.
func (p *ReturnPoster) WithNow(now func() time.Time) *ReturnPoster {
	if now != nil {
		p.now = now
	}
	return p
}

// returnLine — the subset of fields the poster reads from each entry
// in a return's `lines` array. Mirrors invoiceLine on the inventory
// hook so the wire format stays consistent across the platform.
type returnLine struct {
	ItemID    string          `json:"item_id"`
	Qty       decimal.Decimal `json:"qty"`
	UnitPrice decimal.Decimal `json:"unit_price"`
	UnitCost  decimal.Decimal `json:"unit_cost"`
}

// returnLines lifts the line array off the return's JSONB payload.
type returnLinesEnvelope struct {
	Lines []returnLine `json:"lines"`
}

// Sentinel errors. Callers translate these into HTTP status codes
// (404 for not-a-return, 409 for invalid-state, 400 for invalid data).
var (
	ErrReturnNotFound     = errors.New("sales: return not found")
	ErrNotReturn          = errors.New("sales: record is not a sales.return")
	ErrInvalidReturnState = errors.New("sales: return is not in a state for this transition")
	ErrReturnAmountZero   = errors.New("sales: return total must be > 0")
	// ErrReturnInvalidInput is the wrapper sentinel returned for
	// validation failures inside ReturnPoster (missing warehouse_id,
	// empty lines, missing original_invoice_id, etc.). HTTP handlers
	// map errors.Is(err, ErrReturnInvalidInput) to 422 Unprocessable
	// Entity. Wrap each specific message with %w so the caller still
	// gets the human-readable detail but the sentinel-based switch
	// in writeReturnError doesn't have to fall back to substring
	// heuristics on err.Error().
	ErrReturnInvalidInput = errors.New("sales: invalid return input")
)

// Approve flips a return from requested → approved with no posting
// side-effects. Idempotent: returning a record already in "approved"
// short-circuits to the current snapshot.
func (p *ReturnPoster) Approve(ctx context.Context, tenantID, returnID, actorID uuid.UUID) (*record.KRecord, error) {
	rec, current, err := p.loadReturn(ctx, tenantID, returnID)
	if err != nil {
		return nil, err
	}
	status := stringOr(current, "status", ReturnStatusRequested)
	if status == ReturnStatusApproved {
		return rec, nil
	}
	if status != ReturnStatusRequested {
		return nil, fmt.Errorf("%w: cannot approve in %q", ErrInvalidReturnState, status)
	}
	current["status"] = ReturnStatusApproved
	return p.persist(ctx, rec, current, &actorID)
}

// Receive transitions an approved return into "received" by
// appending one positive-qty inventory move per (item, warehouse)
// pair. The warehouse_id is read from the return header; per-line
// `warehouse_id` overrides are not supported in N9a (the audit log
// would not have an obvious "which warehouse" answer for a partially
// distributed receipt, and ERPNext models partial returns by
// authoring multiple separate Returns instead).
func (p *ReturnPoster) Receive(ctx context.Context, tenantID, returnID, actorID uuid.UUID) (*record.KRecord, error) {
	rec, current, err := p.loadReturn(ctx, tenantID, returnID)
	if err != nil {
		return nil, err
	}
	status := stringOr(current, "status", ReturnStatusRequested)
	if status == ReturnStatusReceived || status == ReturnStatusRefunded {
		return rec, nil
	}
	if status != ReturnStatusApproved {
		return nil, fmt.Errorf("%w: cannot receive in %q", ErrInvalidReturnState, status)
	}

	warehouseID, err := refUUID(current, "warehouse_id")
	if err != nil || warehouseID == uuid.Nil {
		return nil, fmt.Errorf("%w: warehouse_id required to receive return", ErrReturnInvalidInput)
	}

	var lines returnLinesEnvelope
	if len(rec.Data) > 0 {
		if err := json.Unmarshal(rec.Data, &lines); err != nil {
			return nil, fmt.Errorf("sales: decode return lines: %w", err)
		}
	}
	if len(lines.Lines) == 0 {
		return nil, fmt.Errorf("%w: return has no lines to receive", ErrReturnInvalidInput)
	}

	// Aggregate by item so a multi-line return for the same SKU
	// posts a single move. Unlike inventory.PosterHook (which
	// carries separate qty / qtyAbs fields to handle the
	// shipped-vs-credited sign flip), receipts here are always
	// signed-positive — stock comes back IN — so a single `qty`
	// (running absolute total) is sufficient. `costTimesQty`
	// accumulates unit_cost * qty so the bucket's weighted-average
	// unit cost is derivable as costTimesQty / qty.
	type bucket struct {
		itemID       uuid.UUID
		qty          decimal.Decimal
		costTimesQty decimal.Decimal
	}
	order := []uuid.UUID{}
	agg := map[uuid.UUID]*bucket{}
	for i, line := range lines.Lines {
		if line.ItemID == "" || line.Qty.IsZero() {
			continue
		}
		itemID, err := uuid.Parse(line.ItemID)
		if err != nil {
			return nil, fmt.Errorf("sales: line %d invalid item_id: %w", i, err)
		}
		unitCost := line.UnitCost
		if unitCost.IsZero() {
			unitCost = line.UnitPrice
		}
		qty := line.Qty.Abs()
		cur, ok := agg[itemID]
		if !ok {
			cur = &bucket{itemID: itemID}
			agg[itemID] = cur
			order = append(order, itemID)
		}
		cur.qty = cur.qty.Add(qty)
		cur.costTimesQty = cur.costTimesQty.Add(unitCost.Mul(qty))
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("%w: return has no item lines with qty > 0", ErrReturnInvalidInput)
	}

	sourceID := rec.ID
	for _, itemID := range order {
		b := agg[itemID]
		if !b.qty.IsPositive() {
			continue
		}
		unitCost := b.costTimesQty.Div(b.qty)
		_, err := p.inventory.RecordMove(ctx, inventory.Move{
			TenantID:    tenantID,
			ItemID:      itemID,
			WarehouseID: warehouseID,
			Qty:         b.qty,
			UnitCost:    unitCost,
			SourceKType: inventory.MoveSourceSalesReturn,
			SourceID:    &sourceID,
			CreatedBy:   actorID,
		})
		if err != nil && !errors.Is(err, inventory.ErrDuplicateSourceMove) {
			return nil, fmt.Errorf("sales: post receipt move (item %s, warehouse %s): %w", itemID, warehouseID, err)
		}
	}

	current["status"] = ReturnStatusReceived
	current["received_at"] = p.now().Format(time.RFC3339)
	return p.persist(ctx, rec, current, &actorID)
}

// Refund transitions a received return into "refunded" by posting a
// finance.credit_note that reverses AR + revenue on the original
// invoice. The credit-note record itself is created on demand and
// stamped onto the return so a retry after a partial failure reuses
// it rather than allocating a duplicate (which the ledger's
// (source_ktype, source_id) JE uniqueness index would block, but
// the duplicate credit-note row itself would leak through and
// confuse the credit-note list view).
func (p *ReturnPoster) Refund(ctx context.Context, tenantID, returnID, actorID uuid.UUID) (*record.KRecord, error) {
	rec, current, err := p.loadReturn(ctx, tenantID, returnID)
	if err != nil {
		return nil, err
	}
	status := stringOr(current, "status", ReturnStatusRequested)
	if status == ReturnStatusRefunded {
		return rec, nil
	}
	if status != ReturnStatusReceived {
		return nil, fmt.Errorf("%w: cannot refund in %q", ErrInvalidReturnState, status)
	}

	originalInvoiceID, err := refUUID(current, "original_invoice_id")
	if err != nil || originalInvoiceID == uuid.Nil {
		return nil, fmt.Errorf("%w: original_invoice_id required to refund", ErrReturnInvalidInput)
	}
	total := decimalOr(current, "total")
	if !total.IsPositive() {
		return nil, ErrReturnAmountZero
	}
	currency := stringOr(current, "currency", "USD")

	// Resumable allocation of the credit note. Two recovery paths
	// cover the partial-failure shapes:
	//
	//  1. credit_note_id is stamped on the return — the previous
	//     run got past Create+Update; we just reload the CN by ID
	//     and re-enter PostCreditNote (which is idempotent via the
	//     (source_ktype, source_id) JE uniqueness check).
	//
	//  2. credit_note_id is NOT stamped — the previous run either
	//     never reached Create, OR (the narrow leak the bot's
	//     finding 3303293709 calls out) Create succeeded but the
	//     intermediate Update that stamps credit_note_id on the
	//     return failed (version conflict, transient DB error).
	//     The CN row is committed to the database with
	//     created_from_return_id = rec.ID in its body, but the
	//     return's pointer to it is missing. Before falling through
	//     to a fresh Create — which would mint a SECOND credit note
	//     and leak the first as a draft orphan — we look up any
	//     finance.credit_note with created_from_return_id matching
	//     this return. If one exists, we adopt it; the stamp + JE
	//     post downstream then re-run idempotently.
	//
	// created_from_return_id is set unconditionally in the body
	// below (line 303 onward), so the orphan from a partially-
	// failed Refund is always findable. ListByField is the same
	// pattern payroll uses to look up payslips by pay_run_id and
	// presence uses to look up attendance by employee_id — it's
	// the canonical JSONB-foreign-key lookup primitive in this
	// codebase.
	var noteRec *record.KRecord
	if existingNoteID, _ := refUUID(current, "credit_note_id"); existingNoteID != uuid.Nil {
		noteRec, err = p.records.Get(ctx, tenantID, existingNoteID)
		if err != nil {
			return nil, fmt.Errorf("sales: load existing credit_note %s: %w", existingNoteID, err)
		}
	} else if orphans, lookupErr := p.records.ListByField(ctx, tenantID,
		record.ListFilter{KType: finance.KTypeCreditNote},
		"created_from_return_id", rec.ID.String(),
	); lookupErr != nil {
		return nil, fmt.Errorf("sales: look up orphan credit_note for return %s: %w", rec.ID, lookupErr)
	} else if len(orphans) > 0 {
		// At most one orphan is possible by construction: a CN is
		// only created with this return's ID as
		// created_from_return_id, and Create is only reached when
		// no prior CN was stamped on the return. If multiple
		// orphans are ever observed that's a strict-mode invariant
		// violation worth surfacing — but in the happy retry case
		// orphans[0] is the single previously-committed row, and
		// adopting it is the correct recovery.
		if len(orphans) > 1 {
			return nil, fmt.Errorf("sales: multiple orphan credit_notes for return %s (%d found) — manual intervention required", rec.ID, len(orphans))
		}
		adopted := orphans[0]
		noteRec = &adopted
		// Stamp the adopted CN onto the return so subsequent runs
		// hit the fast path (Get by credit_note_id) and not the
		// ListByField scan. Failure here is non-fatal: the orphan
		// will be re-adopted on the next retry, and PostCreditNote
		// below is still idempotent against the adopted ID. We do
		// not return early on Update failure for the same reason
		// the resume path doesn't — the downstream post is what
		// matters, the stamp is best-effort.
		current["credit_note_id"] = noteRec.ID.String()
		rec.Data, _ = json.Marshal(current)
		rec.UpdatedBy = &actorID
		if updated, updateErr := p.records.Update(ctx, *rec); updateErr == nil {
			rec = updated
			if err := json.Unmarshal(rec.Data, &current); err != nil {
				return nil, fmt.Errorf("sales: re-decode return after orphan adoption: %w", err)
			}
		} else {
			log.Printf("sales: refund orphan adoption: stamp credit_note_id failed tenant=%s return=%s credit_note=%s: %v (continuing — PostCreditNote remains idempotent on retry)",
				tenantID, rec.ID, noteRec.ID, updateErr)
		}
	} else {
		body := map[string]any{
			"original_invoice_id":      originalInvoiceID.String(),
			"credit_note_number":       stringOr(current, "return_number", ""),
			"issue_date":               stringOr(current, "return_date", p.now().Format("2006-01-02")),
			"reason":                   stringOr(current, "reason", "Customer return"),
			// Use InexactFloat64 here even though it can lose
			// precision beyond ~$90T (>2^53 cents) — the
			// finance.credit_note KType schema declares `amount`
			// as `"type": "number"` (see internal/finance/ktypes.go),
			// and the validator's toFloat64 only accepts numeric
			// JSON kinds (float64 / json.Number), so encoding as
			// a decimal string would fail validation with
			// `amount: must be number`. The financial posting in
			// PostCreditNote reads amounts straight from the
			// original invoice's decimal columns, so this body
			// field is metadata only; the theoretical precision
			// risk has no real-world impact at SME invoice
			// magnitudes. A future schema migration could promote
			// finance.* amount fields to a decimal-string type
			// alongside an updated validator + payment/credit
			// note/journal entry consumers, but that's outside
			// the scope of this PR.
			"amount":                   total.InexactFloat64(),
			"currency":                 currency,
			"status":                   "draft",
			"created_from_return_id":   rec.ID.String(),
		}
		bytes, _ := json.Marshal(body)
		noteRec, err = p.records.Create(ctx, record.KRecord{
			ID:        uuid.New(),
			TenantID:  tenantID,
			KType:     finance.KTypeCreditNote,
			Data:      bytes,
			CreatedBy: actorID,
		})
		if err != nil {
			return nil, fmt.Errorf("sales: create credit_note: %w", err)
		}
		current["credit_note_id"] = noteRec.ID.String()
		rec.Data, _ = json.Marshal(current)
		rec.UpdatedBy = &actorID
		updated, err := p.records.Update(ctx, *rec)
		if err != nil {
			return nil, fmt.Errorf("sales: persist credit_note_id: %w", err)
		}
		rec = updated
		// Refresh `current` against the persisted state so the
		// downstream patch sees the updated version + timestamps.
		if err := json.Unmarshal(rec.Data, &current); err != nil {
			return nil, fmt.Errorf("sales: re-decode return after credit note stamp: %w", err)
		}
	}

	entry, err := p.invoice.PostCreditNote(ctx, tenantID, noteRec.ID, actorID)
	if err != nil && !errors.Is(err, ledger.ErrCreditNoteAlreadyPosted) {
		return nil, fmt.Errorf("sales: post credit_note: %w", err)
	}
	// On already-posted, reload the source-row JE so the return can
	// stamp the correct journal_entry_id even when the resume path
	// kicks in. The (source_ktype="finance.credit_note", source_id)
	// lookup is the canonical way to find it.
	//
	// A lookup error here is non-fatal: the credit note is already
	// persisted and posted (PostCreditNote completed above) so the
	// return's transition to refunded is still correct. The only
	// consequence of a swallowed error is that
	// current["journal_entry_id"] stays unstamped on this run; a
	// future maintenance path can backfill it via the same source
	// lookup. We log with full context (tenant + return + credit
	// note IDs) rather than discard via _, matching the log.Printf
	// style used by inventory.Reorder and finance.budget for
	// similarly best-effort branches — so the silent gap is at
	// least visible in the logs if it ever fires.
	if entry == nil && p.ledger != nil {
		var lookupErr error
		entry, lookupErr = p.ledger.GetJournalEntryBySource(ctx, tenantID, "finance.credit_note", noteRec.ID)
		if lookupErr != nil {
			log.Printf("sales: refund resume: GetJournalEntryBySource tenant=%s return=%s credit_note=%s: %v",
				tenantID, rec.ID, noteRec.ID, lookupErr)
			entry = nil
		}
	}

	current["status"] = ReturnStatusRefunded
	current["credit_note_id"] = noteRec.ID.String()
	if entry != nil {
		current["journal_entry_id"] = entry.ID.String()
	}
	current["refunded_at"] = p.now().Format(time.RFC3339)
	return p.persist(ctx, rec, current, &actorID)
}

// Cancel marks a return as cancelled. Permitted from any pre-refund
// state.
//
// Cancel always attempts to reverse any inventory moves the return
// emitted, regardless of the current status. The common case is a
// return cancelled from "received": Receive has posted positive-qty
// moves to put the units back into stock and Cancel must drain them
// back via contra rows so the stock_levels view returns to its
// pre-receive level. But Cancel also has to defend against a narrower
// edge case where Receive committed its inventory moves and then
// failed to persist the status flip (e.g. version conflict, transient
// DB error): the return is left in "approved" with orphaned moves
// against its source_id, and a status-only cancel would silently
// inflate warehouse stock. By calling reverseReceiptMoves on every
// Cancel (and letting it no-op when ListMoves finds no source-keyed
// rows), the contract — cancelling a return never leaves orphaned
// stock — holds independent of which transition crashed. The extra
// indexed lookup on (tenant_id, source_ktype, source_id) is cheap;
// asymmetric branching on status was a finer scalpel than the
// invariant warrants. This mirrors ERPNext's Stock Entry cancellation
// behaviour (auto-posts reverse Stock Ledger Entries with
// is_cancelled=1).
//
// Idempotency: ReverseMove is guarded by
// inventory_moves_reversal_of_uniq, so a retried Cancel surfaces
// ErrAlreadyReversed on any move already reversed and Cancel treats
// that as success. The status flip on the return itself short-circuits
// when the current status is already "cancelled".
//
// Refunded returns cannot be cancelled — the credit-note JE has
// already reversed AR + revenue, and the financial side cannot be
// undone by a status flip. Operators must issue a fresh sales
// invoice to recover from a mistaken refund, just as ERPNext requires.
func (p *ReturnPoster) Cancel(ctx context.Context, tenantID, returnID, actorID uuid.UUID) (*record.KRecord, error) {
	rec, current, err := p.loadReturn(ctx, tenantID, returnID)
	if err != nil {
		return nil, err
	}
	status := stringOr(current, "status", ReturnStatusRequested)
	if status == ReturnStatusCancelled {
		return rec, nil
	}
	if status == ReturnStatusRefunded {
		return nil, fmt.Errorf("%w: refunded returns cannot be cancelled", ErrInvalidReturnState)
	}
	// reverseReceiptMoves is a no-op when ListMoves finds no
	// source-keyed rows — covers Cancel from requested / approved
	// in the normal case, and reverses orphaned moves left behind
	// by a partially-failed Receive.
	if err := p.reverseReceiptMoves(ctx, tenantID, rec.ID, actorID); err != nil {
		return nil, err
	}
	current["status"] = ReturnStatusCancelled
	return p.persist(ctx, rec, current, &actorID)
}

// reverseReceiptMoves enumerates every first-class (non-contra) move
// the return emitted during Receive and posts a reversing contra row
// for each. Idempotent: ErrAlreadyReversed is treated as success so a
// retried Cancel collapses to a no-op after the first successful run.
//
// p.inventory is guaranteed non-nil by NewReturnPoster's contract
// (it accepts *inventory.PGStore as a required positional parameter,
// and the two production call sites — deps_build.go and
// services/kchat-bridge/main.go — pass live stores). Receive and
// Refund use p.inventory and p.invoice without nil checks for the
// same reason; making this one path asymmetric was defense-in-depth
// noise rather than a real guard.
func (p *ReturnPoster) reverseReceiptMoves(ctx context.Context, tenantID, returnID, actorID uuid.UUID) error {
	// 500 is the inventory store's per-page cap; a single return
	// never aggregates more than a handful of moves (one per
	// distinct item bucket), so a single page is always sufficient
	// in practice. Walking with a cursor would add complexity for
	// a payload size that does not warrant it.
	moves, err := p.inventory.ListMoves(ctx, tenantID, inventory.MoveFilter{
		SourceKType: inventory.MoveSourceSalesReturn,
		SourceID:    &returnID,
		Limit:       500,
	})
	if err != nil {
		return fmt.Errorf("sales: list receipt moves for cancel: %w", err)
	}
	for i := range moves {
		// Skip contra rows defensively — ReverseMove rejects them
		// with ErrCannotReverseContra anyway, but the filter
		// shouldn't return them either since contras carry
		// source_ktype/source_id NULL.
		if moves[i].ReversalOf != nil {
			continue
		}
		_, err := p.inventory.ReverseMove(ctx, tenantID, moves[i].ID, actorID, "sales.return cancelled")
		if err != nil && !errors.Is(err, inventory.ErrAlreadyReversed) {
			return fmt.Errorf("sales: reverse receipt move %d on cancel: %w", moves[i].ID, err)
		}
	}
	return nil
}

// loadReturn fetches the return KRecord and decodes the JSONB header
// into a map. Centralised here so every transition runs the same
// not-a-return guard.
func (p *ReturnPoster) loadReturn(ctx context.Context, tenantID, returnID uuid.UUID) (*record.KRecord, map[string]any, error) {
	if p == nil || p.records == nil {
		return nil, nil, errors.New("sales: return poster not configured")
	}
	if tenantID == uuid.Nil || returnID == uuid.Nil {
		return nil, nil, fmt.Errorf("%w: tenant_id and return_id required", ErrReturnInvalidInput)
	}
	rec, err := p.records.Get(ctx, tenantID, returnID)
	if err != nil {
		// Only the record store's not-found sentinel translates
		// into the 404-mapped ErrReturnNotFound. Other errors
		// (RLS violations, transient DB outages, decode failures
		// on the row itself) must surface verbatim so the HTTP
		// handler emits a 5xx and operators can distinguish
		// "the return id is wrong" from "the database is down".
		if errors.Is(err, record.ErrNotFound) {
			return nil, nil, ErrReturnNotFound
		}
		return nil, nil, fmt.Errorf("sales: load return %s: %w", returnID, err)
	}
	if rec == nil || rec.KType != KTypeSalesReturn {
		return nil, nil, ErrNotReturn
	}
	var current map[string]any
	if err := json.Unmarshal(rec.Data, &current); err != nil {
		return nil, nil, fmt.Errorf("sales: decode return: %w", err)
	}
	return rec, current, nil
}

// persist writes the mutated payload back to the record store with
// optimistic concurrency. Mirrors POSPoster's pattern so the audit
// log and event outbox fire on every transition.
func (p *ReturnPoster) persist(ctx context.Context, rec *record.KRecord, body map[string]any, actorID *uuid.UUID) (*record.KRecord, error) {
	bytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("sales: marshal return: %w", err)
	}
	rec.Data = bytes
	rec.UpdatedBy = actorID
	updated, err := p.records.Update(ctx, *rec)
	if err != nil {
		return nil, fmt.Errorf("sales: persist return: %w", err)
	}
	return updated, nil
}
