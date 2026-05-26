package finance

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestComputeAllocation_ByQty validates the pure-arithmetic core of
// AllocateVoucher under the by_qty method. Shares scale linearly
// with target.Qty and sum exactly to totalCharges (residual
// absorbed by the last share to handle four-decimal rounding).
func TestComputeAllocation_ByQty(t *testing.T) {
	totalCharges := decimal.NewFromInt(1000)
	targets := []LandedCostTarget{
		{Qty: decimal.NewFromInt(10)},
		{Qty: decimal.NewFromInt(20)},
		{Qty: decimal.NewFromInt(70)},
	}
	shares, err := computeAllocation(LandedCostByQty, totalCharges, targets)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(shares) != 3 {
		t.Fatalf("want 3 shares, got %d", len(shares))
	}
	// 100 qty total → 10% / 20% / 70%.
	wantA := decimal.NewFromInt(100)
	wantB := decimal.NewFromInt(200)
	wantC := decimal.NewFromInt(700)
	if !shares[0].Equal(wantA) {
		t.Errorf("share[0] = %s, want %s", shares[0], wantA)
	}
	if !shares[1].Equal(wantB) {
		t.Errorf("share[1] = %s, want %s", shares[1], wantB)
	}
	if !shares[2].Equal(wantC) {
		t.Errorf("share[2] = %s, want %s", shares[2], wantC)
	}
	if !sumShares(shares).Equal(totalCharges) {
		t.Errorf("sum of shares %s != totalCharges %s", sumShares(shares), totalCharges)
	}
}

// TestComputeAllocation_ByAmount validates by_amount weighting:
// each target's share is proportional to (qty * unit_cost).
func TestComputeAllocation_ByAmount(t *testing.T) {
	totalCharges := decimal.NewFromInt(500)
	// Two targets: A=100*1, B=100*4 → 80% to B, 20% to A.
	targets := []LandedCostTarget{
		{Qty: decimal.NewFromInt(100), UnitCost: decimal.NewFromInt(1), Amount: decimal.NewFromInt(100)},
		{Qty: decimal.NewFromInt(100), UnitCost: decimal.NewFromInt(4), Amount: decimal.NewFromInt(400)},
	}
	shares, err := computeAllocation(LandedCostByAmount, totalCharges, targets)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	wantA := decimal.NewFromInt(100) // 20% of 500
	wantB := decimal.NewFromInt(400) // 80% of 500
	if !shares[0].Equal(wantA) {
		t.Errorf("share[0] = %s, want %s", shares[0], wantA)
	}
	if !shares[1].Equal(wantB) {
		t.Errorf("share[1] = %s, want %s", shares[1], wantB)
	}
}

// TestComputeAllocation_ByWeight validates by_weight weighting:
// each target's share is proportional to target.Weight.
func TestComputeAllocation_ByWeight(t *testing.T) {
	totalCharges := decimal.NewFromInt(900)
	targets := []LandedCostTarget{
		{Weight: decimal.NewFromInt(1)},
		{Weight: decimal.NewFromInt(2)},
	}
	shares, err := computeAllocation(LandedCostByWeight, totalCharges, targets)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	wantA := decimal.NewFromInt(300) // 1/3 of 900
	wantB := decimal.NewFromInt(600) // 2/3 of 900
	if !shares[0].Equal(wantA) {
		t.Errorf("share[0] = %s, want %s", shares[0], wantA)
	}
	if !shares[1].Equal(wantB) {
		t.Errorf("share[1] = %s, want %s", shares[1], wantB)
	}
}

// TestComputeAllocation_ByWeight_AllZero rejects an allocation
// request where every target has weight=0 — the allocator cannot
// divide by a zero total without picking an arbitrary split.
func TestComputeAllocation_ByWeight_AllZero(t *testing.T) {
	totalCharges := decimal.NewFromInt(100)
	targets := []LandedCostTarget{
		{Weight: decimal.Zero},
		{Weight: decimal.Zero},
	}
	_, err := computeAllocation(LandedCostByWeight, totalCharges, targets)
	if err == nil {
		t.Fatalf("want ErrLandedCostZeroWeightTotal, got nil")
	}
}

// TestComputeAllocation_BadMethod surfaces unknown allocation
// methods rather than silently falling through to by_qty.
func TestComputeAllocation_BadMethod(t *testing.T) {
	_, err := computeAllocation("by_volume", decimal.NewFromInt(100), []LandedCostTarget{{Qty: decimal.NewFromInt(1)}})
	if err == nil {
		t.Fatalf("want ErrLandedCostBadMethod, got nil")
	}
}

