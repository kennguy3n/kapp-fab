package lms

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// EnrollmentRow is the projection of an lms.enrollment a course's
// analytics needs: which user, which enrollment, and its status.
type EnrollmentRow struct {
	EnrollmentID uuid.UUID `json:"enrollment_id"`
	UserID       uuid.UUID `json:"user_id"`
	Status       string    `json:"status"`
}

// ProgressRow is one lesson_progress row used by analytics.
type ProgressRow struct {
	EnrollmentID uuid.UUID        `json:"enrollment_id"`
	LessonID     uuid.UUID        `json:"lesson_id"`
	Status       string           `json:"status"`
	Score        *decimal.Decimal `json:"score,omitempty"`
}

// LessonDropOff captures where learners stall: how many enrollments
// reached a lesson (any progress row) vs. completed it.
type LessonDropOff struct {
	LessonID    uuid.UUID `json:"lesson_id"`
	Reached     int       `json:"reached"`
	Completed   int       `json:"completed"`
	DropOffRate float64   `json:"drop_off_rate"`
}

// LearnerProgress is the per-learner row in the instructor dashboard.
type LearnerProgress struct {
	UserID           uuid.UUID        `json:"user_id"`
	EnrollmentID     uuid.UUID        `json:"enrollment_id"`
	Status           string           `json:"status"`
	LessonsCompleted int              `json:"lessons_completed"`
	LessonsTotal     int              `json:"lessons_total"`
	AverageScore     *decimal.Decimal `json:"average_score,omitempty"`
}

// CourseAnalytics is the aggregate instructor view of a course.
type CourseAnalytics struct {
	CourseID        uuid.UUID         `json:"course_id"`
	EnrollmentCount int               `json:"enrollment_count"`
	CompletedCount  int               `json:"completed_count"`
	CompletionRate  float64           `json:"completion_rate"`
	AverageScore    *decimal.Decimal  `json:"average_score,omitempty"`
	LessonDropOff   []LessonDropOff   `json:"lesson_drop_off"`
	PerLearner      []LearnerProgress `json:"per_learner"`
}

// ComputeCourseAnalytics aggregates enrollment + progress rows into the
// instructor dashboard view. Pure (no I/O) so every rollup — completion
// rate, average score, drop-off — is unit-tested against fixed inputs.
//
//   - completion_rate = completed enrollments / total enrollments
//   - average_score   = mean of all scored progress rows (lessons with
//     a quiz/assignment); nil when nothing is scored
//   - lesson_drop_off = per lesson, reached (has a progress row) vs.
//     completed, with drop_off_rate = 1 - completed/reached
//   - per_learner     = each enrollment's lessons completed / total and
//     mean score
//
// lessonIDs is the course's full lesson set so LessonsTotal and the
// drop-off denominator reflect lessons with zero progress too.
func ComputeCourseAnalytics(courseID uuid.UUID, enrollments []EnrollmentRow, progress []ProgressRow, lessonIDs []uuid.UUID) CourseAnalytics {
	// Initialize the slice fields as empty (not nil) so json.Marshal emits
	// `[]` rather than `null` for a course with no lessons/enrollments yet.
	// The TS contract types these as arrays (CourseAnalytics.lesson_drop_off:
	// LessonDropOff[]); a `null` would crash the dashboard on `.length`.
	out := CourseAnalytics{
		CourseID:        courseID,
		EnrollmentCount: len(enrollments),
		LessonDropOff:   []LessonDropOff{},
		PerLearner:      []LearnerProgress{},
	}

	for _, e := range enrollments {
		if e.Status == "completed" {
			out.CompletedCount++
		}
	}
	if out.EnrollmentCount > 0 {
		out.CompletionRate = float64(out.CompletedCount) / float64(out.EnrollmentCount)
	}

	// Index progress by enrollment for the per-learner rollup and by
	// lesson for drop-off.
	byEnrollment := make(map[uuid.UUID][]ProgressRow)
	reached := make(map[uuid.UUID]int)
	completedByLesson := make(map[uuid.UUID]int)
	scoreSum := decimal.Zero
	scoreCount := 0
	for _, p := range progress {
		byEnrollment[p.EnrollmentID] = append(byEnrollment[p.EnrollmentID], p)
		reached[p.LessonID]++
		if p.Status == ProgressCompleted {
			completedByLesson[p.LessonID]++
		}
		if p.Score != nil {
			scoreSum = scoreSum.Add(*p.Score)
			scoreCount++
		}
	}
	if scoreCount > 0 {
		avg := scoreSum.Div(decimal.NewFromInt(int64(scoreCount))).Round(2)
		out.AverageScore = &avg
	}

	for _, lid := range lessonIDs {
		r := reached[lid]
		c := completedByLesson[lid]
		rate := 0.0
		if r > 0 {
			rate = 1 - float64(c)/float64(r)
		}
		out.LessonDropOff = append(out.LessonDropOff, LessonDropOff{
			LessonID: lid, Reached: r, Completed: c, DropOffRate: rate,
		})
	}

	for _, e := range enrollments {
		rows := byEnrollment[e.EnrollmentID]
		lp := LearnerProgress{
			UserID:       e.UserID,
			EnrollmentID: e.EnrollmentID,
			Status:       e.Status,
			LessonsTotal: len(lessonIDs),
		}
		lSum := decimal.Zero
		lCount := 0
		for _, p := range rows {
			if p.Status == ProgressCompleted {
				lp.LessonsCompleted++
			}
			if p.Score != nil {
				lSum = lSum.Add(*p.Score)
				lCount++
			}
		}
		if lCount > 0 {
			avg := lSum.Div(decimal.NewFromInt(int64(lCount))).Round(2)
			lp.AverageScore = &avg
		}
		out.PerLearner = append(out.PerLearner, lp)
	}
	// Stable per-learner order (by user id) for deterministic output +
	// CSV export.
	sort.SliceStable(out.PerLearner, func(i, j int) bool {
		return out.PerLearner[i].UserID.String() < out.PerLearner[j].UserID.String()
	})
	return out
}

