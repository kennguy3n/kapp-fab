package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/sales"
)

// RegisterSalesReturnsTools attaches the Phase N9a sales return
// state-machine tools to an executor. Mirrors RegisterFinanceTools
// — wiring runs at service startup once the return poster is
// available. A nil poster is tolerated (commit mode returns a clear
// error) so tests that never spin up the ledger/inventory schema
// still pass.
//
// Five tools are registered:
//
//	sales.create_return  — drafts a sales.return KRecord (requested state).
//	sales.approve_return — requested → approved (status flip).
//	sales.receive_return — approved → received (posts inventory moves).
//	sales.refund_return  — received → refunded (posts credit-note JE).
//	sales.cancel_return  — pre-refund → cancelled (status flip).
//
// All five require confirmation: create_return writes a new KRecord;
// the four lifecycle transitions either move money or move stock.
func RegisterSalesReturnsTools(x *Executor, recordStore *record.PGStore, poster *sales.ReturnPoster) {
	x.Register(&createReturnTool{records: recordStore})
	x.Register(&returnTransitionTool{
		name:    "sales.approve_return",
		verb:    "approve",
		dryRunF: "Would approve return %s",
		commitF: "Approved return %s",
		fn:      func(p *sales.ReturnPoster) returnTransitionFn { return p.Approve },
		poster:  poster,
	})
	x.Register(&returnTransitionTool{
		name:    "sales.receive_return",
		verb:    "receive",
		dryRunF: "Would receive return %s (posts inventory moves)",
		commitF: "Received return %s; inventory receipts posted",
		fn:      func(p *sales.ReturnPoster) returnTransitionFn { return p.Receive },
		poster:  poster,
	})
	x.Register(&returnTransitionTool{
		name:    "sales.refund_return",
		verb:    "refund",
		dryRunF: "Would refund return %s (posts credit-note JE)",
		commitF: "Refunded return %s; credit-note JE posted",
		fn:      func(p *sales.ReturnPoster) returnTransitionFn { return p.Refund },
		poster:  poster,
	})
	x.Register(&returnTransitionTool{
		name:    "sales.cancel_return",
		verb:    "cancel",
		dryRunF: "Would cancel return %s",
		commitF: "Cancelled return %s",
		fn:      func(p *sales.ReturnPoster) returnTransitionFn { return p.Cancel },
		poster:  poster,
	})
}

// ----- sales.create_return -----

// createReturnInput is the JSON body the LLM produces when drafting a
// new return. Fields mirror the sales.return schema; we forward the
// payload verbatim to record.PGStore.Create so the schema validator
// is the single source of truth for what's accepted (no duplicate
// validation logic between agent + HTTP).
type createReturnInput struct {
	ReturnNumber      string          `json:"return_number,omitempty"`
	OriginalInvoiceID uuid.UUID       `json:"original_invoice_id"`
	CustomerID        uuid.UUID       `json:"customer_id"`
	WarehouseID       uuid.UUID       `json:"warehouse_id"`
	ReturnDate        string          `json:"return_date"`
	Reason            string          `json:"reason,omitempty"`
	Lines             json.RawMessage `json:"lines"`
	Subtotal          float64         `json:"subtotal,omitempty"`
	TaxAmount         float64         `json:"tax_amount,omitempty"`
	Total             float64         `json:"total"`
	Currency          string          `json:"currency,omitempty"`
}

type createReturnTool struct {
	records *record.PGStore
}

// Name satisfies the executor Tool interface. The wire-stable id
// matches the constant referenced from KChat command help text and
// from the agent-tool registry in services/api/deps_build.go.
func (t *createReturnTool) Name() string { return "sales.create_return" }

// RequiresConfirmation is true because create_return writes a fresh
// sales.return KRecord — the operator should always see the dry-run
// preview before the row is materialised.
func (t *createReturnTool) RequiresConfirmation() bool { return true }

