-- Marketplace listing metadata — category, screenshots, and ratings.
--
-- Extends the B2 marketplace registry (000068) with the three
-- app-store surfaces the catalog UI needs but the original schema did
-- not model:
--
--   1. category    — a single curated taxonomy bucket per listing,
--                    publisher-declared via the create-extension
--                    control surface. Drives the Browse category
--                    filter. CHECK-constrained to the known set so a
--                    direct SQL write cannot poison the facet list;
--                    the application validator is the first line of
--                    defence, the constraint is the last (same
--                    belt-and-braces pattern as the name/status
--                    CHECKs in 000068).
--
--   2. screenshots — publisher-declared gallery for the detail page.
--                    Stored as a JSONB array of {url, caption}
--                    objects; HTTPS-only URLs are enforced by the
--                    application validator, the DB CHECK only pins the
--                    JSON shape to an array so a malformed scalar/object
--                    cannot land.
--
--   3. ratings     — real, tenant-authored 1..5 star ratings. The
--                    per-rating rows live in the new tenant-scoped
--                    marketplace_extension_ratings table (RLS-isolated,
--                    one row per (tenant, extension)). The marketplace
--                    average is a CROSS-tenant aggregate, which an
--                    RLS-scoped SELECT cannot compute (the app pool only
--                    sees its own tenant's rows). So the aggregate is
--                    denormalised onto the GLOBAL marketplace_extensions
--                    row as rating_sum / rating_count and maintained
--                    incrementally by an AFTER trigger that reads only
--                    OLD/NEW — never a cross-tenant scan. Browse renders
--                    avg = rating_sum / rating_count; a tenant reads its
--                    own star value back through the RLS SELECT.

-- --------------------------------------------------------------------------
-- 1. Catalog-listing columns on the global extensions row.
-- --------------------------------------------------------------------------
ALTER TABLE marketplace_extensions
    ADD COLUMN IF NOT EXISTS category     TEXT   NOT NULL DEFAULT 'other',
    ADD COLUMN IF NOT EXISTS screenshots  JSONB  NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS rating_sum   BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS rating_count INTEGER NOT NULL DEFAULT 0;

-- Curated category taxonomy. Mirrors marketplace.ValidCategories in
-- internal/marketplace/types.go — keep the two in lock-step. 'other'
-- is the default so pre-existing rows (and listings whose publisher
-- declared no category) satisfy the CHECK.
ALTER TABLE marketplace_extensions
    ADD CONSTRAINT marketplace_extensions_category_valid
        CHECK (category IN (
            'productivity','finance','sales','marketing','crm','hr',
            'inventory','analytics','communication','developer_tools',
            'integrations','other'
        ));

ALTER TABLE marketplace_extensions
    ADD CONSTRAINT marketplace_extensions_screenshots_array
        CHECK (jsonb_typeof(screenshots) = 'array');

-- rating_sum / rating_count are maintained only by the aggregate
-- trigger below; the CHECK is a backstop that a logic error in the
-- delta arithmetic surfaces as a constraint violation rather than a
-- silently-negative average.
ALTER TABLE marketplace_extensions
    ADD CONSTRAINT marketplace_extensions_rating_nonneg
        CHECK (rating_sum >= 0 AND rating_count >= 0);

-- Facet index for the Browse category filter (always paired with the
-- status = 'listed' predicate the tenant catalog query applies).
CREATE INDEX IF NOT EXISTS marketplace_extensions_category_idx
    ON marketplace_extensions (status, category);

COMMENT ON COLUMN marketplace_extensions.category IS
    'Publisher-declared taxonomy bucket from the curated set (see marketplace.ValidCategories). Drives the marketplace Browse category filter. Defaults to ''other''.';
COMMENT ON COLUMN marketplace_extensions.screenshots IS
    'Publisher-declared gallery for the detail page: JSONB array of {url, caption}. URLs are HTTPS-only (enforced by the create-extension validator); the DB CHECK only pins the array shape.';
COMMENT ON COLUMN marketplace_extensions.rating_sum IS
    'Denormalised sum of every tenant''s star rating for this listing. Maintained incrementally by marketplace_extension_rating_aggregate(); average = rating_sum / rating_count. Denormalised because the per-rating rows are RLS-scoped and a cross-tenant AVG() is not visible to the application pool.';
COMMENT ON COLUMN marketplace_extensions.rating_count IS
    'Denormalised count of tenant ratings for this listing. Maintained by the rating aggregate trigger alongside rating_sum.';

-- --------------------------------------------------------------------------
-- 2. Tenant-scoped per-rating rows.
-- --------------------------------------------------------------------------
-- One row per (tenant, extension). A tenant may revise its own rating
-- (upsert on the unique key); the aggregate trigger keeps the
-- denormalised rollup exact. Canonical tenant-scoped table shape:
-- (tenant_id, id) primary key, ENABLE + FORCE ROW LEVEL SECURITY, a
-- tenant_isolation policy keyed off app.tenant_id, GRANT to kapp_app.
-- FORCE mirrors the sibling marketplace_extension_installations table
-- so even an owner-role maintenance query cannot read across tenants.
CREATE TABLE IF NOT EXISTS marketplace_extension_ratings (
    id            UUID NOT NULL DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    extension_id  UUID NOT NULL REFERENCES marketplace_extensions(id) ON DELETE CASCADE,
    stars         SMALLINT NOT NULL,
    created_by    UUID REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, id),
    CONSTRAINT marketplace_extension_ratings_tenant_extension_unique
        UNIQUE (tenant_id, extension_id),
    CONSTRAINT marketplace_extension_ratings_stars_range
        CHECK (stars BETWEEN 1 AND 5)
);

