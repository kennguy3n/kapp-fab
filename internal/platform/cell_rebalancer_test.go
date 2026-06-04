package platform

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

// fakeTenantCellRepo is an in-memory tenantCellRepo for exercising the
// Rebalancer orchestration without a database.
type fakeTenantCellRepo struct {
	moved   bool
	err     error
	gotMove tenantCellMove
	calls   int
}

func (f *fakeTenantCellRepo) moveTenantCell(_ context.Context, m tenantCellMove) (bool, error) {
	f.calls++
	f.gotMove = m
	return f.moved, f.err
}

type fakeInvalidator struct {
	calledWith []uuid.UUID
}

func (f *fakeInvalidator) InvalidateTenant(id uuid.UUID) {
	f.calledWith = append(f.calledWith, id)
}

func newTestRebalancer(repo tenantCellRepo) *Rebalancer {
	return &Rebalancer{repo: repo, logger: slog.Default()}
}

func TestNormalizeCellID(t *testing.T) {
	cases := map[string]string{
		"":          DefaultCellID,
		"   ":       DefaultCellID,
		"default":   "default",
		" cell-a ":  "cell-a",
		"cell-eu-1": "cell-eu-1",
	}
	for in, want := range cases {
		if got := normalizeCellID(in); got != want {
			t.Errorf("normalizeCellID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeMigration(t *testing.T) {
	tid := uuid.New()

	t.Run("nil tenant", func(t *testing.T) {
		if _, err := normalizeMigration(uuid.Nil, "a", "b"); err == nil {
			t.Fatal("want error for nil tenant id")
		}
	})
	t.Run("empty destination", func(t *testing.T) {
		if _, err := normalizeMigration(tid, "a", "   "); err == nil {
			t.Fatal("want error for empty destination")
		}
	})
	t.Run("noop when equal", func(t *testing.T) {
		if _, err := normalizeMigration(tid, "cell-a", "cell-a"); !errors.Is(err, ErrNoOpMigration) {
			t.Fatalf("want ErrNoOpMigration, got %v", err)
		}
	})
	t.Run("noop when both default", func(t *testing.T) {
		// empty from normalises to default; explicit default destination.
		if _, err := normalizeMigration(tid, "", "default"); !errors.Is(err, ErrNoOpMigration) {
			t.Fatalf("want ErrNoOpMigration, got %v", err)
		}
	})
	t.Run("default source normalisation", func(t *testing.T) {
		m, err := normalizeMigration(tid, "", "cell-b")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if m.FromCellID != DefaultCellID || m.ToCellID != "cell-b" {
			t.Fatalf("unexpected move: %#v", m)
		}
	})
}

func TestMigrateTenant_Success(t *testing.T) {
	repo := &fakeTenantCellRepo{moved: true}
	inv := &fakeInvalidator{}
	r := newTestRebalancer(repo).WithCacheInvalidator(inv)
	tid := uuid.New()

	if err := r.MigrateTenant(context.Background(), tid, "cell-a", "cell-b"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if repo.gotMove.TenantID != tid || repo.gotMove.FromCellID != "cell-a" || repo.gotMove.ToCellID != "cell-b" {
		t.Errorf("unexpected move: %#v", repo.gotMove)
	}
	if len(inv.calledWith) != 1 || inv.calledWith[0] != tid {
		t.Errorf("invalidator not called with tenant id: %v", inv.calledWith)
	}
}

func TestMigrateTenant_NotOnSourceCell(t *testing.T) {
	repo := &fakeTenantCellRepo{moved: false}
	inv := &fakeInvalidator{}
	r := newTestRebalancer(repo).WithCacheInvalidator(inv)

	err := r.MigrateTenant(context.Background(), uuid.New(), "cell-a", "cell-b")
	if !errors.Is(err, ErrTenantNotOnSourceCell) {
		t.Fatalf("want ErrTenantNotOnSourceCell, got %v", err)
	}
	if len(inv.calledWith) != 0 {
		t.Errorf("invalidator must not fire when nothing moved")
	}
}

func TestMigrateTenant_RepoError(t *testing.T) {
	repo := &fakeTenantCellRepo{err: errors.New("db down")}
	r := newTestRebalancer(repo)
	err := r.MigrateTenant(context.Background(), uuid.New(), "cell-a", "cell-b")
	if err == nil || errors.Is(err, ErrTenantNotOnSourceCell) {
		t.Fatalf("want wrapped repo error, got %v", err)
	}
}

func TestMigrateTenant_NoOpSkipsRepo(t *testing.T) {
	repo := &fakeTenantCellRepo{moved: true}
	r := newTestRebalancer(repo)
	err := r.MigrateTenant(context.Background(), uuid.New(), "cell-a", "cell-a")
	if !errors.Is(err, ErrNoOpMigration) {
		t.Fatalf("want ErrNoOpMigration, got %v", err)
	}
	if repo.calls != 0 {
		t.Errorf("repo must not be called for a no-op migration, calls=%d", repo.calls)
	}
}

func TestMigrateTenant_NotConfigured(t *testing.T) {
	var r *Rebalancer
	if err := r.MigrateTenant(context.Background(), uuid.New(), "a", "b"); err == nil {
		t.Fatal("want error on nil rebalancer")
	}
	r2 := &Rebalancer{logger: slog.Default()} // nil repo
	if err := r2.MigrateTenant(context.Background(), uuid.New(), "a", "b"); err == nil {
		t.Fatal("want error on nil repo")
	}
}
