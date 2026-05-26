package sales

import (
	"encoding/json"
	"testing"
)

// TestReturnKTypesParse asserts the sales.return schema is valid
// JSON and the embedded `name`/`version` match the registry-side
// constants. Catches accidental drift between the schema heredoc
// and the exported KTypeSalesReturn identifier.
func TestReturnKTypesParse(t *testing.T) {
	for _, kt := range ReturnKTypes() {
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

// TestReturnKTypesFreshSlice verifies ReturnKTypes() returns a fresh
// slice on each call so a downstream caller mutating one entry
// can't leak into a sibling.
func TestReturnKTypesFreshSlice(t *testing.T) {
	a := ReturnKTypes()
	b := ReturnKTypes()
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("ReturnKTypes() returned empty slice")
	}
	if &a[0] == &b[0] {
		t.Fatal("ReturnKTypes() returned the same backing slice across calls")
	}
}

// TestReturnSchemaShape pins the on-wire field set the poster +
// builders read. Drift here (e.g. dropping `warehouse_id` or
// renaming `credit_note_id`) silently breaks the ReturnPoster
// because the JSONB payload is the only contract between the API
// surface and the state-machine.
func TestReturnSchemaShape(t *testing.T) {
	var schema struct {
		Name   string `json:"name"`
		Fields []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
		} `json:"fields"`
		Workflow struct {
			InitialState string   `json:"initial_state"`
			States       []string `json:"states"`
			Transitions  []struct {
				From   []string `json:"from"`
				To     string   `json:"to"`
				Action string   `json:"action"`
			} `json:"transitions"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(salesReturnSchema, &schema); err != nil {
		t.Fatalf("schema invalid JSON: %v", err)
	}
	want := map[string]struct {
		ty       string
		required bool
	}{
		"original_invoice_id": {"ref", true},
		"customer_id":         {"ref", true},
		"warehouse_id":        {"ref", true},
		"return_date":         {"date", true},
		"total":               {"number", true},
		"status":              {"enum", false},
		"credit_note_id":      {"string", false},
		"journal_entry_id":    {"string", false},
	}
	got := map[string]struct {
		ty       string
		required bool
	}{}
	for _, f := range schema.Fields {
		got[f.Name] = struct {
			ty       string
			required bool
		}{f.Type, f.Required}
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("schema is missing field %q", name)
			continue
		}
		if g.ty != w.ty {
			t.Errorf("field %q: type=%q, want %q", name, g.ty, w.ty)
		}
		if g.required != w.required {
			t.Errorf("field %q: required=%v, want %v", name, g.required, w.required)
		}
	}
	if schema.Workflow.InitialState != ReturnStatusRequested {
		t.Errorf("initial_state=%q, want %q", schema.Workflow.InitialState, ReturnStatusRequested)
	}
	wantStates := map[string]bool{
		ReturnStatusRequested: true,
		ReturnStatusApproved:  true,
		ReturnStatusReceived:  true,
		ReturnStatusRefunded:  true,
		ReturnStatusCancelled: true,
	}
	for _, s := range schema.Workflow.States {
		delete(wantStates, s)
	}
	if len(wantStates) > 0 {
		t.Errorf("workflow states missing: %v", wantStates)
	}
	wantTransitions := map[string]bool{
		"approve": true,
		"receive": true,
		"refund":  true,
		"cancel":  true,
	}
	for _, tr := range schema.Workflow.Transitions {
		delete(wantTransitions, tr.Action)
	}
	if len(wantTransitions) > 0 {
		t.Errorf("workflow missing transitions: %v", wantTransitions)
	}
}

// TestReturnSchemaAgentTools pins the `agent_tools` array on the
// sales.return schema to the full set of tools `RegisterSalesReturnsTools`
// actually wires into the executor. Any consumer that reads this
// schema field to discover/display the available agent tools for the
// KType (e.g. a future "what can the assistant do here?" surface)
// drives off a single source of truth — so the array must include
// every transition, not just the forward path.
func TestReturnSchemaAgentTools(t *testing.T) {
	var schema struct {
		AgentTools []string `json:"agent_tools"`
	}
	if err := json.Unmarshal(salesReturnSchema, &schema); err != nil {
		t.Fatalf("schema invalid JSON: %v", err)
	}
	want := map[string]bool{
		"sales.create_return":  true,
		"sales.approve_return": true,
		"sales.receive_return": true,
		"sales.refund_return":  true,
		"sales.cancel_return":  true,
	}
	got := map[string]bool{}
	for _, name := range schema.AgentTools {
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("schema agent_tools is missing %q", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("schema agent_tools advertises unknown tool %q", name)
		}
	}
}
