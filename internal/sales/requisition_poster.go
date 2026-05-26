package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// RequisitionPoster drives the procurement.purchase_requisition
// state machine. It mirrors the ReturnPoster shape and is wired
// into the API the same way:
//
//	NewRequisitionPoster(records).
//	    Approve(...) / Convert(...) / Cancel(...).
//
// Every transition is idempotent in the strict sense:
//
//   - Approve patches the record from "requested" to "approved"
//     and stamps approved_by + approved_at. A retry against a
//     record already in "approved" returns the current snapshot
//     without re-emitting the transition event.
//
//   - Convert allocates exactly one procurement.purchase_order
//     KRecord whose `lines[]` mirror the requisition's, then
//     stamps `po_id` on the requisition. The two writes are
//     ordered Create-then-Patch so a crash between them is
//     resumable: the next Convert sees the existing `po_id`,
//     skips the Create, and proceeds to the Patch — never
//     allocating a duplicate PO.
//
//   - Cancel marks the requisition cancelled (pre-ordered only)
//     and is a pure status flip with no posting side-effects.
//     The conversion-side PO, if it already exists, is left
//     untouched on cancel; operators can cancel it through the
//     PO surface independently.
type RequisitionPoster struct {
	records *record.PGStore
	now     func() time.Time
}

