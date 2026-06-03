package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Store persists the billing_subscriptions / billing_invoices /
// billing_events tables.
//
// Two pools are held, matching the pattern in
// internal/scheduler.Store and internal/auth.SSOService:
//
//   - pool      the tenant-scoped kapp_app pool. Every per-tenant
//     read/write runs under dbutil.WithTenantTx so the RLS
//     policies in migration 000078 are enforced.
//   - adminPool the BYPASSRLS kapp_admin pool, used ONLY for the
//     reverse lookups the Stripe webhook path needs: given
//     a Stripe customer/subscription id and no tenant
//     context, find the owning tenant. Those queries
//     legitimately span tenants and cannot set
//     app.tenant_id (we don't know it yet). adminPool may
//     be nil, in which case the reverse lookups return
//     ErrUnknownCustomer (degraded mode — the webhook
//     handler then 404s rather than mis-attributing).
type Store struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
	now       func() time.Time
}

// NewStore binds a Store to the tenant-scoped pool. Use WithAdminPool
// to wire the BYPASSRLS pool required by the webhook reverse lookups.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool: pool,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// WithAdminPool returns a copy of s using adminPool for the
// cross-tenant webhook reverse lookups. adminPool must connect as a
// BYPASSRLS role (kapp_admin).
func (s *Store) WithAdminPool(adminPool *pgxpool.Pool) *Store {
	cp := *s
	cp.adminPool = adminPool
	return &cp
}

// WithNow pins the clock for deterministic tests.
func (s *Store) WithNow(now func() time.Time) *Store {
	if now != nil {
		cp := *s
		cp.now = now
		return &cp
	}
	return s
}

const subscriptionColumns = `id, tenant_id, plan, status, stripe_customer_id,
	stripe_subscription_id, stripe_subscription_item_id, cancel_at_period_end,
	current_period_end, trial_end, created_at, updated_at`

// UpsertSubscription inserts or updates the tenant's single
// subscription row (UNIQUE(tenant_id) makes this a clean
// ON CONFLICT (tenant_id)). The id is preserved across updates. Runs
// under the tenant GUC so RLS WITH CHECK passes.
func (s *Store) UpsertSubscription(ctx context.Context, sub Subscription) (*Subscription, error) {
	if sub.TenantID == uuid.Nil {
		return nil, errors.New("billing: tenant id required")
	}
	if sub.ID == uuid.Nil {
		sub.ID = uuid.New()
	}
	var out Subscription
	err := dbutil.WithTenantTx(ctx, s.pool, sub.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanSubscription(tx.QueryRow(ctx,
			`INSERT INTO billing_subscriptions
			   (id, tenant_id, plan, status, stripe_customer_id,
			    stripe_subscription_id, stripe_subscription_item_id,
			    cancel_at_period_end, current_period_end, trial_end)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			 ON CONFLICT (tenant_id) DO UPDATE SET
			    plan = EXCLUDED.plan,
			    status = EXCLUDED.status,
			    stripe_customer_id = EXCLUDED.stripe_customer_id,
			    stripe_subscription_id = EXCLUDED.stripe_subscription_id,
			    stripe_subscription_item_id = EXCLUDED.stripe_subscription_item_id,
			    cancel_at_period_end = EXCLUDED.cancel_at_period_end,
			    current_period_end = EXCLUDED.current_period_end,
			    trial_end = EXCLUDED.trial_end,
			    updated_at = now()
			 RETURNING `+subscriptionColumns,
			sub.ID, sub.TenantID, sub.Plan, sub.Status, sub.StripeCustomerID,
			sub.StripeSubscriptionID, sub.StripeSubscriptionItemID,
			sub.CancelAtPeriodEnd, sub.CurrentPeriodEnd, sub.TrialEnd,
		), &out)
	})
	if err != nil {
		return nil, fmt.Errorf("billing: upsert subscription: %w", err)
	}
	return &out, nil
}

// GetSubscriptionByTenant returns the tenant's subscription or
// ErrNoSubscription. Runs under the tenant GUC.
func (s *Store) GetSubscriptionByTenant(ctx context.Context, tenantID uuid.UUID) (*Subscription, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("billing: tenant id required")
	}
	var out Subscription
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanSubscription(tx.QueryRow(ctx,
			`SELECT `+subscriptionColumns+`
			 FROM billing_subscriptions WHERE tenant_id = $1`, tenantID), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSubscription
		}
		return nil, fmt.Errorf("billing: get subscription: %w", err)
	}
	return &out, nil
}

// GetSubscriptionByStripeCustomer resolves the tenant that owns the
// given Stripe customer id. This is the webhook reverse lookup: it
// runs on the admin (BYPASSRLS) pool because no tenant context is
// available until the row is found. Returns ErrUnknownCustomer when
// no row matches or no admin pool is configured.
func (s *Store) GetSubscriptionByStripeCustomer(ctx context.Context, customerID string) (*Subscription, error) {
	return s.lookupByAdmin(ctx, "stripe_customer_id", customerID)
}

// GetSubscriptionByStripeSubID resolves the tenant that owns the
// given Stripe subscription id. Admin-pool reverse lookup; see
// GetSubscriptionByStripeCustomer.
func (s *Store) GetSubscriptionByStripeSubID(ctx context.Context, subID string) (*Subscription, error) {
	return s.lookupByAdmin(ctx, "stripe_subscription_id", subID)
}

