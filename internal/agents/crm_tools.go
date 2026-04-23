package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// RegisterCRMTools attaches every Phase B CRM / Tasks / Approvals tool to
// an executor in one call. Callers wire this during agent-tools service
// startup; tests can wire it around an in-memory executor too.
func RegisterCRMTools(x *Executor) {
	x.Register(&createDealTool{executor: x})
	x.Register(&advanceDealTool{executor: x})
	x.Register(&summarizePipelineTool{executor: x})
	x.Register(&createTaskTool{executor: x})
	x.Register(&requestApprovalTool{executor: x})
	x.Register(&decideApprovalTool{executor: x})
	x.Register(&createCustomerTool{executor: x})
	x.Register(&createSupplierTool{executor: x})
}

// ----- crm.create_deal -----

type createDealInput struct {
	Name         string    `json:"name"`
	Stage        string    `json:"stage,omitempty"`
	Amount       float64   `json:"amount,omitempty"`
	Currency     string    `json:"currency,omitempty"`
	Owner        uuid.UUID `json:"owner,omitempty"`
	Contact      uuid.UUID `json:"contact,omitempty"`
	Organization uuid.UUID `json:"organization,omitempty"`
	Notes        string    `json:"notes,omitempty"`
}

type createDealTool struct{ executor *Executor }

func (t *createDealTool) Name() string                  { return "crm.create_deal" }
func (t *createDealTool) RequiresConfirmation() bool    { return false }
func (t *createDealTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createDealInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, errors.New("crm.create_deal: name required")
	}
	if in.Stage == "" {
		in.Stage = "qualification"
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	data := map[string]any{
		"name":     in.Name,
		"stage":    in.Stage,
		"currency": in.Currency,
	}
	if in.Amount > 0 {
		data["amount"] = in.Amount
	}
	if in.Owner != uuid.Nil {
		data["owner"] = in.Owner.String()
	}
	if in.Contact != uuid.Nil {
		data["contact"] = in.Contact.String()
	}
	if in.Organization != uuid.Nil {
		data["organization"] = in.Organization.String()
	}
	if in.Notes != "" {
		data["notes"] = in.Notes
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would create deal %q in stage %s", in.Name, in.Stage),
			Preview: preview,
		}, nil
	}

	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     crm.KTypeDeal,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	// Best-effort: start the pipeline run so /crm.advance_deal works next.
	run, _ := t.executor.workflow.StartRun(
		ctx, inv.TenantID, crm.WorkflowDealPipeline,
		rec.ID, in.Stage, inv.ActorID,
	)
	return &Result{
		Summary: fmt.Sprintf("Created deal %s in stage %s", rec.ID, in.Stage),
		Record:  rec,
		Run:     run,
	}, nil
}

// ----- crm.advance_deal -----

type advanceDealInput struct {
	RecordID uuid.UUID `json:"record_id"`
	Action   string    `json:"action"`
}

type advanceDealTool struct{ executor *Executor }

func (t *advanceDealTool) Name() string               { return "crm.advance_deal" }
func (t *advanceDealTool) RequiresConfirmation() bool { return false }
func (t *advanceDealTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in advanceDealInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.RecordID == uuid.Nil || in.Action == "" {
		return nil, errors.New("crm.advance_deal: record_id and action required")
	}
	run, err := t.executor.workflow.GetRunByRecord(ctx, inv.TenantID, in.RecordID)
	if err != nil && !errors.Is(err, workflow.ErrRunNotFound) {
		return nil, err
	}
	if inv.Mode == ModeDryRun {
		current := ""
		if run != nil {
			current = run.State
		}
		preview, _ := json.Marshal(map[string]any{
			"current_state": current,
			"action":        in.Action,
		})
		return &Result{
			Summary: fmt.Sprintf("Would apply %q to deal %s (current=%s)", in.Action, in.RecordID, current),
			Preview: preview,
			Run:     run,
		}, nil
	}
	if run == nil {
		// Start a fresh run at the workflow's initial state so the
		// transition call has something to advance.
		run, err = t.executor.workflow.StartRun(
			ctx, inv.TenantID, crm.WorkflowDealPipeline,
			in.RecordID, "", inv.ActorID,
		)
		if err != nil {
			return nil, err
		}
	}
	run, err = t.executor.workflow.Transition(ctx, inv.TenantID, run.ID, in.Action, inv.ActorID)
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Deal %s → %s", in.RecordID, run.State),
		Run:     run,
	}, nil
}

// ----- crm.summarize_pipeline -----

type summarizePipelineTool struct{ executor *Executor }

