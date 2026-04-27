package projects

import (
	"encoding/json"
	"testing"
)

// TestAllSchemasParse asserts each embedded KType schema parses
// as JSON and carries the canonical name expected by the agent
// tools / migrations / KType registry. Catches accidental
// breakage on edits to the heredoc literals before the init()
// validator runs.
func TestAllSchemasParse(t *testing.T) {
	for _, kt := range All() {
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

// TestAllReturnsFreshSlice — registers should not share backing
// arrays so a downstream mutation can't leak into a sibling
// caller. Mirrors the pattern in internal/hr.
func TestAllReturnsFreshSlice(t *testing.T) {
	a := All()
	b := All()
	if &a[0] == &b[0] {
		t.Fatal("All() returned the same backing slice across calls")
	}
}
