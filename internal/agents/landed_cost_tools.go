package agents

// Phase N9c — Landed Cost agent tools.
//
// Exposes three tools so the LLM / KChat can drive the landed-cost
// flow without going through the HTTP surface:
//
//   * finance.create_landed_cost_voucher — header + charges + targets
//   * finance.allocate_landed_cost       — compute per-target shares
//   * finance.post_landed_cost           — write inventory_moves + JE
//
// Each tool supports dry-run mode that returns a preview JSON
// payload without mutating state, matching the convention
// established by the rest of the finance tool suite.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
)

// RegisterLandedCostTools attaches the three landed-cost tools to
// the executor. Wired by services/api/deps_build.go after
// landedCostStore has been constructed.
func RegisterLandedCostTools(x *Executor, store *finance.LandedCostStore) {
	x.Register(&createLandedCostVoucherTool{executor: x, store: store})
	x.Register(&allocateLandedCostTool{executor: x, store: store})
	x.Register(&postLandedCostTool{executor: x, store: store})
}

// ---------------------------------------------------------------------------
// finance.create_landed_cost_voucher
// ---------------------------------------------------------------------------

type createLandedCostVoucherInput struct {
	VoucherNumber    string                   `json:"voucher_number"`
	Description      string                   `json:"description,omitempty"`
	AllocationMethod string                   `json:"allocation_method,omitempty"`
	Charges          []landedCostChargeInput  `json:"charges"`
	Targets          []landedCostTargetInput  `json:"targets"`
}

type landedCostChargeInput struct {
	Description string          `json:"description"`
	Amount      decimal.Decimal `json:"amount"`
	AccountCode string          `json:"account_code,omitempty"`
}

