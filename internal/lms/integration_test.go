//go:build integration
// +build integration

// DB-backed tests for the Session-17 LMS stores. Run with:
//
//	KAPP_TEST_DB_URL=postgres://kapp_app:kapp_app_dev@localhost:5432/kapp?sslmode=disable \
//	  go test -tags=integration ./internal/lms/...
//
// The pool connects as kapp_app (the non-superuser application role) so
// RLS is enforced exactly as in production. Tests are skipped when
// KAPP_TEST_DB_URL is unset, keeping `go test ./...` hermetic.
package lms

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/files"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

func buildScormZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("KAPP_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("KAPP_TEST_DB_URL not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := platform.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// countAudit returns how many audit_log rows match an action for a tenant.
func countAudit(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, action string) int {
	t.Helper()
	var n int
	err := platform.WithTenantTx(context.Background(), pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log WHERE tenant_id=$1 AND action=$2`, tenantID, action).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func TestLearningPathStoreLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	auditor := audit.NewPGLogger(pool)
	store := NewLearningPathStore(pool, auditor)
	tenant := uuid.New()

	path, err := store.CreatePath(ctx, LearningPath{
		TenantID: tenant, Title: "Onboarding", TargetRoles: []string{"sales", "sales"}, Difficulty: DifficultyBeginner,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if path.Status != PathStatusDraft || len(path.TargetRoles) != 1 {
		t.Fatalf("unexpected path: %+v", path)
	}

	c1, c2 := uuid.New(), uuid.New()
	if _, err := store.AddCourse(ctx, LearningPathCourse{TenantID: tenant, LearningPathID: path.ID, CourseID: c1, SequenceOrder: 1, IsMandatory: true}, nil); err != nil {
		t.Fatalf("add c1: %v", err)
	}
	// Idempotent add (same course) must not duplicate.
	if _, err := store.AddCourse(ctx, LearningPathCourse{TenantID: tenant, LearningPathID: path.ID, CourseID: c1, SequenceOrder: 1, IsMandatory: true}, nil); err != nil {
		t.Fatalf("re-add c1: %v", err)
	}
	if _, err := store.AddCourse(ctx, LearningPathCourse{TenantID: tenant, LearningPathID: path.ID, CourseID: c2, SequenceOrder: 2, IsMandatory: false}, nil); err != nil {
		t.Fatalf("add c2: %v", err)
	}
	courses, err := store.ListCourses(ctx, tenant, path.ID)
	if err != nil || len(courses) != 2 {
		t.Fatalf("list courses: %v n=%d", err, len(courses))
	}

	// Publish so it is auto-enroll eligible.
	path.Status = PathStatusPublished
	if _, err := store.UpdatePath(ctx, *path, nil); err != nil {
		t.Fatalf("publish: %v", err)
	}

	user := uuid.New()
	enr, err := store.Enroll(ctx, tenant, path.ID, user, EnrollSourceManual, nil)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// Idempotent enroll.
	if _, err := store.Enroll(ctx, tenant, path.ID, user, EnrollSourceManual, nil); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if enr.Status != PathEnrollmentEnrolled {
		t.Fatalf("status %q", enr.Status)
	}

	// Completing only the mandatory course (c1) completes the path.
	updated, res, err := store.RecomputeCompletion(ctx, tenant, path.ID, user, map[uuid.UUID]bool{c1: true}, nil)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if !res.Complete || updated.Status != PathEnrollmentCompleted {
		t.Fatalf("expected complete, got res=%+v enr=%+v", res, updated)
	}

	// Audit fired for create/add/enroll/etc.
	if n := countAudit(t, pool, tenant, "lms.learning_path.create"); n != 1 {
		t.Fatalf("expected 1 create audit, got %d", n)
	}
}

func TestLearningPathAutoEnroll(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewLearningPathStore(pool, audit.NewPGLogger(pool))
	tenant := uuid.New()

	p1, _ := store.CreatePath(ctx, LearningPath{TenantID: tenant, Title: "Sales 101", TargetRoles: []string{"sales"}})
	p1.Status = PathStatusPublished
	store.UpdatePath(ctx, *p1, nil)
	// Draft path with matching role must NOT auto-enroll.
	store.CreatePath(ctx, LearningPath{TenantID: tenant, Title: "Draft", TargetRoles: []string{"sales"}})

	enroller := NewPathAutoEnroller(store)
	user := uuid.New()
	enrolled, err := enroller.OnRolesAssigned(ctx, tenant, user, []string{"sales", "ops"})
	if err != nil {
		t.Fatalf("auto-enroll: %v", err)
	}
	if len(enrolled) != 1 || enrolled[0] != p1.ID {
		t.Fatalf("expected only published path, got %v", enrolled)
	}
	got, err := store.GetEnrollment(ctx, tenant, p1.ID, user)
	if err != nil || got.Source != EnrollSourceAuto {
		t.Fatalf("expected auto enrollment, got %+v err=%v", got, err)
	}
}

func TestLearningPathRLSIsolation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewLearningPathStore(pool, audit.NewPGLogger(pool))
	tenantA, tenantB := uuid.New(), uuid.New()

	pa, err := store.CreatePath(ctx, LearningPath{TenantID: tenantA, Title: "A path"})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	// Tenant B must not see tenant A's path.
	if _, err := store.GetPath(ctx, tenantB, pa.ID); err == nil {
		t.Fatal("RLS leak: tenant B read tenant A path")
	}
	listB, err := store.ListPaths(ctx, tenantB, "")
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	for _, p := range listB {
		if p.ID == pa.ID {
			t.Fatal("RLS leak: tenant A path in tenant B list")
		}
	}
}

func TestGamificationStore(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewGamificationStore(pool, audit.NewPGLogger(pool))
	tenant := uuid.New()

	badge, err := store.CreateBadge(ctx, Badge{TenantID: tenant, Name: "First Course", CriteriaType: CriteriaCourseComplete, Active: true})
	if err != nil {
		t.Fatalf("create badge: %v", err)
	}
	user := uuid.New()
	_, awarded, err := store.AwardBadge(ctx, tenant, user, badge.ID, json.RawMessage(`{"course_id":"x"}`), nil)
	if err != nil || !awarded {
		t.Fatalf("award: %v awarded=%v", err, awarded)
	}
	// Idempotent: second award is a no-op.
	_, awarded2, err := store.AwardBadge(ctx, tenant, user, badge.ID, nil, nil)
	if err != nil || awarded2 {
		t.Fatalf("re-award should be no-op: %v awarded=%v", err, awarded2)
	}
	held, err := store.ListUserBadges(ctx, tenant, user)
	if err != nil || len(held) != 1 {
		t.Fatalf("list user badges: %v n=%d", err, len(held))
	}
	counts, err := store.BadgeCountsByUser(ctx, tenant)
	if err != nil || counts[user] != 1 {
		t.Fatalf("counts: %v %+v", err, counts)
	}

	// A second learner earns the same badge; the tenant-wide award
	// history must surface awards across all learners (not just one).
	user2 := uuid.New()
	if _, awarded3, err := store.AwardBadge(ctx, tenant, user2, badge.ID, json.RawMessage(`{"course_id":"y"}`), nil); err != nil || !awarded3 {
		t.Fatalf("award user2: %v awarded=%v", err, awarded3)
	}
	all, err := store.ListAwards(ctx, tenant, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("list awards (tenant-wide): %v n=%d", err, len(all))
	}
	seen := map[uuid.UUID]bool{}
	for _, a := range all {
		seen[a.UserID] = true
	}
	if !seen[user] || !seen[user2] {
		t.Fatalf("award history missing a learner: %+v", seen)
	}
	// A different tenant sees none of these awards (RLS isolation).
	other, err := store.ListAwards(ctx, uuid.New(), 0)
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant award leak: %v n=%d", err, len(other))
	}
}

func TestDiscussionStore(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewDiscussionStore(pool, audit.NewPGLogger(pool))
	tenant := uuid.New()
	course := uuid.New()
	author := uuid.New()

	th, err := store.CreateThread(ctx, DiscussionThread{TenantID: tenant, CourseID: course, AuthorID: author, Title: "Q1", Body: "help"})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	r1, err := store.AddReply(ctx, DiscussionReply{TenantID: tenant, ThreadID: th.ID, AuthorID: author, Body: "answer"}, &author)
	if err != nil {
		t.Fatalf("add reply: %v", err)
	}
	got, err := store.GetThread(ctx, tenant, th.ID)
	if err != nil || got.ReplyCount != 1 {
		t.Fatalf("reply_count not bumped: %v count=%d", err, got.ReplyCount)
	}
	if err := store.MarkAnswer(ctx, tenant, th.ID, r1.ID, &author); err != nil {
		t.Fatalf("mark answer: %v", err)
	}
	got, _ = store.GetThread(ctx, tenant, th.ID)
	if got.Status != ThreadStatusResolved {
		t.Fatalf("thread not resolved: %q", got.Status)
	}
	replies, _ := store.ListReplies(ctx, tenant, th.ID)
	if len(replies) != 1 || !replies[0].IsAnswer {
		t.Fatalf("answer flag wrong: %+v", replies)
	}
}

func TestScormCommitRuntime(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewScormStore(pool, files.NewMemoryStore(), audit.NewPGLogger(pool))
	tenant := uuid.New()
	enr := uuid.New()
	lesson := uuid.New()

	// First commit: in progress, accumulates 600s.
	p, err := store.CommitRuntime(ctx, tenant, enr, lesson, CMIData{
		Version: ContentTypeScorm12, LessonStatus: "incomplete", SessionTime: "00:10:00", SuspendData: "s1",
	}, nil)
	if err != nil {
		t.Fatalf("commit1: %v", err)
	}
	if p.Status != ProgressInProgress {
		t.Fatalf("status %q", p.Status)
	}
	// Second commit: passed, adds 300s and completes.
	p, err = store.CommitRuntime(ctx, tenant, enr, lesson, CMIData{
		Version: ContentTypeScorm12, LessonStatus: "passed", ScoreRaw: f64(95), SessionTime: "00:05:00", SuspendData: "s2",
	}, nil)
	if err != nil {
		t.Fatalf("commit2: %v", err)
	}
	if p.Status != ProgressCompleted || p.CompletedAt == nil {
		t.Fatalf("expected completed, got %+v", p)
	}

	// Verify accumulated time + suspend_data persisted.
	var secs int64
	var meta []byte
	err = platform.WithTenantTx(ctx, pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT time_spent_seconds, metadata FROM lesson_progress WHERE tenant_id=$1 AND enrollment_id=$2 AND lesson_id=$3`,
			tenant, enr, lesson).Scan(&secs, &meta)
	})
	if err != nil {
		t.Fatalf("read progress: %v", err)
	}
	if secs != 900 {
		t.Fatalf("time_spent = %d, want 900", secs)
	}
	var m map[string]string
	json.Unmarshal(meta, &m)
	if m["suspend_data"] != "s2" {
		t.Fatalf("suspend_data = %q, want s2", m["suspend_data"])
	}

	// Third commit: a revisit re-reporting "incomplete" must NOT regress
	// the already-completed status (completion is terminal).
	p, err = store.CommitRuntime(ctx, tenant, enr, lesson, CMIData{
		Version: ContentTypeScorm12, LessonStatus: "incomplete", SessionTime: "00:01:00", SuspendData: "s3",
	}, nil)
	if err != nil {
		t.Fatalf("commit3: %v", err)
	}
	if p.Status != ProgressCompleted || p.CompletedAt == nil {
		t.Fatalf("revisit regressed completion: %+v", p)
	}

	// Fourth commit: a post-completion revisit reporting a LOWER score must
	// not regress the recorded grade — score is a high-water mark once the
	// lesson is completed (95 stays, 40 ignored).
	p, err = store.CommitRuntime(ctx, tenant, enr, lesson, CMIData{
		Version: ContentTypeScorm12, LessonStatus: "incomplete", ScoreRaw: f64(40), SessionTime: "00:01:00", SuspendData: "s4",
	}, nil)
	if err != nil {
		t.Fatalf("commit4: %v", err)
	}
	if p.Score == nil || !p.Score.Equal(decimal.NewFromInt(95)) {
		t.Fatalf("post-completion lower score regressed grade: score = %v, want 95", p.Score)
	}
}

