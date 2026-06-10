package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// maxScormUploadBytes caps a SCORM package upload. SCORM courses are
// typically a few MB; 256 MB is a generous ceiling that still protects
// the extractor (and the tenant object store) from a hostile upload.
const maxScormUploadBytes = 256 << 20

// lmsHandlers exposes the Session-17 LMS REST surface under
// /api/v1/lms. Every mutating call inherits the route group's
// Idempotency + rate-limit + quota middleware; the stores themselves
// audit each mutation and enforce tenant RLS via platform.WithTenantTx.
type lmsHandlers struct {
	paths     *lms.LearningPathStore
	enroller  *lms.PathAutoEnroller
	gamify    *lms.GamificationStore
	discuss   *lms.DiscussionStore
	scorm     *lms.ScormStore
	xapi      *lms.XAPIStore
	analytics *lms.AnalyticsStore
	adapters  lmsRecordAdapters
}

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

func (h *lmsHandlers) tenantID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return uuid.Nil, false
	}
	return t.ID, true
}

func (h *lmsHandlers) actor(r *http.Request) *uuid.UUID {
	a := actorOrDefault(r.Context())
	return &a
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		http.Error(w, "invalid "+name, http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

// writeLMSError maps the lms sentinel errors onto HTTP status codes so
// validation failures surface as 400 and missing rows as 404 instead of
// a blanket 500.
func writeLMSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, lms.ErrLearningPathNotFound),
		errors.Is(err, lms.ErrPathEnrollmentNotFound),
		errors.Is(err, lms.ErrBadgeNotFound),
		errors.Is(err, lms.ErrThreadNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, lms.ErrInvalidLearningPath),
		errors.Is(err, lms.ErrInvalidBadge),
		errors.Is(err, lms.ErrInvalidThread),
		errors.Is(err, lms.ErrInvalidReply),
		errors.Is(err, lms.ErrInvalidScormPackage),
		errors.Is(err, lms.ErrScormManifestMissing),
		errors.Is(err, lms.ErrInvalidStatement):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// Learning paths.
// ---------------------------------------------------------------------------

type createPathRequest struct {
	Title                  string   `json:"title"`
	Description            string   `json:"description"`
	Status                 string   `json:"status"`
	TargetRoles            []string `json:"target_roles"`
	EstimatedDurationHours int      `json:"estimated_duration_hours"`
	Difficulty             string   `json:"difficulty"`
}

func (h *lmsHandlers) createPath(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	var req createPathRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	path, err := h.paths.CreatePath(r.Context(), lms.LearningPath{
		TenantID:               tenantID,
		Title:                  req.Title,
		Description:            req.Description,
		Status:                 req.Status,
		TargetRoles:            req.TargetRoles,
		EstimatedDurationHours: req.EstimatedDurationHours,
		Difficulty:             req.Difficulty,
	})
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, path)
}

func (h *lmsHandlers) listPaths(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	paths, err := h.paths.ListPaths(r.Context(), tenantID, r.URL.Query().Get("status"))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"learning_paths": paths})
}

func (h *lmsHandlers) getPath(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	path, err := h.paths.GetPath(r.Context(), tenantID, id)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	courses, err := h.paths.ListCourses(r.Context(), tenantID, id)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"learning_path": path, "courses": courses})
}

func (h *lmsHandlers) updatePath(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req createPathRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	path, err := h.paths.UpdatePath(r.Context(), lms.LearningPath{
		TenantID:               tenantID,
		ID:                     id,
		Title:                  req.Title,
		Description:            req.Description,
		Status:                 req.Status,
		TargetRoles:            req.TargetRoles,
		EstimatedDurationHours: req.EstimatedDurationHours,
		Difficulty:             req.Difficulty,
	}, h.actor(r))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, path)
}

