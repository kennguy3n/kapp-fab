package main

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// TestWorkOrderActionRequestCompleteInputForwardsTracking pins the
// lot/serial passthrough on the /complete handler. The handler decodes
// the JSON body into workOrderActionRequest and must forward every
// tracking field to the store via completeInput(); a regression where
// only ActualQty is forwarded makes the entire HTTP surface for
// completing lot-/serial-tracked work orders non-functional (the store's
// Phase-1 validateTrackedMove rejects with ErrSerialRequired/
// ErrLotRequired even though the client supplied the data). This test
// decodes a representative body and asserts each field survives the trip
// onto CompleteWorkOrderInput, so the drop can never silently reappear.
func TestWorkOrderActionRequestCompleteInputForwardsTracking(t *testing.T) {
	t.Parallel()

	finBatch := uuid.New()
	compA := uuid.New()
	compB := uuid.New()
	compABatch := uuid.New()

	body := `{
		"actual_qty": "42.5",
		"finished_batch_id": "` + finBatch.String() + `",
		"finished_serials": ["FG1", "FG2"],
		"component_batches": {"` + compA.String() + `": "` + compABatch.String() + `"},
		"component_serials": {"` + compB.String() + `": ["CB1", "CB2", "CB3"]}
	}`

	var req workOrderActionRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	in := req.completeInput()

	if !in.ActualQty.Equal(decimal.RequireFromString("42.5")) {
		t.Errorf("ActualQty: want 42.5, got %s", in.ActualQty)
	}
	if in.FinishedBatchID == nil || *in.FinishedBatchID != finBatch {
		t.Errorf("FinishedBatchID: want %s, got %v", finBatch, in.FinishedBatchID)
	}
	if got := in.FinishedSerials; len(got) != 2 || got[0] != "FG1" || got[1] != "FG2" {
		t.Errorf("FinishedSerials: want [FG1 FG2], got %v", got)
	}
	if got, ok := in.ComponentBatches[compA]; !ok || got != compABatch {
		t.Errorf("ComponentBatches[%s]: want %s, got %v (ok=%v)", compA, compABatch, got, ok)
	}
	if got := in.ComponentSerials[compB]; len(got) != 3 || got[0] != "CB1" || got[2] != "CB3" {
		t.Errorf("ComponentSerials[%s]: want [CB1 CB2 CB3], got %v", compB, got)
	}
}

// TestWorkOrderActionRequestCompleteInputEmptyBody confirms the common
// "complete with actual = planned" path: an empty request leaves every
// tracking field nil so untracked items complete unchanged.
func TestWorkOrderActionRequestCompleteInputEmptyBody(t *testing.T) {
	t.Parallel()

	var req workOrderActionRequest
	in := req.completeInput()

	if in.FinishedBatchID != nil {
		t.Errorf("FinishedBatchID: want nil, got %v", in.FinishedBatchID)
	}
	if in.FinishedSerials != nil {
		t.Errorf("FinishedSerials: want nil, got %v", in.FinishedSerials)
	}
	if in.ComponentBatches != nil {
		t.Errorf("ComponentBatches: want nil, got %v", in.ComponentBatches)
	}
	if in.ComponentSerials != nil {
		t.Errorf("ComponentSerials: want nil, got %v", in.ComponentSerials)
	}
}
