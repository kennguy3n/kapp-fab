// Package settings implements the per-install settings validator
// used by both the runtime engine (Install / UpdateSettings) and
// the B6 API handler. Extensions ship a `settings.json` JSON
// Schema (draft 2020-12) at manifest.settings_schema and the
// platform validates per-tenant settings against it before
// persisting.
//
// Rather than pulling in a full JSON Schema implementation (the
// codebase does not currently depend on one and the surface area
// of draft-2020-12 is large), this package implements a *bounded
// subset* sufficient for the patterns extensions actually use:
//
//   - type: string | number | integer | boolean | object | array | null
//   - type as array of the above (union types)
//   - required: list of property names that MUST be present in object
//   - properties: per-property schemas, recursively validated
//   - additionalProperties: bool (false rejects unknown keys)
//   - enum: closed set of allowed scalar values
//   - minimum / maximum / exclusiveMinimum / exclusiveMaximum (number/integer)
//   - minLength / maxLength (string)
//   - pattern: regex (string)
//   - format: "email" | "uri" | "uuid" | "date-time" (recognised hints only;
//     unknown formats are silently passing per draft-2020-12)
//   - items: per-element schema (array)
//   - minItems / maxItems (array)
//   - const: exact value match
//
// Constructs explicitly NOT supported in v1 (rejected at schema-
// load time so a publisher knows their schema won't validate):
//   - $ref / definitions / $defs (no recursion across files)
//   - oneOf / anyOf / allOf / not
//   - if/then/else
//   - patternProperties
//   - dependentSchemas
//   - contains
//
// A future upgrade can swap this for a third-party library without
// changing the public API (Validator.Validate).
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ErrInvalidSchema is returned by NewValidator when the schema
// document itself cannot be parsed or uses an unsupported keyword.
// API handlers should translate this into a 500 — it indicates a
// programming error on the publisher side that ought to have been
// caught at manifest-upload time. By the time we reach a tenant
// install, the schema is already in the catalog.
var ErrInvalidSchema = errors.New("settings: invalid schema")

// ErrValidation is returned by Validator.Validate when the input
// document does not conform to the schema. API handlers translate
// this into a 400 with the wrapped detail (the per-field message
// chain) surfaced to the publisher.
var ErrValidation = errors.New("settings: validation failed")

// ValidationError reports the per-field failure. Multiple are
// reported as a joined error via errors.Join.
type ValidationError struct {
	Path    string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return e.Path + ": " + e.Message
}

func (e *ValidationError) Unwrap() error { return ErrValidation }

// Validator is a compiled JSON-Schema document. NewValidator
// parses + validates the schema once at construction; later
// Validate calls run against the compiled form.
//
// Validator is safe for concurrent use by multiple goroutines —
// after construction it is immutable, so the API handler can hold
// a single instance per (extension, version) pair across requests.
type Validator struct {
	root *schemaNode
}

