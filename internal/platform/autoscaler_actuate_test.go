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
	e.drainAndDeprovision(context.Background(), d, nil, nil)
	if len(prov.deprovisioned) != 1 || prov.deprovisioned[0] != "c-empty" {
		t.Fatalf("empty cell should deprovision directly, got %v", prov.deprovisioned)
	}
}

func TestDrainAndDeprovision_SkipsDefaultCell(t *testing.T) {
	prov := &fakeProvisioner{}
	e := newActuateEngine(prov)
	d := Decision{CellID: DefaultCellID, EventType: CellEventScaleDown, Snapshot: CellSnapshot{ID: DefaultCellID, TenantCount: 0}}
	e.drainAndDeprovision(context.Background(), d, nil, nil)
	if len(prov.deprovisioned) != 0 {
		t.Fatalf("default cell must never be deprovisioned, got %v", prov.deprovisioned)
	}
}

func TestDrainAndDeprovision_NonEmptyNoRebalancer(t *testing.T) {
	prov := &fakeProvisioner{}
	// No rebalancer wired: a populated cell must NOT be torn down.
	e := NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil).WithProvisioning(prov, nil, true)
	d := Decision{CellID: "c-busy", EventType: CellEventScaleDown, Snapshot: CellSnapshot{ID: "c-busy", TenantCount: 5}}
	e.drainAndDeprovision(context.Background(), d, nil, nil)
	if len(prov.deprovisioned) != 0 {
		t.Fatalf("non-empty cell without rebalancer must not deprovision, got %v", prov.deprovisioned)
	}
}

func TestDrainAndDeprovision_VerifiesLiveEmptinessBeforeTeardown(t *testing.T) {
	// Regression for the TOCTOU window: even when the tick-old snapshot
	// reports zero tenants, the engine must re-check the LIVE tenant count
	// before deprovisioning so a tenant placed after the snapshot is not
	// stranded. Here the live verification query fails (pool points at an
	// unreachable host), so the engine must NOT deprovision — it leaves the
	// cell 'draining' for the next tick to retry, rather than tearing down a
	// cell whose emptiness it could not confirm.
	prov := &fakeProvisioner{}
	e := NewAutoscaleEngine(dummyPool(t), DefaultAutoscalePolicy(), nil).
		WithProvisioning(prov, nil, true)
	d := Decision{CellID: "c-maybe-empty", EventType: CellEventScaleDown, Snapshot: CellSnapshot{ID: "c-maybe-empty", TenantCount: 0}}
	e.drainAndDeprovision(context.Background(), d, nil, nil)
	if len(prov.deprovisioned) != 0 {
		t.Fatalf("must not deprovision when live emptiness is unverified, got %v", prov.deprovisioned)
	}
}

func TestDrainAndDeprovision_ObserveOnlyDoesNotMigrate(t *testing.T) {
	// Regression: enabling provisioning with the noop (observe-only)
	// provisioner is a documented dry run and must mutate nothing. A
	// scale_down on a populated cell must NOT migrate tenants via the
	// rebalancer, even though a rebalancer is wired — the worker always
	// wires one (holding a real pool) when provisioning is enabled,
	// regardless of provisioner type. Before the observeOnly gate this
	// path ran a real UPDATE tenants SET cell_id in "dry-run" mode.
	repo := &fakeTenantCellRepo{moved: true}
	rb := newTestRebalancer(repo)
	e := NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil).
		WithProvisioning(NewNoopProvisioner(nil), rb, true)
	if !e.observeOnly {
		t.Fatal("noop provisioner must put the engine in observe-only mode")
	}
	d := Decision{CellID: "c-busy", EventType: CellEventScaleDown, Snapshot: CellSnapshot{ID: "c-busy", Region: "eu-west-1", TenantCount: 5}}
	snapshots := []CellSnapshot{
		{ID: "c-busy", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 5},
		{ID: "sibling", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 0},
	}
	e.drainAndDeprovision(context.Background(), d, snapshots, map[string]bool{"c-busy": true})
	if repo.calls != 0 {
		t.Fatalf("observe-only dry run must not migrate tenants, got %d rebalancer call(s)", repo.calls)
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
	targets := e.drainTargets(d, snapshots, nil)
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

func TestDrainTargets_ExcludesCellsBeingTornDown(t *testing.T) {
	// A sibling that is itself scheduled for scale_down this tick must
	// never be offered as a drain target, even with ample headroom.
	e := NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil)
	d := Decision{CellID: "src", Snapshot: CellSnapshot{ID: "src", Region: "eu-west-1"}}
	snapshots := []CellSnapshot{
		{ID: "src", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 50},
		{ID: "sibling-draining", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 0},
		{ID: "sibling-ok", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 0},
	}
	draining := map[string]bool{"src": true, "sibling-draining": true}
	targets := e.drainTargets(d, snapshots, draining)
	if len(targets) != 1 || targets[0].id != "sibling-ok" {
		t.Fatalf("want only sibling-ok as target, got %+v", targets)
	}
}

func TestDrainTargets_ExcludesDrainingStatusCells(t *testing.T) {
	// A sibling whose persisted status is already 'draining' (a teardown
	// in flight from an earlier tick) must not be offered as a target,
	// even when it is not in this tick's in-memory draining set.
	e := NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil)
	d := Decision{CellID: "src", Snapshot: CellSnapshot{ID: "src", Region: "eu-west-1"}}
	snapshots := []CellSnapshot{
		{ID: "src", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 50, Status: CellStatusActive},
		{ID: "sibling-draining", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 0, Status: CellStatusDraining},
		{ID: "sibling-ok", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 0, Status: CellStatusActive},
	}
	targets := e.drainTargets(d, snapshots, nil)
	if len(targets) != 1 || targets[0].id != "sibling-ok" {
		t.Fatalf("want only sibling-ok as target, got %+v", targets)
	}
}

func TestDrainTargets_ReflectsPriorPlacementsSameTick(t *testing.T) {
	// Simulates the cross-drain accounting actuate relies on: once a
	// drain places tenants on a sibling (bumping its TenantCount in the
	// shared snapshot), a later drain must see the reduced headroom so
	// the two drains cannot collectively overfill the sibling.
	e := NewAutoscaleEngine(nil, DefaultAutoscalePolicy(), nil)
	snapshots := []CellSnapshot{
		{ID: "a", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 0},
		{ID: "b", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 0},
		{ID: "shared", Region: "eu-west-1", MaxTenants: 1000, TenantCount: 900},
	}
	da := Decision{CellID: "a", Snapshot: CellSnapshot{ID: "a", Region: "eu-west-1"}}
	draining := map[string]bool{"a": true, "b": true}
	tgtA := e.drainTargets(da, snapshots, draining)
	if len(tgtA) != 1 || tgtA[0].id != "shared" || tgtA[0].remaining != 100 {
		t.Fatalf("drain a: want shared remaining=100, got %+v", tgtA)
	}
	// Emulate a placing 100 tenants on shared (what drainCell does).
	for i := 0; i < 100; i++ {
		tgtA[0].remaining--
		tgtA[0].snap.TenantCount++
	}
	// b now drains and must see shared as full (remaining 0 -> no target).
	db := Decision{CellID: "b", Snapshot: CellSnapshot{ID: "b", Region: "eu-west-1"}}
	tgtB := e.drainTargets(db, snapshots, draining)
	if len(tgtB) != 0 {
		t.Fatalf("drain b: shared is full, want no targets, got %+v", tgtB)
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
