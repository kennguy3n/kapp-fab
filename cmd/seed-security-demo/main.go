// Command seed-security-demo provisions a small, deterministic two-tenant
// dataset into a local Kapp Postgres instance so the privacy & security blog
// series can quote real query output as evidence.
//
// It exercises the REAL production code paths:
//
//   - tenant.KeyManager + record.PGStore.WithEncryptor  → per-tenant
//     AES-256-GCM field encryption (krecords.data carries the
//     "kapp:enc:v1:" ciphertext prefix).
//   - record.PGStore.WithAuditHasher + audit.PGLogger    → redacted
//     audit_log rows + SHA-256 hash-chained entries.
//   - events.PGPublisher                                 → redacted event
//     outbox payloads (no plaintext sensitive fields).
//
// Control-plane rows (tenants, users, memberships, roles, permissions,
// sessions, retention policies) are inserted directly via the superuser
// pool because they are either non-RLS control-plane tables or seeded
// across two tenants in one pass.
//
// Usage (from repo root, with docker-compose postgres up):
//
//	$env:KAPP_MASTER_KEY="blog-demo-master-key-32bytes-min-aaaa-bbbb"
//	go run ./cmd/seed-security-demo
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

const (
	superDBURL = "postgres://kapp:kapp_dev@localhost:5432/kapp?sslmode=disable"
	appDBURL   = "postgres://kapp_app:kapp_app_dev@localhost:5432/kapp?sslmode=disable"

	// Deterministic IDs so re-runs produce stable evidence output.
	acmeID   = "00000000-0000-4000-8000-000000000001"
	globexID = "00000000-0000-4000-8000-000000000002"

	alexID   = "00000000-0000-4000-8000-000000000011" // Acme admin
	priyaID  = "00000000-0000-4000-8000-000000000012" // Acme HR
	morganID = "00000000-0000-4000-8000-000000000021" // Globex admin
)

// employeeSchema declares an hr.employee KType with a confidential salary
// (encrypted + blind-indexed) and a secret ssn (classified secret, so
// encrypted + redacted from audit/events even without the legacy
// {"encrypted": true} flag).
const employeeSchema = `{
  "name": "hr.employee",
  "version": 1,
  "fields": [
    {"name": "full_name", "type": "string", "required": true},
    {"name": "department", "type": "string"},
    {"name": "salary", "type": "string", "encrypted": true, "classification": "confidential", "indexed": true},
    {"name": "ssn", "type": "string", "classification": "secret", "indexed": true}
  ]
}`

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := tenant.FailClosedOnMissingMasterKey(loadMasterKeyErr()); err != nil {
		log.Fatalf("master key: %v", err)
	}

	superPool, err := platform.NewPool(ctx, superDBURL)
	if err != nil {
		log.Fatalf("open super pool: %v", err)
	}
	defer superPool.Close()
	appPool, err := platform.NewPool(ctx, appDBURL)
	if err != nil {
		log.Fatalf("open app pool: %v", err)
	}
	defer appPool.Close()

	resetDB(ctx, superPool)
	seedControlPlane(ctx, superPool)

	// Register the KType through the real registry (ktypes is shared,
	// non-RLS, so the kapp_app pool can write it).
	cache := platform.NewLRUCache(64, time.Minute)
	registry := ktype.NewPGRegistry(appPool, cache)
	if err := registry.Register(ctx, ktype.KType{
		Name:    "hr.employee",
		Version: 1,
		Schema:  json.RawMessage(employeeSchema),
	}); err != nil {
		log.Fatalf("register ktype: %v", err)
	}

	// Wire the real record store with per-tenant encryption + audit HMAC.
	masterKey, _ := tenant.LoadMasterKey()
	km, err := tenant.NewKeyManager(masterKey, time.Hour)
	if err != nil {
		log.Fatalf("key manager: %v", err)
	}
	publisher := events.NewPGPublisher(appPool)
	auditor := audit.NewPGLogger(appPool)
	records := record.NewPGStore(appPool, registry, publisher, auditor).
		WithEncryptor(km).WithAuditHasher(km).WithBlindIndexer(km)

	seedRecords(ctx, records)
	seedExtraAudit(ctx, auditor)

	fmt.Println("seed-security-demo: done")
}

