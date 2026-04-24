package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// ActionTypeRecurringInvoice is the scheduled_actions.action_type the
// generator registers under. The tenant wizard seeds one row of this
// type per tenant; per-recurring-invoice cadences are encoded in the
// individual finance.recurring_invoice KRecord rather than as
// separate scheduled_actions rows.
const ActionTypeRecurringInvoice = "recurring_invoice"

// DefaultRecurringInvoiceIntervalSeconds is the cadence the wizard
// seeds the sweeper with. Hourly is fine because per-row eligibility
// is gated on next_generation_date (a date, not a timestamp), so a
// run more often than once per day costs no extra invoices — only
// SQL filter passes.
const DefaultRecurringInvoiceIntervalSeconds = 3600

// Frequency identifiers for the recurring_invoice schema. The values
// match the enum in finance.recurring_invoice's KType definition; any
// drift trips ValidateData on the next Create call.
const (
	FrequencyDaily     = "daily"
	FrequencyWeekly    = "weekly"
	FrequencyMonthly   = "monthly"
	FrequencyQuarterly = "quarterly"
	FrequencyYearly    = "yearly"
)

// Status identifiers for recurring_invoice rows.
const (
	RecurringStatusActive    = "active"
	RecurringStatusPaused    = "paused"
	RecurringStatusCompleted = "completed"
)

// SalesInvoicePosterFunc is the slice of ledger.InvoicePoster the
// generator depends on. Defined as a function type so finance/ does
// not need to import internal/ledger (which already depends on
// internal/finance through the KType constants — going the other
// way would create a cycle). The caller wires it as:
//
//	finance.NewRecurringEngine(records, func(ctx context.Context, t, i, a uuid.UUID) error {
//	    _, err := poster.PostSalesInvoice(ctx, t, i, a)
//	    return err
//	})
type SalesInvoicePosterFunc func(ctx context.Context, tenantID, invoiceID, actorID uuid.UUID) error

// RecurringEngine is the scheduler.ActionHandler that materialises
// finance.recurring_invoice rows into fresh AR invoices each time
// next_generation_date <= today. The engine is stateless across
// calls; per-row cursor (next_generation_date / last_generated_at)
// lives in the recurring_invoice KRecord itself.
//
// auto_post=true rows are posted via the supplied SalesInvoicePoster
// after the draft Create succeeds. A posting failure leaves the draft
// in place so the operator can retry from the UI without rerunning
// the cloning logic — duplicates are avoided because the recurring
// row has already advanced next_generation_date by then.
type RecurringEngine struct {
	records *record.PGStore
	poster  SalesInvoicePosterFunc
	now     func() time.Time
	// systemActor stamps Created/UpdatedBy on the generated invoice
	// and the recurring_invoice update so audit trails attribute the
	// row to a deterministic synthetic user. Defaults to a zero UUID
	// which the audit log treats as "system".
	systemActor uuid.UUID
}

// NewRecurringEngine wires the engine against the shared record
// store and a sales-invoice poster. poster may be nil — in that case
// the engine still clones drafts but never auto-posts (any row with
// auto_post=true logs a warning and is left as a draft).
func NewRecurringEngine(records *record.PGStore, poster SalesInvoicePosterFunc) *RecurringEngine {
	return &RecurringEngine{
		records:     records,
		poster:      poster,
		now:         func() time.Time { return time.Now().UTC() },
		systemActor: uuid.Nil,
	}
}

// WithClock substitutes the engine's time source for deterministic
// tests. Calling code that leaves it unset falls back to time.Now UTC.
func (e *RecurringEngine) WithClock(now func() time.Time) *RecurringEngine {
	if now != nil {
		e.now = now
	}
	return e
}

// WithSystemActor stamps a non-zero actor UUID on generated invoices
// and the recurring-row update. Useful in tests; production wiring
// should pick a stable system-user UUID so audit queries can pivot
// on it.
func (e *RecurringEngine) WithSystemActor(actor uuid.UUID) *RecurringEngine {
	if actor != uuid.Nil {
		e.systemActor = actor
	}
	return e
}

