package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/lms"
)

func TestRecommendPaths(t *testing.T) {
	t.Parallel()
	paths := []lms.LearningPath{
		{Title: "Zeta Ops", TargetRoles: []string{"ops"}, EstimatedDurationHours: 10},
		{Title: "Alpha Sales", TargetRoles: []string{"sales"}, EstimatedDurationHours: 8},
		{Title: "Sales Deep Dive", TargetRoles: []string{"sales", "manager"}, EstimatedDurationHours: 20},
		{Title: "Quick Sales Primer", TargetRoles: []string{"SALES"}, EstimatedDurationHours: 4},
	}

	t.Run("role-relevant paths sort first, shorter ranks higher", func(t *testing.T) {
		t.Parallel()
		got := RecommendPaths(paths, "sales", 10)
		if len(got) != 4 {
			t.Fatalf("len = %d, want 4", len(got))
		}
		// Role matches (case-insensitive) come first, ordered by duration asc.
		want := []string{"Quick Sales Primer", "Alpha Sales", "Sales Deep Dive", "Zeta Ops"}
		for i, w := range want {
			if got[i].Title != w {
				t.Fatalf("rank %d = %q, want %q (full=%v)", i, got[i].Title, w, titles(got))
			}
		}
	})

	t.Run("topN truncates", func(t *testing.T) {
		t.Parallel()
		got := RecommendPaths(paths, "sales", 2)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0].Title != "Quick Sales Primer" || got[1].Title != "Alpha Sales" {
			t.Fatalf("got %v", titles(got))
		}
	})

	t.Run("no role: pure duration then title ordering", func(t *testing.T) {
		t.Parallel()
		got := RecommendPaths(paths, "", 10)
		want := []string{"Quick Sales Primer", "Alpha Sales", "Zeta Ops", "Sales Deep Dive"}
		for i, w := range want {
			if got[i].Title != w {
				t.Fatalf("rank %d = %q, want %q (full=%v)", i, got[i].Title, w, titles(got))
			}
		}
	})

	t.Run("does not mutate input", func(t *testing.T) {
		t.Parallel()
		first := paths[0].Title
		_ = RecommendPaths(paths, "sales", 10)
		if paths[0].Title != first {
			t.Fatalf("input slice reordered: %v", titles(paths))
		}
	})
}

func titles(ps []lms.LearningPath) []string {
	out := make([]string, len(ps))
	for i := range ps {
		out[i] = ps[i].Title
	}
	return out
}

// TestLearningPathToolsDryRun verifies the tools validate inputs and
// return a preview without touching the store in dry-run mode (store is
// nil, so any store access would panic).
func TestLearningPathToolsDryRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenant := uuid.New()
	actor := uuid.New()

	t.Run("create dry-run previews without store", func(t *testing.T) {
		t.Parallel()
		tool := &createLearningPathTool{paths: nil}
		in, _ := json.Marshal(createLearningPathInput{Title: "Onboarding"})
		res, err := tool.Invoke(ctx, Invocation{TenantID: tenant, ActorID: actor, Inputs: in, Mode: ModeDryRun})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if res.Preview == nil {
			t.Fatal("expected preview")
		}
	})

	t.Run("create rejects blank title", func(t *testing.T) {
		t.Parallel()
		tool := &createLearningPathTool{paths: nil}
		in, _ := json.Marshal(createLearningPathInput{Title: "  "})
		if _, err := tool.Invoke(ctx, Invocation{TenantID: tenant, ActorID: actor, Inputs: in, Mode: ModeDryRun}); err == nil {
			t.Fatal("expected error for blank title")
		}
	})

	t.Run("enroll dry-run defaults user to actor", func(t *testing.T) {
		t.Parallel()
		tool := &enrollInPathTool{paths: nil}
		in, _ := json.Marshal(enrollInPathInput{LearningPathID: uuid.New()})
		res, err := tool.Invoke(ctx, Invocation{TenantID: tenant, ActorID: actor, Inputs: in, Mode: ModeDryRun})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if res.Preview == nil {
			t.Fatal("expected preview")
		}
	})

	t.Run("enroll requires learning_path_id", func(t *testing.T) {
		t.Parallel()
		tool := &enrollInPathTool{paths: nil}
		in, _ := json.Marshal(enrollInPathInput{})
		if _, err := tool.Invoke(ctx, Invocation{TenantID: tenant, ActorID: actor, Inputs: in, Mode: ModeDryRun}); err == nil {
			t.Fatal("expected error for missing learning_path_id")
		}
	})

	t.Run("commit without store errors cleanly", func(t *testing.T) {
		t.Parallel()
		tool := &createLearningPathTool{paths: nil}
		in, _ := json.Marshal(createLearningPathInput{Title: "X"})
		if _, err := tool.Invoke(ctx, Invocation{TenantID: tenant, ActorID: actor, Inputs: in, Mode: ModeCommit}); err == nil {
			t.Fatal("expected error when store not configured")
		}
	})
}
