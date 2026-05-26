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

// RegisterRequisitionTools attaches the Phase N9b purchase
// requisition state-machine tools to an executor. Mirrors
// RegisterSalesReturnsTools — wiring runs at service startup once
// the requisition poster is available. A nil poster is tolerated
// (commit mode returns a clear error) so tests that never spin up
// the record schema still pass.
//
// Four tools are registered:
//
//	procurement.create_requisition         — drafts a requisition (requested state).
//	procurement.approve_requisition        — requested → approved (status flip).
//	procurement.convert_requisition_to_po  — approved → ordered (allocates PO).
//	procurement.cancel_requisition         — pre-ordered → cancelled (status flip).
//
// All require confirmation: create writes a new KRecord; convert
// allocates a procurement.purchase_order; approve/cancel are status
// flips but the audit log still captures the human-in-the-loop
// intent.
func RegisterRequisitionTools(x *Executor, recordStore *record.PGStore, poster *sales.RequisitionPoster) {
	x.Register(&createRequisitionTool{records: recordStore})
	x.Register(&requisitionTransitionTool{
		name:    "procurement.approve_requisition",
		verb:    "approve",
		dryRunF: "Would approve requisition %s",
		commitF: "Approved requisition %s",
		fn:      func(p *sales.RequisitionPoster) requisitionTransitionFn { return p.Approve },
		poster:  poster,
	})
	x.Register(&requisitionTransitionTool{
		name:    "procurement.convert_requisition_to_po",
		verb:    "convert",
		dryRunF: "Would convert requisition %s into a purchase order",
		commitF: "Converted requisition %s; purchase order created",
		fn:      func(p *sales.RequisitionPoster) requisitionTransitionFn { return p.Convert },
		poster:  poster,
	})
	x.Register(&requisitionTransitionTool{
		name:    "procurement.cancel_requisition",
		verb:    "cancel",
		dryRunF: "Would cancel requisition %s",
		commitF: "Cancelled requisition %s",
		fn:      func(p *sales.RequisitionPoster) requisitionTransitionFn { return p.Cancel },
		poster:  poster,
	})
}

// ----- procurement.create_requisition -----

// createRequisitionInput is the JSON body the LLM produces when
// drafting a new requisition. Fields mirror the
// procurement.purchase_requisition schema; we forward the payload
// verbatim to record.PGStore.Create so the schema validator is the
// single source of truth for what's accepted.
type createRequisitionInput struct {
	RequisitionNumber string          `json:"requisition_number,omitempty"`
	RequestedBy       uuid.UUID       `json:"requested_by"`
	Department        string          `json:"department,omitempty"`
	CostCenter        string          `json:"cost_center,omitempty"`
	SupplierID        uuid.UUID       `json:"supplier_id,omitempty"`
	RequestDate       string          `json:"request_date"`
	NeededBy          string          `json:"needed_by,omitempty"`
	Justification     string          `json:"justification,omitempty"`
	Lines             json.RawMessage `json:"lines"`
	Subtotal          float64         `json:"subtotal,omitempty"`
	Currency          string          `json:"currency,omitempty"`
}

type createRequisitionTool struct {
	records *record.PGStore
}

// Name returns the wire identifier the agent resolver uses to
// dispatch tool calls. Stable string — changing it requires a
// migration of any saved playbooks that reference it.
func (t *createRequisitionTool) Name() string { return "procurement.create_requisition" }

// RequiresConfirmation flags the tool as side-effectful so the
// executor surfaces a Preview before the Commit phase: a draft
// requisition is an audit-visible record (it shows up in approver
// queues) and must not be auto-created on a hallucinated invocation.
func (t *createRequisitionTool) RequiresConfirmation() bool { return true }