// Handle implements scheduler.ActionHandler. It enumerates active
// recurring_invoice rows for the action's tenant, generates an
// invoice for each whose next_generation_date <= today (UTC), and
// updates the recurring row's cursor. Single-row failures are
// logged-and-skipped so one bad template does not stall the others.
func (e *RecurringEngine) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if e == nil || e.records == nil {
		return errors.New("finance: recurring engine not wired")
	}
	today := e.now().UTC().Truncate(24 * time.Hour)
	rows, err := e.records.List(ctx, tenantID, record.ListFilter{
		KType:  KTypeRecurringInvoice,
		Status: "active",
		Limit:  500,
	})
	if err != nil {
		return fmt.Errorf("finance: list recurring invoices: %w", err)
	}
	for i := range rows {
		row := rows[i]
		if err := e.generateOne(ctx, tenantID, row, today); err != nil {
			log.Printf("finance: recurring tenant=%s row=%s: %v",
				tenantID, row.ID, err)
			continue
		}
	}
	return nil
}

// maxCatchUpIterations caps the per-sweep catch-up loop so a
// misconfigured row (e.g. next_generation_date=2001-01-01, frequency
// daily) cannot produce 9000 invoices in a single sweep. The next
// sweep will pick up where this one left off.
const maxCatchUpIterations = 120

// generateOne is the per-row body. It is broken out so Handle can
// log-and-continue on individual errors instead of aborting the
// entire sweep.
//
// When next_generation_date is many cadence steps behind today (e.g.
// worker outage across several periods), the body emits one invoice
// per missed step and advances the cursor each time, so no periods
// are silently squashed into a single invoice. Cadence is always
// advanced from the stored next_generation_date, never from today —
// that keeps monthly invoices anchored to the original day-of-month
// even when the sweeper fires late.
func (e *RecurringEngine) generateOne(
	ctx context.Context,
	tenantID uuid.UUID,
	row record.KRecord,
	today time.Time,
) error {
	var data map[string]any
	if err := json.Unmarshal(row.Data, &data); err != nil {
		return fmt.Errorf("decode recurring_invoice: %w", err)
	}
	status, _ := data["status"].(string)
	if status != "" && status != RecurringStatusActive {
		return nil
	}
	nextDateStr, _ := data["next_generation_date"].(string)
	if nextDateStr == "" {
		return errors.New("recurring_invoice missing next_generation_date")
	}
	cursor, err := parseDate(nextDateStr)
	if err != nil {
		return fmt.Errorf("parse next_generation_date: %w", err)
	}
	if cursor.After(today) {
		return nil
	}
	endStr, _ := data["end_date"].(string)
	var endDate time.Time
	hasEnd := false
	if endStr != "" {
		endDate, err = parseDate(endStr)
		if err != nil {
			return fmt.Errorf("parse end_date: %w", err)
		}
		hasEnd = true
		if today.After(endDate) && cursor.After(endDate) {
			// Cadence has run out — flip the row to completed so
			// the next sweep skips it cheaply rather than
			// computing the cursor every time.
			data["status"] = RecurringStatusCompleted
			return e.persistRecurring(ctx, tenantID, row, data)
		}
	}
	templateIDStr, _ := data["template_invoice_id"].(string)
	if templateIDStr == "" {
		return errors.New("recurring_invoice missing template_invoice_id")
	}
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		return fmt.Errorf("parse template_invoice_id: %w", err)
	}
	template, err := e.records.Get(ctx, tenantID, templateID)
	if err != nil {
		return fmt.Errorf("load template invoice: %w", err)
	}
	freq, _ := data["frequency"].(string)
	autoPost, _ := data["auto_post"].(bool)

	// Catch-up loop: one iteration per missed cadence step. Each
	// iteration clones the template against the cursor (the
	// scheduled date — not today), advances the cursor from that
	// same date, and stops when the cursor moves past today or the
	// end_date.
	for i := 0; i < maxCatchUpIterations; i++ {
		if cursor.After(today) {
			break
		}
		if hasEnd && cursor.After(endDate) {
			data["status"] = RecurringStatusCompleted
			break
		}
		created, err := e.cloneTemplate(ctx, tenantID, template, cursor)
		if err != nil {
			return fmt.Errorf("clone template: %w", err)
		}
		if autoPost {
			if e.poster == nil {
				log.Printf("finance: auto_post requested but poster nil; tenant=%s recurring=%s draft=%s",
					tenantID, row.ID, created.ID)
			} else if err := e.poster(ctx, tenantID, created.ID, e.systemActor); err != nil {
				log.Printf("finance: auto_post failed; tenant=%s recurring=%s draft=%s: %v",
					tenantID, row.ID, created.ID, err)
			}
		}
		advanced, err := AdvanceDate(cursor, freq)
		if err != nil {
			return fmt.Errorf("advance cadence: %w", err)
		}
		data["last_generated_at"] = e.now().UTC().Format(time.RFC3339)
		data["last_generated_invoice_id"] = created.ID.String()
		cursor = advanced
	}
	data["next_generation_date"] = cursor.Format(dateLayout)
	if hasEnd && cursor.After(endDate) {
		data["status"] = RecurringStatusCompleted
	}
	return e.persistRecurring(ctx, tenantID, row, data)
}

