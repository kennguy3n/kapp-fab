package record

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Sentinel value used across the leak tests. If any redaction chokepoint
// lets this string reach an audit/event surface, the test fails. The same
// sentinel is the recommended value for the integration-level CI gate
// documented in docs/SECURITY_HARDENING_PLAN.md (P0-2): create a KType
// with an encrypted field set to this value, run Create/Update, and assert
// it never appears in krecords.data, audit_log, events.payload,
// notifications, SSE bodies, or worker logs.
const leakSentinel = "DO_NOT_LEAK_9f3c2a"

// stubHasher is a deterministic AuditHasher stand-in. It returns a fixed
// digest that is guaranteed NOT to contain the sentinel, so the leak tests
// can assert the HMAC path itself does not leak the plaintext.
type stubHasher struct{}

func (stubHasher) HMACString(uuid.UUID, string) (string, error) {
	return "stub-digest-not-sensitive", nil
}

// encryptedSchema builds a KType schema whose `salary` and `ssn` fields
// are marked encrypted, matching the shape the field encryptor redacts.
func encryptedSchema(t *testing.T) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{
		"name":"employee",
		"version":1,
		"fields":[
			{"name":"name","type":"string"},
			{"name":"salary","type":"string","encrypted":true},
			{"name":"ssn","type":"string","encrypted":true}
		]
	}`)
}

func dataWithSentinel(t *testing.T) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{"name":"Alice","salary":"` + leakSentinel + `","ssn":"` + leakSentinel + `-ssn"}`)
}

// assertNoSentinel fails the test if the sentinel appears anywhere in the
// byte slice. It is the single assertion every leak subtest routes
// through so a failure points at the exact surface that leaked.
func assertNoSentinel(t *testing.T, label string, b []byte) {
	t.Helper()
	if strings.Contains(string(b), leakSentinel) {
		t.Errorf("%s: sentinel %q leaked into surface: %s", label, leakSentinel, b)
	}
}

// TestRedactDataNoSentinelLeak verifies the audit before/after chokepoint
// replaces every encrypted field with "<redacted>" and never emits the
// plaintext sentinel.
func TestRedactDataNoSentinelLeak(t *testing.T) {
	schema := encryptedSchema(t)
	data := dataWithSentinel(t)

	out := RedactData(schema, data)
	assertNoSentinel(t, "RedactData", out)

	// json.Marshal HTML-escapes "<" to "\u003c", so check the escaped
	// form of the placeholder rather than the literal angle brackets.
	if !strings.Contains(string(out), "redacted") {
		t.Errorf("RedactData: expected redacted placeholder, got %s", out)
	}
	if !strings.Contains(string(out), `"salary"`) || !strings.Contains(string(out), `"ssn"`) {
		t.Errorf("RedactData: expected redacted field names listed, got %s", out)
	}
	if !strings.Contains(string(out), `"Alice"`) {
		t.Errorf("RedactData: non-encrypted field should pass through, got %s", out)
	}
}

// TestDiffSummaryNoSentinelLeak verifies the audit Context chokepoint
// exposes changed/redacted field names and per-field digests without ever
// emitting the plaintext sentinel.
func TestDiffSummaryNoSentinelLeak(t *testing.T) {
	schema := encryptedSchema(t)
	before := json.RawMessage(`{"name":"Alice","salary":"old","ssn":"` + leakSentinel + `"}`)
	after := json.RawMessage(`{"name":"Alice","salary":"` + leakSentinel + `","ssn":"` + leakSentinel + `"}`)
	tenantID := uuid.New()

	summary := DiffSummary(schema, before, after, stubHasher{}, tenantID)
	assertNoSentinel(t, "DiffSummary", summary)

	if !strings.Contains(string(summary), `"changed_fields"`) {
		t.Errorf("DiffSummary: expected changed_fields, got %s", summary)
	}
	if !strings.Contains(string(summary), `"redacted_fields"`) {
		t.Errorf("DiffSummary: expected redacted_fields, got %s", summary)
	}
	if !strings.Contains(string(summary), "stub-digest-not-sensitive") {
		t.Errorf("DiffSummary: expected HMAC digest present, got %s", summary)
	}
}