// NewValidator compiles a JSON Schema document (the bytes of
// settings.json from the bundle). Returns ErrInvalidSchema for
// any structural failure or unsupported keyword.
func NewValidator(schemaJSON []byte) (*Validator, error) {
	if len(schemaJSON) == 0 {
		// An empty schema accepts any document. This is the
		// "no settings_schema declared" path: the engine passes
		// nil schemaJSON and the validator accepts whatever the
		// operator submits. Modeled as an always-passing
		// Validator rather than a nil pointer so callers don't
		// need to nil-check at every Validate call site.
		return &Validator{root: &schemaNode{empty: true}}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(schemaJSON, &raw); err != nil {
		return nil, fmt.Errorf("%w: parse: %w", ErrInvalidSchema, err)
	}
	node, err := compileNode(raw, "")
	if err != nil {
		return nil, err
	}
	return &Validator{root: node}, nil
}

// Validate runs the document against the compiled schema. Returns
// nil on success; on failure, the returned error wraps every
// ValidationError via errors.Join so the API handler can surface
// each path-level message.
func (v *Validator) Validate(doc any) error {
	if v == nil || v.root == nil || v.root.empty {
		return nil
	}
	var errs []error
	v.root.validate(doc, "", &errs)
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// ValidateRaw is the JSON-bytes convenience wrapper. Decodes into
// interface{} (so JSON numbers become float64 — the validator
// treats integers via the schema-declared type, not the Go
// runtime type).
func (v *Validator) ValidateRaw(docJSON []byte) error {
	if v == nil || v.root == nil || v.root.empty {
		return nil
	}
	if len(docJSON) == 0 {
		// Empty body is an empty object — many extensions have
		// optional settings.
		return v.Validate(map[string]any{})
	}
	var doc any
	if err := json.Unmarshal(docJSON, &doc); err != nil {
		return fmt.Errorf("%w: parse body: %w", ErrValidation, err)
	}
	return v.Validate(doc)
}

// --- compiled schema tree ---

type schemaNode struct {
	empty bool

	// Allowed JSON types ("string", "number", "integer", "boolean",
	// "object", "array", "null"). Empty means "any".
	types []string

	// Object keywords.
	required             []string
	properties           map[string]*schemaNode
	additionalProperties *bool // nil = unconstrained, &true / &false

	// Scalar/enum keywords.
	enum  []any
	cVal  any
	cSet  bool

	// Numeric keywords. Pointers distinguish "absent" from "zero".
	minimum          *float64
	maximum          *float64
	exclusiveMinimum *float64
	exclusiveMaximum *float64

	// String keywords.
	minLength *int
	maxLength *int
	pattern   *regexp.Regexp
	format    string

	// Array keywords.
	items    *schemaNode
	minItems *int
	maxItems *int
}

// compileNode walks a parsed schema map into a *schemaNode,
// rejecting any unsupported keyword.
func compileNode(raw map[string]any, path string) (*schemaNode, error) {
	n := &schemaNode{}
	// `type` may be a single string or an array of strings.
	if t, ok := raw["type"]; ok {
		switch tt := t.(type) {
		case string:
			n.types = []string{tt}
		case []any:
			for _, e := range tt {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("%w: %s/type must be string or []string", ErrInvalidSchema, path)
				}
				n.types = append(n.types, s)
			}
		default:
			return nil, fmt.Errorf("%w: %s/type must be string or []string", ErrInvalidSchema, path)
		}
		for _, ty := range n.types {
			if !knownType(ty) {
				return nil, fmt.Errorf("%w: %s/type %q not supported", ErrInvalidSchema, path, ty)
			}
		}
	}
	if req, ok := raw["required"]; ok {
		arr, ok := req.([]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s/required must be []string", ErrInvalidSchema, path)
		}
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%w: %s/required must be []string", ErrInvalidSchema, path)
			}
			n.required = append(n.required, s)
		}
	}
	if props, ok := raw["properties"]; ok {
		propsMap, ok := props.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s/properties must be object", ErrInvalidSchema, path)
		}
		n.properties = make(map[string]*schemaNode, len(propsMap))
		// Walk in sorted-key order so a malformed property
		// produces a deterministic error message across runs.
		keys := make([]string, 0, len(propsMap))
		for k := range propsMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, ok := propsMap[k].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: %s/properties/%s must be object", ErrInvalidSchema, path, k)
			}
			child, err := compileNode(sub, path+"/properties/"+k)
			if err != nil {
				return nil, err
			}
			n.properties[k] = child
		}
	}
	if ap, ok := raw["additionalProperties"]; ok {
		b, ok := ap.(bool)
		if !ok {
			return nil, fmt.Errorf("%w: %s/additionalProperties must be bool (object schemas not supported in v1)", ErrInvalidSchema, path)
		}
		n.additionalProperties = &b
	}
	if e, ok := raw["enum"]; ok {
		arr, ok := e.([]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s/enum must be array", ErrInvalidSchema, path)
		}
		n.enum = arr
	}
	if c, ok := raw["const"]; ok {
		n.cVal = c
		n.cSet = true
	}
	if v, ok := raw["minimum"]; ok {
		f, err := numKeyword(v, path, "minimum")
		if err != nil {
			return nil, err
		}
		n.minimum = &f
	}
	if v, ok := raw["maximum"]; ok {
		f, err := numKeyword(v, path, "maximum")
		if err != nil {
			return nil, err
		}
		n.maximum = &f
	}
	if v, ok := raw["exclusiveMinimum"]; ok {
		f, err := numKeyword(v, path, "exclusiveMinimum")
		if err != nil {
			return nil, err
		}
		n.exclusiveMinimum = &f
	}
	if v, ok := raw["exclusiveMaximum"]; ok {
		f, err := numKeyword(v, path, "exclusiveMaximum")
		if err != nil {
			return nil, err
		}
		n.exclusiveMaximum = &f
	}
	if v, ok := raw["minLength"]; ok {
		i, err := intKeyword(v, path, "minLength")
		if err != nil {
			return nil, err
		}
		n.minLength = &i
	}
	if v, ok := raw["maxLength"]; ok {
		i, err := intKeyword(v, path, "maxLength")
		if err != nil {
			return nil, err
		}
		n.maxLength = &i
	}
	if v, ok := raw["pattern"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s/pattern must be string", ErrInvalidSchema, path)
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("%w: %s/pattern: %w", ErrInvalidSchema, path, err)
		}
		n.pattern = re
	}
	if v, ok := raw["format"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s/format must be string", ErrInvalidSchema, path)
		}
		n.format = s
	}
	if v, ok := raw["items"]; ok {
		sub, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s/items must be object", ErrInvalidSchema, path)
		}
		child, err := compileNode(sub, path+"/items")
		if err != nil {
			return nil, err
		}
		n.items = child
	}
	if v, ok := raw["minItems"]; ok {
		i, err := intKeyword(v, path, "minItems")
		if err != nil {
			return nil, err
		}
		n.minItems = &i
	}
	if v, ok := raw["maxItems"]; ok {
		i, err := intKeyword(v, path, "maxItems")
		if err != nil {
			return nil, err
		}
		n.maxItems = &i
	}

	// Reject unsupported keywords explicitly. Unknown keywords are
	// commonly typos (`tipe` vs `type`); silently passing them would
	// let a publisher ship a schema that LOOKS strict but accepts
	// everything. The allowed list is the union of keywords this
	// validator implements plus a handful of universally-allowed
	// annotations (title, description, default, examples) that the
	// validator doesn't enforce but that publishers will use for
	// documentation.
	for k := range raw {
		if !allowedKeyword(k) {
			return nil, fmt.Errorf("%w: %s/%s is not supported in v1", ErrInvalidSchema, path, k)
		}
	}
	return n, nil
}

