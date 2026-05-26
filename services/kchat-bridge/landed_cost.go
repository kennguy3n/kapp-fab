package main

// Phase N9c — /landed-cost slash command.
//
// Sub-commands:
//   /landed-cost list                       — recent vouchers (any status)
//   /landed-cost list draft|allocated|posted
//   /landed-cost show <voucher_id>          — header + charges + targets
//   /landed-cost allocate <voucher_id>      — compute shares; transitions to allocated
//   /landed-cost post <voucher_id>          — write moves + JE; transitions to posted
//
// Heavy operations (create / charge / target edit) are intentionally
// not exposed here — they have many fields and belong on the
// dedicated Landed Costs page. The slash command is the operator
// shortcut for "I'm reviewing this voucher in chat, push it through
// the lifecycle".

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/finance"
)

func (d *CommandDispatcher) landedCostCmd(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.landedCost == nil {
		return CommandResponse{Text: "/landed-cost: landed-cost store not configured"}, nil
	}
	if len(req.Args) == 0 {
		return CommandResponse{Text: "/landed-cost: usage /landed-cost {list|show|allocate|post} ..."}, nil
	}
	sub := strings.ToLower(req.Args[0])
	rest := req.Args[1:]
	switch sub {
	case "list":
		return d.landedCostList(ctx, req, rest)
	case "show":
		return d.landedCostShow(ctx, req, rest)
	case "allocate":
		return d.landedCostAllocate(ctx, req, rest)
	case "post":
		return d.landedCostPost(ctx, req, rest)
	default:
		return CommandResponse{Text: fmt.Sprintf("/landed-cost: unknown sub-command %q", sub)}, nil
	}
}

func (d *CommandDispatcher) landedCostList(ctx context.Context, req CommandRequest, rest []string) (CommandResponse, error) {
	filter := finance.LandedCostFilter{}
	if len(rest) > 0 {
		filter.Status = strings.ToLower(rest[0])
	}
	out, err := d.landedCost.ListVouchers(ctx, req.TenantID, filter)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost list: %v", err)}, nil
	}
	if len(out) == 0 {
		return CommandResponse{Text: "/landed-cost list: no vouchers"}, nil
	}
	var b strings.Builder
	if filter.Status != "" {
		fmt.Fprintf(&b, "Landed cost vouchers (status=%s):\n", filter.Status)
	} else {
		fmt.Fprintln(&b, "Landed cost vouchers:")
	}
	for i := range out {
		v := &out[i]
		fmt.Fprintf(&b, "• %s — %s (%s, %s)\n", v.VoucherNumber, v.ID, v.AllocationMethod, v.Status)
	}
	return CommandResponse{Text: strings.TrimRight(b.String(), "\n")}, nil
}

func (d *CommandDispatcher) landedCostShow(ctx context.Context, req CommandRequest, rest []string) (CommandResponse, error) {
	if len(rest) == 0 {
		return CommandResponse{Text: "/landed-cost show: voucher_id required"}, nil
	}
	id, err := uuid.Parse(rest[0])
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost show: voucher_id invalid: %v", err)}, nil
	}
	v, err := d.landedCost.GetVoucher(ctx, req.TenantID, id)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost show: %v", err)}, nil
	}
	charges, err := d.landedCost.ListCharges(ctx, req.TenantID, id)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost show: list charges: %v", err)}, nil
	}
	targets, err := d.landedCost.ListTargets(ctx, req.TenantID, id)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost show: list targets: %v", err)}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%s* (%s) — %s, %s\n", v.VoucherNumber, v.ID, v.AllocationMethod, v.Status)
	if v.Description != "" {
		fmt.Fprintf(&b, "_%s_\n", v.Description)
	}
	if len(charges) > 0 {
		fmt.Fprintln(&b, "Charges:")
		for i := range charges {
			c := &charges[i]
			acct := c.AccountCode
			if acct == "" {
				acct = "(default)"
			}
			fmt.Fprintf(&b, "  • %s — %s @ %s\n", c.Description, c.Amount.String(), acct)
		}
	}
	if len(targets) > 0 {
		fmt.Fprintln(&b, "Targets:")
		for i := range targets {
			t := &targets[i]
			fmt.Fprintf(&b, "  • item=%s warehouse=%s qty=%s unit_cost=%s allocated=%s applied=%t\n",
				t.ItemID, t.WarehouseID, t.Qty.String(), t.UnitCost.String(), t.AllocatedAmount.String(), t.Applied)
		}
	}
	return CommandResponse{Text: strings.TrimRight(b.String(), "\n")}, nil
}

func (d *CommandDispatcher) landedCostAllocate(ctx context.Context, req CommandRequest, rest []string) (CommandResponse, error) {
	if len(rest) == 0 {
		return CommandResponse{Text: "/landed-cost allocate: voucher_id required"}, nil
	}
	id, err := uuid.Parse(rest[0])
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost allocate: voucher_id invalid: %v", err)}, nil
	}
	targets, err := d.landedCost.AllocateVoucher(ctx, req.TenantID, id)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost allocate: %v", err)}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Allocated voucher %s across %d target(s):\n", id, len(targets))
	for i := range targets {
		t := &targets[i]
		fmt.Fprintf(&b, "  • item=%s qty=%s allocated=%s\n", t.ItemID, t.Qty.String(), t.AllocatedAmount.String())
	}
	return CommandResponse{Text: strings.TrimRight(b.String(), "\n")}, nil
}

func (d *CommandDispatcher) landedCostPost(ctx context.Context, req CommandRequest, rest []string) (CommandResponse, error) {
	if len(rest) == 0 {
		return CommandResponse{Text: "/landed-cost post: voucher_id required"}, nil
	}
	id, err := uuid.Parse(rest[0])
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost post: voucher_id invalid: %v", err)}, nil
	}
	v, je, err := d.landedCost.PostVoucher(ctx, req.TenantID, id, req.UserID)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/landed-cost post: %v", err)}, nil
	}
	return CommandResponse{Text: fmt.Sprintf("Posted voucher %s — JE %s", v.VoucherNumber, je.ID)}, nil
}
