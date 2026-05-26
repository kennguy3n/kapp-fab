package agents

// Phase N9d — Cycle Count agent tools.
//
// Three tools registered on the executor:
//
//   inventory.create_cycle_count — opens a new draft session.
//   inventory.seed_cycle_count   — populates expected_qty from
//                                  the live stock_levels view.
//   inventory.post_cycle_count   — writes variance moves and
//                                  marks the session posted.
//
// All three honour ModeDryRun so the LLM can preview a destructive
// action before the operator approves it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/inventory"
)

// RegisterCycleCountTools attaches the Phase N9d cycle-count tools.
// A nil store is tolerated so unit tests that don't spin up the
// cycle-count schema still pass — commit-mode calls return a clear
// error instead of panicking.
func RegisterCycleCountTools(x *Executor, store *inventory.CycleCountStore) {
	x.Register(&createCycleCountTool{store: store})
	x.Register(&seedCycleCountTool{store: store})
	x.Register(&postCycleCountTool{store: store})
}

// ---------------------------------------------------------------------------
// inventory.create_cycle_count
// ---------------------------------------------------------------------------

type createCycleCountInput struct {
	Code        string    `json:"code"`
	Description string    `json:"description,omitempty"`
	WarehouseID uuid.UUID `json:"warehouse_id"`
}

type createCycleCountTool struct {
	store *inventory.CycleCountStore
}

// Name returns the executor-facing tool name.
func (t *createCycleCountTool) Name() string { return "inventory.create_cycle_count" }

// RequiresConfirmation gates create behind explicit operator
// approval — opening a new audit session shows up in the journal
// and is a real business event.
func (t *createCycleCountTool) RequiresConfirmation() bool { return true }

// Invoke creates a draft cycle-count session header. On ModeDryRun
// it returns a preview without touching the database.
func (t *createCycleCountTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createCycleCountInput
	if err := json.Unmarshal(inv.Inputs, &in); err != nil {
		return nil, fmt.Errorf("inventory.create_cycle_count: invalid input: %w", err)
	}
	if in.Code == "" || in.WarehouseID == uuid.Nil {
		return nil, errors.New("inventory.create_cycle_count: code and warehouse_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(map[string]any{
			"action":       "inventory.create_cycle_count",
			"mode":         "dry_run",
			"code":         in.Code,
			"warehouse_id": in.WarehouseID,
		})
		return &Result{
			Summary: fmt.Sprintf("Would open cycle-count session %s in warehouse %s", in.Code, in.WarehouseID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("inventory.create_cycle_count: cycle-count store not configured")
	}
	out, err := t.store.CreateSession(ctx, inventory.CycleCountSession{
		TenantID:    inv.TenantID,
		Code:        in.Code,
		Description: in.Description,
		WarehouseID: in.WarehouseID,
		CreatedBy:   inv.ActorID,
	})
	if err != nil {
		return nil, fmt.Errorf("inventory.create_cycle_count: %w", err)
	}
	payload, _ := json.Marshal(out)
	return &Result{
		Summary: fmt.Sprintf("Opened cycle-count session %s (id %s)", out.Code, out.ID),
		Preview: payload,
	}, nil
}

// ---------------------------------------------------------------------------
// inventory.seed_cycle_count
// ---------------------------------------------------------------------------

type seedCycleCountInput struct {
	SessionID uuid.UUID `json:"session_id"`
}

type seedCycleCountTool struct {
	store *inventory.CycleCountStore
}

// Name returns the executor-facing tool name.
func (t *seedCycleCountTool) Name() string { return "inventory.seed_cycle_count" }

// RequiresConfirmation returns false: seeding writes lines with
// counted_qty=0 and is fully recoverable (operator simply edits the
// counted values), so it doesn't need an approval round-trip.
func (t *seedCycleCountTool) RequiresConfirmation() bool { return false }

// Invoke populates the session's lines with expected_qty pulled
// from the stock_levels view. Idempotent — re-running refreshes
// expected_qty against the latest stock_levels snapshot.
func (t *seedCycleCountTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in seedCycleCountInput
	if err := json.Unmarshal(inv.Inputs, &in); err != nil {
		return nil, fmt.Errorf("inventory.seed_cycle_count: invalid input: %w", err)
	}
	if in.SessionID == uuid.Nil {
		return nil, errors.New("inventory.seed_cycle_count: session_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(map[string]any{
			"action":     "inventory.seed_cycle_count",
			"mode":       "dry_run",
			"session_id": in.SessionID,
		})
		return &Result{
			Summary: fmt.Sprintf("Would seed cycle-count session %s from stock_levels", in.SessionID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("inventory.seed_cycle_count: cycle-count store not configured")
	}
	if err := t.store.SeedExpectedFromStock(ctx, inv.TenantID, in.SessionID); err != nil {
		return nil, fmt.Errorf("inventory.seed_cycle_count: %w", err)
	}
	lines, err := t.store.ListLines(ctx, inv.TenantID, in.SessionID)
	if err != nil {
		return nil, fmt.Errorf("inventory.seed_cycle_count: %w", err)
	}
	payload, _ := json.Marshal(lines)
	return &Result{
		Summary: fmt.Sprintf("Seeded %d lines on cycle-count session %s", len(lines), in.SessionID),
		Preview: payload,
	}, nil
}

// ---------------------------------------------------------------------------
// inventory.post_cycle_count
// ---------------------------------------------------------------------------

type postCycleCountInput struct {
	SessionID uuid.UUID `json:"session_id"`
}

type postCycleCountTool struct {
	store *inventory.CycleCountStore
}

// Name returns the executor-facing tool name.
func (t *postCycleCountTool) Name() string { return "inventory.post_cycle_count" }

// RequiresConfirmation gates post behind explicit approval — once
// posted the variance moves are on the ledger and the session
// becomes immutable, so the operator must opt in.
func (t *postCycleCountTool) RequiresConfirmation() bool { return true }

// Invoke walks every non-zero variance line and writes a variance
// inventory_move keyed on (MoveSourceCycleCount, line.id). Posting
// is idempotent — a retry on an already-posted session returns
// the existing session header without booking duplicates.
func (t *postCycleCountTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in postCycleCountInput
	if err := json.Unmarshal(inv.Inputs, &in); err != nil {
		return nil, fmt.Errorf("inventory.post_cycle_count: invalid input: %w", err)
	}
	if in.SessionID == uuid.Nil {
		return nil, errors.New("inventory.post_cycle_count: session_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(map[string]any{
			"action":     "inventory.post_cycle_count",
			"mode":       "dry_run",
			"session_id": in.SessionID,
		})
		return &Result{
			Summary: fmt.Sprintf("Would post cycle-count session %s and book variance moves", in.SessionID),
			Preview: preview,
		}, nil
	}
	if t.store == nil {
		return nil, errors.New("inventory.post_cycle_count: cycle-count store not configured")
	}
	out, err := t.store.PostSession(ctx, inv.TenantID, in.SessionID, inv.ActorID)
	if err != nil {
		return nil, fmt.Errorf("inventory.post_cycle_count: %w", err)
	}
	payload, _ := json.Marshal(out)
	return &Result{
		Summary: fmt.Sprintf("Posted cycle-count session %s", out.Code),
		Preview: payload,
	}, nil
}