// Invoke validates the inputs, returns a dry-run preview when called
// in ModeDryRun, and writes the sales.return KRecord via the record
// store when called in ModeCommit. The schema validator on Create()
// is the single source of truth for what the payload may contain.
func (t *createReturnTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createReturnInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.OriginalInvoiceID == uuid.Nil {
		return nil, errors.New("sales.create_return: original_invoice_id required")
	}
	if in.CustomerID == uuid.Nil {
		return nil, errors.New("sales.create_return: customer_id required")
	}
	if in.WarehouseID == uuid.Nil {
		return nil, errors.New("sales.create_return: warehouse_id required")
	}
	if in.ReturnDate == "" {
		return nil, errors.New("sales.create_return: return_date required")
	}
	if in.Total <= 0 {
		return nil, errors.New("sales.create_return: total must be > 0")
	}
	currency := in.Currency
	if currency == "" {
		currency = "USD"
	}
	lines := in.Lines
	if len(lines) == 0 {
		lines = json.RawMessage("[]")
	}
	body := map[string]any{
		"return_number":       in.ReturnNumber,
		"original_invoice_id": in.OriginalInvoiceID.String(),
		"customer_id":         in.CustomerID.String(),
		"warehouse_id":        in.WarehouseID.String(),
		"return_date":         in.ReturnDate,
		"reason":              in.Reason,
		"lines":               lines,
		"subtotal":            in.Subtotal,
		"tax_amount":          in.TaxAmount,
		"total":               in.Total,
		"currency":            currency,
		"status":              sales.ReturnStatusRequested,
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(body)
		return &Result{
			Summary: fmt.Sprintf("Would create return against invoice %s for %.2f %s", in.OriginalInvoiceID, in.Total, currency),
			Preview: preview,
		}, nil
	}
	if t.records == nil {
		return nil, errors.New("sales.create_return: record store not configured")
	}
	bytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("sales.create_return: marshal body: %w", err)
	}
	rec, err := t.records.Create(ctx, record.KRecord{
		ID:        uuid.New(),
		TenantID:  inv.TenantID,
		KType:     sales.KTypeSalesReturn,
		Data:      bytes,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, fmt.Errorf("sales.create_return: create record: %w", err)
	}
	// Preview format aligned with the other agent tools in this
	// package (finance, inventory, lms, crm, and the
	// returnTransitionTool below): the full KRecord envelope is
	// marshalled so downstream UI / audit consumers see a
	// consistent shape — id, tenant_id, ktype, data, version,
	// timestamps — instead of getting the bare Data payload from
	// some tools and the envelope from others.
	preview, _ := json.Marshal(rec)
	return &Result{
		Summary: fmt.Sprintf("Drafted return %s (state=requested, total=%.2f %s)", rec.ID, in.Total, currency),
		Preview: preview,
		Record:  rec,
		Extra:   map[string]any{"return_id": rec.ID.String()},
	}, nil
}

// ----- sales.{approve,receive,refund,cancel}_return -----

// returnTransitionFn is the shared shape of every state-machine
// transition on sales.ReturnPoster. We thread the bound method
// through a per-tool closure so each tool's dry-run / commit
// branches stay terse and identical-modulo-verbs.
type returnTransitionFn func(ctx context.Context, tenantID, returnID, actorID uuid.UUID) (*record.KRecord, error)

type returnTransitionInput struct {
	ReturnID uuid.UUID `json:"return_id"`
}

// returnTransitionTool is the shared implementation behind the four
// transition tools. Each one differs only in name, summary
// templates, and the bound poster method — collapsing them onto one
// type means the audit trail, validation, and error paths are
// authored exactly once.
type returnTransitionTool struct {
	name    string
	verb    string
	dryRunF string
	commitF string
	fn      func(p *sales.ReturnPoster) returnTransitionFn
	poster  *sales.ReturnPoster
}

// Name satisfies the executor Tool interface; the value is set per
// instance by RegisterSalesReturnsTools so the four lifecycle verbs
// (approve / receive / refund / cancel) appear in the tool catalog
// under distinct ids.
func (t *returnTransitionTool) Name() string { return t.name }

// RequiresConfirmation is true for every transition: receive posts
// inventory moves and refund posts a credit-note JE, both of which
// require operator sign-off in commit mode. Approve and cancel are
// pure status flips but still require confirmation so the audit log
// captures the human-in-the-loop intent.
func (t *returnTransitionTool) RequiresConfirmation() bool { return true }

// Invoke threads the bound poster method through a per-tool
// dispatch. Dry-run returns the rendered summary + input payload
// without touching state; commit calls the underlying transition
// and returns the updated KRecord so callers can inspect the
// stamped credit_note_id / journal_entry_id / received_at /
// refunded_at fields.
func (t *returnTransitionTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in returnTransitionInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ReturnID == uuid.Nil {
		return nil, fmt.Errorf("%s: return_id required", t.name)
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf(t.dryRunF, in.ReturnID),
			Preview: preview,
		}, nil
	}
	if t.poster == nil {
		return nil, fmt.Errorf("%s: return poster not configured", t.name)
	}
	rec, err := t.fn(t.poster)(ctx, inv.TenantID, in.ReturnID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(rec)
	return &Result{
		Summary: fmt.Sprintf(t.commitF, in.ReturnID),
		Preview: body,
		Record:  rec,
		Extra:   map[string]any{"return_id": in.ReturnID.String(), "verb": t.verb},
	}, nil
}