type landedCostTargetInput struct {
	SourceKType string          `json:"source_ktype,omitempty"`
	SourceID    string          `json:"source_id"`
	ItemID      string          `json:"item_id"`
	WarehouseID string          `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	UnitCost    decimal.Decimal `json:"unit_cost"`
	Weight      decimal.Decimal `json:"weight,omitempty"`
}

type createLandedCostVoucherTool struct {
	executor *Executor
	store    *finance.LandedCostStore
}

// Name returns the executor-facing tool name.
func (t *createLandedCostVoucherTool) Name() string { return "finance.create_landed_cost_voucher" }

// RequiresConfirmation gates the create behind an explicit approve
// step — creating a voucher kicks off a state machine that writes
// to inventory + ledger on Post, so the gate is a defence-in-depth
// safeguard even though Create itself is harmless.
func (t *createLandedCostVoucherTool) RequiresConfirmation() bool { return true }

// Invoke writes the voucher header, then any provided charges and
// targets, returning the persisted ids.
func (t *createLandedCostVoucherTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createLandedCostVoucherInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.VoucherNumber == "" {
		return nil, errors.New("finance.create_landed_cost_voucher: voucher_number required")
	}
	if len(in.Charges) == 0 {
		return nil, errors.New("finance.create_landed_cost_voucher: at least one charge required")
	}
	if len(in.Targets) == 0 {
		return nil, errors.New("finance.create_landed_cost_voucher: at least one target required")
	}
	if in.AllocationMethod == "" {
		in.AllocationMethod = finance.LandedCostByQty
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would create landed cost voucher %q with %d charge(s) and %d target(s)", in.VoucherNumber, len(in.Charges), len(in.Targets)),
			Preview: preview,
		}, nil
	}

	if t.store == nil {
		return nil, errors.New("finance.create_landed_cost_voucher: store not wired")
	}

	voucher, err := t.store.CreateVoucher(ctx, finance.LandedCostVoucher{
		TenantID:         inv.TenantID,
		VoucherNumber:    in.VoucherNumber,
		Description:      in.Description,
		AllocationMethod: in.AllocationMethod,
		CreatedBy:        inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	for _, c := range in.Charges {
		if _, err := t.store.UpsertCharge(ctx, finance.LandedCostCharge{
			TenantID:    inv.TenantID,
			VoucherID:   voucher.ID,
			Description: c.Description,
			Amount:      c.Amount,
			AccountCode: c.AccountCode,
		}); err != nil {
			return nil, fmt.Errorf("finance.create_landed_cost_voucher: charge %q: %w", c.Description, err)
		}
	}
	for i, tgt := range in.Targets {
		sourceID, err := uuid.Parse(tgt.SourceID)
		if err != nil {
			return nil, fmt.Errorf("finance.create_landed_cost_voucher: target %d source_id invalid: %w", i, err)
		}
		itemID, err := uuid.Parse(tgt.ItemID)
		if err != nil {
			return nil, fmt.Errorf("finance.create_landed_cost_voucher: target %d item_id invalid: %w", i, err)
		}
		warehouseID, err := uuid.Parse(tgt.WarehouseID)
		if err != nil {
			return nil, fmt.Errorf("finance.create_landed_cost_voucher: target %d warehouse_id invalid: %w", i, err)
		}
		if _, err := t.store.UpsertTarget(ctx, finance.LandedCostTarget{
			TenantID:    inv.TenantID,
			VoucherID:   voucher.ID,
			SourceKType: tgt.SourceKType,
			SourceID:    sourceID,
			ItemID:      itemID,
			WarehouseID: warehouseID,
			Qty:         tgt.Qty,
			UnitCost:    tgt.UnitCost,
			Weight:      tgt.Weight,
		}); err != nil {
			return nil, fmt.Errorf("finance.create_landed_cost_voucher: target %d: %w", i, err)
		}
	}

	return &Result{
		Summary: fmt.Sprintf("Created landed cost voucher %s (%s) with %d charge(s) and %d target(s)", voucher.VoucherNumber, voucher.ID, len(in.Charges), len(in.Targets)),
		Record:  nil,
	}, nil
}

// ---------------------------------------------------------------------------
// finance.allocate_landed_cost
// ---------------------------------------------------------------------------

type allocateLandedCostInput struct {
	VoucherID string `json:"voucher_id"`
}

type allocateLandedCostTool struct {
	executor *Executor
	store    *finance.LandedCostStore
}

// Name returns the executor-facing tool name.
func (t *allocateLandedCostTool) Name() string { return "finance.allocate_landed_cost" }

// RequiresConfirmation is true because allocation transitions the
// voucher status from draft to allocated, which freezes the charge
// + target snapshots that posting will replay.
func (t *allocateLandedCostTool) RequiresConfirmation() bool { return true }

// Invoke runs the allocation arithmetic across every target and
// returns the per-target shares.
func (t *allocateLandedCostTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in allocateLandedCostInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.VoucherID == "" {
		return nil, errors.New("finance.allocate_landed_cost: voucher_id required")
	}
	voucherID, err := uuid.Parse(in.VoucherID)
	if err != nil {
		return nil, fmt.Errorf("finance.allocate_landed_cost: voucher_id invalid: %w", err)
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would allocate landed cost voucher %s", voucherID),
			Preview: preview,
		}, nil
	}

	if t.store == nil {
		return nil, errors.New("finance.allocate_landed_cost: store not wired")
	}
	targets, err := t.store.AllocateVoucher(ctx, inv.TenantID, voucherID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(targets)
	return &Result{
		Summary: fmt.Sprintf("Allocated landed cost voucher %s across %d target(s)", voucherID, len(targets)),
		Preview: body,
	}, nil
}

// ---------------------------------------------------------------------------
// finance.post_landed_cost
// ---------------------------------------------------------------------------

type postLandedCostInput struct {
	VoucherID string `json:"voucher_id"`
}

type postLandedCostTool struct {
	executor *Executor
	store    *finance.LandedCostStore
}

// Name returns the executor-facing tool name.
func (t *postLandedCostTool) Name() string { return "finance.post_landed_cost" }

// RequiresConfirmation is true because posting writes inventory
// moves and a booking JE — both are durable side-effects.
func (t *postLandedCostTool) RequiresConfirmation() bool { return true }

// Invoke writes per-target reversal + forward inventory moves and
// the booking JE, returning the persisted voucher and JE id. The
// flow is idempotent — a retry returns the existing JE.
func (t *postLandedCostTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in postLandedCostInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.VoucherID == "" {
		return nil, errors.New("finance.post_landed_cost: voucher_id required")
	}
	voucherID, err := uuid.Parse(in.VoucherID)
	if err != nil {
		return nil, fmt.Errorf("finance.post_landed_cost: voucher_id invalid: %w", err)
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would post landed cost voucher %s", voucherID),
			Preview: preview,
		}, nil
	}

	if t.store == nil {
		return nil, errors.New("finance.post_landed_cost: store not wired")
	}
	voucher, je, err := t.store.PostVoucher(ctx, inv.TenantID, voucherID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{
		"voucher":       voucher,
		"journal_entry": je,
	})
	return &Result{
		Summary: fmt.Sprintf("Posted landed cost voucher %s (JE %s)", voucher.VoucherNumber, je.ID),
		Preview: body,
	}, nil
}
