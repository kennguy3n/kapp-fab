package sales

import (
	"encoding/json"
	"testing"
)

// TestPOSKTypesParse asserts each POS schema is valid JSON and
// the embedded `name` matches the registry-side constant. Catches
// accidental drift between the schema heredoc and the exported
// KTypePOSProfile / KTypePOSInvoice identifiers.
func TestPOSKTypesParse(t *testing.T) {
	for _, kt := range POSKTypes() {
		var got struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
		}
		if err := json.Unmarshal(kt.Schema, &got); err != nil {
			t.Fatalf("%s: schema invalid JSON: %v", kt.Name, err)
		}
		if got.Name != kt.Name {
			t.Fatalf("%s: schema name=%q does not match registry name", kt.Name, got.Name)
		}
		if got.Version != kt.Version {
			t.Fatalf("%s: schema version=%d, registry version=%d", kt.Name, got.Version, kt.Version)
		}
	}
}

// TestPOSKTypesFreshSlice verifies POSKTypes() returns a fresh
// slice on each call so a downstream caller mutating one entry
// can't leak into a sibling.
func TestPOSKTypesFreshSlice(t *testing.T) {
	a := POSKTypes()
	b := POSKTypes()
	if &a[0] == &b[0] {
		t.Fatal("POSKTypes() returned the same backing slice across calls")
	}
}
