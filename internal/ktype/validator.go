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
			return fmt.Errorf("invalid pattern: %v", err)
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
