package sales

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// Phase M Task 6 — POS module.
//
// Two new KTypes (sales.pos_profile + sales.pos_invoice) plus a
// PostPOSInvoice helper that turns a finalised pos_invoice into a
// posted ar_invoice + reconciled payment in a single call. The
// helper reuses ledger.InvoicePoster.PostSalesInvoice (which
// already triggers inventory.PosterHook for goods delivery) and
// ledger.PaymentPoster.PostPayment so the AR + revenue + COGS
// double-entry stays the single source of truth — pos_invoice is
// just a richer source document with cart UX metadata.

const (
	KTypePOSProfile = "sales.pos_profile"
	KTypePOSInvoice = "sales.pos_invoice"
)

var posProfileSchema = []byte(`{
  "name": "sales.pos_profile",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "warehouse_id", "type": "ref", "ktype": "inventory.warehouse", "required": true},
    {"name": "default_customer_id", "type": "ref", "ktype": "crm.organization"},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "payment_methods", "type": "json"},
    {"name": "ar_account_code", "type": "string", "max_length": 32},
    {"name": "revenue_account_code", "type": "string", "max_length": 32},
    {"name": "bank_account_code", "type": "string", "max_length": 32},
    {"name": "tax_code", "type": "string", "max_length": 32},
    {"name": "tax_account_code", "type": "string", "max_length": 32},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["name", "warehouse_id", "currency", "active"]},
    "form": {"sections": [
      {"title": "Profile", "fields": ["name", "warehouse_id", "default_customer_id", "currency", "active"]},
      {"title": "Payment", "fields": ["payment_methods"]},
      {"title": "Accounts", "fields": ["ar_account_code", "revenue_account_code", "bank_account_code", "tax_code", "tax_account_code"]}
    ]}
  },
  "cards": {"summary": "{{name}} — {{warehouse_id}} ({{currency}})"},
  "permissions": {"read": ["tenant.member"], "write": ["sales.admin", "tenant.admin"]}
}`)

var posInvoiceSchema = []byte(`{
  "name": "sales.pos_invoice",
  "version": 1,
  "fields": [
    {"name": "profile_id", "type": "ref", "ktype": "sales.pos_profile", "required": true},
    {"name": "customer_id", "type": "ref", "ktype": "crm.organization"},
    {"name": "invoice_number", "type": "string", "max_length": 64},
    {"name": "issue_date", "type": "date"},
    {"name": "lines", "type": "array"},
    {"name": "payments", "type": "array"},
    {"name": "subtotal", "type": "number", "min": 0},
    {"name": "tax_amount", "type": "number", "min": 0},
    {"name": "total", "type": "number", "min": 0},
    {"name": "tendered", "type": "number", "min": 0},
    {"name": "change_due", "type": "number"},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["draft", "posted", "voided"], "default": "draft"},
    {"name": "ar_invoice_id", "type": "ref", "ktype": "finance.ar_invoice"},
    {"name": "payment_id", "type": "ref", "ktype": "finance.payment"},
    {"name": "idempotency_key", "type": "string", "max_length": 128},
    {"name": "cashier_id", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["invoice_number", "profile_id", "customer_id", "total", "currency", "status"]},
    "form": {"sections": [
      {"title": "Receipt", "fields": ["invoice_number", "profile_id", "customer_id", "issue_date", "currency", "cashier_id"]},
      {"title": "Cart", "fields": ["lines", "subtotal", "tax_amount", "total"]},
      {"title": "Payment", "fields": ["payments", "tendered", "change_due"]},
      {"title": "Posting", "fields": ["status", "ar_invoice_id", "payment_id", "idempotency_key"]}
    ]}
  },
  "cards": {"summary": "POS {{invoice_number}} — {{total}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["sales.member", "tenant.admin"]},
  "workflow": {
    "name": "sales.pos_invoice.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "posted", "voided"],
    "transitions": [
      {"from": ["draft"], "to": "posted", "action": "finalize"},
      {"from": ["draft"], "to": "voided", "action": "void"}
    ]
  },
  "agent_tools": []
}`)

