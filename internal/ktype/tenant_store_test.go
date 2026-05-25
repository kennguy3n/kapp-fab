package ktype

import (
	"encoding/json"
	"strings"
	"testing"
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

// TestIsCustomNameAndPattern pins the namespace gate. The DB CHECK
// and the Go regex must agree — any tightening here implies an
// updated migration.
func TestIsCustomNameAndPattern(t *testing.T) {
	good := []string{
		"custom.asset_register",
		"custom.x",
		"custom.invoice_v2",
	}
	bad := []string{
		"crm.deal",            // not in custom.* namespace
		"custom.",             // empty slug
		"custom.Asset",        // uppercase
		"custom.a-b",          // dash not allowed
		"custom.1asset",       // leading digit
		"custom.nested.ktype", // multi-dot
		"",                    // empty
	}
	for _, n := range good {
		if !IsCustomName(n) {
			t.Errorf("IsCustomName(%q) = false, want true", n)
		}
		if !customNamePattern.MatchString(n) {
			t.Errorf("customNamePattern.Match(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if !strings.HasPrefix(n, "custom.") {
			// IsCustomName returns false for these — that's the
			// namespace short-circuit at the resolver layer.
			continue
		}
		if customNamePattern.MatchString(n) {
			t.Errorf("customNamePattern.Match(%q) = true, want false", n)
		}
	}
}