func knownType(t string) bool {
	switch t {
	case "string", "number", "integer", "boolean", "object", "array", "null":
		return true
	}
	return false
}

func allowedKeyword(k string) bool {
	switch k {
	case "type", "required", "properties", "additionalProperties",
		"enum", "const",
		"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
		"minLength", "maxLength", "pattern", "format",
		"items", "minItems", "maxItems",
		// Annotation-only keywords (validator does not enforce
		// them; they're harmless metadata).
		"title", "description", "default", "examples", "$schema", "$id":
		return true
	}
	return false
}

func numKeyword(v any, path, key string) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	}
	return 0, fmt.Errorf("%w: %s/%s must be number", ErrInvalidSchema, path, key)
}

func intKeyword(v any, path, key string) (int, error) {
	f, err := numKeyword(v, path, key)
	if err != nil {
		return 0, err
	}
	if f < 0 || f != float64(int(f)) {
		return 0, fmt.Errorf("%w: %s/%s must be non-negative integer", ErrInvalidSchema, path, key)
	}
	return int(f), nil
}

// --- runtime validation ---

func (n *schemaNode) validate(doc any, path string, errs *[]error) {
	if n.empty {
		return
	}
	// type
	if len(n.types) > 0 {
		actual := jsonType(doc)
		matched := false
		for _, t := range n.types {
			if typesMatch(t, actual, doc) {
				matched = true
				break
			}
		}
		if !matched {
			*errs = append(*errs, &ValidationError{
				Path:    path,
				Message: fmt.Sprintf("expected type %s, got %s", strings.Join(n.types, "|"), actual),
			})
			// Type mismatch usually invalidates downstream checks
			// — keep going so the caller sees all top-level
			// failures, but the per-type checks below silently
			// no-op when the cast fails.
		}
	}
	if n.cSet {
		if !jsonEqual(doc, n.cVal) {
			*errs = append(*errs, &ValidationError{
				Path:    path,
				Message: fmt.Sprintf("must equal const %v", n.cVal),
			})
		}
	}
	if len(n.enum) > 0 {
		found := false
		for _, e := range n.enum {
			if jsonEqual(doc, e) {
				found = true
				break
			}
		}
		if !found {
			*errs = append(*errs, &ValidationError{
				Path:    path,
				Message: fmt.Sprintf("value %v not in enum", doc),
			})
		}
	}
	switch v := doc.(type) {
	case map[string]any:
		n.validateObject(v, path, errs)
	case []any:
		n.validateArray(v, path, errs)
	case string:
		n.validateString(v, path, errs)
	case float64:
		n.validateNumber(v, path, errs)
	}
}

