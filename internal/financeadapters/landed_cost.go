// Package financeadapters provides concrete implementations of the
// narrow interfaces declared in internal/finance — specifically
// finance.InventoryBackend and finance.LedgerBackend.
//
// Why a separate package: internal/finance cannot directly import
// internal/inventory or internal/ledger because internal/ledger
// already imports internal/finance for KType constants (closing
// the cycle finance → inventory → ledger → finance). The
// landed-cost feature solves this with dependency inversion:
// finance declares the interfaces, and this package — which is
// a leaf with no inverse dependency — provides the concrete
// adapters that bridge ledger.PGStore / inventory.PGStore onto
// those interfaces.
package financeadapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
)

// LandedCostInventoryAdapter satisfies finance.InventoryBackend by
// proxying to a real *inventory.PGStore. ErrAlreadyReversed and
// ErrDuplicateSourceMove are folded into no-ops so a posting retry
// after a partial failure walks each target idempotently.
type LandedCostInventoryAdapter struct {
	store *inventory.PGStore
	now   func() time.Time
}

// NewLandedCostInventoryAdapter wires the adapter to an existing
// *inventory.PGStore.
func NewLandedCostInventoryAdapter(store *inventory.PGStore) *LandedCostInventoryAdapter {
	return &LandedCostInventoryAdapter{
		store: store,
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// GetMoveBySource returns a narrow snapshot of the inventory move
// that the landed-cost poster will reverse + re-record.
func (a *LandedCostInventoryAdapter) GetMoveBySource(ctx context.Context, tenantID uuid.UUID, sourceKType string, sourceID, itemID, warehouseID uuid.UUID) (*finance.InventoryMoveSnapshot, error) {
	if a == nil || a.store == nil {
		return nil, errors.New("inventory: nil landed cost adapter")
	}
	m, err := a.store.GetMoveBySource(ctx, tenantID, sourceKType, sourceID, itemID, warehouseID)
	if err != nil {
		return nil, err
	}
	return &finance.InventoryMoveSnapshot{
		ID:          m.ID,
		Qty:         m.Qty,
		UnitCost:    m.UnitCost,
		ItemID:      m.ItemID,
		WarehouseID: m.WarehouseID,
	}, nil
}

// ReverseMove reverses an existing move. ErrAlreadyReversed is
// folded to a no-op so an idempotent retry after a partial failure
// walks each target without surfacing the sentinel.
func (a *LandedCostInventoryAdapter) ReverseMove(ctx context.Context, tenantID uuid.UUID, moveID int64, actor uuid.UUID, memo string) error {
	if a == nil || a.store == nil {
		return errors.New("inventory: nil landed cost adapter")
	}
	if _, err := a.store.ReverseMove(ctx, tenantID, moveID, actor, memo); err != nil {
		if errors.Is(err, inventory.ErrAlreadyReversed) {
			return nil
		}
		return err
	}
	return nil
}

// RecordLandedCostMove writes the forward inventory move with
// source_ktype = finance.MoveSourceLandedCost and source_id = the
// per-target id so the inventory_moves_source_uniq tuple stays
// unique even when a single voucher has multiple targets on the
// same item+warehouse. A retry returns ErrDuplicateSourceMove
// (folded to no-op here).
func (a *LandedCostInventoryAdapter) RecordLandedCostMove(ctx context.Context, in finance.LandedCostMoveInput) error {
	if a == nil || a.store == nil {
		return errors.New("inventory: nil landed cost adapter")
	}
	sourceID := in.TargetID
	if sourceID == uuid.Nil {
		// Fall back to voucher.id for backward compatibility with
		// any caller that still hasn't populated TargetID.
		sourceID = in.VoucherID
	}
	m := inventory.Move{
		TenantID:    in.TenantID,
		ItemID:      in.ItemID,
		WarehouseID: in.WarehouseID,
		Qty:         in.Qty,
		UnitCost:    in.UnitCost,
		SourceKType: finance.MoveSourceLandedCost,
		SourceID:    &sourceID,
		MovedAt:     in.MovedAt,
		CreatedBy:   in.ActorID,
	}
	if _, err := a.store.RecordMove(ctx, m); err != nil {
		if errors.Is(err, inventory.ErrDuplicateSourceMove) {
			return nil
		}
		return err
	}
	return nil
}

// LandedCostLedgerAdapter satisfies finance.LedgerBackend by
// proxying to a real *ledger.PGStore.
type LandedCostLedgerAdapter struct {
	store *ledger.PGStore
}

// NewLandedCostLedgerAdapter wires the adapter to an existing
// *ledger.PGStore.
func NewLandedCostLedgerAdapter(store *ledger.PGStore) *LandedCostLedgerAdapter {
	return &LandedCostLedgerAdapter{store: store}
}

// GetJournalEntryBySource returns the existing JE for a given
// source tuple. The landed-cost poster uses this for idempotency
// (existing posted entry → return; otherwise → write fresh).
//
// The ledger store returns (nil, ledger.ErrEntryNotFound) on a
// miss, never (nil, nil) — see ledger/store.go:493. The finance
// package can't import the ledger sentinel without re-closing the
// import cycle the LedgerBackend abstraction precisely exists to
// break, so the adapter absorbs ErrEntryNotFound here and returns
// (nil, nil) to match the LedgerBackend interface contract that
// PostVoucher relies on (internal/finance/landed_cost.go:1119-1121).
// Without this translation, first-time PostVoucher fails because
// the "no existing JE" lookup is wrapped into a posting error,
// preventing any voucher from ever being posted the first time.
// All sibling ledger consumers (invoice.go:207, payment.go:154)
// already filter the sentinel; this brings the landed-cost adapter
// into line.
func (a *LandedCostLedgerAdapter) GetJournalEntryBySource(ctx context.Context, tenantID uuid.UUID, sourceKType string, sourceID uuid.UUID) (*finance.LandedCostJournalEntry, error) {
	if a == nil || a.store == nil {
		return nil, errors.New("ledger: nil landed cost adapter")
	}
	je, err := a.store.GetJournalEntryBySource(ctx, tenantID, sourceKType, sourceID)
	if err != nil {
		if errors.Is(err, ledger.ErrEntryNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if je == nil {
		return nil, nil
	}
	return &finance.LandedCostJournalEntry{
		ID:       je.ID,
		PostedAt: je.PostedAt,
	}, nil
}

// GetJournalEntry returns the JE identified by id. The poster
// calls this on the idempotent retry path to return the existing
// JE projection.
//
// Same ErrEntryNotFound translation as GetJournalEntryBySource
// above — the ledger store returns the sentinel on a miss, the
// finance package can't import it, and PostVoucher's idempotent-
// retry path expects (nil, nil) when a voucher has voucher.je_id
// set but the entry has been hard-deleted (an operational anomaly
// rather than the normal case, but worth handling consistently
// so the surface returns a clean ErrLandedCostNotFound-style
// error rather than wrapping a sentinel that can't be inspected).
func (a *LandedCostLedgerAdapter) GetJournalEntry(ctx context.Context, tenantID, id uuid.UUID) (*finance.LandedCostJournalEntry, error) {
	if a == nil || a.store == nil {
		return nil, errors.New("ledger: nil landed cost adapter")
	}
	je, err := a.store.GetJournalEntry(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, ledger.ErrEntryNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if je == nil {
		return nil, nil
	}
	return &finance.LandedCostJournalEntry{
		ID:       je.ID,
		PostedAt: je.PostedAt,
	}, nil
}

// PostLandedCostJournalEntry writes the booking JE with
// source_ktype = finance.MoveSourceLandedCost and source_id =
// in.VoucherID. The (source_ktype, source_id) tuple is unique-
// indexed on journal_entries, so a retry returns the existing
// entry rather than double-booking.
func (a *LandedCostLedgerAdapter) PostLandedCostJournalEntry(ctx context.Context, in finance.LandedCostJEInput) (*finance.LandedCostJournalEntry, error) {
	if a == nil || a.store == nil {
		return nil, errors.New("ledger: nil landed cost adapter")
	}
	voucherID := in.VoucherID
	lines := make([]ledger.JournalLine, 0, len(in.Lines))
	for _, ln := range in.Lines {
		lines = append(lines, ledger.JournalLine{
			TenantID:    in.TenantID,
			AccountCode: ln.AccountCode,
			Debit:       ln.Debit,
			Credit:      ln.Credit,
			Currency:    ln.Currency,
			Memo:        ln.Memo,
		})
	}
	je := ledger.JournalEntry{
		TenantID:    in.TenantID,
		PostedAt:    in.PostedAt,
		Memo:        in.Memo,
		SourceKType: finance.MoveSourceLandedCost,
		SourceID:    &voucherID,
		CreatedBy:   in.ActorID,
		Lines:       lines,
	}
	posted, err := a.store.PostJournalEntry(ctx, je)
	if err != nil {
		// A concurrent landed-cost poster beat us to the JE insert —
		// the (source_ktype, source_id) unique index on
		// journal_entries surfaces this as ErrDuplicateSourceEntry.
		// Mirror the InvoicePoster recovery at
		// internal/ledger/invoice.go:243-251 by reloading the
		// already-committed JE and returning it as if our Post had
		// succeeded. The voucher is keyed on (source_ktype,
		// source_id) = (finance.MoveSourceLandedCost, voucherID) so
		// the reload is the entry the winning poster wrote.
		// Absorbing the sentinel here (in the adapter) rather than
		// in finance.PostVoucher keeps the LedgerBackend abstraction
		// one-way: the finance package never has to import the
		// ledger sentinel directly.
		if errors.Is(err, ledger.ErrDuplicateSourceEntry) {
			reloaded, reloadErr := a.store.GetJournalEntryBySource(ctx, in.TenantID, finance.MoveSourceLandedCost, voucherID)
			if reloadErr != nil {
				return nil, fmt.Errorf("ledger: reload duplicate landed-cost JE: %w", reloadErr)
			}
			if reloaded == nil {
				// Defensive: ErrDuplicateSourceEntry from
				// PostJournalEntry with no matching row from
				// GetJournalEntryBySource is an invariant
				// violation (the unique index that fired the
				// insert error covers exactly the tuple we're
				// looking up). Surface the original error.
				return nil, err
			}
			return &finance.LandedCostJournalEntry{
				ID:       reloaded.ID,
				PostedAt: reloaded.PostedAt,
			}, nil
		}
		return nil, err
	}
	return &finance.LandedCostJournalEntry{
		ID:       posted.ID,
		PostedAt: posted.PostedAt,
	}, nil
}

// Compile-time interface satisfaction.
var (
	_ finance.InventoryBackend = (*LandedCostInventoryAdapter)(nil)
	_ finance.LedgerBackend    = (*LandedCostLedgerAdapter)(nil)
)
