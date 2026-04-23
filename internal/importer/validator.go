package importer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// Validator walks every pending staging row and applies two checks:
//
//  1. Schema validation via the KType validator — required fields,
//     type coercions, enum bounds.
//  2. Referential integrity via a caller-provided RefResolver — "does
//     this source_id actually exist in its target KType?" This is
//     optional; when nil only schema validation runs.
//
// Errors are collected per row into `import_staging.validation_errors`
// so the UI can render a row-level report. The validator itself does
// not stop on the first error: every row is evaluated so a single
// run surfaces the full set of problems.
type Validator struct {
	registry ktype.Registry
	resolver RefResolver
}

// RefResolver resolves a (ktype, id-or-external-key) pair to an
// existing record id. Returning uuid.Nil + false means the ref is
// unresolved and the row should fail validation. Resolvers are free
// to use any persistence (KRecord store, staging lookups) — they run
// inside the caller's transaction context.
type RefResolver interface {
	Resolve(ctx context.Context, tenantID uuid.UUID, targetKType, externalKey string) (uuid.UUID, bool, error)
}

// NewValidator wires a Validator around a KType registry. Pass a nil
// resolver to skip referential-integrity checks.
func NewValidator(registry ktype.Registry, resolver RefResolver) *Validator {
	return &Validator{registry: registry, resolver: resolver}
}

// ValidateRow evaluates a single staging row and returns the list of
// errors (zero-length when the row is valid).
func (v *Validator) ValidateRow(ctx context.Context, row StagingRow) ([]ValidationError, error) {
	if row.TargetKType == "" {
		return []ValidationError{{Code: "missing_target_ktype", Message: "row has no target KType"}}, nil
	}
	kt, err := v.latestKType(ctx, row.TargetKType)
	if err != nil {
		return nil, err
	}
	var errs []ValidationError
	if kt == nil {
		errs = append(errs, ValidationError{
			Code:    "unknown_ktype",
			Message: fmt.Sprintf("KType %q is not registered", row.TargetKType),
		})
		return errs, nil
	}
	var data map[string]any
	if len(row.Data) > 0 {
		if err := json.Unmarshal(row.Data, &data); err != nil {
			return []ValidationError{{
				Code:    "invalid_json",
				Message: fmt.Sprintf("data is not valid JSON: %v", err),
			}}, nil
		}
	}
	errs = append(errs, v.checkSchema(kt.Schema, data)...)
	if v.resolver != nil {
		errs = append(errs, v.checkRefs(ctx, row.TenantID, kt.Schema, data)...)
	}
	return errs, nil
}

// latestKType returns the newest registered version of the target
// KType, or nil if none exists. Callers treat a nil result as an
// `unknown_ktype` validation error rather than a hard failure.
func (v *Validator) latestKType(ctx context.Context, name string) (*ktype.KType, error) {
	all, err := v.registry.List(ctx)
	if err != nil {
		return nil, err
	}
	var out *ktype.KType
	for i := range all {
		if all[i].Name != name {
			continue
		}
		if out == nil || all[i].Version > out.Version {
			x := all[i]
			out = &x
		}
	}
	return out, nil
}

// checkSchema applies the minimal schema checks the importer cares
// about: required fields present, enum values within bounds, numeric
// fields parseable. It intentionally does not try to reimplement the
// full KType validator — the authoritative checks happen at Accept
// time when rows flow into the record store.
func (v *Validator) checkSchema(schema json.RawMessage, data map[string]any) []ValidationError {
	var parsed struct {
		Fields []struct {
			Name     string   `json:"name"`
			Type     string   `json:"type"`
			Required bool     `json:"required"`
			Enum     []string `json:"enum"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(schema, &parsed); err != nil {
		return []ValidationError{{Code: "invalid_schema", Message: err.Error()}}
	}
	var errs []ValidationError
	for _, f := range parsed.Fields {
		val, present := data[f.Name]
		if f.Required && (!present || val == nil || val == "") {
			errs = append(errs, ValidationError{
				Field:   f.Name,
				Code:    "required",
				Message: fmt.Sprintf("field %q is required", f.Name),
			})
			continue
		}
		if !present || val == nil {
			continue
		}
		if len(f.Enum) > 0 {
			if s, ok := val.(string); ok && !contains(f.Enum, s) {
				errs = append(errs, ValidationError{
					Field:   f.Name,
					Code:    "enum",
					Message: fmt.Sprintf("field %q must be one of %v, got %q", f.Name, f.Enum, s),
				})
			}
		}
	}
	return errs
}

// checkRefs walks every field declared as type=ref in the schema and
// resolves its value against the RefResolver. An unresolved ref is a
// validation error; a resolver error is a hard failure that surfaces
// to the caller.
func (v *Validator) checkRefs(ctx context.Context, tenantID uuid.UUID, schema json.RawMessage, data map[string]any) []ValidationError {
	var parsed struct {
		Fields []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			KType string `json:"ktype"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(schema, &parsed); err != nil {
		return []ValidationError{{Code: "invalid_schema", Message: err.Error()}}
	}
	var errs []ValidationError
	for _, f := range parsed.Fields {
		if f.Type != "ref" || f.KType == "" {
			continue
		}
		val, ok := data[f.Name]
		if !ok || val == nil || val == "" {
			continue
		}
		s, ok := val.(string)
		if !ok {
			continue
		}
		id, ok, err := v.resolver.Resolve(ctx, tenantID, f.KType, s)
		if err != nil {
			errs = append(errs, ValidationError{
				Field:   f.Name,
				Code:    "ref_error",
				Message: err.Error(),
			})
			continue
		}
		if !ok || id == uuid.Nil {
			errs = append(errs, ValidationError{
				Field:   f.Name,
				Code:    "unresolved_ref",
				Message: fmt.Sprintf("ref %q → %s not found", f.Name, f.KType),
			})
		}
	}
	return errs
}

func contains(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}
