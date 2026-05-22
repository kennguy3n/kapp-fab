package main

import (
	"context"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/projects"
	"github.com/kennguy3n/kapp-fab/internal/sales"
)

// registerBootKTypes upserts every KType the API surface depends on
// so a fresh deployment has a working schema set without requiring
// an out-of-band migration step. The registry upserts on conflict so
// repeated restarts (every replica every rollout) are a safe no-op.
//
// New KType domains MUST be registered here, not at handler-init
// time, so the boot sequence is the single source of truth for
// "which schemas does this API binary advertise". Two patterns
// coexist:
//
//  1. Domain helpers exported as `RegisterKTypes(ctx, registry)` —
//     these compose into the boot sequence with no fmt.Errorf wrap
//     since they already attribute internally.
//  2. Per-domain catalogues exposed as `XxxKTypes() []ktype.KType` —
//     these need the per-type Register loop because they were
//     designed to be opt-in (a deployment dropping the call here
//     simply doesn't advertise the schema).
//
// Both shapes return on the first failure: a half-registered
// catalogue would leave the API surface broken in non-obvious ways,
// so we fail fast at boot.
func registerBootKTypes(ctx context.Context, registry *ktype.PGRegistry) error {
	if err := finance.RegisterKTypes(ctx, registry); err != nil {
		return err
	}
	if err := inventory.RegisterKTypes(ctx, registry); err != nil {
		return err
	}
	if err := hr.RegisterKTypes(ctx, registry); err != nil {
		return err
	}
	if err := lms.RegisterKTypes(ctx, registry); err != nil {
		return err
	}
	if err := crm.RegisterKTypes(ctx, registry); err != nil {
		return err
	}
	// Phase G additions — sales/procurement, bank reconciliation,
	// cost centres, and payroll live next to (not inside) the
	// finance/hr catalogs so a deployment can opt out by dropping
	// the registration calls.
	if err := sales.RegisterKTypes(ctx, registry); err != nil {
		return err
	}
	for _, kt := range sales.POSKTypes() {
		if err := registry.RegisterIfChanged(ctx, kt); err != nil {
			return fmt.Errorf("register pos ktype %s: %w", kt.Name, err)
		}
	}
	for _, kt := range ledger.BankKTypes() {
		if err := registry.RegisterIfChanged(ctx, kt); err != nil {
			return fmt.Errorf("register bank ktype %s: %w", kt.Name, err)
		}
	}
	if err := registry.RegisterIfChanged(ctx, ledger.CostCenterKType()); err != nil {
		return fmt.Errorf("register cost_center ktype: %w", err)
	}
	for _, kt := range hr.PayrollKTypes() {
		if err := registry.RegisterIfChanged(ctx, kt); err != nil {
			return fmt.Errorf("register payroll ktype %s: %w", kt.Name, err)
		}
	}
	// Phase M shift scheduling. Registered separately from the
	// Phase E HR catalog so an existing deployment can opt out by
	// dropping these two lines without touching the older
	// hr.RegisterKTypes call.
	for _, kt := range hr.ShiftKTypes() {
		if err := registry.RegisterIfChanged(ctx, kt); err != nil {
			return fmt.Errorf("register shift ktype %s: %w", kt.Name, err)
		}
	}
	for _, kt := range hr.AppraisalKTypes() {
		if err := registry.RegisterIfChanged(ctx, kt); err != nil {
			return fmt.Errorf("register appraisal ktype %s: %w", kt.Name, err)
		}
	}
	if err := projects.RegisterKTypes(ctx, registry); err != nil {
		return err
	}
	// Phase I — helpdesk KTypes. The helpdesk store manages typed
	// SLA policies + breach log while tickets themselves ride the
	// generic KRecord plumbing.
	if err := helpdesk.RegisterKTypes(ctx, registry); err != nil {
		return err
	}
	// Phase I — exchange-rate KType so it shows up in the KType
	// registry + records surface alongside other finance masters.
	if err := registry.RegisterIfChanged(ctx, ledger.ExchangeRateKType()); err != nil {
		return fmt.Errorf("register exchange_rate ktype: %w", err)
	}
	return nil
}
