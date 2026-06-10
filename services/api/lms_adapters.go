package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// lmsRecordAdapters bridges the generic record.PGStore to the typed
// interfaces the Session-17 LMS stores depend on (analytics listers,
// xAPI actor/enrollment resolvers, learning-path completion lookup).
// Keeping the adapters at the API layer is what lets internal/lms stay
// decoupled from the record store and from the user directory.
type lmsRecordAdapters struct {
	records *record.PGStore
}

// listFilterAll is the bounded full-scan filter the adapters use. The
// matching set per (course, field) is small for SME tenants — a course
// has tens of modules/lessons and hundreds of enrollments — so a single
// ListByField call stays well under record.ListAllMaxRows.
func listFilter(ktype string) record.ListFilter {
	return record.ListFilter{KType: ktype, Limit: 0}
}

// enrollmentFields is the subset of an lms.enrollment KRecord the
// adapters read.
type enrollmentFields struct {
	UserID   string `json:"user_id"`
	CourseID string `json:"course_id"`
	Status   string `json:"status"`
}

type moduleFields struct {
	CourseID string `json:"course_id"`
}

type lessonFields struct {
	ModuleID string `json:"module_id"`
}

type userFields struct {
	Email string `json:"email"`
}

// CourseEnrollments implements lms.EnrollmentLister.
func (a lmsRecordAdapters) CourseEnrollments(ctx context.Context, tenantID, courseID uuid.UUID) ([]lms.EnrollmentRow, error) {
	recs, err := a.records.ListByField(ctx, tenantID, listFilter(lms.KTypeEnrollment), "course_id", courseID.String())
	if err != nil {
		return nil, err
	}
	out := make([]lms.EnrollmentRow, 0, len(recs))
	for _, rec := range recs {
		var f enrollmentFields
		if err := json.Unmarshal(rec.Data, &f); err != nil {
			continue
		}
		userID, _ := uuid.Parse(f.UserID)
		out = append(out, lms.EnrollmentRow{
			EnrollmentID: rec.ID,
			UserID:       userID,
			Status:       f.Status,
		})
	}
	return out, nil
}

// CourseLessonIDs implements lms.LessonLister. It walks the course's
// modules then each module's lessons. Lesson ordering is irrelevant to
// the analytics rollup so the flat id set is sufficient.
func (a lmsRecordAdapters) CourseLessonIDs(ctx context.Context, tenantID, courseID uuid.UUID) ([]uuid.UUID, error) {
	modules, err := a.records.ListByField(ctx, tenantID, listFilter(lms.KTypeModule), "course_id", courseID.String())
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0)
	for _, m := range modules {
		lessons, err := a.records.ListByField(ctx, tenantID, listFilter(lms.KTypeLesson), "module_id", m.ID.String())
		if err != nil {
			return nil, err
		}
		for _, l := range lessons {
			out = append(out, l.ID)
		}
	}
	return out, nil
}

// ResolveActor implements lms.ActorResolver. An xAPI account identity of
// the form "homePage|name" where name is a Kapp user UUID resolves
// directly (the common case for Kapp-issued statements). Otherwise the
// mbox email is matched against the `user` KType. Unknown actors return
// ok=false so the statement is still stored, just without a user link.
func (a lmsRecordAdapters) ResolveActor(ctx context.Context, tenantID uuid.UUID, identity string) (uuid.UUID, bool, error) {
	if identity == "" {
		return uuid.Nil, false, nil
	}
	if i := strings.LastIndex(identity, "|"); i >= 0 {
		if id, err := uuid.Parse(identity[i+1:]); err == nil {
			return id, true, nil
		}
	}
	// Treat the identity as an email and match the user directory.
	recs, err := a.records.ListByField(ctx, tenantID, listFilter("user"), "email", identity)
	if err != nil {
		// A missing `user` KType (users live in iam-core for some
		// deployments) is not fatal — the statement is stored unlinked.
		return uuid.Nil, false, nil
	}
	for _, rec := range recs {
		var f userFields
		if err := json.Unmarshal(rec.Data, &f); err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(f.Email), identity) {
			return rec.ID, true, nil
		}
	}
	return uuid.Nil, false, nil
}

// EnrollmentForLesson resolves the lms.enrollment that links a user to
// the course owning a lesson, so an xAPI statement can be projected
// onto the right lesson_progress row. ok=false when the user is not
// enrolled in the lesson's course.
func (a lmsRecordAdapters) EnrollmentForLesson(ctx context.Context, tenantID, lessonID, userID uuid.UUID) (uuid.UUID, bool, error) {
	lesson, err := a.records.Get(ctx, tenantID, lessonID)
	if err != nil || lesson == nil {
		return uuid.Nil, false, nil
	}
	var lf lessonFields
	if err := json.Unmarshal(lesson.Data, &lf); err != nil {
		return uuid.Nil, false, nil
	}
	moduleID, err := uuid.Parse(lf.ModuleID)
	if err != nil {
		return uuid.Nil, false, nil
	}
	module, err := a.records.Get(ctx, tenantID, moduleID)
	if err != nil || module == nil {
		return uuid.Nil, false, nil
	}
	var mf moduleFields
	if err := json.Unmarshal(module.Data, &mf); err != nil {
		return uuid.Nil, false, nil
	}
	enrolls, err := a.records.ListByField(ctx, tenantID, listFilter(lms.KTypeEnrollment), "user_id", userID.String())
	if err != nil {
		return uuid.Nil, false, nil
	}
	for _, e := range enrolls {
		var ef enrollmentFields
		if err := json.Unmarshal(e.Data, &ef); err != nil {
			continue
		}
		if ef.CourseID == mf.CourseID {
			return e.ID, true, nil
		}
	}
	return uuid.Nil, false, nil
}

// CompletedCoursesForUser returns the set of course ids the user has a
// completed enrollment for. Drives learning-path completion recompute.
func (a lmsRecordAdapters) CompletedCoursesForUser(ctx context.Context, tenantID, userID uuid.UUID) (map[uuid.UUID]bool, error) {
	enrolls, err := a.records.ListByField(ctx, tenantID, listFilter(lms.KTypeEnrollment), "user_id", userID.String())
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool)
	for _, e := range enrolls {
		var ef enrollmentFields
		if err := json.Unmarshal(e.Data, &ef); err != nil {
			continue
		}
		if ef.Status != lms.ProgressCompleted {
			continue
		}
		if cid, err := uuid.Parse(ef.CourseID); err == nil {
			out[cid] = true
		}
	}
	return out, nil
}