// POSKTypes returns the Phase M Task 6 POS KTypes as a fresh slice
// so callers can register them alongside the rest of the sales
// catalog. Symmetric with sales.All() but kept opt-in (mirrors
// hr.PayrollKTypes) so a deployment that doesn't enable the POS
// feature flag can simply skip the call.
func POSKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypePOSProfile, Version: 1, Schema: posProfileSchema},
		{Name: KTypePOSInvoice, Version: 1, Schema: posInvoiceSchema},
	}
}

// posPoster carries the dependencies PostPOSInvoice needs. The
// constructor takes the same record + invoice + payment poster
// instances services/api/main.go already wires up so the helper
// can be slotted into existing handlers without re-plumbing the
// dependency tree.
type POSPoster struct {
	records *record.PGStore
	invoice *ledger.InvoicePoster
	payment *ledger.PaymentPoster
}

// NewPOSPoster builds the helper. All three dependencies are
// required at runtime; tests can stub them via interfaces if
// needed but this surface keeps the prod call site terse.
func NewPOSPoster(records *record.PGStore, invoice *ledger.InvoicePoster, payment *ledger.PaymentPoster) *POSPoster {
	return &POSPoster{records: records, invoice: invoice, payment: payment}
}

// PostPOSInvoice promotes a draft pos_invoice into a posted state.
//
// Steps:
//  1. Read the pos_invoice and its referenced pos_profile.
//  2. Build a finance.ar_invoice KRecord from the cart lines and
//     post it via InvoicePoster.PostSalesInvoice — which fires
//     the inventory hook for goods delivery automatically.
//  3. Build a finance.payment KRecord covering the same total and
//     post it via PaymentPoster.PostPayment so AR is reconciled in
//     the same call.
//  4. Update the pos_invoice with status=posted, ar_invoice_id,
//     and payment_id refs. Idempotent against the
//     pos_invoice.idempotency_key field — a duplicate finalize
//     call short-circuits to the prior result.
func (p *POSPoster) PostPOSInvoice(ctx context.Context, tenantID, posInvoiceID, actorID uuid.UUID) (*record.KRecord, error) {
	if p == nil || p.records == nil || p.invoice == nil || p.payment == nil {
		return nil, errors.New("pos: poster not configured")
	}
	posRec, err := p.records.Get(ctx, tenantID, posInvoiceID)
	if err != nil {
		return nil, fmt.Errorf("pos: load invoice: %w", err)
	}
	if posRec == nil || posRec.KType != KTypePOSInvoice {
		return nil, fmt.Errorf("pos: %s not a pos_invoice", posInvoiceID)
	}
	var current map[string]any
	if err := json.Unmarshal(posRec.Data, &current); err != nil {
		return nil, fmt.Errorf("pos: decode invoice: %w", err)
	}
	status, _ := current["status"].(string)
	if status == "posted" {
		return posRec, nil
	}
	if status != "draft" && status != "" {
		return nil, fmt.Errorf("pos: cannot finalize invoice in %q state", status)
	}

	profileID, err := refUUID(current, "profile_id")
	if err != nil {
		return nil, fmt.Errorf("pos: profile_id: %w", err)
	}
	profileRec, err := p.records.Get(ctx, tenantID, profileID)
	if err != nil || profileRec == nil {
		return nil, fmt.Errorf("pos: load profile %s: %w", profileID, err)
	}
	var profile map[string]any
	if err := json.Unmarshal(profileRec.Data, &profile); err != nil {
		return nil, fmt.Errorf("pos: decode profile: %w", err)
	}

	customerID, _ := refUUID(current, "customer_id")
	if customerID == uuid.Nil {
		customerID, _ = refUUID(profile, "default_customer_id")
	}
	if customerID == uuid.Nil {
		return nil, errors.New("pos: customer_id required (set on pos_invoice or pos_profile.default_customer_id)")
	}

	currency := stringOr(current, "currency", stringOr(profile, "currency", "USD"))
	total := decimalOr(current, "total")
	if !total.IsPositive() {
		return nil, errors.New("pos: total must be > 0")
	}

	// 1. Build + post AR invoice.
	// Numeric fields ride as float64 so the AR-invoice schema's
	// `type: number` validator accepts them — decimal.Decimal
	// renders as a quoted string under json.Marshal which the
	// validator correctly rejects. decimalOr() in the AR poster
	// unwraps either form, so we lose no precision on the values
	// we care about (cents-scale POS totals).
	// Dates default to today (UTC) so finalising an in-memory
	// pos_invoice that never set issue_date doesn't trip the
	// ISO-8601 validator one layer deeper.
	issueDate := stringOr(current, "issue_date", time.Now().UTC().Format("2006-01-02"))
	arBody := map[string]any{
		"customer_id":          customerID.String(),
		"invoice_number":       stringOr(current, "invoice_number", ""),
		"issue_date":           issueDate,
		"due_date":             stringOr(current, "due_date", issueDate),
		"lines":                rawArray(posRec.Data, "lines"),
		"subtotal":             decimalOr(current, "subtotal").InexactFloat64(),
		"tax_amount":           decimalOr(current, "tax_amount").InexactFloat64(),
		"total":                total.InexactFloat64(),
		"currency":             currency,
		"status":               "draft",
		"ar_account_code":      stringOr(profile, "ar_account_code", ""),
		"revenue_account_code": stringOr(profile, "revenue_account_code", ""),
		"tax_account_code":     stringOr(profile, "tax_account_code", ""),
	}
	arBytes, _ := json.Marshal(arBody)

	// Resumable state machine: if a previous attempt already
	// allocated the AR invoice but failed before flipping the
	// pos_invoice to posted, reuse that AR rather than creating a
	// fresh one. Without this, every retry walks past the
	// `status == "posted"` short-circuit, calls
	// records.Create with a new uuid, and PostSalesInvoice's
	// (source_ktype, source_id) idempotency check sees a different
	// source_id — so the duplicate gets posted, double-counts
	// revenue, and double-debits inventory.
	var arRec *record.KRecord
	if existingARID, _ := refUUID(current, "ar_invoice_id"); existingARID != uuid.Nil {
		arRec, err = p.records.Get(ctx, tenantID, existingARID)
		if err != nil {
			return nil, fmt.Errorf("pos: load existing ar_invoice %s: %w", existingARID, err)
		}
	} else {
		arRec, err = p.records.Create(ctx, record.KRecord{
			ID:        uuid.New(),
			TenantID:  tenantID,
			KType:     finance.KTypeARInvoice,
			Data:      arBytes,
			CreatedBy: actorID,
		})
		if err != nil {
			return nil, fmt.Errorf("pos: create ar_invoice: %w", err)
		}
		// Persist the AR id onto the pos_invoice immediately so a
		// retry after a partial failure reuses this AR. We have not
		// posted it yet — that's deliberate, the post step is
		// idempotent on (source_ktype, source_id) so calling it
		// again with the same arRec.ID short-circuits.
		current["ar_invoice_id"] = arRec.ID.String()
		posRec.Data, _ = json.Marshal(current)
		posRec.UpdatedBy = &actorID
		updated, err := p.records.Update(ctx, *posRec)
		if err != nil {
			return nil, fmt.Errorf("pos: persist ar_invoice_id: %w", err)
		}
		posRec = updated
	}
	// Tolerate ErrInvoiceAlreadyPosted: a previous attempt may have
	// already posted the AR (status flipped to posted, JE created)
	// and only crashed on the way back to update the pos_invoice.
	// The poster's status guard short-circuits with that sentinel,
	// which the resume path treats as success — the JE is already
	// on the ledger, calling it again would be a no-op anyway.
	if _, err := p.invoice.PostSalesInvoice(ctx, tenantID, arRec.ID, actorID); err != nil && !errors.Is(err, ledger.ErrInvoiceAlreadyPosted) {
		return nil, fmt.Errorf("pos: post ar_invoice: %w", err)
	}

	// 2. Build + post payment that allocates against the AR invoice.
	// Same resumable pattern as the AR step above so a partial
	// failure between Create and PostPayment doesn't allocate a
	// duplicate payment on retry.
	payBody := map[string]any{
		"payment_type":    "receive",
		"party_type":      "customer",
		"party_id":        customerID.String(),
		"amount":          total.InexactFloat64(),
		"currency":        currency,
		"payment_date":    issueDate,
		"reference":       stringOr(current, "invoice_number", "POS"),
		"bank_account":    stringOr(profile, "bank_account_code", ""),
		"ar_account_code": stringOr(profile, "ar_account_code", ""),
		"allocations": []map[string]any{
			{"invoice_id": arRec.ID.String(), "allocated_amount": total.InexactFloat64()},
		},
	}
	payBytes, _ := json.Marshal(payBody)
	var payRec *record.KRecord
	if existingPayID, _ := refUUID(current, "payment_id"); existingPayID != uuid.Nil {
		payRec, err = p.records.Get(ctx, tenantID, existingPayID)
		if err != nil {
			return nil, fmt.Errorf("pos: load existing payment %s: %w", existingPayID, err)
		}
	} else {
		payRec, err = p.records.Create(ctx, record.KRecord{
			ID:        uuid.New(),
			TenantID:  tenantID,
			KType:     finance.KTypePayment,
			Data:      payBytes,
			CreatedBy: actorID,
		})
		if err != nil {
			return nil, fmt.Errorf("pos: create payment: %w", err)
		}
		current["payment_id"] = payRec.ID.String()
		posRec.Data, _ = json.Marshal(current)
		posRec.UpdatedBy = &actorID
		updated, err := p.records.Update(ctx, *posRec)
		if err != nil {
			return nil, fmt.Errorf("pos: persist payment_id: %w", err)
		}
		posRec = updated
	}
	// Same tolerate-already-posted semantics as the AR step: a
	// crash between PostPayment success and the pos_invoice flip
	// would otherwise permanently block the resume.
	if _, err := p.payment.PostPayment(ctx, tenantID, payRec.ID, actorID); err != nil && !errors.Is(err, ledger.ErrPaymentAlreadyPosted) {
		return nil, fmt.Errorf("pos: post payment: %w", err)
	}

	// 3. Flip pos_invoice to posted with refs back to AR + payment.
	current["status"] = "posted"
	current["ar_invoice_id"] = arRec.ID.String()
	current["payment_id"] = payRec.ID.String()
	updated, _ := json.Marshal(current)
	posRec.Data = updated
	posRec.UpdatedBy = &actorID
	updatedRec, err := p.records.Update(ctx, *posRec)
	if err != nil {
		return nil, fmt.Errorf("pos: update pos_invoice: %w", err)
	}
	return updatedRec, nil
}

// helpers (unexported) ------------------------------------------

func refUUID(m map[string]any, key string) (uuid.UUID, error) {
	raw, ok := m[key].(string)
	if !ok || raw == "" {
		return uuid.Nil, errors.New("missing")
	}
	return uuid.Parse(raw)
}

func stringOr(m map[string]any, key, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func decimalOr(m map[string]any, key string) decimal.Decimal {
	switch v := m[key].(type) {
	case float64:
		return decimal.NewFromFloat(v)
	case string:
		d, err := decimal.NewFromString(v)
		if err == nil {
			return d
		}
	case decimal.Decimal:
		return v
	}
	return decimal.Zero
}

// rawArray extracts the JSON value at top-level key as a json.RawMessage,
// preserving the exact serialization (decimals, ordering) the caller
// supplied. Returns an empty array when the key is missing so the
// downstream `lines` consumer always sees a parseable array.
func rawArray(data []byte, key string) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return json.RawMessage("[]")
	}
	if raw, ok := top[key]; ok {
		return raw
	}
	return json.RawMessage("[]")
}
