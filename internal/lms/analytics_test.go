package lms

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestComputeCourseAnalytics(t *testing.T) {
	course := uuid.New()
	l1, l2 := uuid.New(), uuid.New()
	e1, e2, e3 := uuid.New(), uuid.New(), uuid.New()
	u1, u2, u3 := uuid.New(), uuid.New(), uuid.New()

	enrollments := []EnrollmentRow{
		{EnrollmentID: e1, UserID: u1, Status: "completed"},
		{EnrollmentID: e2, UserID: u2, Status: "in_progress"},
		{EnrollmentID: e3, UserID: u3, Status: "enrolled"},
	}
	s90 := decimal.NewFromInt(90)
	s70 := decimal.NewFromInt(70)
	progress := []ProgressRow{
		{EnrollmentID: e1, LessonID: l1, Status: ProgressCompleted, Score: &s90},
		{EnrollmentID: e1, LessonID: l2, Status: ProgressCompleted, Score: &s70},
		{EnrollmentID: e2, LessonID: l1, Status: ProgressCompleted},
		{EnrollmentID: e2, LessonID: l2, Status: ProgressInProgress},
		// e3 never started any lesson.
	}

	a := ComputeCourseAnalytics(course, enrollments, progress, []uuid.UUID{l1, l2})

	if a.EnrollmentCount != 3 || a.CompletedCount != 1 {
		t.Fatalf("counts wrong: %+v", a)
	}
	if a.CompletionRate < 0.33 || a.CompletionRate > 0.34 {
		t.Fatalf("completion rate = %f, want ~0.333", a.CompletionRate)
	}
	if a.AverageScore == nil || !a.AverageScore.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("avg score = %v, want 80", a.AverageScore)
	}

	// Drop-off: l1 reached by e1,e2 (2), completed by both (2) => 0 drop.
	// l2 reached by e1,e2 (2), completed by e1 only (1) => 0.5 drop.
	dropByLesson := map[uuid.UUID]LessonDropOff{}
	for _, d := range a.LessonDropOff {
		dropByLesson[d.LessonID] = d
	}
	if d := dropByLesson[l1]; d.Reached != 2 || d.Completed != 2 || d.DropOffRate != 0 {
		t.Fatalf("l1 drop-off wrong: %+v", d)
	}
	if d := dropByLesson[l2]; d.Reached != 2 || d.Completed != 1 || d.DropOffRate != 0.5 {
		t.Fatalf("l2 drop-off wrong: %+v", d)
	}

	if len(a.PerLearner) != 3 {
		t.Fatalf("want 3 learners, got %d", len(a.PerLearner))
	}
	// Find e1 learner row.
	var found bool
	for _, lp := range a.PerLearner {
		if lp.EnrollmentID == e1 {
			found = true
			if lp.LessonsCompleted != 2 || lp.LessonsTotal != 2 {
				t.Fatalf("e1 lessons wrong: %+v", lp)
			}
			if lp.AverageScore == nil || !lp.AverageScore.Equal(decimal.NewFromInt(80)) {
				t.Fatalf("e1 avg = %v, want 80", lp.AverageScore)
			}
		}
	}
	if !found {
		t.Fatal("e1 learner row missing")
	}
}

func TestComputeCourseAnalyticsEmpty(t *testing.T) {
	a := ComputeCourseAnalytics(uuid.New(), nil, nil, nil)
	if a.EnrollmentCount != 0 || a.CompletionRate != 0 || a.AverageScore != nil {
		t.Fatalf("empty analytics wrong: %+v", a)
	}
	// The slice fields must be non-nil empty slices so they marshal as
	// `[]` (not `null`) — the TS dashboard accesses `.length` directly.
	if a.LessonDropOff == nil || a.PerLearner == nil {
		t.Fatalf("slice fields must be non-nil: %+v", a)
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if s := string(b); strings.Contains(s, `"lesson_drop_off":null`) || strings.Contains(s, `"per_learner":null`) {
		t.Fatalf("empty analytics marshaled a null slice: %s", s)
	}
}

func TestComputeQuizDistribution(t *testing.T) {
	scores := []decimal.Decimal{
		decimal.NewFromInt(100),
		decimal.NewFromInt(95),
		decimal.NewFromInt(80),
		decimal.NewFromInt(55),
		decimal.NewFromInt(0),
		decimal.NewFromInt(-5),  // clamped to 0
		decimal.NewFromInt(150), // clamped to 100
	}
	d := ComputeQuizDistribution(scores, decimal.NewFromInt(60))
	if d.Total != 7 {
		t.Fatalf("total = %d, want 7", d.Total)
	}
	// passed: 100,95,80 and clamped 150 (>=60) => 4; failed: 55,0,-5 => 3.
	if d.Passed != 4 || d.Failed != 3 {
		t.Fatalf("pass/fail = %d/%d, want 4/3", d.Passed, d.Failed)
	}
	if d.PassRate < 0.57 || d.PassRate > 0.58 {
		t.Fatalf("pass rate = %f", d.PassRate)
	}
	// bucket 10 catches exact 100 plus clamped 150 => 2.
	if d.Buckets[10] != 2 {
		t.Fatalf("bucket[10] = %d, want 2", d.Buckets[10])
	}
	// bucket 0 catches 0 and clamped -5 => 2.
	if d.Buckets[0] != 2 {
		t.Fatalf("bucket[0] = %d, want 2", d.Buckets[0])
	}
	if d.Buckets[9] != 1 { // 95
		t.Fatalf("bucket[9] = %d, want 1", d.Buckets[9])
	}
}