// Invoke validates the agent input, then either returns a dry-run
// preview of the requisition body (when inv.Mode == ModeDryRun) or
// persists a procurement.purchase_requisition KRecord in
// `requested` state. The Approve / Convert / Cancel transitions
// are exposed as separate tools to keep this entrypoint a pure
// create.
func (t *createRequisitionTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createRequisitionInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.RequestedBy == uuid.Nil {
		return nil, errors.New("procurement.create_requisition: requested_by required")
	}
	if in.RequestDate == "" {
		return nil, errors.New("procurement.create_requisition: request_date required")
	}
	currency := in.Currency
	if currency == "" {
		currency = "USD"
	}
	lines := in.Lines
	if len(lines) == 0 {
		lines = json.RawMessage("[]")
	}
	// Required fields go in the base map; optional fields are
	// conditionally included only when the LLM supplied a non-empty
	// value. Mirrors crm_tools.go createOpportunityTool (lines
	// 61-80), which omits Owner/Contact/Organization/Notes when
	// the caller didn't set them so the resulting JSONB doesn't
	// accumulate empty-string keys that downstream BI and audit
	// surfaces have to filter out. needed_by additionally MUST
	// be omitted on empty: the KType validator rejects empty
	// strings for date-typed fields (parses as ISO-8601 / RFC3339
	// and fails), so unconditionally including "needed_by": ""
	// would fail validation.
	body := map[string]any{
		"requested_by": in.RequestedBy.String(),
		"request_date": in.RequestDate,
		"lines":        lines,
		"subtotal":     in.Subtotal,
		"currency":     currency,
		"status":       sales.RequisitionStatusRequested,
	}
	if in.RequisitionNumber != "" {
		body["requisition_number"] = in.RequisitionNumber
	}
	if in.Department != "" {
		body["department"] = in.Department
	}
	if in.CostCenter != "" {
		body["cost_center"] = in.CostCenter
	}
	if in.Justification != "" {
		body["justification"] = in.Justification
	}
	if in.SupplierID != uuid.Nil {
		body["supplier_id"] = in.SupplierID.String()
	}
	if in.NeededBy != "" {
		body["needed_by"] = in.NeededBy
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(body)
		return &Result{
			Summary: fmt.Sprintf("Would create requisition for %.2f %s", in.Subtotal, currency),
			Preview: preview,
		}, nil
	}
	if t.records == nil {
		return nil, errors.New("procurement.create_requisition: record store not configured")
	}
	bytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("procurement.create_requisition: marshal body: %w", err)
	}
	rec, err := t.records.Create(ctx, record.KRecord{
		ID:        uuid.New(),
		TenantID:  inv.TenantID,
		KType:     sales.KTypePurchaseRequisition,
		Data:      bytes,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, fmt.Errorf("procurement.create_requisition: create record: %w", err)
	}
	// Preview format aligned with the other agent tools in this
	// package (finance, inventory, lms, crm, and the
	// requisitionTransitionTool below): the full KRecord envelope
	// is marshalled so downstream UI / audit consumers see a
	// consistent shape — id, tenant_id, ktype, data, version,
	// timestamps — instead of getting the bare Data payload from
	// some tools and the envelope from others.
	preview, _ := json.Marshal(rec)
	return &Result{
		Summary: fmt.Sprintf("Drafted requisition %s (state=requested, subtotal=%.2f %s)", rec.ID, in.Subtotal, currency),
		Preview: preview,
		Record:  rec,
		Extra:   map[string]any{"requisition_id": rec.ID.String()},
	}, nil
}

// ----- procurement.{approve,convert,cancel}_requisition -----
//
// The agent tool *name* stays `procurement.convert_requisition_to_po`
// (a descriptive tool-registry identifier) but the *verb* reported
// in tool results is `convert` to match every other surface (the
// HTTP route `/convert`, the KChat command `convert`, the TS client
// `convert`, and the frontend `resolveVerb`). The workflow schema
// action is also `convert` (see internal/sales/requisition.go) so
// audit logs that record `verb=` and workflow-derived breadcrumbs
// agree across every code path that touches the transition.

type requisitionTransitionFn func(ctx context.Context, tenantID, requisitionID, actorID uuid.UUID) (*record.KRecord, error)

type requisitionTransitionInput struct {
	RequisitionID uuid.UUID `json:"requisition_id"`
}

type requisitionTransitionTool struct {
	name    string
	verb    string
	dryRunF string
	commitF string
	fn      func(p *sales.RequisitionPoster) requisitionTransitionFn
	poster  *sales.RequisitionPoster
}

// Name returns the wire identifier (the per-instance verb, e.g.
// procurement.approve_requisition / .convert_requisition /
// .cancel_requisition).
func (t *requisitionTransitionTool) Name() string { return t.name }

// RequiresConfirmation flags the tool as side-effectful so the
// executor stages a Preview before Commit. Convert in particular
// allocates a procurement.purchase_order via the poster's
// idempotent Create+Patch path, so a blind invocation could spawn a
// duplicate PO; the preview lets the operator confirm the
// requisition target before the irreversible step runs.
func (t *requisitionTransitionTool) RequiresConfirmation() bool { return true }

// Invoke runs the lifecycle transition through the shared
// RequisitionPoster so HTTP, KChat, and agent paths all converge on
// the same state-machine semantics. Dry-run returns the human
// description from dryRunF; commit returns the updated KRecord and
// (for Convert) stamps the freshly-allocated po_id onto Extra.
func (t *requisitionTransitionTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in requisitionTransitionInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.RequisitionID == uuid.Nil {
		return nil, fmt.Errorf("%s: requisition_id required", t.name)
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf(t.dryRunF, in.RequisitionID),
			Preview: preview,
		}, nil
	}
	if t.poster == nil {
		return nil, fmt.Errorf("%s: requisition poster not configured", t.name)
	}
	rec, err := t.fn(t.poster)(ctx, inv.TenantID, in.RequisitionID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(rec)
	return &Result{
		Summary: fmt.Sprintf(t.commitF, in.RequisitionID),
		Preview: body,
		Record:  rec,
		Extra:   map[string]any{"requisition_id": in.RequisitionID.String(), "verb": t.verb},
	}, nil
}