// TestUpsertProgressCompletionTerminal covers the generic Store writer
// (used by quiz scoring, assignment grading agent tools, and the
// integration suites) — like the SCORM and xAPI paths, a later in_progress
// upsert must not regress an already-completed lesson, while score and
// attempts still update.
func TestUpsertProgressCompletionTerminal(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	tenant := uuid.New()
	enr := uuid.New()
	lesson := uuid.New()
	now := time.Now().UTC()

	first := decimal.NewFromInt(90)
	p, err := store.UpsertProgress(ctx, Progress{
		TenantID: tenant, EnrollmentID: enr, LessonID: lesson,
		Status: ProgressCompleted, Score: &first, StartedAt: &now, CompletedAt: &now,
	})
	if err != nil || p.Status != ProgressCompleted || p.CompletedAt == nil {
		t.Fatalf("first upsert: err=%v p=%+v", err, p)
	}

	// Revisit reporting in_progress with a new score: status stays
	// completed, completed_at survives, score updates, attempts increments.
	second := decimal.NewFromInt(95)
	p, err = store.UpsertProgress(ctx, Progress{
		TenantID: tenant, EnrollmentID: enr, LessonID: lesson,
		Status: ProgressInProgress, Score: &second,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if p.Status != ProgressCompleted || p.CompletedAt == nil {
		t.Fatalf("revisit regressed completion: %+v", p)
	}
	if p.Score == nil || !p.Score.Equal(second) {
		t.Fatalf("score = %v; want %s", p.Score, second)
	}
	if p.Attempts != 2 {
		t.Fatalf("attempts = %d; want 2", p.Attempts)
	}

	// Score is a high-water mark once completed: a later revisit reporting a
	// LOWER score must not regress the recorded grade (95 stays, 50 ignored).
	lower := decimal.NewFromInt(50)
	p, err = store.UpsertProgress(ctx, Progress{
		TenantID: tenant, EnrollmentID: enr, LessonID: lesson,
		Status: ProgressInProgress, Score: &lower,
	})
	if err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if p.Score == nil || !p.Score.Equal(second) {
		t.Fatalf("post-completion lower score regressed grade: score = %v; want %s", p.Score, second)
	}
	if p.Status != ProgressCompleted {
		t.Fatalf("third revisit regressed completion: %+v", p)
	}
}

type stubResolver struct{ id uuid.UUID }

func (s stubResolver) ResolveActor(_ context.Context, _ uuid.UUID, _ string) (uuid.UUID, bool, error) {
	return s.id, true, nil
}

func TestXAPIIngest(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewXAPIStore(pool, audit.NewPGLogger(pool))
	tenant := uuid.New()
	user := uuid.New()
	lesson := uuid.New()
	enr := uuid.New()

	enrollmentFor := func(_ context.Context, _, _, _ uuid.UUID) (uuid.UUID, bool, error) {
		return enr, true, nil
	}
	st := XAPIStatement{
		Actor:  XAPIActor{Mbox: "mailto:jane@acme.test"},
		Verb:   XAPIVerb{ID: "http://adlnet.gov/expapi/verbs/completed"},
		Object: XAPIObject{ID: "https://kapp/lessons/" + lesson.String()},
		Result: &XAPIResult{Score: &XAPIScore{Raw: f64(88)}},
	}
	res, err := store.Ingest(ctx, tenant, st, stubResolver{user}, enrollmentFor, nil)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !res.WroteProgress || res.ProgressStatus != ProgressCompleted {
		t.Fatalf("expected progress write, got %+v", res)
	}

	// Progress row created.
	var status string
	var score *decimal.Decimal
	err = platform.WithTenantTx(ctx, pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, score FROM lesson_progress WHERE tenant_id=$1 AND enrollment_id=$2 AND lesson_id=$3`,
			tenant, enr, lesson).Scan(&status, &score)
	})
	if err != nil || status != ProgressCompleted {
		t.Fatalf("progress: %v status=%q", err, status)
	}

	list, err := store.ListStatements(ctx, tenant, &user, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list statements: %v n=%d", err, len(list))
	}

	// A later "experienced" statement (maps to in_progress) arriving after
	// completion must NOT regress the lesson's terminal "completed" status;
	// completed_at must also survive. Mirrors the SCORM CommitRuntime guard.
	regress := XAPIStatement{
		Actor:  XAPIActor{Mbox: "mailto:jane@acme.test"},
		Verb:   XAPIVerb{ID: "http://adlnet.gov/expapi/verbs/experienced"},
		Object: XAPIObject{ID: "https://kapp/lessons/" + lesson.String()},
	}
	rres, err := store.Ingest(ctx, tenant, regress, stubResolver{user}, enrollmentFor, nil)
	if err != nil {
		t.Fatalf("ingest regress: %v", err)
	}
	if rres.ProgressStatus != ProgressCompleted {
		t.Fatalf("xapi result regressed completion: %+v", rres)
	}
	var (
		status2     string
		completedAt *time.Time
	)
	err = platform.WithTenantTx(ctx, pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, completed_at FROM lesson_progress WHERE tenant_id=$1 AND enrollment_id=$2 AND lesson_id=$3`,
			tenant, enr, lesson).Scan(&status2, &completedAt)
	})
	if err != nil || status2 != ProgressCompleted || completedAt == nil {
		t.Fatalf("revisit regressed completion: err=%v status=%q completedAt=%v", err, status2, completedAt)
	}
}

