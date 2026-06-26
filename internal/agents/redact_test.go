package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/record"
)

// stubSchemaSource returns a fixed schema for testing.
type stubSchemaSource struct {
	schema json.RawMessage
}

func (s *stubSchemaSource) GetSchema(_ context.Context, _ string) (json.RawMessage, error) {
	return s.schema, nil
}

// redactTestHandler is a minimal handler that returns a record with
// sensitive data so the executor's redaction wrapper can be tested.
type redactTestHandler struct {
	rec *record.KRecord
}

func (h *redactTestHandler) Name() string { return "test.redact" }
func (h *redactTestHandler) RequiresConfirmation() bool { return false }
func (h *redactTestHandler) Invoke(_ context.Context, _ Invocation) (*Result, error) {
	return &Result{Record: h.rec}, nil
}

func TestExecutor_RedactsSensitiveFields(t *testing.T) {
	schema := json.RawMessage(`{
		"name": "test_rec",
		"version": 1,
		"fields": [
			{"name": "public_field", "type": "string"},
			{"name": "secret_field", "type": "string", "encrypted": true}
		]
	}`)
	schemaSrc := &stubSchemaSource{schema: schema}

	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	rec := &record.KRecord{
		ID:       uuid.New(),
		TenantID: tenant,
		KType:    "test_rec",
		Data:     json.RawMessage(`{"public_field":"hello","secret_field":"top-secret"}`),
		Status:   "active",
	}

	exec := NewExecutor(nil, nil, nil, schemaSrc)
	exec.Register(&redactTestHandler{rec: rec})

	res, err := exec.Invoke(context.Background(), Invocation{
		TenantID: tenant,
		ActorID:  uuid.New(),
		ToolName: "test.redact",
		Mode:     ModeDryRun,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Record == nil {
		t.Fatal("record is nil")
	}
	var doc map[string]any
	if err := json.Unmarshal(res.Record.Data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["public_field"] != "hello" {
		t.Fatalf("public_field should be visible: %v", doc["public_field"])
	}
	if doc["secret_field"] != "<redacted>" {
		t.Fatalf("secret_field should be redacted: %v", doc["secret_field"])
	}
}

func TestExecutor_NoSchemaSource_NoRedaction(t *testing.T) {
	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	rec := &record.KRecord{
		ID:       uuid.New(),
		TenantID: tenant,
		KType:    "test_rec",
		Data:     json.RawMessage(`{"public_field":"hello","secret_field":"top-secret"}`),
		Status:   "active",
	}

	exec := NewExecutor(nil, nil, nil, nil)
	exec.Register(&redactTestHandler{rec: rec})

	res, err := exec.Invoke(context.Background(), Invocation{
		TenantID: tenant,
		ActorID:  uuid.New(),
		ToolName: "test.redact",
		Mode:     ModeDryRun,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(res.Record.Data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Without schema source, no redaction is applied.
	if doc["secret_field"] != "top-secret" {
		t.Fatalf("secret_field should be visible without schema source: %v", doc["secret_field"])
	}
}

func TestExecutor_FilterFieldsByRole(t *testing.T) {
	schema := json.RawMessage(`{
		"name": "test_rec",
		"version": 1,
		"fields": [
			{"name": "public_field", "type": "string"},
			{"name": "manager_only", "type": "string"}
		],
		"field_permissions": {
			"manager_only": {"read": ["manager"], "write": ["manager"]}
		}
	}`)
	schemaSrc := &stubSchemaSource{schema: schema}

	tenant := uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	rec := &record.KRecord{
		ID:       uuid.New(),
		TenantID: tenant,
		KType:    "test_rec",
		Data:     json.RawMessage(`{"public_field":"hello","manager_only":"confidential"}`),
		Status:   "active",
	}

	exec := NewExecutor(nil, nil, nil, schemaSrc)
	exec.Register(&redactTestHandler{rec: rec})

	// Viewer role — should NOT see manager_only.
	res, err := exec.Invoke(context.Background(), Invocation{
		TenantID:  tenant,
		ActorID:   uuid.New(),
		ToolName:  "test.redact",
		Mode:      ModeDryRun,
		UserRoles: []string{"viewer"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(res.Record.Data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc["manager_only"]; ok {
		t.Fatal("manager_only should be filtered out for viewer role")
	}
	if doc["public_field"] != "hello" {
		t.Fatalf("public_field should be visible: %v", doc["public_field"])
	}
}
