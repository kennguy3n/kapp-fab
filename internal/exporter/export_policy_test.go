package exporter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// fakeSchemaSource returns a fixed schema for testing.
type fakeSchemaSource struct {
	schema json.RawMessage
}

func (f *fakeSchemaSource) GetSchema(_ context.Context, _ string) (json.RawMessage, error) {
	return f.schema, nil
}

// schemaWithPermissions builds a schema with field_permissions and an
// encrypted field so we can verify both FilterFields and RedactData
// are applied during export.
func schemaWithPermissions(t *testing.T) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{
		"name": "test_export",
		"version": 1,
		"fields": [
			{"name": "public_field", "type": "string"},
			{"name": "secret_field", "type": "string", "encrypted": true},
			{"name": "manager_only", "type": "string"}
		],
		"field_permissions": {
			"manager_only": {"read": ["manager", "admin"], "write": ["manager", "admin"]}
		}
	}`)
}

func TestProcessKType_AppliesFilterFieldsAndRedaction(t *testing.T) {
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	schema := schemaWithPermissions(t)
	schemaSrc := &fakeSchemaSource{schema: schema}

	src := &fakeKRecordSource{
		rows: []record.KRecord{
			{
				ID:       uuid.MustParse("00000000-0000-0000-0000-00000000a001"),
				TenantID: tenant,
				KType:    "test_export",
				Data:     json.RawMessage(`{"public_field":"hello","secret_field":"top-secret","manager_only":"confidential"}`),
				Status:   "active",
			},
		},
	}

	// User with "viewer" role — should NOT see manager_only.
	payload, _, err := ProcessKType(context.Background(), src, schemaSrc, tenant, "test_export", FormatJSON, []string{"viewer"})
	if err != nil {
		t.Fatalf("ProcessKType: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	data := out[0]["data"].(map[string]any)
	// public_field should be visible
	if data["public_field"] != "hello" {
		t.Fatalf("public_field should be visible: %v", data["public_field"])
	}
	// secret_field should be redacted
	if data["secret_field"] != "<redacted>" {
		t.Fatalf("secret_field should be redacted: %v", data["secret_field"])
	}
	// manager_only should be filtered out for viewer role
	if _, ok := data["manager_only"]; ok {
		t.Fatal("manager_only should be filtered out for viewer role")
	}
}

func TestProcessKType_ManagerRoleSeesManagerOnly(t *testing.T) {
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	schema := schemaWithPermissions(t)
	schemaSrc := &fakeSchemaSource{schema: schema}

	src := &fakeKRecordSource{
		rows: []record.KRecord{
			{
				ID:       uuid.MustParse("00000000-0000-0000-0000-00000000a001"),
				TenantID: tenant,
				KType:    "test_export",
				Data:     json.RawMessage(`{"public_field":"hello","secret_field":"top-secret","manager_only":"confidential"}`),
				Status:   "active",
			},
		},
	}

	// User with "manager" role — should see manager_only (but secret is still redacted).
	payload, _, err := ProcessKType(context.Background(), src, schemaSrc, tenant, "test_export", FormatJSON, []string{"manager"})
	if err != nil {
		t.Fatalf("ProcessKType: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data := out[0]["data"].(map[string]any)
	if data["manager_only"] != "confidential" {
		t.Fatalf("manager should see manager_only: %v", data["manager_only"])
	}
	// secret_field is still redacted regardless of role
	if data["secret_field"] != "<redacted>" {
		t.Fatalf("secret_field should be redacted even for manager: %v", data["secret_field"])
	}
}

func TestProcessKType_NoSchemaSource_NoFiltering(t *testing.T) {
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	src := &fakeKRecordSource{
		rows: []record.KRecord{
			{
				ID:       uuid.MustParse("00000000-0000-0000-0000-00000000a001"),
				TenantID: tenant,
				KType:    "test_export",
				Data:     json.RawMessage(`{"public_field":"hello","secret_field":"top-secret"}`),
				Status:   "active",
			},
		},
	}

	// No schema source — no filtering, no redaction (legacy behaviour).
	payload, _, err := ProcessKType(context.Background(), src, nil, tenant, "test_export", FormatJSON, nil)
	if err != nil {
		t.Fatalf("ProcessKType: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data := out[0]["data"].(map[string]any)
	if data["secret_field"] != "top-secret" {
		t.Fatalf("without schema source, secret_field should be visible: %v", data["secret_field"])
	}
	if !strings.Contains(string(payload), "top-secret") {
		t.Fatal("payload should contain plaintext without schema source")
	}
}
