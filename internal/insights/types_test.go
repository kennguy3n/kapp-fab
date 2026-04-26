package insights

import (
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/reporting"
)

// TestCacheKeyDeterministic locks in the canonical-JSON contract: the
// same logical (tenant, definition, filter_params) tuple must hash to
// the same query_hash + filter_hash regardless of map iteration
// order or struct field declaration order.
func TestCacheKeyDeterministic(t *testing.T) {
	tenantID := uuid.MustParse("12345678-1234-1234-1234-123456789012")
	def := QueryDefinition{
		Definition: reporting.Definition{
			Source:  "ktype:crm.deal",
			Columns: []string{"name", "amount"},
		},
	}
	filters := map[string]any{"a": 1, "b": "two"}

	q1, f1, err := CacheKey(tenantID, def, filters)
	if err != nil {
		t.Fatalf("CacheKey 1: %v", err)
	}
	q2, f2, err := CacheKey(tenantID, def, filters)
	if err != nil {
		t.Fatalf("CacheKey 2: %v", err)
	}
	if q1 != q2 {
		t.Fatalf("query_hash not deterministic: %q vs %q", q1, q2)
	}
	if f1 != f2 {
		t.Fatalf("filter_hash not deterministic: %q vs %q", f1, f2)
	}
}

// TestCacheKeyDistinguishesFilters guarantees a different filter
// map produces a different filter_hash. Otherwise two parameterized
// runs would collide and serve the wrong rows.
func TestCacheKeyDistinguishesFilters(t *testing.T) {
	tenantID := uuid.MustParse("12345678-1234-1234-1234-123456789012")
	def := QueryDefinition{
		Definition: reporting.Definition{Source: "ktype:crm.deal", Columns: []string{"name"}},
	}
	_, fA, _ := CacheKey(tenantID, def, map[string]any{"x": 1})
	_, fB, _ := CacheKey(tenantID, def, map[string]any{"x": 2})
	if fA == fB {
		t.Fatalf("filter_hash collided across different filter values: %q", fA)
	}
}

// TestQueryDefinitionValidate rejects calculated columns missing a
// name or expression and duplicate names.
func TestQueryDefinitionValidate(t *testing.T) {
	base := reporting.Definition{Source: "ktype:crm.deal", Columns: []string{"name"}}
	cases := []struct {
		name    string
		def     QueryDefinition
		wantErr bool
	}{
		{name: "valid no calc", def: QueryDefinition{Definition: base}, wantErr: false},
		{name: "calc missing name", def: QueryDefinition{Definition: base, CalculatedColumns: []CalculatedColumn{{Expression: "a + b"}}}, wantErr: true},
		{name: "calc missing expr", def: QueryDefinition{Definition: base, CalculatedColumns: []CalculatedColumn{{Name: "x"}}}, wantErr: true},
		{name: "calc duplicate", def: QueryDefinition{Definition: base, CalculatedColumns: []CalculatedColumn{{Name: "x", Expression: "1"}, {Name: "x", Expression: "2"}}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.def.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateVizType rejects unknown viz strings so the JSONB row
// never holds a value the frontend doesn't render.
func TestValidateVizType(t *testing.T) {
	for _, ok := range []string{VizTable, VizBar, VizLine, VizPie, VizDonut, VizFunnel, VizNumberCard, VizPivot} {
		if err := ValidateVizType(ok); err != nil {
			t.Fatalf("ValidateVizType(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "scatter", "heatmap"} {
		if err := ValidateVizType(bad); err == nil {
			t.Fatalf("ValidateVizType(%q) = nil; want error", bad)
		}
	}
}
