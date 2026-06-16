package manufacturing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("date %q: %v", s, err)
	}
	return d
}

// TestBackwardSchedule pins the lead-time subtraction and the negative
// lead clamp.
func TestBackwardSchedule(t *testing.T) {
	t.Parallel()
	due := mustDate(t, "2026-06-10")
	if got := backwardSchedule(due, 7); !got.Equal(mustDate(t, "2026-06-03")) {
		t.Errorf("7-day lead = %s, want 2026-06-03", got.Format("2006-01-02"))
	}
	if got := backwardSchedule(due, 0); !got.Equal(due) {
		t.Errorf("0-day lead = %s, want due", got.Format("2006-01-02"))
	}
	if got := backwardSchedule(due, -5); !got.Equal(due) {
		t.Errorf("negative lead should clamp to due, got %s", got.Format("2006-01-02"))
	}
}

// TestMakeLeadTimeDays covers the routing-derived lead time, the
// no-routing default, and the zero-capacity divide-by-zero guard.
func TestMakeLeadTimeDays(t *testing.T) {
	t.Parallel()
	wcA := uuid.New()
	wcB := uuid.New()
	wcAvail := map[uuid.UUID]decimal.Decimal{
		wcA: decimal.NewFromInt(480), // 8h
		wcB: decimal.NewFromInt(240), // 4h
		// a third center is deliberately absent / zero to exercise the guard
	}

	if got := makeLeadTimeDays(nil, decimal.NewFromInt(10), wcAvail); got != defaultMakeLeadTimeDays {
		t.Errorf("nil routing lead = %d, want %d", got, defaultMakeLeadTimeDays)
	}

	routing := &Routing{
		Operations: []RoutingOperation{
			// 60 + 5*100 = 560 min on wcA(480/day) => ceil(560/480)=2 days
			{Sequence: 1, WorkCenterID: wcA, SetupTimeMinutes: decimal.NewFromInt(60), CycleTimeMinutes: decimal.NewFromInt(5)},
			// 0 + 1*100 = 100 min on wcB(240/day) => ceil(100/240)=1 day
			{Sequence: 2, WorkCenterID: wcB, SetupTimeMinutes: decimal.Zero, CycleTimeMinutes: decimal.NewFromInt(1)},
		},
	}
	if got := makeLeadTimeDays(routing, decimal.NewFromInt(100), wcAvail); got != 3 {
		t.Errorf("routing lead = %d, want 3", got)
	}

	zeroCap := &Routing{
		Operations: []RoutingOperation{
			{Sequence: 1, WorkCenterID: uuid.New(), SetupTimeMinutes: decimal.NewFromInt(10), CycleTimeMinutes: decimal.NewFromInt(1)},
		},
	}
	if got := makeLeadTimeDays(zeroCap, decimal.NewFromInt(5), wcAvail); got != 1 {
		t.Errorf("zero-capacity op lead = %d, want 1", got)
	}
}

// TestComputeLowLevelCodesCycle verifies a cyclic active-BOM graph is
// rejected rather than looping forever.
func TestComputeLowLevelCodesCycle(t *testing.T) {
	t.Parallel()
	a := uuid.New()
	b := uuid.New()
	// A's BOM consumes B; B's BOM consumes A — a cycle.
	data := map[uuid.UUID]mrpItemData{
		a: {ActiveBOM: &BOM{ID: uuid.New(), ItemID: a, OutputQty: decimal.NewFromInt(1),
			Components: []BOMComponent{{ComponentItemID: b, Qty: decimal.NewFromInt(1)}}}},
		b: {ActiveBOM: &BOM{ID: uuid.New(), ItemID: b, OutputQty: decimal.NewFromInt(1),
			Components: []BOMComponent{{ComponentItemID: a, Qty: decimal.NewFromInt(1)}}}},
	}
	independent := map[uuid.UUID]mrpDemand{a: {qty: decimal.NewFromInt(1), due: mustDate(t, "2026-06-10")}}
	if _, err := computeLowLevelCodes(independent, data); !errors.Is(err, ErrMRPCyclicBOM) {
		t.Fatalf("computeLowLevelCodes cycle err = %v, want ErrMRPCyclicBOM", err)
	}
}

