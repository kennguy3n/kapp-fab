package platform

import (
	"testing"
	"time"
)

func TestDecide_HoldByDefault(t *testing.T) {
	s := CellSnapshot{ID: "c1", MaxTenants: 1000, TenantCount: 100, CPUPct: 30, MemoryPct: 40, ConnSaturationPct: 20}
	d := Decide(s, DefaultAutoscalePolicy())
	if d.EventType != CellEventHold {
		t.Fatalf("want hold, got %s (%s)", d.EventType, d.Reason)
	}
}

func TestDecide_ScaleUpOnTenants(t *testing.T) {
	s := CellSnapshot{ID: "c1", MaxTenants: 1000, TenantCount: 1000, CPUPct: 10}
	d := Decide(s, DefaultAutoscalePolicy())
	if d.EventType != CellEventScaleUp {
		t.Fatalf("want scale_up, got %s (%s)", d.EventType, d.Reason)
	}
}

func TestDecide_ScaleUpOnCPU(t *testing.T) {
	s := CellSnapshot{ID: "c1", MaxTenants: 1000, TenantCount: 100, CPUPct: 90}
	d := Decide(s, DefaultAutoscalePolicy())
	if d.EventType != CellEventScaleUp || d.Reason == "" {
		t.Fatalf("want scale_up reason cpu, got %s (%s)", d.EventType, d.Reason)
	}
}

func TestDecide_ScaleUpOnMemory(t *testing.T) {
	s := CellSnapshot{ID: "c1", MaxTenants: 1000, TenantCount: 100, CPUPct: 10, MemoryPct: 95}
	d := Decide(s, DefaultAutoscalePolicy())
	if d.EventType != CellEventScaleUp {
		t.Fatalf("want scale_up, got %s", d.EventType)
	}
}

func TestDecide_ScaleUpOnConn(t *testing.T) {
	s := CellSnapshot{ID: "c1", MaxTenants: 1000, TenantCount: 100, ConnSaturationPct: 80}
	d := Decide(s, DefaultAutoscalePolicy())
	if d.EventType != CellEventScaleUp {
		t.Fatalf("want scale_up, got %s", d.EventType)
	}
}

func TestDecide_ScaleDownWhenIdle(t *testing.T) {
	p := DefaultAutoscalePolicy()
	s := CellSnapshot{
		ID: "c1", MaxTenants: 1000, TenantCount: 50,
		CPUPct: 10, MemoryPct: 15, ConnSaturationPct: 5,
	}
	d := Decide(s, p)
	if d.EventType != CellEventScaleDown {
		t.Fatalf("want scale_down, got %s (%s)", d.EventType, d.Reason)
	}
}

func TestDecide_ScaleDownBlockedByOneHotMetric(t *testing.T) {
	p := DefaultAutoscalePolicy()
	s := CellSnapshot{
		ID: "c1", MaxTenants: 1000, TenantCount: 50,
		CPUPct: 10, MemoryPct: 15, ConnSaturationPct: 50, // > 75/2
	}
	d := Decide(s, p)
	if d.EventType != CellEventHold {
		t.Fatalf("want hold (one hot metric blocks scale_down), got %s (%s)", d.EventType, d.Reason)
	}
}

func TestDecide_CooldownBlocksFlapping(t *testing.T) {
	p := DefaultAutoscalePolicy()
	s := CellSnapshot{
		ID: "c1", MaxTenants: 1000, TenantCount: 1000,
		LastScaleEventAt:   time.Now().Add(-time.Minute),
		LastScaleEventType: CellEventScaleUp,
	}
	d := Decide(s, p)
	if d.EventType != CellEventHold {
		t.Fatalf("expected cooldown hold, got %s (%s)", d.EventType, d.Reason)
	}
}

func TestDecide_CooldownExpired(t *testing.T) {
	p := DefaultAutoscalePolicy()
	s := CellSnapshot{
		ID: "c1", MaxTenants: 1000, TenantCount: 1000,
		LastScaleEventAt:   time.Now().Add(-30 * time.Minute),
		LastScaleEventType: CellEventScaleUp,
	}
	d := Decide(s, p)
	if d.EventType != CellEventScaleUp {
		t.Fatalf("want scale_up after cooldown, got %s (%s)", d.EventType, d.Reason)
	}
}
