//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// seedCustomer inserts a crm.customer KRecord with the supplied
// credit_limit so the ledger's credit-check has a counterparty to
// look up. Returns the customer's UUID (stringified for JSONB
// refs). Registration of the crm KTypes is the caller's job.
func seedCustomer(t *testing.T, h *harness, tenantID, actorID uuid.UUID, name string, creditLimit decimal.Decimal) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	limitF, _ := creditLimit.Float64()
	data := map[string]any{
		"name":         name,
		"credit_limit": limitF,
		"currency":     "USD",
		"status":       "active",
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal customer: %v", err)
	}
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     crm.KTypeCustomer,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	return rec.ID
}

// TestPostSalesInvoiceRejectsOverCreditLimit verifies the credit-limit
// guard: the first invoice for 800 posts cleanly against a limit of
// 1000, but the second invoice for 300 (which would push outstanding
// AR to 1100) is rejected with ErrCreditLimitExceeded before any
// journal entry is written.
func TestPostSalesInvoiceRejectsOverCreditLimit(t *testing.T) {
	h := newHarness(t)
	tn, _, poster := newTenantForFinance(t, h)
	// Ensure crm.customer is registered — the phase_c helper only
	// registers finance KTypes.
	if err := crm.RegisterKTypes(context.Background(), h.ktypes); err != nil {
		t.Fatalf("register crm ktypes: %v", err)
	}
	ctx := context.Background()
	actor := uuid.New()

	customerID := seedCustomer(t, h, tn.ID, actor, "Acme Co", decimal.NewFromInt(1000))

	// First invoice — 800, posts cleanly.
	first := createARInvoiceRecord(t, h, tn.ID, actor, "INV-1", customerID.String(),
		decimal.NewFromInt(800), decimal.Zero, "")
	if _, err := poster.PostSalesInvoice(ctx, tn.ID, first, actor); err != nil {
		t.Fatalf("first invoice (under limit): %v", err)
	}

	// Second invoice — 300, would push outstanding to 1100 > 1000.
	second := createARInvoiceRecord(t, h, tn.ID, actor, "INV-2", customerID.String(),
		decimal.NewFromInt(300), decimal.Zero, "")
	_, err := poster.PostSalesInvoice(ctx, tn.ID, second, actor)
	if !errors.Is(err, ledger.ErrCreditLimitExceeded) {
		t.Fatalf("second invoice: want ErrCreditLimitExceeded, got %v", err)
	}

	// A smaller second invoice that keeps the total under the limit
	// must still post cleanly — so the guard is tight, not sticky.
	third := createARInvoiceRecord(t, h, tn.ID, actor, "INV-3", customerID.String(),
		decimal.NewFromInt(150), decimal.Zero, "")
	if _, err := poster.PostSalesInvoice(ctx, tn.ID, third, actor); err != nil {
		t.Fatalf("third invoice (under limit): %v", err)
	}
}

// TestPostSalesInvoiceZeroCreditLimitIsUnlimited covers the ERPNext
// convention: credit_limit = 0 (or missing) disables the cap entirely,
// so a tenant that hasn't configured the field keeps posting.
func TestPostSalesInvoiceZeroCreditLimitIsUnlimited(t *testing.T) {
	h := newHarness(t)
	tn, _, poster := newTenantForFinance(t, h)
	if err := crm.RegisterKTypes(context.Background(), h.ktypes); err != nil {
		t.Fatalf("register crm ktypes: %v", err)
	}
	ctx := context.Background()
	actor := uuid.New()

	customerID := seedCustomer(t, h, tn.ID, actor, "NoLimit Co", decimal.Zero)

	inv := createARInvoiceRecord(t, h, tn.ID, actor, "INV-NL",
		customerID.String(), decimal.NewFromInt(99999), decimal.Zero, "")
	if _, err := poster.PostSalesInvoice(ctx, tn.ID, inv, actor); err != nil {
		t.Fatalf("zero-limit customer: %v", err)
	}
}

// TestPostSalesInvoiceUnknownCustomerSkipsCheck covers the defensive
// path where customer_id points at a record that does not exist (or
// lives in another tenant's scope, which RLS hides). The check
// degrades open rather than rejecting the post, which keeps
// historical / imported invoices working.
func TestPostSalesInvoiceUnknownCustomerSkipsCheck(t *testing.T) {
	h := newHarness(t)
	tn, _, poster := newTenantForFinance(t, h)
	ctx := context.Background()
	actor := uuid.New()

	inv := createARInvoiceRecord(t, h, tn.ID, actor, "INV-ORPHAN",
		uuid.NewString(), decimal.NewFromInt(50), decimal.Zero, "")
	if _, err := poster.PostSalesInvoice(ctx, tn.ID, inv, actor); err != nil {
		t.Fatalf("unknown customer: %v", err)
	}
}