// TestPlanMRPBuyItem nets a single purchased item against on-hand and
// scheduled receipts and backward schedules it by the buy lead time.
func TestPlanMRPBuyItem(t *testing.T) {
	t.Parallel()
	item := uuid.New()
	due := mustDate(t, "2026-06-30")
	independent := map[uuid.UUID]mrpDemand{item: {qty: decimal.NewFromInt(100), due: due}}
	data := map[uuid.UUID]mrpItemData{
		item: {OnHand: decimal.NewFromInt(30), Scheduled: decimal.NewFromInt(20)}, // net 50
	}
	planned, err := planMRP(independent, data, nil, 7)
	if err != nil {
		t.Fatalf("planMRP: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("got %d planned orders, want 1", len(planned))
	}
	po := planned[0]
	if po.OrderType != MRPOrderTypeBuy {
		t.Errorf("order type = %s, want buy", po.OrderType)
	}
	if !po.Qty.Equal(decimal.NewFromInt(50)) {
		t.Errorf("net qty = %s, want 50", po.Qty.String())
	}
	if !po.SuggestedStartDate.Equal(mustDate(t, "2026-06-23")) {
		t.Errorf("start = %s, want 2026-06-23", po.SuggestedStartDate.Format("2006-01-02"))
	}
	if po.ExplosionLevel != 0 {
		t.Errorf("level = %d, want 0", po.ExplosionLevel)
	}
}

// TestPlanMRPFullyCovered verifies an item whose on-hand fully covers
// demand produces no planned order and is not exploded.
func TestPlanMRPFullyCovered(t *testing.T) {
	t.Parallel()
	item := uuid.New()
	independent := map[uuid.UUID]mrpDemand{item: {qty: decimal.NewFromInt(10), due: mustDate(t, "2026-06-30")}}
	data := map[uuid.UUID]mrpItemData{item: {OnHand: decimal.NewFromInt(25)}}
	planned, err := planMRP(independent, data, nil, 7)
	if err != nil {
		t.Fatalf("planMRP: %v", err)
	}
	if len(planned) != 0 {
		t.Fatalf("got %d planned orders, want 0", len(planned))
	}
}

// TestPlanMRPMakeExplodes drives the core explosion + netting:
// a finished make item explodes its BOM into component demand, the
// component planned order is netted against its own on-hand, and the
// component is due by the parent make order's suggested start date.
func TestPlanMRPMakeExplodes(t *testing.T) {
	t.Parallel()
	finished := uuid.New()
	comp := uuid.New()
	wc := uuid.New()
	due := mustDate(t, "2026-06-30")

	bom := &BOM{
		ID:        uuid.New(),
		ItemID:    finished,
		OutputQty: decimal.NewFromInt(1),
		Components: []BOMComponent{
			{ComponentItemID: comp, Qty: decimal.NewFromInt(2)},
		},
	}
	routing := &Routing{
		ID:     uuid.New(),
		ItemID: finished,
		Operations: []RoutingOperation{
			// 0 + 1*10 = 10 min on wc(480/day) => 1 day lead
			{Sequence: 1, WorkCenterID: wc, CycleTimeMinutes: decimal.NewFromInt(1)},
		},
	}

	independent := map[uuid.UUID]mrpDemand{finished: {qty: decimal.NewFromInt(10), due: due}}
	data := map[uuid.UUID]mrpItemData{
		finished: {ActiveBOM: bom, ActiveRouting: routing},
		comp:     {OnHand: decimal.NewFromInt(5)}, // gross 20, net 15
	}
	wcAvail := map[uuid.UUID]decimal.Decimal{wc: decimal.NewFromInt(480)}

	planned, err := planMRP(independent, data, wcAvail, 7)
	if err != nil {
		t.Fatalf("planMRP: %v", err)
	}
	if len(planned) != 2 {
		t.Fatalf("got %d planned orders, want 2", len(planned))
	}

	// Ordered by level: finished (0) first, component (1) next.
	mk := planned[0]
	if mk.ItemID != finished || mk.OrderType != MRPOrderTypeMake {
		t.Fatalf("first order = %v/%s, want finished/make", mk.ItemID, mk.OrderType)
	}
	if mk.BOMID == nil || *mk.BOMID != bom.ID {
		t.Errorf("make order bom_id not snapshotted")
	}
	if mk.RoutingID == nil || *mk.RoutingID != routing.ID {
		t.Errorf("make order routing_id not snapshotted")
	}
	wantMakeStart := mustDate(t, "2026-06-29") // due - 1 day
	if !mk.SuggestedStartDate.Equal(wantMakeStart) {
		t.Errorf("make start = %s, want %s", mk.SuggestedStartDate.Format("2006-01-02"), wantMakeStart.Format("2006-01-02"))
	}

	cp := planned[1]
	if cp.ItemID != comp || cp.OrderType != MRPOrderTypeBuy {
		t.Fatalf("second order = %v/%s, want component/buy", cp.ItemID, cp.OrderType)
	}
	if !cp.Qty.Equal(decimal.NewFromInt(15)) {
		t.Errorf("component net qty = %s, want 15", cp.Qty.String())
	}
	if cp.ExplosionLevel != 1 {
		t.Errorf("component level = %d, want 1", cp.ExplosionLevel)
	}
	// Component is required by the parent make order's start date and
	// backward scheduled from there by the 7-day buy lead time.
	if !cp.DueDate.Equal(wantMakeStart) {
		t.Errorf("component due = %s, want %s", cp.DueDate.Format("2006-01-02"), wantMakeStart.Format("2006-01-02"))
	}
	if !cp.SuggestedStartDate.Equal(mustDate(t, "2026-06-22")) {
		t.Errorf("component start = %s, want 2026-06-22", cp.SuggestedStartDate.Format("2006-01-02"))
	}
}

// TestPlanMRPScrapAndOutputQty verifies the explosion math honours BOM
// output_qty and component scrap percent.
func TestPlanMRPScrapAndOutputQty(t *testing.T) {
	t.Parallel()
	finished := uuid.New()
	comp := uuid.New()
	scrap := decimal.NewFromInt(10) // +10%
	bom := &BOM{
		ID:        uuid.New(),
		ItemID:    finished,
		OutputQty: decimal.NewFromInt(2), // recipe yields 2 finished per batch
		Components: []BOMComponent{
			{ComponentItemID: comp, Qty: decimal.NewFromInt(3), ScrapPercent: &scrap},
		},
	}
	independent := map[uuid.UUID]mrpDemand{finished: {qty: decimal.NewFromInt(20), due: mustDate(t, "2026-07-01")}}
	data := map[uuid.UUID]mrpItemData{
		finished: {ActiveBOM: bom},
		comp:     {},
	}
	planned, err := planMRP(independent, data, nil, 7)
	if err != nil {
		t.Fatalf("planMRP: %v", err)
	}
	// component gross = effectiveQty(3 * 1.1 = 3.3) * net(20) / outputQty(2) = 33
	var compOrder *MRPPlannedOrder
	for i := range planned {
		if planned[i].ItemID == comp {
			compOrder = &planned[i]
		}
	}
	if compOrder == nil {
		t.Fatal("no component planned order")
	}
	if !compOrder.Qty.Equal(mustDecimal(t, "33")) {
		t.Errorf("component qty = %s, want 33", compOrder.Qty.String())
	}
}

// TestPlanMRPZeroOutputQty guards the explosion divisor: a make BOM
// with a non-positive output_qty (corrupt/legacy data CreateBOM would
// reject) must surface ErrMRPInvalidBOMOutputQty rather than panic on
// a divide-by-zero.
func TestPlanMRPZeroOutputQty(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		outputQty decimal.Decimal
	}{
		{"zero", decimal.Zero},
		{"negative", decimal.NewFromInt(-1)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			finished := uuid.New()
			comp := uuid.New()
			bom := &BOM{
				ID:        uuid.New(),
				ItemID:    finished,
				OutputQty: tc.outputQty,
				Components: []BOMComponent{
					{ComponentItemID: comp, Qty: decimal.NewFromInt(3)},
				},
			}
			independent := map[uuid.UUID]mrpDemand{finished: {qty: decimal.NewFromInt(20), due: mustDate(t, "2026-07-01")}}
			data := map[uuid.UUID]mrpItemData{
				finished: {ActiveBOM: bom},
				comp:     {},
			}
			_, err := planMRP(independent, data, nil, 7)
			if !errors.Is(err, ErrMRPInvalidBOMOutputQty) {
				t.Fatalf("planMRP err = %v, want ErrMRPInvalidBOMOutputQty", err)
			}
		})
	}
}
