package notifications

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

// Webhook mirrors one row of the `webhooks` table. The secret is
// returned in clear so handlers can surface it to tenant admins for
// copy-paste into their verification tooling; callers that only need
// the dispatch metadata can ignore it.
type Webhook struct {
	ID           uuid.UUID       `json:"id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	URL          string          `json:"url"`
	Secret       string          `json:"secret"`
	EventFilters json.RawMessage `json:"event_filters"`
	Active       bool            `json:"active"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// WebhookDelivery mirrors a `webhook_deliveries` row. One row is
// written per attempt; retries share an `event_id` so the UI can
// render a grouped "attempts for event X" timeline.
type WebhookDelivery struct {
	ID           uuid.UUID  `json:"id"`
	TenantID     uuid.UUID  `json:"tenant_id"`
	WebhookID    uuid.UUID  `json:"webhook_id"`
	EventID      uuid.UUID  `json:"event_id"`
	EventType    string     `json:"event_type"`
	StatusCode   *int       `json:"status_code,omitempty"`
	ResponseBody string     `json:"response_body,omitempty"`
	Attempt      int        `json:"attempt"`
	Delivered    bool       `json:"delivered"`
	Error        string     `json:"error,omitempty"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// CreateWebhookInput is the payload accepted by WebhookStore.Create.
type CreateWebhookInput struct {
	URL          string          `json:"url"`
	Secret       string          `json:"secret"`
	EventFilters json.RawMessage `json:"event_filters,omitempty"`
	Active       *bool           `json:"active,omitempty"`
}

// UpdateWebhookInput carries the subset of fields the PUT handler
// allows to be modified. Nil fields are left untouched so a minimal
// patch can flip `active` without re-sending the secret.
type UpdateWebhookInput struct {
	URL          *string         `json:"url,omitempty"`
	Secret       *string         `json:"secret,omitempty"`
	EventFilters json.RawMessage `json:"event_filters,omitempty"`
	Active       *bool           `json:"active,omitempty"`
}

// DeliveryInput is the payload the worker writes after every attempt.
// Delivered=true marks the row terminal; Delivered=false with a
// NextRetryAt schedules the next attempt.
type DeliveryInput struct {
	WebhookID    uuid.UUID
	EventID      uuid.UUID
	EventType    string
	StatusCode   *int
	ResponseBody string
	Attempt      int
	Delivered    bool
	Error        string
	NextRetryAt  *time.Time
}

// PendingRetry is one claimed retry candidate returned by
// ClaimPendingRetries. The Attempt field is the attempt number of
// the previously-failed delivery row — callers re-deliver under
// Attempt+1.
type PendingRetry struct {
	TenantID   uuid.UUID
	DeliveryID uuid.UUID
	WebhookID  uuid.UUID
	EventID    uuid.UUID
	EventType  string
	Attempt    int
}

// StoredEvent is the subset of the events table the webhook retry
// loop needs to re-assemble an events.Event for a fresh POST. Kept
// internal to this package so callers don't pull the events package
// just to replay one row.
type StoredEvent struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Type     string
	Payload  json.RawMessage
}

// ErrWebhookNotFound is surfaced when the id is missing for the
// requesting tenant — handlers map it to HTTP 404.
var ErrWebhookNotFound = errors.New("webhook: not found")

// WebhookStore is the Postgres-backed persistence for webhooks and
// their delivery log. All reads and writes run inside
// `dbutil.WithTenantTx` so RLS enforces tenant scope regardless of
// the calling path.
type WebhookStore struct {
	pool *pgxpool.Pool
}

// NewWebhookStore binds a store to the shared pool.
func NewWebhookStore(pool *pgxpool.Pool) *WebhookStore {
	return &WebhookStore{pool: pool}
}

// List returns every webhook registered for the tenant, newest
// first. Includes inactive rows so the UI can render an enable toggle.
func (s *WebhookStore) List(ctx context.Context, tenantID uuid.UUID) ([]Webhook, error) {
	out := make([]Webhook, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, url, secret, event_filters, active, created_at, updated_at
			  FROM webhooks
			 WHERE tenant_id = $1
			 ORDER BY created_at DESC`,
			tenantID,
		)
		if err != nil {
			return fmt.Errorf("webhook: list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var w Webhook
			if err := rows.Scan(
				&w.ID, &w.TenantID, &w.URL, &w.Secret, &w.EventFilters,
				&w.Active, &w.CreatedAt, &w.UpdatedAt,
			); err != nil {
				return fmt.Errorf("webhook: scan: %w", err)
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListActiveAcrossTenants returns every active webhook regardless of
// tenant. Used by the worker to fan out outbox events; the caller
// must itself filter by the event's tenant_id before POSTing. This
// path runs under the admin pool because the worker has no single
// tenant context — it serves every tenant from one process.
func (s *WebhookStore) ListActiveAcrossTenants(ctx context.Context, adminPool *pgxpool.Pool) ([]Webhook, error) {
	if adminPool == nil {
		return nil, errors.New("webhook: admin pool required for cross-tenant scan")
	}
	rows, err := adminPool.Query(ctx, `
		SELECT id, tenant_id, url, secret, event_filters, active, created_at, updated_at
		  FROM webhooks
		 WHERE active = TRUE`)
	if err != nil {
		return nil, fmt.Errorf("webhook: list active: %w", err)
	}
	defer rows.Close()
	out := make([]Webhook, 0)
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(
			&w.ID, &w.TenantID, &w.URL, &w.Secret, &w.EventFilters,
			&w.Active, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("webhook: scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListActiveForTenant returns every active webhook for a single
// tenant using the admin pool. Used by the worker's event fan-out
// path so each event triggers an O(per_tenant_webhooks) query
// instead of a full-table scan. The admin pool is required because
// the worker serves every tenant from one process — RLS is
// enforced by the explicit tenant_id predicate below rather than
// by the GUC-based dbutil.WithTenantTx helper.
func (s *WebhookStore) ListActiveForTenant(ctx context.Context, adminPool *pgxpool.Pool, tenantID uuid.UUID) ([]Webhook, error) {
	if adminPool == nil {
		return nil, errors.New("webhook: admin pool required for cross-tenant scan")
	}
	rows, err := adminPool.Query(ctx, `
		SELECT id, tenant_id, url, secret, event_filters, active, created_at, updated_at
		  FROM webhooks
		 WHERE active = TRUE AND tenant_id = $1`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("webhook: list active for tenant: %w", err)
	}
	defer rows.Close()
	out := make([]Webhook, 0)
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(
			&w.ID, &w.TenantID, &w.URL, &w.Secret, &w.EventFilters,
			&w.Active, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("webhook: scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Get returns the webhook by id or ErrWebhookNotFound.
func (s *WebhookStore) Get(ctx context.Context, tenantID, id uuid.UUID) (*Webhook, error) {
	var w Webhook
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, tenant_id, url, secret, event_filters, active, created_at, updated_at
			  FROM webhooks
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(
			&w.ID, &w.TenantID, &w.URL, &w.Secret, &w.EventFilters,
			&w.Active, &w.CreatedAt, &w.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	return &w, nil
}

// Create inserts a new webhook. When Secret is blank we refuse — the
// platform does not support unsigned outbound webhooks.
func (s *WebhookStore) Create(ctx context.Context, tenantID uuid.UUID, in CreateWebhookInput) (*Webhook, error) {
	if in.URL == "" {
		return nil, errors.New("webhook: url required")
	}
	if in.Secret == "" {
		return nil, errors.New("webhook: secret required")
	}
	filters := in.EventFilters
	if len(filters) == 0 {
		filters = json.RawMessage("[]")
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	w := Webhook{
		ID:           uuid.New(),
		TenantID:     tenantID,
		URL:          in.URL,
		Secret:       in.Secret,
		EventFilters: filters,
		Active:       active,
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO webhooks (id, tenant_id, url, secret, event_filters, active)
			VALUES ($1,$2,$3,$4,$5::jsonb,$6)
			RETURNING created_at, updated_at`,
			w.ID, w.TenantID, w.URL, w.Secret, w.EventFilters, w.Active,
		).Scan(&w.CreatedAt, &w.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("webhook: insert: %w", err)
	}
	return &w, nil
}

// Update applies the non-nil fields of patch to the webhook.
func (s *WebhookStore) Update(ctx context.Context, tenantID, id uuid.UUID, patch UpdateWebhookInput) (*Webhook, error) {
	var w Webhook
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id, tenant_id, url, secret, event_filters, active, created_at, updated_at
			  FROM webhooks
			 WHERE tenant_id = $1 AND id = $2
			 FOR UPDATE`,
			tenantID, id,
		).Scan(
			&w.ID, &w.TenantID, &w.URL, &w.Secret, &w.EventFilters,
			&w.Active, &w.CreatedAt, &w.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrWebhookNotFound
			}
			return fmt.Errorf("webhook: select: %w", err)
		}
		if patch.URL != nil {
			w.URL = *patch.URL
		}
		if patch.Secret != nil {
			w.Secret = *patch.Secret
		}
		if len(patch.EventFilters) > 0 {
			w.EventFilters = patch.EventFilters
		}
		if patch.Active != nil {
			w.Active = *patch.Active
		}
		return tx.QueryRow(ctx, `
			UPDATE webhooks
			   SET url = $1, secret = $2, event_filters = $3::jsonb,
			       active = $4, updated_at = now()
			 WHERE tenant_id = $5 AND id = $6
			 RETURNING updated_at`,
			w.URL, w.Secret, w.EventFilters, w.Active, tenantID, id,
		).Scan(&w.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// Delete removes the webhook. Delivery rows are preserved so the
// audit history survives the unsubscribe.
func (s *WebhookStore) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM webhooks WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return fmt.Errorf("webhook: delete: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrWebhookNotFound
		}
		return nil
	})
}

// RecordDelivery appends one attempt to webhook_deliveries. The
// worker calls this once per POST (success or failure) so the full
// attempt history is recoverable.
func (s *WebhookStore) RecordDelivery(ctx context.Context, tenantID uuid.UUID, in DeliveryInput) (*WebhookDelivery, error) {
	d := WebhookDelivery{
		ID:           uuid.New(),
		TenantID:     tenantID,
		WebhookID:    in.WebhookID,
		EventID:      in.EventID,
		EventType:    in.EventType,
		StatusCode:   in.StatusCode,
		ResponseBody: in.ResponseBody,
		Attempt:      in.Attempt,
		Delivered:    in.Delivered,
		Error:        in.Error,
		NextRetryAt:  in.NextRetryAt,
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO webhook_deliveries
				(id, tenant_id, webhook_id, event_id, event_type,
				 status_code, response_body, attempt, delivered, error, next_retry_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING created_at`,
			d.ID, d.TenantID, d.WebhookID, d.EventID, d.EventType,
			d.StatusCode, d.ResponseBody, d.Attempt, d.Delivered, d.Error, d.NextRetryAt,
		).Scan(&d.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("webhook: record delivery: %w", err)
	}
	return &d, nil
}

// ListDeliveries returns the most recent attempts for a webhook,
// newest first. Used by the UI delivery-log surface.
func (s *WebhookStore) ListDeliveries(ctx context.Context, tenantID, webhookID uuid.UUID, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := make([]WebhookDelivery, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, webhook_id, event_id, event_type,
			       status_code, response_body, attempt, delivered, error, next_retry_at, created_at
			  FROM webhook_deliveries
			 WHERE tenant_id = $1 AND webhook_id = $2
			 ORDER BY created_at DESC
			 LIMIT $3`,
			tenantID, webhookID, limit,
		)
		if err != nil {
			return fmt.Errorf("webhook: list deliveries: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d WebhookDelivery
			if err := rows.Scan(
				&d.ID, &d.TenantID, &d.WebhookID, &d.EventID, &d.EventType,
				&d.StatusCode, &d.ResponseBody, &d.Attempt, &d.Delivered,
				&d.Error, &d.NextRetryAt, &d.CreatedAt,
			); err != nil {
				return fmt.Errorf("webhook: scan delivery: %w", err)
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ClaimPendingRetries atomically selects up to `limit` delivery rows
// whose next_retry_at has elapsed (and whose attempt count is below
// maxAttempts) and clears their next_retry_at so subsequent polls
// will not return them again. Rows already claimed by a concurrent
// worker are skipped via FOR UPDATE SKIP LOCKED, so multiple worker
// replicas can share the retry workload without double-delivery.
//
// This path runs under the admin pool because the worker owns no
// single-tenant context — the cross-tenant scan is exactly the same
// pattern as ListActiveAcrossTenants / event outbox drain.
func (s *WebhookStore) ClaimPendingRetries(ctx context.Context, adminPool *pgxpool.Pool, maxAttempts, limit int) ([]PendingRetry, error) {
	if adminPool == nil {
		return nil, errors.New("webhook: admin pool required for cross-tenant scan")
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := adminPool.Query(ctx, `
		UPDATE webhook_deliveries
		   SET next_retry_at = NULL
		 WHERE (tenant_id, id) IN (
		    SELECT tenant_id, id
		      FROM webhook_deliveries
		     WHERE delivered = FALSE
		       AND next_retry_at IS NOT NULL
		       AND next_retry_at <= now()
		       AND attempt < $1
		     ORDER BY next_retry_at ASC
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED
		 )
		 RETURNING tenant_id, id, webhook_id, event_id, event_type, attempt`,
		maxAttempts, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("webhook: claim retries: %w", err)
	}
	defer rows.Close()
	out := make([]PendingRetry, 0)
	for rows.Next() {
		var p PendingRetry
		if err := rows.Scan(&p.TenantID, &p.DeliveryID, &p.WebhookID, &p.EventID, &p.EventType, &p.Attempt); err != nil {
			return nil, fmt.Errorf("webhook: scan retry: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetAdmin fetches a webhook bypassing RLS. Used by the retry loop
// which has already selected the (tenant_id, webhook_id) pair from
// the claimed delivery row — we trust that tuple rather than re-
// authorising through the tenant session.
func (s *WebhookStore) GetAdmin(ctx context.Context, adminPool *pgxpool.Pool, tenantID, id uuid.UUID) (*Webhook, error) {
	if adminPool == nil {
		return nil, errors.New("webhook: admin pool required")
	}
	var w Webhook
	err := adminPool.QueryRow(ctx, `
		SELECT id, tenant_id, url, secret, event_filters, active, created_at, updated_at
		  FROM webhooks
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	).Scan(
		&w.ID, &w.TenantID, &w.URL, &w.Secret, &w.EventFilters,
		&w.Active, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWebhookNotFound
		}
		return nil, fmt.Errorf("webhook: admin get: %w", err)
	}
	return &w, nil
}

// GetEvent fetches a stored event for retry replay. Reads from the
// events table via the admin pool because the worker has no single
// tenant context. The events table is RLS-enabled for normal app
// sessions; admin-role bypass is the existing pattern used by the
// cross-tenant outbox drain.
func (s *WebhookStore) GetEvent(ctx context.Context, adminPool *pgxpool.Pool, tenantID, eventID uuid.UUID) (*StoredEvent, error) {
	if adminPool == nil {
		return nil, errors.New("webhook: admin pool required")
	}
	var e StoredEvent
	err := adminPool.QueryRow(ctx, `
		SELECT id, tenant_id, type, payload
		  FROM events
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, eventID,
	).Scan(&e.ID, &e.TenantID, &e.Type, &e.Payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("webhook: event %s not found", eventID)
		}
		return nil, fmt.Errorf("webhook: admin event get: %w", err)
	}
	return &e, nil
}
