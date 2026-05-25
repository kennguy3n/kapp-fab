package ktype

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// schema returns a json.RawMessage built from the supplied field
// specs. Keeps the table-driven tests below readable without
// repeating the marshal-and-check boilerplate.
func schemaWith(fields []map[string]any) json.RawMessage {
	body := map[string]any{
		"name":    "custom.thing",
		"version": 1,
		"fields":  fields,
	}
	b, _ := json.Marshal(body)
	return b
}

// TestValidateCustomSchemaRejectsHostileSections pins the safe-subset
// rule for tenant-authored KTypes. None of the developer-only
// surface areas (posting hooks, computed fields, custom agent
// tools, triggers) may sneak into a custom schema. Each case must
// fail with a precise error so the API can return 400 with a
// useful message.
func TestValidateCustomSchemaRejectsHostileSections(t *testing.T) {
	s := NewTenantStore(nil)
	cases := []struct {
		name    string
		schema  string
		wantSub string
	}{
		{
			name:    "posting_hook",
			schema:  `{"name":"custom.x","fields":[{"name":"f","type":"string"}],"posting_hook":{"go":"package main"}}`,
			wantSub: "posting_hook",
		},
		{
			name:    "computed",
			schema:  `{"name":"custom.x","fields":[{"name":"f","type":"string"}],"computed":{"expr":"a+b"}}`,
			wantSub: "computed",
		},
		{
			name:    "agent_tools",
			schema:  `{"name":"custom.x","fields":[{"name":"f","type":"string"}],"agent_tools":[{"name":"create"}]}`,
			wantSub: "agent_tools",
		},
		{
			name:    "triggers",
			schema:  `{"name":"custom.x","fields":[{"name":"f","type":"string"}],"triggers":[{"on":"create"}]}`,
			wantSub: "triggers",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.validateCustomSchema(json.RawMessage(c.schema))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("want error containing %q, got %q", c.wantSub, err.Error())
			}
		})
	}
}

// TestValidateCustomSchemaRejectsUnsafeFieldTypes pins the field-
// type allow-list. The closed set is enforced via SafeCustomFieldTypes;
// the test fails fast on regressions if a new safe-looking type is
// added to the validator's switch without being added to the
// allow-list (which is the supported way to keep object/array out
// of low-code schemas).
func TestValidateCustomSchemaRejectsUnsafeFieldTypes(t *testing.T) {
	s := NewTenantStore(nil)
	for _, ft := range []string{"object", "array", "blob", "executable", "function"} {
		t.Run(ft, func(t *testing.T) {
			err := s.validateCustomSchema(schemaWith([]map[string]any{
				{"name": "f", "type": ft},
			}))
			if err == nil {
				t.Fatalf("type %q must be rejected as unsafe", ft)
			}
			if !strings.Contains(err.Error(), "unsupported field type") {
				t.Fatalf("type %q: want ErrUnsupportedFieldType, got %v", ft, err)
			}
		})
	}
}

// TestValidateCustomSchemaAcceptsSafeFieldTypes is the positive
// pin — every type in SafeCustomFieldTypes must round-trip the
// validator. Catches the case where the map and the switch get out
// of sync (e.g. someone removes a case from the validator but
// leaves it in the allow-list).
func TestValidateCustomSchemaAcceptsSafeFieldTypes(t *testing.T) {
	s := NewTenantStore(nil)
	for ft := range SafeCustomFieldTypes {
		t.Run(ft, func(t *testing.T) {
			f := map[string]any{"name": "f", "type": ft}
			if ft == "enum" {
				f["values"] = []string{"a", "b"}
			}
			if ft == "ref" {
				f["ref"] = "crm.account"
			}
			if err := s.validateCustomSchema(schemaWith([]map[string]any{f})); err != nil {
				t.Fatalf("safe type %q must validate, got %v", ft, err)
			}
		})
	}
}

// TestValidateCustomSchemaEnforcesFieldLimit pins the per-tenant
// field-count cap. The default is 50; the test bumps it to 3 via
// WithFieldLimit so the assertion is fast and obvious.
func TestValidateCustomSchemaEnforcesFieldLimit(t *testing.T) {
	s := NewTenantStore(nil, WithFieldLimit(3))
	fields := make([]map[string]any, 0, 4)
	for i := 0; i < 4; i++ {
		fields = append(fields, map[string]any{"name": "f", "type": "string"})
	}
	err := s.validateCustomSchema(schemaWith(fields))
	if err == nil {
		t.Fatalf("4-field schema must exceed the 3-field cap")
	}
	if !strings.Contains(err.Error(), "exceeds limit of 3") {
		t.Fatalf("want field-limit error, got %v", err)
	}

	// Boundary: exactly the limit succeeds.
	fields = fields[:3]
	if err := s.validateCustomSchema(schemaWith(fields)); err != nil {
		t.Fatalf("3-field schema must succeed at limit 3, got %v", err)
	}
}