func (t *summarizePipelineTool) Name() string               { return "crm.summarize_pipeline" }
func (t *summarizePipelineTool) RequiresConfirmation() bool { return false }
func (t *summarizePipelineTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	rows, err := t.executor.records.List(ctx, inv.TenantID, record.ListFilter{
		KType: crm.KTypeDeal,
		Limit: 500,
	})
	if err != nil {
		return nil, err
	}
	type bucket struct {
		Count int     `json:"count"`
		Total float64 `json:"total"`
	}
	totals := map[string]*bucket{}
	for _, r := range rows {
		var payload struct {
			Stage  string  `json:"stage"`
			Amount float64 `json:"amount"`
		}
		_ = json.Unmarshal(r.Data, &payload)
		key := payload.Stage
		if key == "" {
			key = "unknown"
		}
		b, ok := totals[key]
		if !ok {
			b = &bucket{}
			totals[key] = b
		}
		b.Count++
		b.Total += payload.Amount
	}
	summary, _ := json.Marshal(totals)
	return &Result{
		Summary: fmt.Sprintf("Pipeline summary across %d deals", len(rows)),
		Preview: summary,
	}, nil
}

// ----- tasks.create_task -----

type createTaskInput struct {
	Title       string    `json:"title"`
	Assignee    uuid.UUID `json:"assignee"`
	DueDate     string    `json:"due_date,omitempty"`
	Description string    `json:"description,omitempty"`
	LinkedKType string    `json:"linked_ktype,omitempty"`
	LinkedID    string    `json:"linked_id,omitempty"`
}

type createTaskTool struct{ executor *Executor }

func (t *createTaskTool) Name() string               { return "tasks.create_task" }
func (t *createTaskTool) RequiresConfirmation() bool { return false }
func (t *createTaskTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createTaskInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Title == "" {
		return nil, errors.New("tasks.create_task: title required")
	}
	if in.Assignee == uuid.Nil {
		in.Assignee = inv.ActorID
	}
	data := map[string]any{
		"title":    in.Title,
		"assignee": in.Assignee.String(),
		"status":   "open",
	}
	if in.DueDate != "" {
		data["due_date"] = in.DueDate
	}
	if in.Description != "" {
		data["description"] = in.Description
	}
	if in.LinkedKType != "" {
		data["linked_ktype"] = in.LinkedKType
	}
	if in.LinkedID != "" {
		data["linked_id"] = in.LinkedID
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would create task %q for %s", in.Title, in.Assignee),
			Preview: preview,
		}, nil
	}
	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     crm.KTypeTask,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Created task %s for %s", rec.ID, in.Assignee),
		Record:  rec,
	}, nil
}

// ----- approvals.request -----

type requestApprovalInput struct {
	RecordKType string                 `json:"record_ktype"`
	RecordID    uuid.UUID              `json:"record_id"`
	Chain       workflow.ApprovalChain `json:"chain"`
}

type requestApprovalTool struct{ executor *Executor }

func (t *requestApprovalTool) Name() string               { return "approvals.request" }
func (t *requestApprovalTool) RequiresConfirmation() bool { return false }
func (t *requestApprovalTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in requestApprovalInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.RecordKType == "" || in.RecordID == uuid.Nil {
		return nil, errors.New("approvals.request: record_ktype and record_id required")
	}
	if len(in.Chain.Steps) == 0 {
		return nil, errors.New("approvals.request: chain.steps required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in.Chain)
		return &Result{
			Summary: fmt.Sprintf("Would request approval on %s (%s)", in.RecordKType, in.RecordID),
			Preview: preview,
		}, nil
	}
	approval, err := t.executor.workflow.RequestApproval(
		ctx, inv.TenantID, in.RecordKType, in.RecordID, in.Chain, inv.ActorID,
	)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(approval)
	return &Result{
		Summary: fmt.Sprintf("Requested approval %s on %s", approval.ID, in.RecordID),
		Preview: body,
		Extra:   map[string]any{"approval_id": approval.ID},
	}, nil
}

// ----- approvals.decide -----

// decideApprovalInput carries a single approver's decision. The actor is
// taken from the Invocation envelope — the tool does NOT accept an
// ActorID override so a malicious agent can't vote as someone else.
type decideApprovalInput struct {
	ApprovalID uuid.UUID `json:"approval_id"`
	Decision   string    `json:"decision"`
}

type decideApprovalTool struct{ executor *Executor }

func (t *decideApprovalTool) Name() string               { return "approvals.decide" }
func (t *decideApprovalTool) RequiresConfirmation() bool { return true }
func (t *decideApprovalTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in decideApprovalInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ApprovalID == uuid.Nil || in.Decision == "" {
		return nil, errors.New("approvals.decide: approval_id and decision required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would record %s on approval %s", in.Decision, in.ApprovalID),
			Preview: preview,
		}, nil
	}
	approval, err := t.executor.workflow.Decide(
		ctx, inv.TenantID, in.ApprovalID, in.Decision, inv.ActorID,
	)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(approval)
	return &Result{
		Summary: fmt.Sprintf("Approval %s → %s", approval.ID, approval.State),
		Preview: body,
	}, nil
}

