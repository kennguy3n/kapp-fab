package record

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// AuditHasher is the subset of *tenant.KeyManager used to compute
// per-tenant HMAC digests of sensitive field values for the audit/event
// redaction path. The interface exists so tests can swap in a
// deterministic or no-op implementation without wiring a real master key.
//
// When a store has no AuditHasher wired (e.g. local dev without
// KAPP_MASTER_KEY), redaction still replaces sensitive values with
// "<redacted>"; only the per-field change-detection digests are omitted.
type AuditHasher interface {
	HMACString(tenantID uuid.UUID, value string) (string, error)
}

// redactedPlaceholder is the value substituted for every encrypted field
// when building audit before/after payloads. It is deliberately a plain
// string (not the ciphertext) so the audit trail is human-readable while
// carrying no sensitive material.
const redactedPlaceholder = "<redacted>"

// redactedMarkerKey is the JSON key listing the field names that were
// redacted from a payload, so a reader can see *which* fields were
// sensitive without seeing their values.
const redactedMarkerKey = "_redacted"

// RedactData returns a copy of `data` with every field the schema marks
// sensitive ({"encrypted": true} OR classification "confidential"/"secret")
// replaced by redactedPlaceholder, and a `"_redacted": [...]` array listing
// the names of those fields. Fields with a dotted Path are walked to their
// nested leaf; top-level fields use the field Name as the key. Fields not
// flagged sensitive are passed through unchanged.
//
// RedactData is the single chokepoint for keeping plaintext out of
// audit_log.before/after. Callers MUST route any payload destined for an
// audit row through it before handing it to audit.Logger.LogTx.
//
// If `data` is empty or not a JSON object, it is returned unchanged —
// there is nothing to redact and we never want redaction to turn a
// well-formed empty payload into an error that fails the mutation.
func RedactData(schema, data json.RawMessage) json.RawMessage {
	fields, err := sensitiveFields(schema)
	if err != nil || len(fields) == 0 {
		return data
	}
	if len(data) == 0 || !json.Valid(data) {
		return data
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return data
	}
	if doc == nil {
		return data
	}
	redacted := make([]string, 0, len(fields))
	for _, f := range fields {
		segs := f.PathSegments()
		parent, leaf, ok := walkPath(doc, segs)
		if !ok {
			continue
		}
		parent[leaf] = redactedPlaceholder
		redacted = append(redacted, f.Name)
	}
	if len(redacted) == 0 {
		return data
	}
	sort.Strings(redacted)
	doc[redactedMarkerKey] = redacted
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

// DiffSummary computes a redacted diff context for an audit entry. It
// compares the *plaintext* before/after to determine which fields
// changed, then emits:
//
//   - "changed_fields": the sorted list of field names whose values
//     differ between before and after (including fields present in only
//     one side).
//   - "redacted_fields": the sorted list of encrypted field names that
//     appear in either side.
//   - "<field>_hash_before" / "<field>_hash_after": base64 HMAC-SHA256
//     digests of the plaintext value for each redacted field, when an
//     AuditHasher is wired. Omitted when hasher is nil (dev without a
//     master key) — redaction itself does not depend on the digest.
//
// The digests let a verifier confirm a sensitive field changed (or did
// not) without ever seeing the value. The plaintext values never leave
// this function except through the HMAC.
//
// For a create (empty before) the changed_fields list is every field in
// `after`; for a delete (before == after) it is empty.
func DiffSummary(schema, before, after json.RawMessage, hasher AuditHasher, tenantID uuid.UUID) json.RawMessage {
	fields, err := sensitiveFields(schema)
	if err != nil {
		fields = nil
	}
	b := parseObject(before)
	a := parseObject(after)

	changed := changedFields(b, a)
	redacted := redactedPresentFields(fields, b, a)

	out := map[string]any{
		"changed_fields":  changed,
		"redacted_fields": redacted,
	}

	if hasher != nil && len(redacted) > 0 {
		for _, f := range fields {
			if !containsStr(redacted, f.Name) {
				continue
			}
			segs := f.PathSegments()
			if parent, leaf, ok := walkPath(b, segs); ok {
				if v, ok := stringify(parent[leaf]); ok {
					if h, err := hasher.HMACString(tenantID, v); err == nil && h != "" {
						out[f.Name+"_hash_before"] = h
					}
				}
			}
			if parent, leaf, ok := walkPath(a, segs); ok {
				if v, ok := stringify(parent[leaf]); ok {
					if h, err := hasher.HMACString(tenantID, v); err == nil && h != "" {
						out[f.Name+"_hash_after"] = h
					}
				}
			}
		}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

// eventSummaryPayload builds the JSON payload written to the events outbox.
// It intentionally carries NO record data: only identity, status, and the
// redacted diff summary. Downstream consumers (SSE, webhooks,
// notifications, worker logs) therefore never receive plaintext field
// values, even for encrypted fields.
func eventSummaryPayload(r KRecord, eventType string, summary json.RawMessage, actorID *uuid.UUID, actorKind string) json.RawMessage {
	var sum map[string]any
	_ = json.Unmarshal(summary, &sum)

	payload := map[string]any{
		"id":       r.ID,
		"tenant":   r.TenantID,
		"ktype":    r.KType,
		"version":  r.Version,
		"status":   r.Status,
		"updated":  r.UpdatedAt,
		"created":  r.CreatedAt,
		"actor":    actorID,
		"kind":     actorKind,
		"snapshot": snapshotTypeFor(eventType),
	}
	for k, v := range sum {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		// Fall back to a minimal payload rather than failing the
		// mutation over a marshal error — the audit row still carries
		// the redacted diff.
		raw, _ = json.Marshal(map[string]any{
			"id":       r.ID,
			"tenant":   r.TenantID,
			"ktype":    r.KType,
			"version":  r.Version,
			"status":   r.Status,
			"snapshot": snapshotTypeFor(eventType),
		})
	}
	return raw
}

// changedFields returns the sorted list of top-level keys whose values
// differ between b and a, including keys present in only one side.
// When a top-level key holds a nested object, the function recurses
// into the object and reports the nested field names (using the
// dotted path from the root) so the audit trail is granular enough
// to distinguish "employee.bank.iban changed" from "employee changed".
func changedFields(b, a map[string]any) []string {
	seen := make(map[string]struct{}, len(b)+len(a))
	for k := range b {
		seen[k] = struct{}{}
	}
	for k := range a {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		bv, bok := b[k]
		av, aok := a[k]
		if !bok || !aok {
			if bok != aok {
				out = append(out, k)
			}
			continue
		}
		// If both values are objects, recurse to get granular
		// nested field names in the audit trail.
		bm, bIsMap := bv.(map[string]any)
		am, aIsMap := av.(map[string]any)
		if bIsMap && aIsMap {
			out = append(out, prefixPaths(changedFields(bm, am), k+".")...)
			continue
		}
		if !equalJSON(bv, av) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// prefixPaths prepends prefix to each element in paths. Used to
// build dotted-path names when recursing into nested objects in
// changedFields.
func prefixPaths(paths []string, prefix string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = prefix + p
	}
	return out
}

// redactedPresentFields returns the sorted list of sensitive field names
// whose dotted path resolves in either before or after.
func redactedPresentFields(fields []ktype.FieldSpec, b, a map[string]any) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		segs := f.PathSegments()
		if _, _, ok := walkPath(b, segs); ok {
			out = append(out, f.Name)
			continue
		}
		if _, _, ok := walkPath(a, segs); ok {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// containsStr is a small helper for membership checks on sorted slices.
func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// parseObject decodes a JSON object payload into a map. Empty / invalid /
// non-object payloads yield an empty map so callers can range uniformly.
func parseObject(data json.RawMessage) map[string]any {
	if len(data) == 0 || !json.Valid(data) {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}
	}
	if m == nil {
		return map[string]any{}
	}
	return m
}

// stringify coerces a JSON-decoded value to a string for HMAC input. Only
// string values are hashed; non-string encrypted fields are skipped (the
// field encryptor itself only handles strings, so this matches the
// encryption-side contract).
func stringify(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// equalJSON compares two json.Unmarshal'd values structurally. It handles
// the common scalar / map / slice cases without reflect so the redaction
// path stays allocation-light.
func equalJSON(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !equalJSON(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !equalJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
	}
}
