package ktype

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// FieldSpec mirrors the per-field definition inside a KType schema. It
// captures the subset of attributes documented in ARCHITECTURE.md §6 that
// the Phase A validator understands. Additional attributes (e.g. `indexes`,
// `permissions`) live on the parent schema and are ignored here.
type FieldSpec struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Required  bool            `json:"required,omitempty"`
	MaxLength int             `json:"max_length,omitempty"`
	Min       *float64        `json:"min,omitempty"`
	Max       *float64        `json:"max,omitempty"`
	Pattern   string          `json:"pattern,omitempty"`
	Values    []string        `json:"values,omitempty"`
	Ref       string          `json:"ref,omitempty"`   // ref ktype name
	KType     string          `json:"ktype,omitempty"` // alternative spelling
	Default   json.RawMessage `json:"default,omitempty"`
	// Encrypted marks a field whose stored value must be encrypted at
	// rest with a per-tenant key. The record store enforces this on
	// write and decrypts transparently on read — schema consumers
	// outside the store path can treat the flag as advisory.
	Encrypted bool `json:"encrypted,omitempty"`
	// Classification is the platform-wide data classification of the
	// field. Allowed values: "public", "internal", "confidential",
	// "secret". A field classified "confidential" or "secret" is
	// treated as sensitive for encryption and audit/event redaction
	// even when Encrypted is not explicitly set, so the sensitive-data
	// posture is driven by classification rather than relying on
	// developers remembering {"encrypted": true}. Empty preserves the
	// legacy behaviour (sensitivity is governed by Encrypted alone).
	// ValidateSchema rejects values outside the allowed set.
	Classification string `json:"classification,omitempty"`
	// Path is an optional dotted path into nested JSON objects. When
	// empty, the field name is used as the top-level key (legacy
	// behaviour). When set (e.g. "employee.bank.iban"), encryption and
	// redaction walk the nested object to the leaf and operate on it
	// in place. Intermediate nodes must be objects (map[string]any);
	// arrays are not traversed. Path segments must match [A-Za-z0-9_].
	// ValidateSchema rejects malformed paths.
	Path string `json:"path,omitempty"`
	// Indexed marks a sensitive (encrypted) field as having a blind
	// index entry stored alongside the record. The blind index is
	// HMAC(tenant_search_key, canonical(value)) — a deterministic
	// digest that lets ListByField match on the field without
	// decrypting. Only meaningful when the field is also sensitive
	// (Encrypted or classification confidential/secret); a non-
	// sensitive indexed field is a no-op (the value is already
	// queryable via direct JSONB access).
	Indexed bool `json:"indexed,omitempty"`
}

// AllowedClassification values, in increasing sensitivity order.
const (
	ClassificationPublic       = "public"
	ClassificationInternal     = "internal"
	ClassificationConfidential = "confidential"
	ClassificationSecret       = "secret"
)

// allowedClassifications is the set ValidateSchema accepts.
var allowedClassifications = map[string]struct{}{
	ClassificationPublic:       {},
	ClassificationInternal:     {},
	ClassificationConfidential: {},
	ClassificationSecret:       {},
}

// IsSensitive reports whether a field must be treated as sensitive for
// encryption and audit/event redaction. A field is sensitive when it is
// explicitly marked Encrypted OR classified confidential/secret. This is
// the single predicate the record store consults so classification and
// the legacy Encrypted flag stay consistent.
func (f FieldSpec) IsSensitive() bool {
	if f.Encrypted {
		return true
	}
	switch f.Classification {
	case ClassificationConfidential, ClassificationSecret:
		return true
	default:
		return false
	}
}

// PathSegments returns the dotted-path segments for this field. When
// Path is empty, the field Name is returned as a single-element slice
// (the legacy top-level behaviour). When Path is set, it is split on
// "." and the segments are returned. The caller can then walk a nested
// JSON object to the leaf.
func (f FieldSpec) PathSegments() []string {
	if f.Path == "" {
		return []string{f.Name}
	}
	return splitDottedPath(f.Path)
}

// splitDottedPath splits a dotted path on ".". It does not validate
// segment contents; ValidateSchema does that at schema-load time.
func splitDottedPath(p string) []string {
	return strings.Split(p, ".")
}

// validPathSegment reports whether a single path segment contains only
// [A-Za-z0-9_]. Used by ValidateSchema to reject malformed paths at
// schema definition time so the encryption walker never encounters an
// ambiguous segment.
func validPathSegment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// Schema is the minimal shape of a KType schema consumed by the validator.
// The rest of the schema (indexes, permissions, views, cards, workflow,
// agent_tools, audit) is not consulted during data validation.
type Schema struct {
	Name    string      `json:"name"`
	Version int         `json:"version"`
	Fields  []FieldSpec `json:"fields"`
}

// ValidationError describes one validation failure and is returned inside
// ValidationErrors when multiple fields fail at once.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationErrors aggregates multiple ValidationError entries produced by a
// single ValidateData call. It implements error so callers can treat a batch
// failure as a single error value.
type ValidationErrors []ValidationError

func (es ValidationErrors) Error() string {
	if len(es) == 0 {
		return "validation: ok"
	}
	parts := make([]string, 0, len(es))
	for _, e := range es {
		parts = append(parts, e.Error())
	}
	return "validation: " + strings.Join(parts, "; ")
}

// ValidateData validates a KRecord data payload (JSONB) against a KType
// schema (JSONB). It returns a ValidationErrors slice with one entry per
// failing field, or nil if validation succeeds.
func ValidateData(schema json.RawMessage, data json.RawMessage) error {
	var s Schema
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("ktype: parse schema: %w", err)
	}
	var payload map[string]any
	if len(data) == 0 {
		payload = map[string]any{}
	} else if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("ktype: parse data: %w", err)
	}
	return validateAgainstSchema(s, payload)
}