func (s *Store) lookupByAdmin(ctx context.Context, column, value string) (*Subscription, error) {
	if value == "" {
		return nil, ErrUnknownCustomer
	}
	if s.adminPool == nil {
		return nil, ErrUnknownCustomer
	}
	var out Subscription
	// column is a fixed internal literal (never user input), so the
	// fmt.Sprintf here cannot be an injection vector — the value is
	// always parameterised.
	err := scanSubscription(s.adminPool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM billing_subscriptions WHERE %s = $1`,
			subscriptionColumns, column), value), &out)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUnknownCustomer
		}
		return nil, fmt.Errorf("billing: lookup subscription by %s: %w", column, err)
	}
	return &out, nil
}

func scanSubscription(row pgx.Row, out *Subscription) error {
	return row.Scan(
		&out.ID, &out.TenantID, &out.Plan, &out.Status, &out.StripeCustomerID,
		&out.StripeSubscriptionID, &out.StripeSubscriptionItemID, &out.CancelAtPeriodEnd,
		&out.CurrentPeriodEnd, &out.TrialEnd, &out.CreatedAt, &out.UpdatedAt,
	)
}

// RecordEvent inserts a billing_events row keyed on the Stripe event
// id. The UNIQUE(stripe_event_id) constraint plus ON CONFLICT DO
// NOTHING gives idempotent webhook processing: the boolean return is
// true only the first time an event id is seen. A false return means
// Stripe redelivered an event we already handled and the caller
// should skip re-applying it.
func (s *Store) RecordEvent(ctx context.Context, tenantID uuid.UUID, stripeEventID, eventType string, payload json.RawMessage) (bool, error) {
	if tenantID == uuid.Nil {
		return false, errors.New("billing: tenant id required")
	}
	if stripeEventID == "" {
		return false, errors.New("billing: stripe event id required")
	}
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	var firstTime bool
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO billing_events (id, tenant_id, stripe_event_id, event_type, payload)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (stripe_event_id) DO NOTHING`,
			uuid.New(), tenantID, stripeEventID, eventType, payload,
		)
		if err != nil {
			return err
		}
		firstTime = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("billing: record event: %w", err)
	}
	return firstTime, nil
}

// MarkEventProcessed stamps processed_at on a previously-recorded
// event so the billing_events table doubles as an audit trail of
// which webhooks were fully applied.
func (s *Store) MarkEventProcessed(ctx context.Context, tenantID uuid.UUID, stripeEventID string) error {
	if tenantID == uuid.Nil {
		return errors.New("billing: tenant id required")
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE billing_events SET processed_at = now()
			 WHERE tenant_id = $1 AND stripe_event_id = $2`,
			tenantID, stripeEventID,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("billing: mark event processed: %w", err)
	}
	return nil
}

// UpsertInvoice inserts or updates an invoice row keyed on
// stripe_invoice_id so a replayed invoice webhook updates the same
// row rather than duplicating it.
func (s *Store) UpsertInvoice(ctx context.Context, inv Invoice) error {
	if inv.TenantID == uuid.Nil {
		return errors.New("billing: tenant id required")
	}
	if inv.StripeInvoiceID == "" {
		return errors.New("billing: stripe invoice id required")
	}
	if inv.ID == uuid.Nil {
		inv.ID = uuid.New()
	}
	err := dbutil.WithTenantTx(ctx, s.pool, inv.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO billing_invoices
			   (id, tenant_id, stripe_invoice_id, status, amount_due, amount_paid,
			    currency, hosted_invoice_url, period_start, period_end)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			 ON CONFLICT (stripe_invoice_id) DO UPDATE SET
			    status = EXCLUDED.status,
			    amount_due = EXCLUDED.amount_due,
			    amount_paid = EXCLUDED.amount_paid,
			    currency = EXCLUDED.currency,
			    hosted_invoice_url = EXCLUDED.hosted_invoice_url,
			    period_start = EXCLUDED.period_start,
			    period_end = EXCLUDED.period_end`,
			inv.ID, inv.TenantID, inv.StripeInvoiceID, inv.Status, inv.AmountDue,
			inv.AmountPaid, inv.Currency, inv.HostedInvoiceURL, inv.PeriodStart, inv.PeriodEnd,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("billing: upsert invoice: %w", err)
	}
	return nil
}

// ListInvoices returns the tenant's invoices newest-first, capped at
// limit (limit <= 0 falls back to 50).
func (s *Store) ListInvoices(ctx context.Context, tenantID uuid.UUID, limit int) ([]Invoice, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("billing: tenant id required")
	}
	if limit <= 0 {
		limit = 50
	}
	out := []Invoice{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, stripe_invoice_id, status, amount_due, amount_paid,
			        currency, hosted_invoice_url, period_start, period_end, created_at
			 FROM billing_invoices
			 WHERE tenant_id = $1
			 ORDER BY created_at DESC
			 LIMIT $2`,
			tenantID, limit,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var inv Invoice
			if err := rows.Scan(
				&inv.ID, &inv.TenantID, &inv.StripeInvoiceID, &inv.Status, &inv.AmountDue,
				&inv.AmountPaid, &inv.Currency, &inv.HostedInvoiceURL, &inv.PeriodStart,
				&inv.PeriodEnd, &inv.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, inv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("billing: list invoices: %w", err)
	}
	return out, nil
}
