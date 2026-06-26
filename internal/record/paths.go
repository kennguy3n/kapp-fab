package record

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// walkPath descends a dotted-path through a decoded JSON object. Returns
// the parent map containing the leaf key, the leaf key, and true on
// hit. Returns nil, "", false on any traversal miss (missing key,
// intermediate non-object). Intermediate nodes must be
// map[string]any; arrays are not traversed — matching the contract in
// ktype.FieldSpec.PathSegments.
//
// This is the single path-walking primitive used by encryptFields,
// decryptRecordWith, RedactData, and DiffSummary so nested-path
// encryption stays consistent across all four surfaces.
func walkPath(doc map[string]any, segments []string) (map[string]any, string, bool) {
	if len(segments) == 0 {
		return nil, "", false
	}
	cur := doc
	for i := 0; i < len(segments)-1; i++ {
		next, ok := cur[segments[i]]
		if !ok {
			return nil, "", false
		}
		m, ok := next.(map[string]any)
		if !ok {
			return nil, "", false
		}
		cur = m
	}
	leaf := segments[len(segments)-1]
	if _, ok := cur[leaf]; !ok {
		return nil, "", false
	}
	return cur, leaf, true
}

// setPath walks a dotted path through a decoded JSON object, creating
// intermediate maps as needed, and sets the leaf key to `value`.
// Returns false if an intermediate segment exists but is not a
// map[string]any (type conflict — the caller should treat this as an
// error rather than silently overwriting).
func setPath(doc map[string]any, segments []string, value any) bool {
	if len(segments) == 0 {
		return false
	}
	cur := doc
	for i := 0; i < len(segments)-1; i++ {
		next, ok := cur[segments[i]]
		if !ok {
			m := make(map[string]any)
			cur[segments[i]] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return false
		}
		cur = m
	}
	cur[segments[len(segments)-1]] = value
	return true
}

// sensitiveFields extracts the list of FieldSpecs in schema that are
// sensitive — i.e. carry {"encrypted": true} OR a classification of
// "confidential"/"secret" (ktype.FieldSpec.IsSensitive). Returns an
// empty (non-nil) slice when none are present so callers can range
// over the result unconditionally. Driving both encryption and
// audit/event redaction off this single predicate keeps the legacy
// Encrypted flag and the classification-based posture consistent.
//
// This supersedes the older encryptedFieldNames(map[string]struct{})
// variant: the slice form preserves PathSegments so callers can walk
// nested JSON objects to the leaf.
func sensitiveFields(schema json.RawMessage) ([]ktype.FieldSpec, error) {
	var s ktype.Schema
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, fmt.Errorf("record: parse schema for encryption: %w", err)
	}
	out := make([]ktype.FieldSpec, 0, len(s.Fields))
	for _, f := range s.Fields {
		if f.IsSensitive() {
			out = append(out, f)
		}
	}
	return out, nil
}

// indexedSensitiveFields returns the subset of sensitive fields that
// also have Indexed == true. These fields get a blind index entry
// written to the blind_indexes JSONB column alongside the record.
func indexedSensitiveFields(schema json.RawMessage) ([]ktype.FieldSpec, error) {
	all, err := sensitiveFields(schema)
	if err != nil {
		return nil, err
	}
	out := make([]ktype.FieldSpec, 0, len(all))
	for _, f := range all {
		if f.Indexed {
			out = append(out, f)
		}
	}
	return out, nil
}

// computeBlindIndexes builds the blind_indexes JSON object for a
// record. For each sensitive+indexed field in the schema, it walks
// the dotted path to the leaf value, computes HMAC(tenant_key, value),
// and stores the base64 digest under the field name. Returns nil when
// there are no indexed fields or no values are present — callers
// should store NULL rather than an empty object so the partial index
// on blind_indexes IS NOT NULL stays selective.
func computeBlindIndexes(tenantID uuid.UUID, schema, data json.RawMessage, indexer BlindIndexer) (json.RawMessage, error) {
	if indexer == nil {
		return nil, nil
	}
	fields, err := indexedSensitiveFields(schema)
	if err != nil || len(fields) == 0 {
		return nil, err
	}
	if len(data) == 0 || !json.Valid(data) {
		return nil, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil
	}
	if doc == nil {
		return nil, nil
	}
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		segs := f.PathSegments()
		parent, leaf, ok := walkPath(doc, segs)
		if !ok {
			continue
		}
		v := parent[leaf]
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		digest, err := indexer.BlindIndex(tenantID, s)
		if err != nil {
			return nil, fmt.Errorf("record: blind index field %q: %w", f.Name, err)
		}
		if digest != "" {
			out[f.Name] = digest
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return json.Marshal(out)
}