// cloneTemplate copies the template invoice's data into a fresh
// draft, replaces issue_date/due_date with today / today+offset, and
// strips fields that should not carry over (status, posting cursors).
func (e *RecurringEngine) cloneTemplate(
	ctx context.Context,
	tenantID uuid.UUID,
	template *record.KRecord,
	today time.Time,
) (*record.KRecord, error) {
	var src map[string]any
	if err := json.Unmarshal(template.Data, &src); err != nil {
		return nil, fmt.Errorf("decode template: %w", err)
	}
	clone := make(map[string]any, len(src))
	for k, v := range src {
		clone[k] = v
	}
	// Strip cursors that must not survive the clone.
	delete(clone, "journal_entry_id")
	delete(clone, "invoice_number")
	clone["status"] = "draft"
	clone["issue_date"] = today.Format(dateLayout)
	// Preserve the template's net terms (days between issue and due)
	// so the cloned due_date sits at issue_date + same delta.
	if origIssue, ok := src["issue_date"].(string); ok {
		if origDue, ok := src["due_date"].(string); ok {
			oi, err1 := parseDate(origIssue)
			od, err2 := parseDate(origDue)
			if err1 == nil && err2 == nil {
				delta := od.Sub(oi)
				clone["due_date"] = today.Add(delta).Format(dateLayout)
			}
		}
	}
	cloneJSON, err := json.Marshal(clone)
	if err != nil {
		return nil, fmt.Errorf("encode clone: %w", err)
	}
	actor := e.systemActor
	if actor == uuid.Nil {
		actor = template.CreatedBy
	}
	return e.records.Create(ctx, record.KRecord{
		TenantID:     tenantID,
		KType:        KTypeARInvoice,
		KTypeVersion: template.KTypeVersion,
		Data:         cloneJSON,
		CreatedBy:    actor,
	})
}

// persistRecurring writes the updated recurring_invoice data back
// through the record store so audit + outbox fire normally.
func (e *RecurringEngine) persistRecurring(
	ctx context.Context,
	tenantID uuid.UUID,
	row record.KRecord,
	data map[string]any,
) error {
	patch, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode recurring patch: %w", err)
	}
	actor := e.systemActor
	if actor == uuid.Nil {
		actor = row.CreatedBy
	}
	if _, err := e.records.Update(ctx, record.KRecord{
		ID:        row.ID,
		TenantID:  tenantID,
		KType:     row.KType,
		Data:      patch,
		Version:   row.Version,
		UpdatedBy: &actor,
	}); err != nil {
		return fmt.Errorf("update recurring row: %w", err)
	}
	return nil
}

// AdvanceDate returns the next firing date for the given frequency.
// Exposed so unit tests can drive each branch without going through
// the engine.
func AdvanceDate(from time.Time, frequency string) (time.Time, error) {
	switch frequency {
	case FrequencyDaily:
		return from.AddDate(0, 0, 1), nil
	case FrequencyWeekly:
		return from.AddDate(0, 0, 7), nil
	case FrequencyMonthly:
		return from.AddDate(0, 1, 0), nil
	case FrequencyQuarterly:
		return from.AddDate(0, 3, 0), nil
	case FrequencyYearly:
		return from.AddDate(1, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unknown frequency %q", frequency)
	}
}

const dateLayout = "2006-01-02"

func parseDate(s string) (time.Time, error) {
	// Accept both date-only ("2025-01-31") and full RFC3339 timestamps
	// so a recurring_invoice authored with a full datetime in the
	// next_generation_date field still parses.
	if t, err := time.Parse(dateLayout, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Truncate(24 * time.Hour), nil
	}
	return time.Time{}, fmt.Errorf("invalid date %q", s)
}