// ----- crm.create_customer -----

type createCustomerInput struct {
	Name                string    `json:"name"`
	CustomerGroup       string    `json:"customer_group,omitempty"`
	CreditLimit         float64   `json:"credit_limit,omitempty"`
	DefaultTaxCode      string    `json:"default_tax_code,omitempty"`
	DefaultPaymentTerms string    `json:"default_payment_terms,omitempty"`
	Currency            string    `json:"currency,omitempty"`
	Organization        uuid.UUID `json:"organization,omitempty"`
	Owner               uuid.UUID `json:"owner,omitempty"`
}

type createCustomerTool struct{ executor *Executor }

func (t *createCustomerTool) Name() string               { return "crm.create_customer" }
func (t *createCustomerTool) RequiresConfirmation() bool { return false }
func (t *createCustomerTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createCustomerInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, errors.New("crm.create_customer: name required")
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	data := map[string]any{
		"name":            in.Name,
		"currency":        in.Currency,
		"status":          "active",
		"ar_aging_bucket": "current",
	}
	if in.CustomerGroup != "" {
		data["customer_group"] = in.CustomerGroup
	}
	if in.CreditLimit > 0 {
		data["credit_limit"] = in.CreditLimit
	}
	if in.DefaultTaxCode != "" {
		data["default_tax_code"] = in.DefaultTaxCode
	}
	if in.DefaultPaymentTerms != "" {
		data["default_payment_terms"] = in.DefaultPaymentTerms
	}
	if in.Organization != uuid.Nil {
		data["organization"] = in.Organization.String()
	}
	if in.Owner != uuid.Nil {
		data["owner"] = in.Owner.String()
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would create customer %q", in.Name),
			Preview: preview,
		}, nil
	}
	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     crm.KTypeCustomer,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Created customer %s", rec.ID),
		Record:  rec,
	}, nil
}

// ----- crm.create_supplier -----

type createSupplierInput struct {
	Name                string    `json:"name"`
	SupplierGroup       string    `json:"supplier_group,omitempty"`
	DefaultPaymentTerms string    `json:"default_payment_terms,omitempty"`
	Currency            string    `json:"currency,omitempty"`
	Organization        uuid.UUID `json:"organization,omitempty"`
	Owner               uuid.UUID `json:"owner,omitempty"`
}

type createSupplierTool struct{ executor *Executor }

func (t *createSupplierTool) Name() string               { return "crm.create_supplier" }
func (t *createSupplierTool) RequiresConfirmation() bool { return false }
func (t *createSupplierTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createSupplierInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, errors.New("crm.create_supplier: name required")
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	data := map[string]any{
		"name":            in.Name,
		"currency":        in.Currency,
		"status":          "active",
		"ap_aging_bucket": "current",
	}
	if in.SupplierGroup != "" {
		data["supplier_group"] = in.SupplierGroup
	}
	if in.DefaultPaymentTerms != "" {
		data["default_payment_terms"] = in.DefaultPaymentTerms
	}
	if in.Organization != uuid.Nil {
		data["organization"] = in.Organization.String()
	}
	if in.Owner != uuid.Nil {
		data["owner"] = in.Owner.String()
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would create supplier %q", in.Name),
			Preview: preview,
		}, nil
	}
	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     crm.KTypeSupplier,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Created supplier %s", rec.ID),
		Record:  rec,
	}, nil
}

// decodeInputs unmarshals the Invocation's Inputs into the tool-specific
// struct. Tools call this in their Invoke method; keeping the helper in
// one place gives consistent error messages.
func decodeInputs(inv Invocation, dst any) error {
	if len(inv.Inputs) == 0 {
		return fmt.Errorf("agents: tool %q: inputs required", inv.ToolName)
	}
	if err := json.Unmarshal(inv.Inputs, dst); err != nil {
		return fmt.Errorf("agents: tool %q: decode inputs: %w", inv.ToolName, err)
	}
	return nil
}