// TestValidateCustomSchemaEnforcesEnumAndRefShape checks the per-
// field requirements that the SQL CHECK doesn't catch: enum without
// values, ref without target. Both must fail before INSERT so the
// builder UI gets a precise inline error.
func TestValidateCustomSchemaEnforcesEnumAndRefShape(t *testing.T) {
	s := NewTenantStore(nil)
	t.Run("enum without values", func(t *testing.T) {
		err := s.validateCustomSchema(schemaWith([]map[string]any{
			{"name": "status", "type": "enum"},
		}))
		if err == nil || !strings.Contains(err.Error(), "requires values") {
			t.Fatalf("enum without values must fail, got %v", err)
		}
	})
	t.Run("ref without target", func(t *testing.T) {
		err := s.validateCustomSchema(schemaWith([]map[string]any{
			{"name": "linked", "type": "ref"},
		}))
		if err == nil || !strings.Contains(err.Error(), "ref ktype") {
			t.Fatalf("ref without target must fail, got %v", err)
		}
	})
}

// TestIsCustomNameAndPattern pins the namespace gate. IsCustomName
// is the loose prefix-only routing check; IsValidCustomName is the
// strict input-validation check that must agree with both the DB
// CHECK constraint `tenant_ktypes_name_chk` and Upsert's regex.
// Any tightening here implies an updated migration.
func TestIsCustomNameAndPattern(t *testing.T) {
	good := []string{
		"custom.asset_register",
		"custom.x",
		"custom.invoice_v2",
	}
	// Malformed names that are in the custom.* namespace but
	// don't satisfy the full pattern. IsCustomName MUST still
	// return true for these (so resolveKType routes them to the
	// tenant store, which then returns a precise 400 instead of
	// a confused 404 from the platform registry). IsValidCustomName
	// MUST return false so Get / SetStatus / Upsert reject them
	// before any DB round-trip.
	malformedCustom := []string{
		"custom.",             // empty slug
		"custom.Asset",        // uppercase
		"custom.a-b",          // dash not allowed
		"custom.1asset",       // leading digit
		"custom.nested.ktype", // multi-dot
	}
	// Names outside the custom.* namespace entirely. Both
	// predicates return false.
	nonCustom := []string{
		"crm.deal",
		"",
	}
	for _, n := range good {
		if !IsCustomName(n) {
			t.Errorf("IsCustomName(%q) = false, want true", n)
		}
		if !IsValidCustomName(n) {
			t.Errorf("IsValidCustomName(%q) = false, want true", n)
		}
		if !customNamePattern.MatchString(n) {
			t.Errorf("customNamePattern.Match(%q) = false, want true", n)
		}
	}
	for _, n := range malformedCustom {
		if !IsCustomName(n) {
			t.Errorf("IsCustomName(%q) = false, want true (prefix-only routing must still match)", n)
		}
		if IsValidCustomName(n) {
			t.Errorf("IsValidCustomName(%q) = true, want false (full pattern must reject malformed names)", n)
		}
		if customNamePattern.MatchString(n) {
			t.Errorf("customNamePattern.Match(%q) = true, want false", n)
		}
	}
	for _, n := range nonCustom {
		if IsCustomName(n) {
			t.Errorf("IsCustomName(%q) = true, want false", n)
		}
		if IsValidCustomName(n) {
			t.Errorf("IsValidCustomName(%q) = true, want false", n)
		}
	}
}

// TestGetAndSetStatusRejectMalformedNames pins that the read-path
// input validation matches Upsert's contract — a name like
// `custom.UPPER` returns ErrInvalidCustomName (HTTP 400) before any
// DB round-trip, NOT ErrNotFound (HTTP 404) from a missing row. The
// distinction matters because the builder UI surfaces "invalid name"
// vs "not found" differently, and scripted callers rely on the
// 400/404 split to retry vs. abort.
//
// Both methods take a real *pgxpool.Pool in production but the
// validation we're testing happens before any DB call, so a nil
// pool is fine here.
func TestGetAndSetStatusRejectMalformedNames(t *testing.T) {
	s := NewTenantStore(nil)
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	malformed := []string{
		"custom.",
		"custom.UPPER",
		"custom.a-b",
		"custom.1asset",
		"custom.nested.ktype",
	}
	for _, n := range malformed {
		t.Run("Get/"+n, func(t *testing.T) {
			_, err := s.Get(t.Context(), tenantID, n, 0)
			if !errors.Is(err, ErrInvalidCustomName) {
				t.Errorf("Get(%q) returned %v, want ErrInvalidCustomName", n, err)
			}
		})
		t.Run("SetStatus/"+n, func(t *testing.T) {
			err := s.SetStatus(t.Context(), tenantID, n, 1, CustomStatusActive)
			if !errors.Is(err, ErrInvalidCustomName) {
				t.Errorf("SetStatus(%q) returned %v, want ErrInvalidCustomName", n, err)
			}
		})
	}
}