// TestComputeAllocation_NoTargets is the no-targets guard.
func TestComputeAllocation_NoTargets(t *testing.T) {
	_, err := computeAllocation(LandedCostByQty, decimal.NewFromInt(100), nil)
	if err == nil {
		t.Fatalf("want ErrLandedCostNoTargets, got nil")
	}
}

// TestComputeAllocation_RoundingRemainderAbsorbed verifies the
// residual-trick: the last share absorbs any sub-cent rounding so
// the sum of all shares exactly equals totalCharges.
func TestComputeAllocation_RoundingRemainderAbsorbed(t *testing.T) {
	// 100 / 3 = 33.3333... — naive rounding to four decimals
	// would yield 33.3333 * 3 = 99.9999, leaving 0.0001
	// unaccounted. The last share must absorb the 0.0001.
	totalCharges := decimal.NewFromInt(100)
	targets := []LandedCostTarget{
		{Qty: decimal.NewFromInt(1)},
		{Qty: decimal.NewFromInt(1)},
		{Qty: decimal.NewFromInt(1)},
	}
	shares, err := computeAllocation(LandedCostByQty, totalCharges, targets)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !sumShares(shares).Equal(totalCharges) {
		t.Errorf("sum=%s, want %s. shares=%v", sumShares(shares), totalCharges, shares)
	}
}

// TestComputeAllocation_NegativeWeightTreatedAsZero is a
// defence-in-depth check: a target with a negative Weight (an
// invalid input the DB CHECK would reject) is treated as zero in
// the per-target weight sum so the allocator never produces a
// negative share.
func TestComputeAllocation_NegativeWeightTreatedAsZero(t *testing.T) {
	totalCharges := decimal.NewFromInt(100)
	targets := []LandedCostTarget{
		{Weight: decimal.NewFromInt(-5)}, // treated as 0
		{Weight: decimal.NewFromInt(10)},
	}
	shares, err := computeAllocation(LandedCostByWeight, totalCharges, targets)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !shares[0].Equal(decimal.Zero) {
		t.Errorf("share[0] = %s, want 0", shares[0])
	}
	if !shares[1].Equal(totalCharges) {
		t.Errorf("share[1] = %s, want 100", shares[1])
	}
}

// TestGroupChargesByAccount validates the credit-side bucketing
// used by the JE builder. Two charges to the same account collapse
// into one credit line; empty account_code routes to the platform
// stock-adjustment fallback.
func TestGroupChargesByAccount(t *testing.T) {
	charges := []LandedCostCharge{
		{Description: "DHL freight", Amount: decimal.NewFromInt(100), AccountCode: "6210"},
		{Description: "UPS freight", Amount: decimal.NewFromInt(50), AccountCode: "6210"},
		{Description: "Customs duty", Amount: decimal.NewFromInt(200), AccountCode: "6220"},
		{Description: "Misc", Amount: decimal.NewFromInt(25), AccountCode: ""},
	}
	groups := groupChargesByAccount(charges)
	if !groups["6210"].Equal(decimal.NewFromInt(150)) {
		t.Errorf("6210 = %s, want 150", groups["6210"])
	}
	if !groups["6220"].Equal(decimal.NewFromInt(200)) {
		t.Errorf("6220 = %s, want 200", groups["6220"])
	}
	if !groups[DefaultStockAdjustmentAccountCode].Equal(decimal.NewFromInt(25)) {
		t.Errorf("fallback %s = %s, want 25", DefaultStockAdjustmentAccountCode, groups[DefaultStockAdjustmentAccountCode])
	}
	if len(groups) != 3 {
		t.Errorf("expected 3 groups (6210, 6220, fallback), got %d", len(groups))
	}
}

// TestIsKnownAllocationMethod pins the enum set so a future patch
// adding a new method doesn't accidentally drop one of the
// existing ones.
func TestIsKnownAllocationMethod(t *testing.T) {
	cases := []struct {
		m    string
		want bool
	}{
		{LandedCostByQty, true},
		{LandedCostByAmount, true},
		{LandedCostByWeight, true},
		{"by_volume", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isKnownAllocationMethod(c.m); got != c.want {
			t.Errorf("isKnownAllocationMethod(%q) = %t, want %t", c.m, got, c.want)
		}
	}
}

func sumShares(shares []decimal.Decimal) decimal.Decimal {
	var s decimal.Decimal
	for _, sh := range shares {
		s = s.Add(sh)
	}
	return s
}