func (n *schemaNode) validateObject(obj map[string]any, path string, errs *[]error) {
	for _, req := range n.required {
		if _, ok := obj[req]; !ok {
			*errs = append(*errs, &ValidationError{
				Path:    joinPath(path, req),
				Message: "required",
			})
		}
	}
	for k, v := range obj {
		if child, ok := n.properties[k]; ok {
			child.validate(v, joinPath(path, k), errs)
			continue
		}
		if n.additionalProperties != nil && !*n.additionalProperties {
			*errs = append(*errs, &ValidationError{
				Path:    joinPath(path, k),
				Message: "additional property not allowed",
			})
		}
	}
}

func (n *schemaNode) validateArray(arr []any, path string, errs *[]error) {
	if n.minItems != nil && len(arr) < *n.minItems {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("array length %d < minItems %d", len(arr), *n.minItems),
		})
	}
	if n.maxItems != nil && len(arr) > *n.maxItems {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("array length %d > maxItems %d", len(arr), *n.maxItems),
		})
	}
	if n.items != nil {
		for i, e := range arr {
			n.items.validate(e, fmt.Sprintf("%s/%d", path, i), errs)
		}
	}
}

func (n *schemaNode) validateString(s, path string, errs *[]error) {
	// JSON Schema draft 2020-12 defines minLength/maxLength as the number
	// of Unicode code points ("characters"), not UTF-8 byte length. For
	// ASCII-only inputs the two are identical, but anything with multi-
	// byte characters (emoji, CJK, accented Latin) would otherwise see
	// minLength check too lenient and maxLength too strict. utf8.
	// RuneCountInString is the canonical Go equivalent.
	runes := utf8.RuneCountInString(s)
	if n.minLength != nil && runes < *n.minLength {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("string length %d < minLength %d", runes, *n.minLength),
		})
	}
	if n.maxLength != nil && runes > *n.maxLength {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("string length %d > maxLength %d", runes, *n.maxLength),
		})
	}
	if n.pattern != nil && !n.pattern.MatchString(s) {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("string does not match pattern %s", n.pattern.String()),
		})
	}
	if n.format != "" {
		if !checkFormat(n.format, s) {
			*errs = append(*errs, &ValidationError{
				Path:    path,
				Message: fmt.Sprintf("string does not match format %q", n.format),
			})
		}
	}
}

func (n *schemaNode) validateNumber(f float64, path string, errs *[]error) {
	if n.minimum != nil && f < *n.minimum {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("value %v < minimum %v", f, *n.minimum),
		})
	}
	if n.maximum != nil && f > *n.maximum {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("value %v > maximum %v", f, *n.maximum),
		})
	}
	if n.exclusiveMinimum != nil && f <= *n.exclusiveMinimum {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("value %v <= exclusiveMinimum %v", f, *n.exclusiveMinimum),
		})
	}
	if n.exclusiveMaximum != nil && f >= *n.exclusiveMaximum {
		*errs = append(*errs, &ValidationError{
			Path:    path,
			Message: fmt.Sprintf("value %v >= exclusiveMaximum %v", f, *n.exclusiveMaximum),
		})
	}
}

// --- helpers ---

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// typesMatch reports whether a schema-declared type matches the
// actual JSON type. Note the "integer" wart: per draft 2020-12 an
// integer is a number with zero fractional part — the JSON decoder
// always produces float64, so we have to inspect the value.
func typesMatch(declared, actual string, doc any) bool {
	if declared == actual {
		return true
	}
	if declared == "integer" && actual == "number" {
		f, ok := doc.(float64)
		return ok && f == float64(int64(f))
	}
	return false
}

func jsonEqual(a, b any) bool {
	// Use encoding/json round-trip equality: both values are
	// already JSON-parsed maps/slices/scalars, so a recursive
	// reflect.DeepEqual is the canonical comparison.
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func joinPath(parent, key string) string {
	if parent == "" {
		return "/" + key
	}
	return parent + "/" + key
}

func checkFormat(format, s string) bool {
	switch format {
	case "email":
		_, err := mail.ParseAddress(s)
		return err == nil
	case "uri":
		u, err := url.Parse(s)
		return err == nil && u.Scheme != "" && u.Host != ""
	case "uuid":
		_, err := uuid.Parse(s)
		return err == nil
	case "date-time":
		// RFC 3339 with mandatory timezone — the JSON Schema
		// draft 2020-12 spec is explicit that "date-time" is
		// RFC 3339, not the looser ISO 8601.
		_, err := time.Parse(time.RFC3339, s)
		return err == nil
	}
	// Unknown formats are permissive per draft 2020-12 §7.2.
	return true
}