// QuizScoreDistribution buckets quiz scores (0..100) into deciles and
// reports pass/fail against a threshold expressed on the same 0..100
// scale. score distribution + pass rate are the analytics the current
// progress model supports; per-question breakdowns require answer-level
// capture (not yet stored) and are intentionally omitted rather than
// faked.
type QuizScoreDistribution struct {
	Total    int     `json:"total"`
	Passed   int     `json:"passed"`
	Failed   int     `json:"failed"`
	PassRate float64 `json:"pass_rate"`
	// Buckets[i] counts scores in [i*10, (i+1)*10), with Buckets[10]
	// catching an exact 100.
	Buckets [11]int `json:"buckets"`
}

// ComputeQuizDistribution buckets a set of quiz scores and counts
// pass/fail against passThreshold (0..100). Pure.
func ComputeQuizDistribution(scores []decimal.Decimal, passThreshold decimal.Decimal) QuizScoreDistribution {
	out := QuizScoreDistribution{Total: len(scores)}
	hundred := decimal.NewFromInt(100)
	for _, s := range scores {
		// Clamp into [0,100] before bucketing so an out-of-range score
		// can't index past the array.
		v := s
		if v.IsNegative() {
			v = decimal.Zero
		}
		if v.GreaterThan(hundred) {
			v = hundred
		}
		idx := int(v.Div(decimal.NewFromInt(10)).IntPart())
		if idx > 10 {
			idx = 10
		}
		out.Buckets[idx]++
		if s.GreaterThanOrEqual(passThreshold) {
			out.Passed++
		} else {
			out.Failed++
		}
	}
	if out.Total > 0 {
		out.PassRate = float64(out.Passed) / float64(out.Total)
	}
	return out
}

// ---------------------------------------------------------------------------
// Store: gathers raw rows for ComputeCourseAnalytics.
// ---------------------------------------------------------------------------

// EnrollmentLister supplies a course's enrollments. Implemented at the
// API layer over the lms.enrollment KRecords (record.PGStore.ListByField
// on course_id) so the analytics store stays decoupled from the record
// store.
type EnrollmentLister interface {
	CourseEnrollments(ctx context.Context, tenantID, courseID uuid.UUID) ([]EnrollmentRow, error)
}

// LessonLister supplies a course's lesson ids (across its modules).
type LessonLister interface {
	CourseLessonIDs(ctx context.Context, tenantID, courseID uuid.UUID) ([]uuid.UUID, error)
}

// AnalyticsStore assembles the inputs ComputeCourseAnalytics needs.
type AnalyticsStore struct {
	pool        *pgxpool.Pool
	enrollments EnrollmentLister
	lessons     LessonLister
	now         func() time.Time
}

// NewAnalyticsStore wires an analytics store. The enrollment + lesson
// listers are resolved at the API layer over the record store.
func NewAnalyticsStore(pool *pgxpool.Pool, enrollments EnrollmentLister, lessons LessonLister) *AnalyticsStore {
	return &AnalyticsStore{
		pool:        pool,
		enrollments: enrollments,
		lessons:     lessons,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// CourseAnalytics gathers enrollment + progress + lesson rows for a
// course and returns the computed dashboard view.
func (s *AnalyticsStore) CourseAnalytics(ctx context.Context, tenantID, courseID uuid.UUID) (*CourseAnalytics, error) {
	enrollments, err := s.enrollments.CourseEnrollments(ctx, tenantID, courseID)
	if err != nil {
		return nil, err
	}
	lessonIDs, err := s.lessons.CourseLessonIDs(ctx, tenantID, courseID)
	if err != nil {
		return nil, err
	}

	enrollmentIDs := make([]uuid.UUID, 0, len(enrollments))
	for _, e := range enrollments {
		enrollmentIDs = append(enrollmentIDs, e.EnrollmentID)
	}

	var progress []ProgressRow
	if len(enrollmentIDs) > 0 {
		err = platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			rows, qerr := tx.Query(ctx,
				`SELECT enrollment_id, lesson_id, status, score
				   FROM lesson_progress
				  WHERE tenant_id = $1 AND enrollment_id = ANY($2)`,
				tenantID, enrollmentIDs,
			)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var p ProgressRow
				if err := rows.Scan(&p.EnrollmentID, &p.LessonID, &p.Status, &p.Score); err != nil {
					return err
				}
				progress = append(progress, p)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
	}

	result := ComputeCourseAnalytics(courseID, enrollments, progress, lessonIDs)
	return &result, nil
}
