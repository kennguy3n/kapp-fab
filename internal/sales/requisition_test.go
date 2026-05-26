package sales

import (
	"encoding/json"
	"testing"
)

// TestRequisitionKTypesParse asserts the
// procurement.purchase_requisition schema is valid JSON and the
// embedded `name`/`version` match the registry-side constants.
// Catches accidental drift between the schema heredoc and the
// exported KTypePurchaseRequisition identifier.
func TestRequisitionKTypesParse(t *testing.T) {
	for _, kt := range PurchaseRequisitionKTypes() {
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

// TestRequisitionKTypesFreshSlice verifies PurchaseRequisitionKTypes()
// returns a fresh slice on each call so a downstream caller
// mutating one entry can't leak into a sibling.
func TestRequisitionKTypesFreshSlice(t *testing.T) {
	a := PurchaseRequisitionKTypes()
	b := PurchaseRequisitionKTypes()
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("PurchaseRequisitionKTypes() returned empty slice")
	}
	if &a[0] == &b[0] {
		t.Fatal("PurchaseRequisitionKTypes() returned the same backing slice across calls")
	}
}

// TestRequisitionSchemaShape pins the on-wire field set the poster
// + builders read. Drift here (e.g. dropping `requested_by` or
// renaming `po_id`) silently breaks the RequisitionPoster because
// the JSONB payload is the only contract between the API surface
// and the state-machine.
func TestRequisitionSchemaShape(t *testing.T) {
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
	if err := json.Unmarshal(purchaseRequisitionSchema, &schema); err != nil {
		t.Fatalf("schema invalid JSON: %v", err)
	}
	want := map[string]struct {
		ty       string
		required bool
	}{
		"requested_by": {"ref", true},
		"request_date": {"date", true},
		"supplier_id":  {"ref", false},
		"status":       {"enum", false},
		"po_id":        {"ref", false},
		"approved_by":  {"ref", false},
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
	if schema.Workflow.InitialState != RequisitionStatusRequested {
		t.Errorf("initial_state=%q, want %q", schema.Workflow.InitialState, RequisitionStatusRequested)
	}
	wantStates := map[string]bool{
		RequisitionStatusRequested: true,
		RequisitionStatusApproved:  true,
		RequisitionStatusOrdered:   true,
		RequisitionStatusCancelled: true,
	}
	for _, s := range schema.Workflow.States {
		delete(wantStates, s)
	}
	if len(wantStates) > 0 {
		t.Errorf("workflow states missing: %v", wantStates)
	}
	wantTransitions := map[string]bool{
		"approve": true,
		"convert": true,
		"cancel":  true,
	}
	for _, tr := range schema.Workflow.Transitions {
		delete(wantTransitions, tr.Action)
	}
	if len(wantTransitions) > 0 {
		t.Errorf("workflow missing transitions: %v", wantTransitions)
	}
}

// TestRequisitionSchemaAgentTools pins the `agent_tools` array on
// the procurement.purchase_requisition schema to the full set of
// tools `RegisterRequisitionTools` actually wires into the executor.
func TestRequisitionSchemaAgentTools(t *testing.T) {
	var schema struct {
		AgentTools []string `json:"agent_tools"`
	}
	if err := json.Unmarshal(purchaseRequisitionSchema, &schema); err != nil {
		t.Fatalf("schema invalid JSON: %v", err)
	}
	want := map[string]bool{
		"procurement.create_requisition":        true,
		"procurement.approve_requisition":       true,
		"procurement.convert_requisition_to_po": true,
		"procurement.cancel_requisition":        true,
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

// TestRequisitionRegisteredInAll asserts the requisition KType is
// folded into sales.All() so the platform bootstrap picks it up
// alongside sales orders and POs.
func TestRequisitionRegisteredInAll(t *testing.T) {
	for _, kt := range All() {
		if kt.Name == KTypePurchaseRequisition {
			return
		}
	}
	t.Errorf("All() does not include %q — bootstrap will skip registration", KTypePurchaseRequisition)
}
