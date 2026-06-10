package lms

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func mkCourse(id uuid.UUID, mandatory bool, prereqs ...uuid.UUID) LearningPathCourse {
	return LearningPathCourse{CourseID: id, IsMandatory: mandatory, PrerequisiteCourseIDs: prereqs}
}

func TestEvaluateCompletion(t *testing.T) {
	c1, c2, c3 := uuid.New(), uuid.New(), uuid.New()

	tests := []struct {
		name      string
		courses   []LearningPathCourse
		completed map[uuid.UUID]bool
		want      CompletionResult
	}{
		{
			name:      "no courses is not complete",
			courses:   nil,
			completed: map[uuid.UUID]bool{},
			want:      CompletionResult{},
		},
		{
			name:      "all mandatory complete => complete",
			courses:   []LearningPathCourse{mkCourse(c1, true), mkCourse(c2, true)},
			completed: map[uuid.UUID]bool{c1: true, c2: true},
			want:      CompletionResult{TotalCourses: 2, MandatoryCourses: 2, CompletedMandatory: 2, CompletedTotal: 2, Complete: true},
		},
		{
			name:      "non-mandatory incomplete does not block",
			courses:   []LearningPathCourse{mkCourse(c1, true), mkCourse(c2, false)},
			completed: map[uuid.UUID]bool{c1: true},
			want:      CompletionResult{TotalCourses: 2, MandatoryCourses: 1, CompletedMandatory: 1, CompletedTotal: 1, Complete: true},
		},
		{
			name:      "missing one mandatory => incomplete",
			courses:   []LearningPathCourse{mkCourse(c1, true), mkCourse(c2, true), mkCourse(c3, false)},
			completed: map[uuid.UUID]bool{c1: true, c3: true},
			want:      CompletionResult{TotalCourses: 3, MandatoryCourses: 2, CompletedMandatory: 1, CompletedTotal: 2, Complete: false},
		},
		{
			name:      "only optional courses => never complete",
			courses:   []LearningPathCourse{mkCourse(c1, false)},
			completed: map[uuid.UUID]bool{c1: true},
			want:      CompletionResult{TotalCourses: 1, MandatoryCourses: 0, CompletedMandatory: 0, CompletedTotal: 1, Complete: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateCompletion(tt.courses, tt.completed)
			if got != tt.want {
				t.Fatalf("EvaluateCompletion = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestUnlockedCourseIDs(t *testing.T) {
	c1, c2, c3 := uuid.New(), uuid.New(), uuid.New()
	courses := []LearningPathCourse{
		mkCourse(c1, true),
		mkCourse(c2, true, c1),     // needs c1
		mkCourse(c3, true, c1, c2), // needs c1 and c2
	}

	t.Run("nothing completed unlocks only prereq-free", func(t *testing.T) {
		got := UnlockedCourseIDs(courses, map[uuid.UUID]bool{})
		if !reflect.DeepEqual(got, []uuid.UUID{c1}) {
			t.Fatalf("got %v, want [%v]", got, c1)
		}
	})
	t.Run("completing c1 unlocks c2", func(t *testing.T) {
		got := UnlockedCourseIDs(courses, map[uuid.UUID]bool{c1: true})
		if !reflect.DeepEqual(got, []uuid.UUID{c1, c2}) {
			t.Fatalf("got %v, want [%v %v]", got, c1, c2)
		}
	})
	t.Run("completing c1+c2 unlocks all", func(t *testing.T) {
		got := UnlockedCourseIDs(courses, map[uuid.UUID]bool{c1: true, c2: true})
		if len(got) != 3 {
			t.Fatalf("got %v, want 3 unlocked", got)
		}
	})
}

func TestValidatePath(t *testing.T) {
	tid := uuid.New()
	t.Run("missing tenant", func(t *testing.T) {
		p := LearningPath{Title: "x"}
		if err := validatePath(&p); !errors.Is(err, ErrInvalidLearningPath) {
			t.Fatalf("want ErrInvalidLearningPath, got %v", err)
		}
	})
	t.Run("missing title", func(t *testing.T) {
		p := LearningPath{TenantID: tid, Title: "   "}
		if err := validatePath(&p); !errors.Is(err, ErrInvalidLearningPath) {
			t.Fatalf("want ErrInvalidLearningPath, got %v", err)
		}
	})
	t.Run("defaults applied", func(t *testing.T) {
		p := LearningPath{TenantID: tid, Title: "Onboarding"}
		if err := validatePath(&p); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if p.Status != PathStatusDraft || p.Difficulty != DifficultyBeginner {
			t.Fatalf("defaults not applied: %+v", p)
		}
	})
	t.Run("bad status rejected", func(t *testing.T) {
		p := LearningPath{TenantID: tid, Title: "x", Status: "live"}
		if err := validatePath(&p); err == nil {
			t.Fatal("expected error for bad status")
		}
	})
	t.Run("bad difficulty rejected", func(t *testing.T) {
		p := LearningPath{TenantID: tid, Title: "x", Difficulty: "expert"}
		if err := validatePath(&p); err == nil {
			t.Fatal("expected error for bad difficulty")
		}
	})
	t.Run("negative duration rejected", func(t *testing.T) {
		p := LearningPath{TenantID: tid, Title: "x", EstimatedDurationHours: -1}
		if err := validatePath(&p); err == nil {
			t.Fatal("expected error for negative duration")
		}
	})
}

func TestNormalizeRoles(t *testing.T) {
	got := normalizeRoles([]string{" admin ", "admin", "", "  ", "manager", "manager"})
	want := []string{"admin", "manager"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeRoles = %v, want %v", got, want)
	}
	if got := normalizeRoles(nil); len(got) != 0 {
		t.Fatalf("nil roles => %v, want empty", got)
	}
}
