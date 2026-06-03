-- Workstream 1 — SaaS control plane billing tables.
--
-- Three tenant-scoped tables back the self-service billing surface:
--
--   billing_subscriptions  one row per tenant — the current Stripe
--                          subscription (customer / subscription /
--                          subscription-item ids, status, trial +
--                          period bookkeeping). One subscription per
--                          tenant is enforced with a UNIQUE on
--                          tenant_id so the upsert in
--                          billing.Store.UpsertSubscription is a clean
--                          ON CONFLICT (tenant_id) DO UPDATE.
--
--   billing_invoices       the invoice history Stripe emits per tenant
--                          (invoice.paid / invoice.payment_failed
--                          webhooks). stripe_invoice_id is UNIQUE so a
--                          replayed webhook upserts the same row rather
--                          than duplicating it.
--
--   billing_events         an idempotency + audit log of every Stripe
--                          webhook event we accept. stripe_event_id is
--                          UNIQUE; the webhook handler does INSERT ...
--                          ON CONFLICT (stripe_event_id) DO NOTHING and
--                          treats a zero-row result as "already
--                          processed" so Stripe's at-least-once
--                          delivery never double-applies an event.
--
-- All three carry tenant_id and therefore MUST enable RLS in this same
-- migration (enforced by .github/workflows/migration-rls-check.yml).
-- The policies are the canonical app.tenant_id GUC check copied from
-- migrations/000022_tenant_metering.sql. The webhook path, which has
-- only a Stripe customer/subscription id (no tenant context) until it
-- resolves the owning tenant, runs its reverse lookups under the
-- BYPASSRLS admin pool (see internal/billing/store.go) — exactly the
-- pattern internal/auth/sso.go uses for the cross-tenant membership
-- read.

CREATE TABLE IF NOT EXISTS billing_subscriptions (
    id                          UUID PRIMARY KEY,
    tenant_id                   UUID NOT NULL REFERENCES tenants(id),
    plan                        TEXT NOT NULL,
    status                      TEXT NOT NULL,
    stripe_customer_id          TEXT NOT NULL DEFAULT '',
    stripe_subscription_id      TEXT NOT NULL DEFAULT '',
    stripe_subscription_item_id TEXT NOT NULL DEFAULT '',
    cancel_at_period_end        BOOLEAN NOT NULL DEFAULT FALSE,
    current_period_end          TIMESTAMPTZ,
    trial_end                   TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id)
);

CREATE INDEX IF NOT EXISTS billing_subscriptions_customer_idx
    ON billing_subscriptions (stripe_customer_id);
CREATE INDEX IF NOT EXISTS billing_subscriptions_subscription_idx
    ON billing_subscriptions (stripe_subscription_id);

ALTER TABLE billing_subscriptions ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing_subscriptions;
CREATE POLICY tenant_isolation ON billing_subscriptions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing_subscriptions TO kapp_app;

CREATE TABLE IF NOT EXISTS billing_invoices (
    id                  UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL REFERENCES tenants(id),
    stripe_invoice_id   TEXT NOT NULL UNIQUE,
    status              TEXT NOT NULL,
    amount_due          BIGINT NOT NULL DEFAULT 0,
    amount_paid         BIGINT NOT NULL DEFAULT 0,
    currency            TEXT NOT NULL DEFAULT '',
    hosted_invoice_url  TEXT NOT NULL DEFAULT '',
    period_start        TIMESTAMPTZ,
    period_end          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS billing_invoices_tenant_idx
    ON billing_invoices (tenant_id, created_at DESC);

ALTER TABLE billing_invoices ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing_invoices;
CREATE POLICY tenant_isolation ON billing_invoices
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing_invoices TO kapp_app;

CREATE TABLE IF NOT EXISTS billing_events (
    id              UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    stripe_event_id TEXT NOT NULL UNIQUE,
    event_type      TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    processed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS billing_events_tenant_idx
    ON billing_events (tenant_id, created_at DESC);

ALTER TABLE billing_events ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing_events;
CREATE POLICY tenant_isolation ON billing_events
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing_events TO kapp_app;
