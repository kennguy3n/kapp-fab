package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/lms"
)

// RegisterLearningPathTools wires the Session-17 learning-path agent
// tools onto an executor. A nil store is tolerated — calls return a
// clear error instead of panicking — so harnesses that don't build the
// LMS surface keep working.
func RegisterLearningPathTools(x *Executor, paths *lms.LearningPathStore) {
	x.Register(&createLearningPathTool{paths: paths})
	x.Register(&enrollInPathTool{paths: paths})
	x.Register(&recommendLearningPathTool{paths: paths})
}

// ----- lms.create_learning_path -----

type createLearningPathInput struct {
	Title                  string   `json:"title"`
	Description            string   `json:"description,omitempty"`
	Status                 string   `json:"status,omitempty"`
	TargetRoles            []string `json:"target_roles,omitempty"`
	EstimatedDurationHours int      `json:"estimated_duration_hours,omitempty"`
	Difficulty             string   `json:"difficulty,omitempty"`
}

type createLearningPathTool struct{ paths *lms.LearningPathStore }

func (t *createLearningPathTool) Name() string               { return "lms.create_learning_path" }
func (t *createLearningPathTool) RequiresConfirmation() bool { return true }
func (t *createLearningPathTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createLearningPathInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("lms.create_learning_path: title required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would create learning path %q", in.Title),
			Preview: preview,
		}, nil
	}
	if t.paths == nil {
		return nil, errors.New("lms.create_learning_path: learning path store not configured")
	}
	actor := inv.ActorID
	path, err := t.paths.CreatePath(ctx, lms.LearningPath{
		TenantID:               inv.TenantID,
		Title:                  in.Title,
		Description:            in.Description,
		Status:                 in.Status,
		TargetRoles:            in.TargetRoles,
		EstimatedDurationHours: in.EstimatedDurationHours,
		Difficulty:             in.Difficulty,
		CreatedBy:              &actor,
	})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(path)
	return &Result{
		Summary: fmt.Sprintf("Created learning path %q (%s)", path.Title, path.ID),
		Preview: body,
		Extra:   map[string]any{"learning_path_id": path.ID},
	}, nil
}

// ----- lms.enroll_in_path -----

type enrollInPathInput struct {
	LearningPathID uuid.UUID `json:"learning_path_id"`
	UserID         uuid.UUID `json:"user_id,omitempty"`
}

type enrollInPathTool struct{ paths *lms.LearningPathStore }

func (t *enrollInPathTool) Name() string               { return "lms.enroll_in_path" }
func (t *enrollInPathTool) RequiresConfirmation() bool { return true }
func (t *enrollInPathTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in enrollInPathInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.LearningPathID == uuid.Nil {
		return nil, errors.New("lms.enroll_in_path: learning_path_id required")
	}
	userID := in.UserID
	if userID == uuid.Nil {
		userID = inv.ActorID
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would enroll %s in learning path %s", userID, in.LearningPathID),
			Preview: preview,
		}, nil
	}
	if t.paths == nil {
		return nil, errors.New("lms.enroll_in_path: learning path store not configured")
	}
	actor := inv.ActorID
	enr, err := t.paths.Enroll(ctx, inv.TenantID, in.LearningPathID, userID, lms.EnrollSourceManual, &actor)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(enr)
	return &Result{
		Summary: fmt.Sprintf("Enrolled %s in learning path %s", userID, in.LearningPathID),
		Preview: body,
		Extra:   map[string]any{"learning_path_id": in.LearningPathID, "user_id": userID},
	}, nil
}

// ----- lms.recommend_learning_path -----

type recommendLearningPathInput struct {
	Role string `json:"role,omitempty"`
	TopN int    `json:"top_n,omitempty"`
}

type recommendLearningPathTool struct{ paths *lms.LearningPathStore }

func (t *recommendLearningPathTool) Name() string               { return "lms.recommend_learning_path" }
func (t *recommendLearningPathTool) RequiresConfirmation() bool { return false }
func (t *recommendLearningPathTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in recommendLearningPathInput
	if len(inv.Inputs) > 0 {
		if err := json.Unmarshal(inv.Inputs, &in); err != nil {
			return nil, fmt.Errorf("lms.recommend_learning_path: decode inputs: %w", err)
		}
	}
	if in.TopN <= 0 || in.TopN > 20 {
		in.TopN = 5
	}
	if t.paths == nil {
		return nil, errors.New("lms.recommend_learning_path: learning path store not configured")
	}
	// Only published paths are recommendable — draft/archived paths are
	// not learner-facing.
	paths, err := t.paths.ListPaths(ctx, inv.TenantID, lms.PathStatusPublished)
	if err != nil {
		return nil, err
	}
	recs := RecommendPaths(paths, in.Role, in.TopN)
	body, _ := json.Marshal(recs)
	return &Result{
		Summary: fmt.Sprintf("Recommended %d learning paths", len(recs)),
		Preview: body,
	}, nil
}

// RecommendPaths is the pure ranking rule behind
// lms.recommend_learning_path: when a role is supplied, paths whose
// target_roles include it sort first (role-relevant), then the rest;
// within each group the shorter (lower estimated_duration_hours) path
// ranks higher so a learner gets an achievable next step. Ties break on
// title for deterministic output. Exposed (and unit-tested) separately
// from the store so the ranking can be verified without a database.
func RecommendPaths(paths []lms.LearningPath, role string, topN int) []lms.LearningPath {
	role = strings.TrimSpace(strings.ToLower(role))
	matches := func(p lms.LearningPath) bool {
		if role == "" {
			return false
		}
		for _, r := range p.TargetRoles {
			if strings.EqualFold(strings.TrimSpace(r), role) {
				return true
			}
		}
		return false
	}
	out := make([]lms.LearningPath, len(paths))
	copy(out, paths)
	sort.SliceStable(out, func(i, j int) bool {
		mi, mj := matches(out[i]), matches(out[j])
		if mi != mj {
			return mi // role-relevant first
		}
		if out[i].EstimatedDurationHours != out[j].EstimatedDurationHours {
			return out[i].EstimatedDurationHours < out[j].EstimatedDurationHours
		}
		return out[i].Title < out[j].Title
	})
	if topN < len(out) {
		out = out[:topN]
	}
	return out
}