// NewRequisitionPoster wires the poster against the record store.
// Tests can stub via interfaces if needed; the production surface
// keeps the call site terse and matches the ReturnPoster shape.
func NewRequisitionPoster(records *record.PGStore) *RequisitionPoster {
	return &RequisitionPoster{
		records: records,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithNow lets tests pin the clock so the stamped timestamps
// (`approved_at`, `ordered_at`) are deterministic.
func (p *RequisitionPoster) WithNow(now func() time.Time) *RequisitionPoster {
	if now != nil {
		p.now = now
	}
	return p
}

// requisitionLine — the subset of fields the poster reads from each
// entry in a requisition's `lines` array. Mirrors the PO `lines[]`
// shape so Convert can transpose 1:1.
type requisitionLine struct {
	ItemID             string          `json:"item_id"`
	Description        string          `json:"description"`
	Qty                decimal.Decimal `json:"qty"`
	UOM                string          `json:"uom"`
	EstimatedUnitPrice decimal.Decimal `json:"estimated_unit_price"`
	LineTotal          decimal.Decimal `json:"line_total"`
}

// requisitionLinesEnvelope lifts the line array off the requisition's
// JSONB payload.
type requisitionLinesEnvelope struct {
	Lines []requisitionLine `json:"lines"`
}

// Sentinel errors. Callers translate these into HTTP status codes
// (404 for not-a-requisition, 409 for invalid-state, 400 for
// invalid data).
var (
	ErrRequisitionNotFound     = errors.New("sales: requisition not found")
	ErrNotRequisition          = errors.New("sales: record is not a procurement.purchase_requisition")
	ErrInvalidRequisitionState = errors.New("sales: requisition is not in a state for this transition")
	ErrRequisitionNoLines      = errors.New("sales: requisition has no lines to convert")
	ErrRequisitionNoSupplier   = errors.New("sales: supplier_id required to convert to PO")
)

// Approve flips a requisition from requested → approved with no
// posting side-effects. Idempotent: a record already in "approved"
// short-circuits to the current snapshot.
func (p *RequisitionPoster) Approve(ctx context.Context, tenantID, requisitionID, actorID uuid.UUID) (*record.KRecord, error) {
	rec, current, err := p.loadRequisition(ctx, tenantID, requisitionID)
	if err != nil {
		return nil, err
	}
	status := stringOr(current, "status", RequisitionStatusRequested)
	if status == RequisitionStatusApproved {
		return rec, nil
	}
	if status != RequisitionStatusRequested {
		return nil, fmt.Errorf("%w: cannot approve in %q", ErrInvalidRequisitionState, status)
	}
	current["status"] = RequisitionStatusApproved
	current["approved_by"] = actorID.String()
	current["approved_at"] = p.now().Format(time.RFC3339)
	return p.persist(ctx, rec, current, &actorID)
}

// Convert transitions an approved requisition into "ordered" by
// allocating a procurement.purchase_order KRecord whose lines mirror
// the requisition's. The PO id is stamped onto the requisition
// before status flips so the resume path (Create succeeded but
// status flip didn't) reuses the existing PO rather than minting a
// duplicate.
//
// The supplier_id MUST be set on the requisition before Convert —
// the PO needs a supplier and we refuse to invent one. If the
// requisition was raised without a preferred vendor, procurement
// updates the supplier_id field first (a plain PATCH against the
// KRecord, like any other lifecycle metadata), then Convert.
func (p *RequisitionPoster) Convert(ctx context.Context, tenantID, requisitionID, actorID uuid.UUID) (*record.KRecord, error) {
	// Snapshot the clock once for the entire conversion. The PO's
	// `order_date` (YYYY-MM-DD) and the requisition's `ordered_at`
	// (RFC3339) are derived from the same instant so a Convert call
	// that spans midnight cannot end up with the PO dated one day
	// and the requisition's ordered_at on the next. p.now() is
	// pinned by tests via WithNow so the deterministic shape is
	// preserved.
	now := p.now()
	rec, current, err := p.loadRequisition(ctx, tenantID, requisitionID)
	if err != nil {
		return nil, err
	}
	status := stringOr(current, "status", RequisitionStatusRequested)
	if status == RequisitionStatusOrdered {
		return rec, nil
	}
	if status != RequisitionStatusApproved {
		return nil, fmt.Errorf("%w: cannot convert in %q", ErrInvalidRequisitionState, status)
	}

	supplierID, err := refUUID(current, "supplier_id")
	if err != nil || supplierID == uuid.Nil {
		return nil, ErrRequisitionNoSupplier
	}

	var lines requisitionLinesEnvelope
	if len(rec.Data) > 0 {
		if err := json.Unmarshal(rec.Data, &lines); err != nil {
			return nil, fmt.Errorf("sales: decode requisition lines: %w", err)
		}
	}
	if len(lines.Lines) == 0 {
		return nil, ErrRequisitionNoLines
	}

	// Resumable allocation of the PO. Two recovery paths cover
	// every partial-failure shape:
	//
	//  1. po_id is stamped on the requisition — the previous run
	//     got past Create+Update; we reload the PO by ID and
	//     proceed straight to the status flip.
	//
	//  2. po_id is NOT stamped — the previous run either never
	//     reached Create, OR the orphan-leak race the bot's PR #120
	//     finding calls out: two concurrent Convert callers both
	//     read the requisition at "approved" with no po_id, both
	//     succeed at Create, then one wins the Update (optimistic
	//     concurrency at internal/record/store.go:875) and the
	//     loser fails with ErrVersionConflict. The losing caller's
	//     PO is committed to the DB with requisition_id pointing
	//     back at this requisition, but unreferenced. A naïve retry
	//     would Create yet another PO. Before falling through to
	//     Create we look up any procurement.purchase_order with
	//     data->>'requisition_id' matching this requisition; if
	//     exactly one orphan exists we adopt it.
	//
	// requisition_id is stamped unconditionally in the body below
	// (line 202), so the orphan is always findable. Same
	// ListByField idempotency primitive payroll/presence/inventory
	// use, and matches the orphan-credit-note adoption shipped in
	// the sales-returns poster (PR #119 finding 3303293709).
	//
	// >1 orphan is treated as an invariant violation: a PO is only
	// created with this requisition's ID as requisition_id when no
	// prior po_id is stamped on the requisition, so a multi-orphan
	// state implies external interference (manual KRecord create,
	// concurrent processes outside the poster, etc.) and we refuse
	// to silently pick one.
	var poRec *record.KRecord
	if existingPOID, _ := refUUID(current, "po_id"); existingPOID != uuid.Nil {
		poRec, err = p.records.Get(ctx, tenantID, existingPOID)
		if err != nil {
			return nil, fmt.Errorf("sales: load existing po %s: %w", existingPOID, err)
		}
	} else if orphans, lookupErr := p.records.ListByField(ctx, tenantID,
		record.ListFilter{KType: KTypePurchaseOrder},
		"requisition_id", rec.ID.String(),
	); lookupErr != nil {
		return nil, fmt.Errorf("sales: look up orphan purchase_order for requisition %s: %w", rec.ID, lookupErr)
	} else if len(orphans) > 0 {
		if len(orphans) > 1 {
			return nil, fmt.Errorf("sales: multiple orphan purchase_orders for requisition %s (%d found) — manual intervention required", rec.ID, len(orphans))
		}
		adopted := orphans[0]
		poRec = &adopted
		// Stamp po_id back onto the requisition so subsequent
		// retries hit the fast Get-by-id path. Failure here is
		// non-fatal — the orphan stays adoptable on the next run
		// and the downstream status flip's persist() handles the
		// stamping again. We do not return early because the
		// caller's intent (transition to "ordered") is what
		// matters, and the persist() at the bottom of Convert
		// re-stamps po_id + ordered_at atomically with status.
		current["po_id"] = poRec.ID.String()
	} else {
		// Transpose requisition lines into PO lines. The PO's
		// `lines[]` shape is {item_id, qty, unit_price, line_total};
		// we map estimated_unit_price → unit_price so the PO opens
		// at the budgeted price (procurement edits it as needed
		// after supplier confirms).
		poLines := make([]map[string]any, 0, len(lines.Lines))
		subtotal := decimal.Zero
		for _, line := range lines.Lines {
			lineTotal := line.LineTotal
			if lineTotal.IsZero() {
				lineTotal = line.Qty.Mul(line.EstimatedUnitPrice)
			}
			poLines = append(poLines, map[string]any{
				"item_id":     line.ItemID,
				"description": line.Description,
				"qty":         line.Qty.InexactFloat64(),
				"uom":         line.UOM,
				"unit_price":  line.EstimatedUnitPrice.InexactFloat64(),
				"line_total":  lineTotal.InexactFloat64(),
			})
			subtotal = subtotal.Add(lineTotal)
		}
		currency := stringOr(current, "currency", "USD")
		requisitionNumber := stringOr(current, "requisition_number", "")
		orderDate := now.Format("2006-01-02")
		body := map[string]any{
			"po_number":      requisitionNumber, // procurement renames as needed
			"supplier_id":    supplierID.String(),
			"order_date":     orderDate,
			"lines":          poLines,
			"subtotal":       subtotal.InexactFloat64(),
			"total":          subtotal.InexactFloat64(),
			"currency":       currency,
			"status":         "draft",
			"requisition_id": rec.ID.String(),
		}
		// json.Marshal on a map of primitive types (strings, float64,
		// []map[string]any of the same) cannot return a non-nil error
		// at the runtime level — but we surface it anyway so that a
		// future change that introduces a non-serializable type (a
		// channel, a func, a cyclic struct) fails loudly here instead
		// of silently writing an empty `Data` to the KRecord and
		// propagating an unparseable PO through the rest of the
		// system. Same defensive shape as persist() below.
		bytes, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("sales: marshal purchase_order body: %w", err)
		}
		poRec, err = p.records.Create(ctx, record.KRecord{
			ID:        uuid.New(),
			TenantID:  tenantID,
			KType:     KTypePurchaseOrder,
			Data:      bytes,
			CreatedBy: actorID,
		})
		if err != nil {
			return nil, fmt.Errorf("sales: create purchase_order: %w", err)
		}
		current["po_id"] = poRec.ID.String()
		// Persist the po_id stamp BEFORE the status flip so a
		// crash between the two leaves the requisition with the
		// po_id but in "approved" — Convert resumes correctly.
		//
		// Same defensive marshal as above: the `current` map is
		// hydrated from the requisition's JSONB payload (so every
		// value is already a JSON-representable primitive), but a
		// future change that stashes a non-serializable value on
		// `current` would otherwise silently truncate the record.
		rec.Data, err = json.Marshal(current)
		if err != nil {
			return nil, fmt.Errorf("sales: marshal requisition after po stamp: %w", err)
		}
		rec.UpdatedBy = &actorID
		updated, err := p.records.Update(ctx, *rec)
		if err != nil {
			return nil, fmt.Errorf("sales: persist po_id: %w", err)
		}
		rec = updated
		if err := json.Unmarshal(rec.Data, &current); err != nil {
			return nil, fmt.Errorf("sales: re-decode requisition after po stamp: %w", err)
		}
	}

	current["status"] = RequisitionStatusOrdered
	current["po_id"] = poRec.ID.String()
	current["ordered_at"] = now.Format(time.RFC3339)
	return p.persist(ctx, rec, current, &actorID)
}

// Cancel marks a requisition as cancelled. Permitted from any
// pre-ordered state and is a pure status flip with no posting
// side-effects.
//
// Ordered requisitions cannot be cancelled — the PO has already
// been opened and may have downstream traffic (goods receipt,
// supplier confirmation). Operators must cancel the PO through
// the PO surface, which is its own state machine. The requisition
// remains in "ordered" so the audit trail of "this PO came from
// this requisition" stays intact.
func (p *RequisitionPoster) Cancel(ctx context.Context, tenantID, requisitionID, actorID uuid.UUID) (*record.KRecord, error) {
	rec, current, err := p.loadRequisition(ctx, tenantID, requisitionID)
	if err != nil {
		return nil, err
	}
	status := stringOr(current, "status", RequisitionStatusRequested)
	if status == RequisitionStatusCancelled {
		return rec, nil
	}
	if status == RequisitionStatusOrdered {
		return nil, fmt.Errorf("%w: ordered requisitions cannot be cancelled (cancel the PO instead)", ErrInvalidRequisitionState)
	}
	current["status"] = RequisitionStatusCancelled
	return p.persist(ctx, rec, current, &actorID)
}

// loadRequisition fetches the requisition KRecord and decodes the
// JSONB header into a map. Centralised here so every transition
// runs the same not-a-requisition guard.
func (p *RequisitionPoster) loadRequisition(ctx context.Context, tenantID, requisitionID uuid.UUID) (*record.KRecord, map[string]any, error) {
	if p == nil || p.records == nil {
		return nil, nil, errors.New("sales: requisition poster not configured")
	}
	if tenantID == uuid.Nil || requisitionID == uuid.Nil {
		return nil, nil, errors.New("sales: tenant_id and requisition_id required")
	}
	rec, err := p.records.Get(ctx, tenantID, requisitionID)
	if err != nil {
		if errors.Is(err, record.ErrNotFound) {
			return nil, nil, ErrRequisitionNotFound
		}
		return nil, nil, fmt.Errorf("sales: load requisition %s: %w", requisitionID, err)
	}
	if rec == nil || rec.KType != KTypePurchaseRequisition {
		return nil, nil, ErrNotRequisition
	}
	var current map[string]any
	if err := json.Unmarshal(rec.Data, &current); err != nil {
		return nil, nil, fmt.Errorf("sales: decode requisition: %w", err)
	}
	return rec, current, nil
}

// persist writes the mutated payload back to the record store with
// optimistic concurrency. Mirrors POSPoster's pattern so the audit
// log and event outbox fire on every transition.
func (p *RequisitionPoster) persist(ctx context.Context, rec *record.KRecord, body map[string]any, actorID *uuid.UUID) (*record.KRecord, error) {
	bytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("sales: marshal requisition: %w", err)
	}
	rec.Data = bytes
	rec.UpdatedBy = actorID
	updated, err := p.records.Update(ctx, *rec)
	if err != nil {
		return nil, fmt.Errorf("sales: persist requisition: %w", err)
	}
	return updated, nil
}
