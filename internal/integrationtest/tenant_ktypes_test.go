//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestTenantKTypeBuilderEndToEnd is the Phase N8b verification
// harness. It exercises the full pipeline a builder UI would
// follow when a tenant power user authors a custom business
// object (Asset Register, Compliance Checklist, etc.) and then
// creates records against it:
//
//  1. Tenant authors a draft custom.<slug> KType via TenantStore.Upsert.
//  2. Promotes draft → active.
//  3. Creates a KRecord via record.PGStore.Create — the record
//     store consults the tenant_ktypes table (NOT the global
//     ktypes registry) because the name starts with custom.,
//     validates the payload against the safe-subset schema, and
//     writes the record under RLS.
//  4. Archives the KType. Subsequent creates are rejected with
//     a clear "only active types back records" error.
//
// The test also pins the negative paths the API surface depends
// on: invalid namespace (crm.x), unsafe field type (object),
// posting_hook smuggling, field-count cap.
func TestTenantKTypeBuilderEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("custombuilder"), Name: "Custom Builder Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	actor := uuid.New()
	store := ktype.NewTenantStore(h.pool)
	// The record store must consult tenant_ktypes for custom.*
	// names — wire it via WithTenantKTypes the same way the
	// production deps_build.go does.
	records := record.NewPGStore(h.pool, h.ktypes, h.publisher, h.auditor).WithTenantKTypes(store)

	// --- Step 1: draft a custom.* KType ----------------------------
	// The schema mirrors a real customer use case — a per-tenant
	// asset register. Mixes string / number / enum / date fields
	// to exercise every safe-subset branch.
	schema := json.RawMessage(`{
		"name": "custom.asset_register",
		"version": 1,
		"fields": [
			{"name": "asset_code", "type": "string", "required": true, "max_length": 40},
			{"name": "description", "type": "text"},
			{"name": "purchase_date", "type": "date", "required": true},
			{"name": "cost", "type": "number", "min": 0, "required": true},
			{"name": "depreciation_method", "type": "enum", "values": ["straight_line", "declining"]},
			{"name": "owner_email", "type": "email"}
		]
	}`)
	saved, err := store.Upsert(ctx, ktype.TenantKType{
		TenantID:    tn.ID,
		Name:        "custom.asset_register",
		Version:     1,
		Title:       "Asset Register",
		Description: "Per-tenant fixed asset tracking",
		Schema:      schema,
		CreatedBy:   actor,
	})
	if err != nil {
		t.Fatalf("draft custom ktype: %v", err)
	}
	if saved.Status != ktype.CustomStatusDraft {
		t.Fatalf("default status: got %q want %q", saved.Status, ktype.CustomStatusDraft)
	}

	// Creating a record against a DRAFT custom KType must fail —
	// only active types back records. This is the rule
	// resolveKType enforces.
	_, err = records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     "custom.asset_register",
		Data:      json.RawMessage(`{"asset_code":"A1","purchase_date":"2026-01-01","cost":100}`),
		CreatedBy: actor,
	})
	if err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("create against draft must fail with 'draft' in message, got %v", err)
	}

	// --- Step 2: promote draft → active ----------------------------
	if err := store.SetStatus(ctx, tn.ID, "custom.asset_register", 1, ktype.CustomStatusActive); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// --- Step 3: create a KRecord against the active custom type ---
	rec, err := records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     "custom.asset_register",
		Data:      json.RawMessage(`{"asset_code":"A-001","description":"Server rack 42U","purchase_date":"2026-02-15","cost":4250,"depreciation_method":"straight_line","owner_email":"ops@example.com"}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create record against active custom: %v", err)
	}
	if rec.KType != "custom.asset_register" {
		t.Fatalf("created record ktype: got %q want %q", rec.KType, "custom.asset_register")
	}

	// Required-field enforcement: omit `cost` → ValidateData must
	// surface a "is required" error. This proves the validator is
	// running against the tenant-authored schema, not the global
	// registry (which doesn't know about custom.asset_register).
	_, err = records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     "custom.asset_register",
		Data:      json.RawMessage(`{"asset_code":"A-002","purchase_date":"2026-03-01"}`),
		CreatedBy: actor,
	})
	if err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("missing required field must fail with 'is required', got %v", err)
	}

	// --- Step 4: archive, then re-attempt creates ------------------
	if err := store.SetStatus(ctx, tn.ID, "custom.asset_register", 1, ktype.CustomStatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}
	_, err = records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     "custom.asset_register",
		Data:      json.RawMessage(`{"asset_code":"A-003","purchase_date":"2026-04-01","cost":99}`),
		CreatedBy: actor,
	})
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("create against archived must fail with 'archived', got %v", err)
	}

	// Existing records on an archived custom KType must remain
	// editable — archiving freezes the schema, not the data. This
	// is the data-correction path: a customer realises a record
	// has the wrong purchase_date or cost months after the schema
	// has been frozen and must be able to fix it without
	// unarchiving the type. resolveForUpdate enforces this.
	updated, err := records.Update(ctx, record.KRecord{
		TenantID:  tn.ID,
		ID:        rec.ID,
		Data:      json.RawMessage(`{"description":"Server rack 42U (corrected)"}`),
		UpdatedBy: &actor,
	})
	if err != nil {
		t.Fatalf("update existing record on archived KType must succeed for data corrections, got %v", err)
	}
	if !strings.Contains(string(updated.Data), "corrected") {
		t.Fatalf("update did not persist: %s", string(updated.Data))
	}

	// Reads of historical records on archived custom KTypes must
	// continue to work (no status gate on Get).
	got, err := records.Get(ctx, tn.ID, rec.ID)
	if err != nil {
		t.Fatalf("read existing record on archived KType: %v", err)
	}
	if got.ID != rec.ID {
		t.Fatalf("read returned wrong record: got %s want %s", got.ID, rec.ID)
	}

	// --- Negative paths --------------------------------------------
	// crm.* is NOT in the custom.* namespace — the store rejects
	// before SQL gets a chance.
	_, err = store.Upsert(ctx, ktype.TenantKType{
		TenantID:  tn.ID,
		Name:      "crm.deal",
		Title:     "Hostile rename of platform type",
		Schema:    json.RawMessage(`{"fields":[{"name":"f","type":"string"}]}`),
		CreatedBy: actor,
	})
	if !errors.Is(err, ktype.ErrInvalidCustomName) {
		t.Fatalf("expected ErrInvalidCustomName, got %v", err)
	}

	// object/array are NOT in the safe-subset; the store rejects
	// them with ErrUnsupportedFieldType.
	_, err = store.Upsert(ctx, ktype.TenantKType{
		TenantID:  tn.ID,
		Name:      "custom.unsafe",
		Title:     "Smuggle nested objects",
		Schema:    json.RawMessage(`{"fields":[{"name":"nested","type":"object"}]}`),
		CreatedBy: actor,
	})
	if !errors.Is(err, ktype.ErrUnsupportedFieldType) {
		t.Fatalf("expected ErrUnsupportedFieldType, got %v", err)
	}

	// posting_hook is a developer-only surface — refuse outright.
	_, err = store.Upsert(ctx, ktype.TenantKType{
		TenantID:  tn.ID,
		Name:      "custom.hook_smuggle",
		Title:     "Smuggle a Go posting hook",
		Schema:    json.RawMessage(`{"fields":[{"name":"f","type":"string"}],"posting_hook":{"go":"// arbitrary"}}`),
		CreatedBy: actor,
	})
	if err == nil || !strings.Contains(err.Error(), "posting_hook") {
		t.Fatalf("posting_hook smuggling must be rejected, got %v", err)
	}

	// Field-cap with a small per-tenant limit so the test is fast.
	smallStore := ktype.NewTenantStore(h.pool, ktype.WithFieldLimit(2))
	threeFields := []map[string]any{
		{"name": "a", "type": "string"},
		{"name": "b", "type": "string"},
		{"name": "c", "type": "string"},
	}
	body, _ := json.Marshal(map[string]any{"fields": threeFields})
	_, err = smallStore.Upsert(ctx, ktype.TenantKType{
		TenantID:  tn.ID,
		Name:      "custom.overflow",
		Title:     "Too many fields",
		Schema:    body,
		CreatedBy: actor,
	})
	if !errors.Is(err, ktype.ErrTooManyFields) {
		t.Fatalf("expected ErrTooManyFields, got %v", err)
	}

	// Forward-only lifecycle: archived → active is rejected so a
	// previously-archived schema cannot be silently un-archived
	// from the builder UI. The KType used in steps 1–4 is now
	// `archived`; both SetStatus and Upsert must refuse to move
	// it backward.
	if err := store.SetStatus(ctx, tn.ID, "custom.asset_register", 1, ktype.CustomStatusActive); !errors.Is(err, ktype.ErrInvalidTransition) {
		t.Fatalf("archived → active must return ErrInvalidTransition, got %v", err)
	}
	if err := store.SetStatus(ctx, tn.ID, "custom.asset_register", 1, ktype.CustomStatusDraft); !errors.Is(err, ktype.ErrInvalidTransition) {
		t.Fatalf("archived → draft must return ErrInvalidTransition, got %v", err)
	}
	_, err = store.Upsert(ctx, ktype.TenantKType{
		TenantID:  tn.ID,
		Name:      "custom.asset_register",
		Version:   1,
		Title:     "Asset Register (sneak-active)",
		Schema:    schema,
		Status:    ktype.CustomStatusActive,
		CreatedBy: actor,
	})
	if !errors.Is(err, ktype.ErrInvalidTransition) {
		t.Fatalf("archived → active via Upsert must return ErrInvalidTransition, got %v", err)
	}

	// active → draft is rejected on a fresh KType too, so a
	// builder user who toggles an active KType "back to draft"
	// hits a precise error instead of stranding their records.
	if _, err := store.Upsert(ctx, ktype.TenantKType{
		TenantID: tn.ID, Name: "custom.lifecycle_probe", Version: 1,
		Title: "Lifecycle probe", Schema: schemaWithOneField(),
		CreatedBy: actor,
	}); err != nil {
		t.Fatalf("seed lifecycle_probe: %v", err)
	}
	if err := store.SetStatus(ctx, tn.ID, "custom.lifecycle_probe", 1, ktype.CustomStatusActive); err != nil {
		t.Fatalf("promote lifecycle_probe to active: %v", err)
	}
	if err := store.SetStatus(ctx, tn.ID, "custom.lifecycle_probe", 1, ktype.CustomStatusDraft); !errors.Is(err, ktype.ErrInvalidTransition) {
		t.Fatalf("active → draft must return ErrInvalidTransition, got %v", err)
	}

	// Same-status no-ops are allowed (idempotent re-save).
	if err := store.SetStatus(ctx, tn.ID, "custom.lifecycle_probe", 1, ktype.CustomStatusActive); err != nil {
		t.Fatalf("active → active must be a no-op, got %v", err)
	}

	// Duplicate field names are rejected with ErrDuplicateField
	// before the row is written.
	dupSchema, _ := json.Marshal(map[string]any{
		"name": "custom.dups", "version": 1,
		"fields": []map[string]any{
			{"name": "foo", "type": "string"},
			{"name": "foo", "type": "number"},
		},
	})
	_, err = store.Upsert(ctx, ktype.TenantKType{
		TenantID: tn.ID, Name: "custom.dups", Version: 1,
		Title: "Has duplicates", Schema: dupSchema, CreatedBy: actor,
	})
	if !errors.Is(err, ktype.ErrDuplicateField) {
		t.Fatalf("duplicate field names must return ErrDuplicateField, got %v", err)
	}
}

// TestRecordCreatePrefersLatestActiveCustomVersion pins the
// multi-version resolution rule for record.Create against a
// tenant-authored KType: when v1 is active and v2 is a brand-new
// draft (the iteration-in-progress case), `KTypeVersion=0`
// (the default "latest") must resolve to v1 active, NOT v2 draft.
//
// Pre-fix, the resolver returned `ORDER BY version DESC LIMIT 1`
// unconditionally — so v2 draft shadowed v1 active and creates
// were rejected with "only active types back record creates"
// even though v1 was perfectly usable. After the fix, the
// version=0 + resolveForCreate path queries
// `status='active' ORDER BY version DESC LIMIT 1`, returning v1.
// The new record's KTypeVersion is recorded as 1 so historical
// records always pin the active version they were validated
// against, not the draft that happened to be the row's latest
// version at the time of creation.
func TestRecordCreatePrefersLatestActiveCustomVersion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("multiver"), Name: "Multi Version Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	actor := uuid.New()
	store := ktype.NewTenantStore(h.pool)
	records := record.NewPGStore(h.pool, h.ktypes, h.publisher, h.auditor).WithTenantKTypes(store)

	v1Schema, _ := json.Marshal(map[string]any{
		"name": "custom.work_order", "version": 1,
		"fields": []map[string]any{
			{"name": "code", "type": "string", "required": true},
			{"name": "qty", "type": "number"},
		},
	})
	v2Schema, _ := json.Marshal(map[string]any{
		"name": "custom.work_order", "version": 2,
		"fields": []map[string]any{
			{"name": "code", "type": "string", "required": true},
			{"name": "qty", "type": "number"},
			// New field still being iterated on — the tenant
			// hasn't decided to promote v2 to active yet.
			{"name": "notes", "type": "text"},
		},
	})
	if _, err := store.Upsert(ctx, ktype.TenantKType{
		TenantID: tn.ID, Name: "custom.work_order", Version: 1,
		Title: "Work Order", Schema: v1Schema, CreatedBy: actor,
		Status: ktype.CustomStatusActive,
	}); err != nil {
		t.Fatalf("upsert v1 active: %v", err)
	}
	if _, err := store.Upsert(ctx, ktype.TenantKType{
		TenantID: tn.ID, Name: "custom.work_order", Version: 2,
		Title: "Work Order", Schema: v2Schema, CreatedBy: actor,
		Status: ktype.CustomStatusDraft,
	}); err != nil {
		t.Fatalf("upsert v2 draft: %v", err)
	}

	// version=0 (default) must resolve to v1 active, NOT v2 draft.
	rec, err := records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     "custom.work_order",
		Data:      json.RawMessage(`{"code":"WO-1","qty":3}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create with version=0 must resolve to v1 active, got error %v", err)
	}
	if rec.KTypeVersion != 1 {
		t.Fatalf("expected v1 active (the latest active), got v%d", rec.KTypeVersion)
	}

	// And if the tenant later archives v1, the next create with
	// version=0 falls through to the actionable error naming v2's
	// lifecycle state ("draft") — not a bare "not found".
	if err := store.SetStatus(ctx, tn.ID, "custom.work_order", 1, ktype.CustomStatusArchived); err != nil {
		t.Fatalf("archive v1: %v", err)
	}
	_, err = records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     "custom.work_order",
		Data:      json.RawMessage(`{"code":"WO-2"}`),
		CreatedBy: actor,
	})
	if err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("with no active version, error must name the latest version's lifecycle state, got %v", err)
	}
}

// schemaWithOneField is a tiny helper used by the lifecycle-probe
// assertion below — a valid one-field schema in custom.<slug>
// shape, marshalled to bytes the way the builder UI would send it.
func schemaWithOneField() json.RawMessage {
	body := map[string]any{
		"name": "custom.lifecycle_probe", "version": 1,
		"fields": []map[string]any{{"name": "f", "type": "string"}},
	}
	b, _ := json.Marshal(body)
	return b
}