CREATE INDEX IF NOT EXISTS marketplace_extension_ratings_extension_idx
    ON marketplace_extension_ratings (extension_id);

ALTER TABLE marketplace_extension_ratings ENABLE ROW LEVEL SECURITY;
ALTER TABLE marketplace_extension_ratings FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON marketplace_extension_ratings;
CREATE POLICY tenant_isolation ON marketplace_extension_ratings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON marketplace_extension_ratings TO kapp_app;

COMMENT ON TABLE marketplace_extension_ratings IS
    'Tenant-authored 1..5 star ratings for marketplace listings. RLS isolates rows per tenant; one row per (tenant, extension). The cross-tenant average is denormalised onto marketplace_extensions.rating_sum/rating_count by marketplace_extension_rating_aggregate() because an RLS-scoped query cannot compute it.';

-- --------------------------------------------------------------------------
-- 3. Incremental aggregate maintenance.
-- --------------------------------------------------------------------------
-- Keeps marketplace_extensions.rating_sum / rating_count exact as
-- rating rows are inserted, revised, or removed. CRITICAL: the
-- function reads ONLY the trigger's OLD/NEW tuples, never SELECTs
-- marketplace_extension_ratings — a re-aggregating SELECT would run
-- under the writer's RLS context and see only that tenant's rows,
-- producing a single-tenant average. The delta arithmetic here is
-- tenant-agnostic and therefore correct across the whole catalog.
-- updated_at on the extension row is intentionally NOT bumped: a
-- rating is engagement, not a listing-metadata change, and other
-- code paths reason about updated_at as "the listing was edited".
CREATE OR REPLACE FUNCTION marketplace_extension_rating_aggregate()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE marketplace_extensions
           SET rating_sum   = rating_sum + NEW.stars,
               rating_count = rating_count + 1
         WHERE id = NEW.extension_id;
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        IF NEW.extension_id IS DISTINCT FROM OLD.extension_id THEN
            -- extension_id is effectively immutable for a rating, but
            -- handle a re-target defensively so the rollup never drifts.
            UPDATE marketplace_extensions
               SET rating_sum   = rating_sum - OLD.stars,
                   rating_count = rating_count - 1
             WHERE id = OLD.extension_id;
            UPDATE marketplace_extensions
               SET rating_sum   = rating_sum + NEW.stars,
                   rating_count = rating_count + 1
             WHERE id = NEW.extension_id;
        ELSE
            UPDATE marketplace_extensions
               SET rating_sum = rating_sum + (NEW.stars - OLD.stars)
             WHERE id = NEW.extension_id;
        END IF;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE marketplace_extensions
           SET rating_sum   = rating_sum - OLD.stars,
               rating_count = rating_count - 1
         WHERE id = OLD.extension_id;
        RETURN OLD;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS marketplace_extension_ratings_aggregate_trg
    ON marketplace_extension_ratings;
CREATE TRIGGER marketplace_extension_ratings_aggregate_trg
    AFTER INSERT OR UPDATE OR DELETE ON marketplace_extension_ratings
    FOR EACH ROW
    EXECUTE FUNCTION marketplace_extension_rating_aggregate();
