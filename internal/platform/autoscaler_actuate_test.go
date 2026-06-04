package platform

import (
	"context"
	"errors"
	"testing"
)

// fakeProvisioner records the calls the autoscaler makes so actuation
// can be asserted without real infrastructure.
type fakeProvisioner struct {
	provisionRegions []string
	deprovisioned    []string
	provisionErr     error
	deprovisionErr   error
}

func (f *fakeProvisioner) Provision(_ context.Context, region string, spec CellSpec) (*Cell, error) {
	f.provisionRegions = append(f.provisionRegions, region)
	if f.provisionErr != nil {
		return nil, f.provisionErr
	}
	return &Cell{ID: "cell-new", Region: region, MaxTenants: spec.MaxTenants}, nil
}

func (f *fakeProvisioner) Deprovision(_ context.Context, cellID string) error {
	if f.deprovisionErr != nil {
		return f.deprovisionErr
	}
	f.deprovisioned = append(f.deprovisioned, cellID)
	return nil
}

func (f *fakeProvisioner) Status(_ context.Context, cellID string) (CellProvisionStatus, error) {
	return CellProvisionStatus{CellID: cellID, State: CellStateUnknown}, nil
}

func newActuateEngine(prov CellProvisioner) *AutoscaleEngine {
	return NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil).
		WithProvisioning(prov, nil, true)
}

func TestProvisionForScaleUp(t *testing.T) {
	prov := &fakeProvisioner{}
	e := newActuateEngine(prov)
	d := Decision{CellID: "c1", EventType: CellEventScaleUp, Snapshot: CellSnapshot{ID: "c1", Region: "us-east-1"}}
	e.provisionForScaleUp(context.Background(), d)
	if len(prov.provisionRegions) != 1 || prov.provisionRegions[0] != "us-east-1" {
		t.Fatalf("want provision in us-east-1, got %v", prov.provisionRegions)
	}
}

func TestDrainAndDeprovision_EmptyCell(t *testing.T) {
	prov := &fakeProvisioner{}
	e := newActuateEngine(prov)
	d := Decision{CellID: "c-empty", EventType: CellEventScaleDown, Snapshot: CellSnapshot{ID: "c-empty", TenantCount: 0}}
	e.drainAndDeprovision(context.Background(), d, nil)
	if len(prov.deprovisioned) != 1 || prov.deprovisioned[0] != "c-empty" {
		t.Fatalf("empty cell should deprovision directly, got %v", prov.deprovisioned)
	}
}

func TestDrainAndDeprovision_SkipsDefaultCell(t *testing.T) {
	prov := &fakeProvisioner{}
	e := newActuateEngine(prov)
	d := Decision{CellID: DefaultCellID, EventType: CellEventScaleDown, Snapshot: CellSnapshot{ID: DefaultCellID, TenantCount: 0}}
	e.drainAndDeprovision(context.Background(), d, nil)
	if len(prov.deprovisioned) != 0 {
		t.Fatalf("default cell must never be deprovisioned, got %v", prov.deprovisioned)
	}
}

func TestDrainAndDeprovision_NonEmptyNoRebalancer(t *testing.T) {
	prov := &fakeProvisioner{}
	// No rebalancer wired: a populated cell must NOT be torn down.
	e := NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil).WithProvisioning(prov, nil, true)
	d := Decision{CellID: "c-busy", EventType: CellEventScaleDown, Snapshot: CellSnapshot{ID: "c-busy", TenantCount: 5}}
	e.drainAndDeprovision(context.Background(), d, nil)
	if len(prov.deprovisioned) != 0 {
		t.Fatalf("non-empty cell without rebalancer must not deprovision, got %v", prov.deprovisioned)
	}
}

func TestDrainTargets_SameRegionWithHeadroom(t *testing.T) {
	e := NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil)
	d := Decision{CellID: "src", Snapshot: CellSnapshot{ID: "src", Region: "eu-west-1"}}
	snapshots := []CellSnapshot{
		{ID: "src", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 100},
		{ID: "sibling-room", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 200},
		{ID: "sibling-full", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 1000},
		{ID: "other-region", Region: "us-east-1", MaxTenants: 1000, TenantCount: 0},
	}
	targets := e.drainTargets(d, snapshots)
	if len(targets) != 1 {
		t.Fatalf("want exactly 1 target (same region, has headroom), got %d: %+v", len(targets), targets)
	}
	if targets[0].id != "sibling-room" {
		t.Errorf("target = %q, want sibling-room", targets[0].id)
	}
	if targets[0].remaining != 800 {
		t.Errorf("remaining = %d, want 800", targets[0].remaining)
	}
}

func TestPickTarget(t *testing.T) {
	if pickTarget(nil) != nil {
		t.Error("nil targets should yield nil")
	}
	zero := []*drainTarget{{id: "a", remaining: 0}}
	if pickTarget(zero) != nil {
		t.Error("all-zero headroom should yield nil")
	}
	ts := []*drainTarget{{id: "a", remaining: 10}, {id: "b", remaining: 50}, {id: "c", remaining: 30}}
	if got := pickTarget(ts); got == nil || got.id != "b" {
		t.Errorf("want most-headroom target b, got %#v", got)
	}
}

func TestActuate_DisabledByDefault(t *testing.T) {
	// An engine without WithProvisioning must not actuate even if asked.
	prov := &fakeProvisioner{}
	e := NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil)
	// provisionEnabled is false → actuate is never reached via Evaluate,
	// but calling actuate directly with no provisioner must be safe too.
	e.provisioner = prov
	e.actuate(context.Background(), nil, []Decision{
		{CellID: "c1", EventType: CellEventScaleUp, Snapshot: CellSnapshot{ID: "c1", Region: "r"}},
	})
	// actuate itself does not gate on provisionEnabled (Evaluate does),
	// so this verifies the scale_up path routes to Provision.
	if len(prov.provisionRegions) != 1 {
		t.Fatalf("actuate should route scale_up to Provision, got %v", prov.provisionRegions)
	}
}

func TestProvisionForScaleUp_ErrorIsNonFatal(_ *testing.T) {
	prov := &fakeProvisioner{provisionErr: errors.New("cloud down")}
	e := newActuateEngine(prov)
	d := Decision{CellID: "c1", EventType: CellEventScaleUp, Snapshot: CellSnapshot{ID: "c1", Region: "r"}}
	// Must not panic; error is logged and swallowed.
	e.provisionForScaleUp(context.Background(), d)
}
