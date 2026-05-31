package settings

import (
	"errors"
	"strings"
	"testing"
)

func TestNewValidator_EmptyAcceptsAll(t *testing.T) {
	v, err := NewValidator(nil)
	if err != nil {
		t.Fatalf("NewValidator(nil): %v", err)
	}
	if err := v.Validate(map[string]any{"anything": "goes"}); err != nil {
		t.Fatalf("nil-schema should accept any doc, got %v", err)
	}
	if err := v.Validate(42); err != nil {
		t.Fatalf("nil-schema should accept scalars, got %v", err)
	}
}

func TestNewValidator_ParseFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad json", `{not json}`},
		{"unsupported keyword", `{"oneOf":[{"type":"string"}]}`},
		{"unsupported additionalProperties type", `{"additionalProperties": {"type":"string"}}`},
		{"unsupported type name", `{"type": "bytes"}`},
		{"type wrong shape", `{"type": 42}`},
		{"required wrong shape", `{"required": "foo"}`},
		{"properties wrong shape", `{"properties": "foo"}`},
		{"pattern not regex", `{"pattern": "[unclosed"}`},
		{"minLength negative", `{"minLength": -1}`},
		{"minLength not int", `{"minLength": 1.5}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewValidator([]byte(c.body))
			if !errors.Is(err, ErrInvalidSchema) {
				t.Fatalf("want ErrInvalidSchema, got %v", err)
			}
		})
	}
}

func TestValidate_TypeChecks(t *testing.T) {
	schema := []byte(`{"type": "string"}`)
	v, err := NewValidator(schema)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.Validate("hello"); err != nil {
		t.Errorf("want pass for string, got %v", err)
	}
	if err := v.Validate(42); !errors.Is(err, ErrValidation) {
		t.Errorf("want ErrValidation for number, got %v", err)
	}
}

func TestValidate_RequiredAndProperties(t *testing.T) {
	schema := []byte(`{
		"type": "object",
		"required": ["api_key", "region"],
		"properties": {
			"api_key": {"type": "string", "minLength": 8},
			"region":  {"type": "string", "enum": ["us-east-1","us-west-2","eu-west-1"]},
			"port":    {"type": "integer", "minimum": 1, "maximum": 65535}
		},
		"additionalProperties": false
	}`)
	v, err := NewValidator(schema)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.ValidateRaw([]byte(`{"api_key":"abcdefgh","region":"us-east-1","port":443}`)); err != nil {
		t.Errorf("happy path: %v", err)
	}
	// Missing required
	err = v.ValidateRaw([]byte(`{"api_key":"abcdefgh"}`))
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "/region") {
		t.Errorf("missing required: %v", err)
	}
	// Enum violation
	err = v.ValidateRaw([]byte(`{"api_key":"abcdefgh","region":"mars-1"}`))
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "enum") {
		t.Errorf("enum: %v", err)
	}
	// minLength violation
	err = v.ValidateRaw([]byte(`{"api_key":"x","region":"us-east-1"}`))
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "minLength") {
		t.Errorf("minLength: %v", err)
	}
	// additionalProperties: false
	err = v.ValidateRaw([]byte(`{"api_key":"abcdefgh","region":"us-east-1","secret":"x"}`))
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "secret") {
		t.Errorf("additionalProperties: %v", err)
	}
	// integer range
	err = v.ValidateRaw([]byte(`{"api_key":"abcdefgh","region":"us-east-1","port":70000}`))
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "maximum") {
		t.Errorf("max range: %v", err)
	}
	// Integer requires int — 443.5 should fail as not-integer.
	err = v.ValidateRaw([]byte(`{"api_key":"abcdefgh","region":"us-east-1","port":443.5}`))
	if !errors.Is(err, ErrValidation) {
		t.Errorf("non-integer should fail: %v", err)
	}
}

func TestValidate_ArrayItems(t *testing.T) {
	schema := []byte(`{
		"type": "array",
		"items": {"type": "string", "minLength": 1},
		"minItems": 1,
		"maxItems": 3
	}`)
	v, err := NewValidator(schema)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.ValidateRaw([]byte(`["a","bb"]`)); err != nil {
		t.Errorf("happy: %v", err)
	}
	if err := v.ValidateRaw([]byte(`[]`)); !errors.Is(err, ErrValidation) {
		t.Errorf("minItems: %v", err)
	}
	if err := v.ValidateRaw([]byte(`["a","b","c","d"]`)); !errors.Is(err, ErrValidation) {
		t.Errorf("maxItems: %v", err)
	}
	if err := v.ValidateRaw([]byte(`[""]`)); !errors.Is(err, ErrValidation) {
		t.Errorf("item minLength: %v", err)
	}
}

func TestValidate_Pattern(t *testing.T) {
	schema := []byte(`{"type": "string", "pattern": "^[a-z]+$"}`)
	v, err := NewValidator(schema)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.ValidateRaw([]byte(`"hello"`)); err != nil {
		t.Errorf("happy: %v", err)
	}
	if err := v.ValidateRaw([]byte(`"Hello123"`)); !errors.Is(err, ErrValidation) {
		t.Errorf("pattern: %v", err)
	}
}

func TestValidate_Format(t *testing.T) {
	cases := []struct {
		schema string
		good   string
		bad    string
	}{
		{`{"type":"string","format":"email"}`, `"x@y.com"`, `"not-an-email"`},
		{`{"type":"string","format":"uri"}`, `"https://a.b"`, `"not-a-url"`},
		{`{"type":"string","format":"uuid"}`, `"550e8400-e29b-41d4-a716-446655440000"`, `"not-a-uuid"`},
		{`{"type":"string","format":"date-time"}`, `"2024-01-02T03:04:05Z"`, `"not-a-date"`},
	}
	for _, c := range cases {
		t.Run(c.schema, func(t *testing.T) {
			v, err := NewValidator([]byte(c.schema))
			if err != nil {
				t.Fatalf("NewValidator: %v", err)
			}
			if err := v.ValidateRaw([]byte(c.good)); err != nil {
				t.Errorf("happy: %v", err)
			}
			if err := v.ValidateRaw([]byte(c.bad)); !errors.Is(err, ErrValidation) {
				t.Errorf("bad: %v", err)
			}
		})
	}
}

func TestValidate_NestedProperties(t *testing.T) {
	schema := []byte(`{
		"type": "object",
		"properties": {
			"webhook": {
				"type": "object",
				"required": ["url"],
				"properties": {
					"url":    {"type": "string", "format": "uri"},
					"events": {"type":"array","items":{"type":"string"}}
				}
			}
		}
	}`)
	v, err := NewValidator(schema)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.ValidateRaw([]byte(`{"webhook":{"url":"https://x.com","events":["a","b"]}}`)); err != nil {
		t.Errorf("happy: %v", err)
	}
	err = v.ValidateRaw([]byte(`{"webhook":{"events":["a"]}}`))
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "/webhook/url") {
		t.Errorf("nested required: %v", err)
	}
}

func TestValidate_TypeArrayUnion(t *testing.T) {
	schema := []byte(`{"type": ["string", "null"]}`)
	v, err := NewValidator(schema)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.ValidateRaw([]byte(`"hi"`)); err != nil {
		t.Errorf("string: %v", err)
	}
	if err := v.ValidateRaw([]byte(`null`)); err != nil {
		t.Errorf("null: %v", err)
	}
	if err := v.ValidateRaw([]byte(`42`)); !errors.Is(err, ErrValidation) {
		t.Errorf("number rejected: %v", err)
	}
}