// TestDiffSummaryNilHasherOmitsDigests confirms that when no AuditHasher
// is wired (dev without KAPP_MASTER_KEY), redaction still lists the
// redacted fields and never leaks the sentinel — only the digests are
// absent.
func TestDiffSummaryNilHasherOmitsDigests(t *testing.T) {
	schema := encryptedSchema(t)
	before := json.RawMessage(`{}`)
	after := dataWithSentinel(t)

	summary := DiffSummary(schema, before, after, nil, uuid.New())
	assertNoSentinel(t, "DiffSummary(nil hasher)", summary)
	if strings.Contains(string(summary), "stub-digest") {
		t.Errorf("DiffSummary: nil hasher should not produce digests, got %s", summary)
	}
	if !strings.Contains(string(summary), `"redacted_fields"`) {
		t.Errorf("DiffSummary: redacted_fields should still list, got %s", summary)
	}
}

// TestEventSummaryPayloadNoSentinelLeak verifies the event outbox payload
// carries no record data at all — only identity, status, and the redacted
// diff summary — so downstream consumers (SSE, webhooks, notifications,
// worker logs) never receive the plaintext sentinel.
func TestEventSummaryPayloadNoSentinelLeak(t *testing.T) {
	schema := encryptedSchema(t)
	before := json.RawMessage(`{}`)
	after := dataWithSentinel(t)
	tenantID := uuid.New()

	summary := DiffSummary(schema, before, after, stubHasher{}, tenantID)
	r := KRecord{
		ID:       uuid.New(),
		TenantID: tenantID,
		KType:    "employee",
		Version:  1,
		Status:   "active",
		// Data intentionally carries the sentinel: the in-memory record
		// returned to the API caller may carry plaintext, but the event
		// payload must not.
		Data: after,
	}
	actor := uuid.New()
	payload := eventSummaryPayload(r, "employee.created", summary, &actor, "user")
	assertNoSentinel(t, "eventSummaryPayload", payload)

	if strings.Contains(string(payload), `"data"`) {
		t.Errorf("event payload must not carry a data field, got %s", payload)
	}
	if !strings.Contains(string(payload), `"id"`) || !strings.Contains(string(payload), `"ktype"`) {
		t.Errorf("event payload missing identity fields, got %s", payload)
	}
}

// TestEmitPipelineNoSentinelLeak simulates the exact redaction sequence
// emit() applies before forwarding to the audit logger and event
// publisher, and asserts the sentinel is absent from every surface that
// would be persisted: the audit Before, After, Context, and the event
// Payload. This is the unit-level gate that mirrors the integration CI
// gate in docs/SECURITY_HARDENING_PLAN.md (P0-2).
func TestEmitPipelineNoSentinelLeak(t *testing.T) {
	schema := encryptedSchema(t)
	before := json.RawMessage(`{"name":"Alice","salary":"old","ssn":"` + leakSentinel + `"}`)
	after := dataWithSentinel(t)
	tenantID := uuid.New()

	// Mirror emit(): summary is computed from plaintext, then Before/After
	// are redacted, then Context is set to the summary, then the event
	// payload is built from the summary.
	summary := DiffSummary(schema, before, after, stubHasher{}, tenantID)
	auditBefore := RedactData(schema, before)
	auditAfter := RedactData(schema, after)
	auditContext := summary

	r := KRecord{ID: uuid.New(), TenantID: tenantID, KType: "employee", Version: 2, Status: "active", Data: after}
	actor := uuid.New()
	eventPayload := eventSummaryPayload(r, "employee.updated", summary, &actor, "user")

	assertNoSentinel(t, "audit before", auditBefore)
	assertNoSentinel(t, "audit after", auditAfter)
	assertNoSentinel(t, "audit context", auditContext)
	assertNoSentinel(t, "event payload", eventPayload)
}

// TestRedactDataNoEncryptedFieldsPassthrough confirms redaction is a
// no-op (and never drops data) when the schema has no encrypted fields,
// so non-sensitive KTypes are unaffected by the chokepoint.
func TestRedactDataNoEncryptedFieldsPassthrough(t *testing.T) {
	schema := json.RawMessage(`{"name":"note","version":1,"fields":[{"name":"body","type":"string"}]}`)
	data := json.RawMessage(`{"body":"hello"}`)
	if got := RedactData(schema, data); string(got) != string(data) {
		t.Errorf("RedactData: schema without encrypted fields should passthrough, got %s", got)
	}
}
