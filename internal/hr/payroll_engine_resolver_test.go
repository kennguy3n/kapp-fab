package hr

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestResolveTaxPack_NilResolver verifies the legacy default: with no
// resolver wired the engine resolves no pack and never errors, so the
// slip takes the no-statutory-withholding path.
func TestResolveTaxPack_NilResolver(t *testing.T) {
	e := NewPayrollEngine(nil, nil)
	pack, err := e.resolveTaxPack(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("nil resolver: unexpected error: %v", err)
	}
	if pack != nil {
		t.Fatalf("nil resolver: expected no pack, got %T", pack)
	}
}

// TestResolveTaxPack_EmptyCountry covers a tenant with no configured
// country: empty + nil must fall through to the no-pack path without
// erroring (this is a legitimate "no statutory deductions" tenant).
func TestResolveTaxPack_EmptyCountry(t *testing.T) {
	e := NewPayrollEngine(nil, nil).WithCountryResolver(
		func(context.Context, uuid.UUID) (string, error) { return "", nil },
	)
	pack, err := e.resolveTaxPack(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("empty country: unexpected error: %v", err)
	}
	if pack != nil {
		t.Fatalf("empty country: expected no pack, got %T", pack)
	}
}

// TestResolveTaxPack_KnownCountry confirms a recognised country code
// resolves to its registered pack.
func TestResolveTaxPack_KnownCountry(t *testing.T) {
	e := NewPayrollEngine(nil, nil).WithCountryResolver(
		func(context.Context, uuid.UUID) (string, error) { return "MY", nil },
	)
	pack, err := e.resolveTaxPack(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("known country: unexpected error: %v", err)
	}
	if pack == nil {
		t.Fatal("known country: expected a registered pack, got nil")
	}
	if pack.Country() != "MY" {
		t.Fatalf("known country: expected MY pack, got %q", pack.Country())
	}
}

// TestResolveTaxPack_UnknownCountry confirms a country with no
// registered pack falls back to the no-pack path rather than erroring.
func TestResolveTaxPack_UnknownCountry(t *testing.T) {
	e := NewPayrollEngine(nil, nil).WithCountryResolver(
		func(context.Context, uuid.UUID) (string, error) { return "ZZ", nil },
	)
	pack, err := e.resolveTaxPack(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unknown country: unexpected error: %v", err)
	}
	if pack != nil {
		t.Fatalf("unknown country: expected no pack, got %T", pack)
	}
}

// TestResolveTaxPack_ResolverError is the core regression guard for the
// P1 fix: a resolver failure (e.g. a transient DB error) MUST abort
// rather than be swallowed into the no-withholding path, which would
// silently under-deduct statutory tax for every employee in the run.
func TestResolveTaxPack_ResolverError(t *testing.T) {
	sentinel := errors.New("boom: tenant store unavailable")
	e := NewPayrollEngine(nil, nil).WithCountryResolver(
		func(context.Context, uuid.UUID) (string, error) { return "", sentinel },
	)
	_, err := e.resolveTaxPack(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("resolver error: expected propagated error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("resolver error: expected wrapped sentinel, got %v", err)
	}
}