func (h *lmsHandlers) deletePath(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.paths.DeletePath(r.Context(), tenantID, id, h.actor(r)); err != nil {
		writeLMSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type addCourseRequest struct {
	CourseID              uuid.UUID   `json:"course_id"`
	SequenceOrder         int         `json:"sequence_order"`
	IsMandatory           bool        `json:"is_mandatory"`
	PrerequisiteCourseIDs []uuid.UUID `json:"prerequisite_course_ids"`
}

func (h *lmsHandlers) addPathCourse(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	pathID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req addCourseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	course, err := h.paths.AddCourse(r.Context(), lms.LearningPathCourse{
		TenantID:              tenantID,
		LearningPathID:        pathID,
		CourseID:              req.CourseID,
		SequenceOrder:         req.SequenceOrder,
		IsMandatory:           req.IsMandatory,
		PrerequisiteCourseIDs: req.PrerequisiteCourseIDs,
	}, h.actor(r))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, course)
}

func (h *lmsHandlers) removePathCourse(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	pathID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	courseID, ok := pathUUID(w, r, "courseID")
	if !ok {
		return
	}
	if err := h.paths.RemoveCourse(r.Context(), tenantID, pathID, courseID, h.actor(r)); err != nil {
		writeLMSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type enrollRequest struct {
	UserID uuid.UUID `json:"user_id"`
}

func (h *lmsHandlers) enrollPath(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	pathID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req enrollRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	userID := req.UserID
	if userID == uuid.Nil {
		userID = actorOrDefault(r.Context())
	}
	enr, err := h.paths.Enroll(r.Context(), tenantID, pathID, userID, lms.EnrollSourceManual, h.actor(r))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, enr)
}

func (h *lmsHandlers) completePath(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	pathID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req enrollRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	userID := req.UserID
	if userID == uuid.Nil {
		userID = actorOrDefault(r.Context())
	}
	completed, err := h.adapters.CompletedCoursesForUser(r.Context(), tenantID, userID)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	enr, res, err := h.paths.RecomputeCompletion(r.Context(), tenantID, pathID, userID, completed, h.actor(r))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrollment": enr, "completion": res})
}

// ---------------------------------------------------------------------------
// SCORM runtime.
// ---------------------------------------------------------------------------

func (h *lmsHandlers) scormUpload(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	lessonID, err := uuid.Parse(r.URL.Query().Get("lesson_id"))
	if err != nil {
		http.Error(w, "lesson_id query param required", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxScormUploadBytes+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxScormUploadBytes {
		http.Error(w, "package exceeds size limit", http.StatusRequestEntityTooLarge)
		return
	}
	pkg, err := h.scorm.ExtractPackage(r.Context(), tenantID, lessonID, body)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pkg)
}

type scormCommitRequest struct {
	EnrollmentID uuid.UUID   `json:"enrollment_id"`
	CMI          lms.CMIData `json:"cmi"`
}

func (h *lmsHandlers) scormInitialize(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	lessonID, ok := pathUUID(w, r, "lessonID")
	if !ok {
		return
	}
	var req scormCommitRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.EnrollmentID == uuid.Nil {
		http.Error(w, "enrollment_id required", http.StatusBadRequest)
		return
	}
	state, err := h.scorm.RuntimeState(r.Context(), tenantID, req.EnrollmentID, lessonID)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *lmsHandlers) scormCommit(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	lessonID, ok := pathUUID(w, r, "lessonID")
	if !ok {
		return
	}
	var req scormCommitRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.EnrollmentID == uuid.Nil {
		http.Error(w, "enrollment_id required", http.StatusBadRequest)
		return
	}
	progress, err := h.scorm.CommitRuntime(r.Context(), tenantID, req.EnrollmentID, lessonID, req.CMI, h.actor(r))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// scormTerminate is a final commit: the SCORM RTE sends LMSFinish after
// a last LMSCommit, so terminate persists whatever CMI state the player
// flushes and returns the resulting progress row.
func (h *lmsHandlers) scormTerminate(w http.ResponseWriter, r *http.Request) {
	h.scormCommit(w, r)
}

// ---------------------------------------------------------------------------
// xAPI receiver.
// ---------------------------------------------------------------------------

func (h *lmsHandlers) xapiStatements(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	var st lms.XAPIStatement
	if !decodeJSON(w, r, &st) {
		return
	}
	res, err := h.xapi.Ingest(r.Context(), tenantID, st, h.adapters, h.adapters.EnrollmentForLesson, h.actor(r))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------------------
// Discussions.
// ---------------------------------------------------------------------------

type createThreadRequest struct {
	CourseID uuid.UUID  `json:"course_id"`
	LessonID *uuid.UUID `json:"lesson_id,omitempty"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	Pinned   bool       `json:"pinned"`
}

func (h *lmsHandlers) createThread(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	var req createThreadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	thread, err := h.discuss.CreateThread(r.Context(), lms.DiscussionThread{
		TenantID: tenantID,
		CourseID: req.CourseID,
		LessonID: req.LessonID,
		AuthorID: actorOrDefault(r.Context()),
		Title:    req.Title,
		Body:     req.Body,
		Pinned:   req.Pinned,
	})
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, thread)
}

func (h *lmsHandlers) listThreads(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	courseID, err := uuid.Parse(r.URL.Query().Get("course_id"))
	if err != nil {
		http.Error(w, "course_id query param required", http.StatusBadRequest)
		return
	}
	threads, err := h.discuss.ListThreads(r.Context(), tenantID, courseID)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

func (h *lmsHandlers) getThread(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	thread, err := h.discuss.GetThread(r.Context(), tenantID, id)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	replies, err := h.discuss.ListReplies(r.Context(), tenantID, id)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": thread, "replies": replies})
}

type updateThreadRequest struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Status string `json:"status"`
	Pinned bool   `json:"pinned"`
}

func (h *lmsHandlers) updateThread(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	existing, err := h.discuss.GetThread(r.Context(), tenantID, id)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	var req updateThreadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	existing.Title = req.Title
	existing.Body = req.Body
	existing.Status = req.Status
	existing.Pinned = req.Pinned
	updated, err := h.discuss.UpdateThread(r.Context(), *existing, h.actor(r))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *lmsHandlers) deleteThread(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.discuss.DeleteThread(r.Context(), tenantID, id, h.actor(r)); err != nil {
		writeLMSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type addReplyRequest struct {
	Body   string `json:"body"`
	Source string `json:"source"`
}

func (h *lmsHandlers) addReply(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	threadID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req addReplyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	source := req.Source
	if source == "" {
		source = lms.ReplySourceWeb
	}
	reply, err := h.discuss.AddReply(r.Context(), lms.DiscussionReply{
		TenantID: tenantID,
		ThreadID: threadID,
		AuthorID: actorOrDefault(r.Context()),
		Body:     req.Body,
		Source:   source,
	}, h.actor(r))
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, reply)
}

func (h *lmsHandlers) markAnswer(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	threadID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	replyID, ok := pathUUID(w, r, "replyID")
	if !ok {
		return
	}
	if err := h.discuss.MarkAnswer(r.Context(), tenantID, threadID, replyID, h.actor(r)); err != nil {
		writeLMSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Gamification (badges + leaderboard).
// ---------------------------------------------------------------------------

type createBadgeRequest struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Icon          string            `json:"icon"`
	CriteriaType  string            `json:"criteria_type"`
	CriteriaValue lms.CriteriaValue `json:"criteria_value"`
	Active        *bool             `json:"active,omitempty"`
}

func (h *lmsHandlers) createBadge(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	var req createBadgeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	badge, err := h.gamify.CreateBadge(r.Context(), lms.Badge{
		TenantID:      tenantID,
		Name:          req.Name,
		Description:   req.Description,
		Icon:          req.Icon,
		CriteriaType:  req.CriteriaType,
		CriteriaValue: req.CriteriaValue,
		Active:        active,
	})
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, badge)
}

func (h *lmsHandlers) listBadges(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	activeOnly := r.URL.Query().Get("active") == "true"
	badges, err := h.gamify.ListBadges(r.Context(), tenantID, activeOnly)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"badges": badges})
}

func (h *lmsHandlers) listAwards(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	// With an explicit user_id this is a single-learner lookup; without
	// one it is the tenant-wide award history that the BadgesPage table
	// renders (per-learner rows). RLS scopes either path to the tenant.
	var (
		awards []lms.UserBadge
		err    error
	)
	if raw := r.URL.Query().Get("user_id"); raw != "" {
		userID, perr := uuid.Parse(raw)
		if perr != nil {
			http.Error(w, "invalid user_id", http.StatusBadRequest)
			return
		}
		awards, err = h.gamify.ListUserBadges(r.Context(), tenantID, userID)
	} else {
		awards, err = h.gamify.ListAwards(r.Context(), tenantID, 0)
	}
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"awards": awards})
}

// ---------------------------------------------------------------------------
// Instructor analytics.
// ---------------------------------------------------------------------------

func (h *lmsHandlers) courseAnalytics(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantID(w, r)
	if !ok {
		return
	}
	courseID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	a, err := h.analytics.CourseAnalytics(r.Context(), tenantID, courseID)
	if err != nil {
		writeLMSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