// loadMasterKeyErr returns the error from LoadMasterKey so the production
// fail-closed gate can decide whether a missing key is fatal. In this dev
// seeding context KAPP_MASTER_KEY is expected to be set.
func loadMasterKeyErr() error {
	_, err := tenant.LoadMasterKey()
	return err
}

func resetDB(ctx context.Context, pool *pgxpool.Pool) {
	// Wipe the seeded tenants' data so re-runs are deterministic. Order
	// respects FK + partition dependencies. Superuser bypasses RLS.
	tenantIDs := []any{acmeID, globexID}
	userIDs := []any{alexID, priyaID, morganID}
	stmts := []string{
		`DELETE FROM audit_log WHERE tenant_id IN ($1, $2)`,
		`DELETE FROM events WHERE tenant_id IN ($1, $2)`,
		`DELETE FROM krecords WHERE tenant_id IN ($1, $2)`,
		`DELETE FROM sessions WHERE tenant_id IN ($1, $2)`,
		`DELETE FROM permissions WHERE tenant_id IN ($1, $2)`,
		`DELETE FROM data_retention_policies WHERE tenant_id IN ($1, $2)`,
		`DELETE FROM roles WHERE tenant_id IN ($1, $2)`,
		`DELETE FROM user_tenants WHERE tenant_id IN ($1, $2)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s, tenantIDs...); err != nil {
			log.Fatalf("reset %q: %v", s, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ktypes WHERE name = 'hr.employee'`); err != nil {
		log.Fatalf("reset ktypes: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id IN ($1, $2, $3)`, userIDs...); err != nil {
		log.Fatalf("reset users: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tenants WHERE id IN ($1, $2)`, tenantIDs...); err != nil {
		log.Fatalf("reset tenants: %v", err)
	}
}

func seedControlPlane(ctx context.Context, pool *pgxpool.Pool) {
	// Tenants (control plane, no RLS).
	mustExec(ctx, pool, `INSERT INTO tenants (id, slug, name, cell, status, plan, quota, base_currency, country, timezone)
		VALUES ($1,'acme','Acme Corp','us-east-1','active','starter','{"max_records":50000}','USD','US','America/New_York'),
		       ($2,'globex','Globex GmbH','eu-central-1','active','business','{"max_records":200000}','EUR','DE','Europe/Berlin')`,
		acmeID, globexID)

	// Users (control plane, no RLS).
	mustExec(ctx, pool, `INSERT INTO users (id, kchat_user_id, email, display_name) VALUES
		($1,'kchat-alex','alex@acme.example','Alex Rivera'),
		($2,'kchat-priya','priya@acme.example','Priya Shah'),
		($3,'kchat-morgan','morgan@globex.example','Morgan Becker')`,
		alexID, priyaID, morganID)

	// Memberships (RLS-enabled; superuser bypasses). Acme gets an admin +
	// an HR manager; Globex gets an admin.
	mustExec(ctx, pool, `INSERT INTO user_tenants (user_id, tenant_id, role, status) VALUES
		($1, $4, 'tenant_admin', 'active'),
		($2, $4, 'hr_manager', 'active'),
		($3, $5, 'tenant_admin', 'active')`,
		alexID, priyaID, morganID, acmeID, globexID)

	// Roles baseline (RLS-enabled).
	mustExec(ctx, pool, `INSERT INTO roles (tenant_id, name, permissions) VALUES
		($1, 'tenant_admin', '["*"]'),
		($1, 'hr_manager', '["hr.employee.read","hr.employee.write","hr.employee.approve"]'),
		($1, 'sales_rep', '["crm.lead.read","crm.deal.write"]'),
		($2, 'tenant_admin', '["*"]')`,
		acmeID, globexID)

	// Granular permissions (migration 000015) — object-scoped grants.
	mustExec(ctx, pool, `INSERT INTO permissions (id, tenant_id, role_name, ktype, action, conditions, granted_by) VALUES
		($1, $3, 'hr_manager', 'hr.employee', 'approve', '{"amount_max":50000}', $4),
		($2, $3, 'sales_rep', 'crm.deal', 'write', '{}', $4)`,
		"00000000-0000-4000-8000-000000000031",
		"00000000-0000-4000-8000-000000000032",
		acmeID, priyaID)

	// Sessions (migration 000013). One active Acme session + one revoked.
	mustExec(ctx, pool, `INSERT INTO sessions (id, tenant_id, user_id, refresh_jti, issued_at, expires_at, revoked_at, user_agent, ip_address) VALUES
		($1, $3, $4, 'jti-alex-active', now() - interval '2 hours', now() + interval '22 hours', NULL, 'Mozilla/5.0 Chrome', '203.0.113.10'),
		($2, $3, $5, 'jti-priya-revoked', now() - interval '3 days', now() - interval '2 days', now() - interval '2 days', 'Mozilla/5.0 Firefox', '198.51.100.22')`,
		"00000000-0000-4000-8000-000000000041",
		"00000000-0000-4000-8000-000000000042",
		acmeID, alexID, priyaID)

	// Data retention policies (migration 000032).
	mustExec(ctx, pool, `INSERT INTO data_retention_policies (tenant_id, category, retention_days, enabled) VALUES
		($1, 'audit_log', 2555, true),
		($1, 'events', 90, true),
		($1, 'notifications', 30, true),
		($1, 'webhook_deliveries', 30, true),
		($2, 'audit_log', 3650, true),
		($2, 'events', 180, true)`,
		acmeID, globexID)
}

func seedRecords(ctx context.Context, records *record.PGStore) {
	acme := uuid.MustParse(acmeID)
	globex := uuid.MustParse(globexID)
	alex := uuid.MustParse(alexID)
	morgan := uuid.MustParse(morganID)

	// Acme employee — salary + ssn are encrypted at the field level and
	// redacted from the audit/event payload written by Create.
	acmeEmp := record.KRecord{
		TenantID:  acme,
		KType:     "hr.employee",
		CreatedBy: alex,
		Data: json.RawMessage(`{
			"full_name": "Priya Shah",
			"department": "Engineering",
			"salary": "92500",
			"ssn": "123-45-6789"
		}`),
	}
	if _, err := records.Create(ctx, acmeEmp); err != nil {
		log.Fatalf("create acme employee: %v", err)
	}

	// Globex employee — isolated under a different tenant key.
	globexEmp := record.KRecord{
		TenantID:  globex,
		KType:     "hr.employee",
		CreatedBy: morgan,
		Data: json.RawMessage(`{
			"full_name": "Lena Fischer",
			"department": "Finance",
			"salary": "78000",
			"ssn": "987-65-4321"
		}`),
	}
	if _, err := records.Create(ctx, globexEmp); err != nil {
		log.Fatalf("create globex employee: %v", err)
	}

	// A second Acme employee so the audit hash chain has >1 link.
	acmeEmp2 := record.KRecord{
		TenantID:  acme,
		KType:     "hr.employee",
		CreatedBy: alex,
		Data: json.RawMessage(`{
			"full_name": "Diego Santos",
			"department": "Sales",
			"salary": "64000",
			"ssn": "444-55-6666"
		}`),
	}
	if _, err := records.Create(ctx, acmeEmp2); err != nil {
		log.Fatalf("create acme employee 2: %v", err)
	}
}

// seedExtraAudit appends a few non-record audit entries through the real
// audit.PGLogger so the hash chain keeps growing and the blog can show
// auth.failure + admin + retention actions alongside record mutations.
func seedExtraAudit(ctx context.Context, auditor *audit.PGLogger) {
	acme := uuid.MustParse(acmeID)
	priya := uuid.MustParse(priyaID)

	entries := []audit.Entry{
		{
			TenantID: acme, ActorID: &priya, ActorKind: audit.ActorUser,
			Action: "auth.failure", Context: json.RawMessage(`{"reason":"bad_password","ip":"198.51.100.22"}`),
		},
		{
			TenantID: acme, ActorKind: audit.ActorSystem,
			Action: "admin.tenant.suspend", TargetKType: "tenant",
			TargetID: ptrUUID(uuid.MustParse(globexID)),
			Context:  json.RawMessage(`{"reason":"billing_overdue","actor":"platform_admin"}`),
		},
		{
			TenantID: acme, ActorKind: audit.ActorSystem,
			Action: "retention.sweep.delete", Context: json.RawMessage(`{"category":"events","rows_deleted":128}`),
		},
	}
	for _, e := range entries {
		if err := auditor.Log(ctx, e); err != nil {
			log.Fatalf("audit log %s: %v", e.Action, err)
		}
	}
}

func mustExec(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		log.Fatalf("exec %q: %v", sql, err)
	}
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }

// ensure dbutil import is retained (used implicitly by the audit logger's
// tenant tx). The reference keeps the import stable across edits.
var _ = dbutil.WithTenantTx