func TestAnalyticsStoreGather(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := uuid.New()
	course := uuid.New()
	enr := uuid.New()
	user := uuid.New()
	lesson := uuid.New()

	// Seed a lesson_progress row directly.
	err := platform.WithTenantTx(ctx, pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO lesson_progress (tenant_id, enrollment_id, lesson_id, status, score, attempts, updated_at)
			 VALUES ($1,$2,$3,'completed',90,1,now())`,
			tenant, enr, lesson)
		return e
	})
	if err != nil {
		t.Fatalf("seed progress: %v", err)
	}

	store := NewAnalyticsStore(pool,
		stubEnrollments{[]EnrollmentRow{{EnrollmentID: enr, UserID: user, Status: "completed"}}},
		stubLessons{[]uuid.UUID{lesson}},
	)
	a, err := store.CourseAnalytics(ctx, tenant, course)
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if a.EnrollmentCount != 1 || a.CompletedCount != 1 {
		t.Fatalf("counts: %+v", a)
	}
	if a.AverageScore == nil || !a.AverageScore.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("avg score: %v", a.AverageScore)
	}
}

func TestLearningPathMoreStoreMethods(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewLearningPathStore(pool, audit.NewPGLogger(pool))
	tenant := uuid.New()

	p, err := store.CreatePath(ctx, LearningPath{TenantID: tenant, Title: "P", Status: PathStatusPublished})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c1 := uuid.New()
	if _, err := store.AddCourse(ctx, LearningPathCourse{TenantID: tenant, LearningPathID: p.ID, CourseID: c1, IsMandatory: true}, nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.RemoveCourse(ctx, tenant, p.ID, c1, nil); err != nil {
		t.Fatalf("remove course: %v", err)
	}
	if courses, _ := store.ListCourses(ctx, tenant, p.ID); len(courses) != 0 {
		t.Fatalf("course not removed: %d", len(courses))
	}

	u := uuid.New()
	if _, err := store.Enroll(ctx, tenant, p.ID, u, EnrollSourceManual, nil); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := store.SetEnrollmentStatus(ctx, tenant, p.ID, u, PathEnrollmentInProgress, nil); err != nil {
		t.Fatalf("set status: %v", err)
	}
	enrs, err := store.ListEnrollmentsForUser(ctx, tenant, u)
	if err != nil || len(enrs) != 1 || enrs[0].Status != PathEnrollmentInProgress {
		t.Fatalf("list enrollments: %v %+v", err, enrs)
	}

	// ListPaths with status filter.
	pubs, err := store.ListPaths(ctx, tenant, PathStatusPublished)
	if err != nil || len(pubs) != 1 {
		t.Fatalf("list published: %v n=%d", err, len(pubs))
	}

	if err := store.DeletePath(ctx, tenant, p.ID, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetPath(ctx, tenant, p.ID); err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestDiscussionUpdateDeleteList(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewDiscussionStore(pool, audit.NewPGLogger(pool))
	tenant := uuid.New()
	course := uuid.New()
	author := uuid.New()

	th, err := store.CreateThread(ctx, DiscussionThread{TenantID: tenant, CourseID: course, AuthorID: author, Title: "T1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	th.Pinned = true
	th.Status = ThreadStatusClosed
	updated, err := store.UpdateThread(ctx, *th, &author)
	if err != nil || !updated.Pinned || updated.Status != ThreadStatusClosed {
		t.Fatalf("update: %v %+v", err, updated)
	}
	store.CreateThread(ctx, DiscussionThread{TenantID: tenant, CourseID: course, AuthorID: author, Title: "T2"})
	threads, err := store.ListThreads(ctx, tenant, course)
	if err != nil || len(threads) != 2 {
		t.Fatalf("list: %v n=%d", err, len(threads))
	}
	// Pinned thread sorts first.
	if !threads[0].Pinned {
		t.Fatalf("pinned thread should sort first: %+v", threads[0])
	}
	if err := store.DeleteThread(ctx, tenant, th.ID, &author); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetThread(ctx, tenant, th.ID); err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestGamificationListBadges(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewGamificationStore(pool, audit.NewPGLogger(pool))
	tenant := uuid.New()

	store.CreateBadge(ctx, Badge{TenantID: tenant, Name: "Active", CriteriaType: CriteriaCourseComplete, Active: true})
	store.CreateBadge(ctx, Badge{TenantID: tenant, Name: "Inactive", CriteriaType: CriteriaPathComplete, Active: false})

	all, err := store.ListBadges(ctx, tenant, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %v n=%d", err, len(all))
	}
	active, err := store.ListBadges(ctx, tenant, true)
	if err != nil || len(active) != 1 || active[0].Name != "Active" {
		t.Fatalf("list active: %v %+v", err, active)
	}
}

func TestScormExtractPackage(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	mem := files.NewMemoryStore()
	store := NewScormStore(pool, mem, audit.NewPGLogger(pool))
	tenant := uuid.New()
	lesson := uuid.New()

	zipBytes := buildScormZip(t, map[string]string{
		"imsmanifest.xml": sampleManifest12,
		"index.html":      "<html>hi</html>",
		"assets/app.js":   "console.log(1)",
	})
	pkg, err := store.ExtractPackage(ctx, tenant, lesson, zipBytes)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if pkg.FileCount != 3 {
		t.Fatalf("file count = %d, want 3", pkg.FileCount)
	}
	wantPrefix := "scorm/" + tenant.String() + "/" + lesson.String()
	if pkg.Manifest.LaunchHref != wantPrefix+"/index.html" {
		t.Fatalf("launch href = %q", pkg.Manifest.LaunchHref)
	}
	// Stored object retrievable.
	if _, err := mem.Get(ctx, wantPrefix+"/index.html"); err != nil {
		t.Fatalf("stored index not found: %v", err)
	}
}

func TestScormExtractRejectsMissingManifest(t *testing.T) {
	pool := testPool(t)
	store := NewScormStore(pool, files.NewMemoryStore(), audit.NewPGLogger(pool))
	zipBytes := buildScormZip(t, map[string]string{"index.html": "<html></html>"})
	if _, err := store.ExtractPackage(context.Background(), uuid.New(), uuid.New(), zipBytes); err == nil {
		t.Fatal("expected error for missing manifest")
	}
}

func TestScormExtractRejectsTooManyFiles(t *testing.T) {
	pool := testPool(t)
	store := NewScormStore(pool, files.NewMemoryStore(), audit.NewPGLogger(pool))
	entries := map[string]string{"imsmanifest.xml": sampleManifest12}
	for i := 0; i <= maxScormFiles; i++ { // exceed the entry-count cap
		entries[fmt.Sprintf("f%d.txt", i)] = "x"
	}
	zipBytes := buildScormZip(t, entries)
	_, err := store.ExtractPackage(context.Background(), uuid.New(), uuid.New(), zipBytes)
	if !errors.Is(err, ErrInvalidScormPackage) {
		t.Fatalf("expected ErrInvalidScormPackage for entry-count bomb, got %v", err)
	}
}

type stubEnrollments struct{ rows []EnrollmentRow }

func (s stubEnrollments) CourseEnrollments(_ context.Context, _, _ uuid.UUID) ([]EnrollmentRow, error) {
	return s.rows, nil
}

type stubLessons struct{ ids []uuid.UUID }

func (s stubLessons) CourseLessonIDs(_ context.Context, _, _ uuid.UUID) ([]uuid.UUID, error) {
	return s.ids, nil
}