// ValidateSchema validates the structural well-formedness of a KType
// schema itself (as opposed to ValidateData, which validates a payload
// against a schema). It is the registration-time gate that keeps the
// data-classification posture consistent:
//
//   - every field with a non-empty Classification must use one of the
//     allowed values (public / internal / confidential / secret);
//   - a field classified confidential or secret is treated as sensitive
//     (FieldSpec.IsSensitive) and SHOULD be encrypted — the record
//     store enforces encryption based on IsSensitive, so this is
//     advisory here, not a hard error, to keep the gate forward-
//     compatible with non-string sensitive fields the field encryptor
//     does not yet handle.
//
// Returns a ValidationErrors slice with one entry per failing field, or
// nil if the schema is well-formed.
func ValidateSchema(schema json.RawMessage) error {
	var s Schema
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("ktype: parse schema: %w", err)
	}
	var errs ValidationErrors
	for _, field := range s.Fields {
		if field.Classification == "" {
			// still validate path even when classification is unset
		} else if _, ok := allowedClassifications[field.Classification]; !ok {
			errs = append(errs, ValidationError{
				Field: field.Name,
				Message: fmt.Sprintf("classification %q is not one of %v", field.Classification, []string{
					ClassificationPublic, ClassificationInternal, ClassificationConfidential, ClassificationSecret,
				}),
			})
		}
		// Validate dotted-path syntax so the encryption/redaction
		// walker never encounters an ambiguous segment. An empty Path
		// is fine (legacy top-level field).
		if field.Path != "" {
			for _, seg := range splitDottedPath(field.Path) {
				if !validPathSegment(seg) {
					errs = append(errs, ValidationError{
						Field:   field.Name,
						Message: fmt.Sprintf("path %q contains invalid segment %q", field.Path, seg),
					})
					break
				}
			}
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func validateAgainstSchema(s Schema, payload map[string]any) error {
	var errs ValidationErrors
	for _, field := range s.Fields {
		value, present := payload[field.Name]
		if !present || value == nil {
			if field.Required {
				errs = append(errs, ValidationError{
					Field:   field.Name,
					Message: "is required",
				})
			}
			continue
		}
		if err := validateFieldValue(field, value); err != nil {
			errs = append(errs, ValidationError{Field: field.Name, Message: err.Error()})
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func validateFieldValue(field FieldSpec, value any) error {
	switch field.Type {
	case "string", "text":
		return validateString(field, value)
	case "number", "integer", "float", "decimal":
		return validateNumber(field, value)
	case "boolean":
		if _, ok := value.(bool); !ok {
			return errors.New("must be boolean")
		}
		return nil
	case "enum":
		return validateEnum(field, value)
	case "date", "datetime":
		return validateDate(field, value)
	case "ref":
		return validateRef(field, value)
	case "array":
		if _, ok := value.([]any); !ok {
			return errors.New("must be array")
		}
		return nil
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return errors.New("must be object")
		}
		return nil
	default:
		// Unknown types pass through — forward-compatible with new field
		// kinds introduced in later schema versions.
		return nil
	}
}

func validateString(field FieldSpec, value any) error {
	s, ok := value.(string)
	if !ok {
		return errors.New("must be string")
	}
	if field.MaxLength > 0 && len(s) > field.MaxLength {
		return fmt.Errorf("exceeds max_length %d", field.MaxLength)
	}
	if field.Pattern != "" {
		re, err := patternRegexp(field.Pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern: %w", err)
		}
		if !re.MatchString(s) {
			return fmt.Errorf("does not match pattern %q", field.Pattern)
		}
	}
	return nil
}

func validateNumber(field FieldSpec, value any) error {
	n, ok := toFloat64(value)
	if !ok {
		return errors.New("must be number")
	}
	if field.Type == "integer" && n != float64(int64(n)) {
		return errors.New("must be integer")
	}
	if field.Min != nil && n < *field.Min {
		return fmt.Errorf("must be >= %v", *field.Min)
	}
	if field.Max != nil && n > *field.Max {
		return fmt.Errorf("must be <= %v", *field.Max)
	}
	return nil
}

func validateEnum(field FieldSpec, value any) error {
	s, ok := value.(string)
	if !ok {
		return errors.New("must be string enum value")
	}
	for _, v := range field.Values {
		if s == v {
			return nil
		}
	}
	return fmt.Errorf("must be one of %v", field.Values)
}

func validateDate(field FieldSpec, value any) error {
	s, ok := value.(string)
	if !ok {
		return errors.New("must be ISO-8601 date string")
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"}
	for _, layout := range layouts {
		if _, err := time.Parse(layout, s); err == nil {
			return nil
		}
	}
	_ = field
	return errors.New("must be an ISO-8601 date or datetime")
}

func validateRef(field FieldSpec, value any) error {
	// References are stored as the target record id (string uuid). The
	// referent ktype name is declarative; cross-ktype FK checks happen at the
	// record layer, not in value validation.
	_ = field
	s, ok := value.(string)
	if !ok {
		return errors.New("ref must be a uuid string")
	}
	if len(s) < 32 {
		return errors.New("ref must be a uuid string")
	}
	return nil
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// patternRegexp compiles and caches regexp patterns. KTypes reuse the same
// patterns heavily (e.g. currency codes), so a tiny cache is worthwhile.
func patternRegexp(pattern string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	patternCache.Store(pattern, re)
	return re, nil
}

var patternCache sync.Map
