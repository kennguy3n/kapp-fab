package lms

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestBadgeQualifies(t *testing.T) {
	score := decimal.NewFromFloat(0.8)
	days := 7
	cases := []struct {
		name  string
		badge Badge
		m     Milestone
		want  bool
	}{
		{
			name:  "course_complete matches",
			badge: Badge{Active: true, CriteriaType: CriteriaCourseComplete},
			m:     Milestone{Type: CriteriaCourseComplete},
			want:  true,
		},
		{
			name:  "inactive never qualifies",
			badge: Badge{Active: false, CriteriaType: CriteriaCourseComplete},
			m:     Milestone{Type: CriteriaCourseComplete},
			want:  false,
		},
		{
			name:  "type mismatch",
			badge: Badge{Active: true, CriteriaType: CriteriaPathComplete},
			m:     Milestone{Type: CriteriaCourseComplete},
			want:  false,
		},
		{
			name:  "quiz_score meets threshold",
			badge: Badge{Active: true, CriteriaType: CriteriaQuizScore, CriteriaValue: CriteriaValue{MinScore: &score}},
			m:     Milestone{Type: CriteriaQuizScore, QuizScore: ptrDec(decimal.NewFromFloat(0.85))},
			want:  true,
		},
		{
			name:  "quiz_score below threshold",
			badge: Badge{Active: true, CriteriaType: CriteriaQuizScore, CriteriaValue: CriteriaValue{MinScore: &score}},
			m:     Milestone{Type: CriteriaQuizScore, QuizScore: ptrDec(decimal.NewFromFloat(0.5))},
			want:  false,
		},
		{
			name:  "quiz_score no threshold accepts any",
			badge: Badge{Active: true, CriteriaType: CriteriaQuizScore},
			m:     Milestone{Type: CriteriaQuizScore, QuizScore: ptrDec(decimal.NewFromInt(1))},
			want:  true,
		},
		{
			name:  "quiz_score nil score never qualifies",
			badge: Badge{Active: true, CriteriaType: CriteriaQuizScore},
			m:     Milestone{Type: CriteriaQuizScore},
			want:  false,
		},
		{
			name:  "streak reached",
			badge: Badge{Active: true, CriteriaType: CriteriaStreak, CriteriaValue: CriteriaValue{Days: &days}},
			m:     Milestone{Type: CriteriaStreak, StreakDays: 9},
			want:  true,
		},
		{
			name:  "streak short",
			badge: Badge{Active: true, CriteriaType: CriteriaStreak, CriteriaValue: CriteriaValue{Days: &days}},
			m:     Milestone{Type: CriteriaStreak, StreakDays: 3},
			want:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BadgeQualifies(c.badge, c.m); got != c.want {
				t.Fatalf("BadgeQualifies = %v, want %v", got, c.want)
			}
		})
	}
}

func ptrDec(d decimal.Decimal) *decimal.Decimal { return &d }

func TestValidateBadge(t *testing.T) {
	tid := uuid.New()
	t.Run("ok", func(t *testing.T) {
		b := Badge{TenantID: tid, Name: "First Course", CriteriaType: CriteriaCourseComplete}
		if err := validateBadge(&b); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("missing tenant", func(t *testing.T) {
		b := Badge{Name: "x", CriteriaType: CriteriaCourseComplete}
		if err := validateBadge(&b); !errors.Is(err, ErrInvalidBadge) {
			t.Fatalf("want ErrInvalidBadge, got %v", err)
		}
	})
	t.Run("bad criteria type", func(t *testing.T) {
		b := Badge{TenantID: tid, Name: "x", CriteriaType: "nope"}
		if err := validateBadge(&b); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("streak requires days", func(t *testing.T) {
		b := Badge{TenantID: tid, Name: "x", CriteriaType: CriteriaStreak}
		if err := validateBadge(&b); err == nil {
			t.Fatal("expected error for missing days")
		}
	})
	t.Run("quiz min_score out of range", func(t *testing.T) {
		bad := decimal.NewFromInt(2)
		b := Badge{TenantID: tid, Name: "x", CriteriaType: CriteriaQuizScore, CriteriaValue: CriteriaValue{MinScore: &bad}}
		if err := validateBadge(&b); err == nil {
			t.Fatal("expected error for min_score > 1")
		}
	})
}

func TestRankLeaderboard(t *testing.T) {
	u1, u2, u3, u4 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	entries := []LeaderboardEntry{
		{UserID: u1, CoursesCompleted: 3, TotalScore: decimal.NewFromInt(250), Badges: 2},
		{UserID: u2, CoursesCompleted: 5, TotalScore: decimal.NewFromInt(400), Badges: 4},
		{UserID: u3, CoursesCompleted: 3, TotalScore: decimal.NewFromInt(250), Badges: 2}, // tie with u1
		{UserID: u4, CoursesCompleted: 1, TotalScore: decimal.NewFromInt(80), Badges: 0},
	}
	ranked := RankLeaderboard(entries)

	if ranked[0].UserID != u2 || ranked[0].Rank != 1 {
		t.Fatalf("top entry = %+v", ranked[0])
	}
	// u1 and u3 tie for rank 2 (3rd field tiebreak by user id string).
	if ranked[1].Rank != 2 || ranked[2].Rank != 2 {
		t.Fatalf("tie ranks wrong: %d %d", ranked[1].Rank, ranked[2].Rank)
	}
	// Competition ranking: next distinct entry gets rank 4.
	if ranked[3].UserID != u4 || ranked[3].Rank != 4 {
		t.Fatalf("last entry = %+v, want rank 4", ranked[3])
	}
	// Input slice not mutated.
	if entries[0].Rank != 0 {
		t.Fatal("RankLeaderboard mutated input")
	}
}
