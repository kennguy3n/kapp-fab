package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// recruitmentHandlers exposes the Session 16 recruitment HTTP surface
// (/api/v1/hr/recruitment/*). Tenant scope, feature gating
// (FeatureRecruitment), authz and idempotency are enforced by the
// middleware stack in routes.go; these handlers translate HTTP into
// hr.RecruitmentStore calls and map the package's sentinel errors to the
// status codes the web client expects.
type recruitmentHandlers struct {
	store *hr.RecruitmentStore
}

// ---------------------------------------------------------------------------
// Job openings
// ---------------------------------------------------------------------------

type jobOpeningRequest struct {
	Title           string           `json:"title"`
	Department      string           `json:"department,omitempty"`
	Description     string           `json:"description,omitempty"`
	Requirements    string           `json:"requirements,omitempty"`
	EmploymentType  string           `json:"employment_type,omitempty"`
	Location        string           `json:"location,omitempty"`
	SalaryRangeMin  *decimal.Decimal `json:"salary_range_min,omitempty"`
	SalaryRangeMax  *decimal.Decimal `json:"salary_range_max,omitempty"`
	Currency        string           `json:"currency,omitempty"`
	HiringManagerID *uuid.UUID       `json:"hiring_manager_id,omitempty"`
	MaxPositions    int              `json:"max_positions,omitempty"`
	ClosesAt        *time.Time       `json:"closes_at,omitempty"`
}

func (h *recruitmentHandlers) createJobOpening(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req jobOpeningRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.CreateJobOpening(r.Context(), t.ID, actorOrDefault(r.Context()), hr.CreateJobOpeningInput{
		Title:           req.Title,
		Department:      req.Department,
		Description:     req.Description,
		Requirements:    req.Requirements,
		EmploymentType:  req.EmploymentType,
		Location:        req.Location,
		SalaryRangeMin:  req.SalaryRangeMin,
		SalaryRangeMax:  req.SalaryRangeMax,
		Currency:        req.Currency,
		HiringManagerID: req.HiringManagerID,
		MaxPositions:    req.MaxPositions,
		ClosesAt:        req.ClosesAt,
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *recruitmentHandlers) listJobOpenings(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out, err := h.store.ListJobOpenings(r.Context(), t.ID, hr.JobOpeningFilter{
		Status:     r.URL.Query().Get("status"),
		Department: r.URL.Query().Get("department"),
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *recruitmentHandlers) getJobOpening(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	out, err := h.store.GetJobOpening(r.Context(), t.ID, id)
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *recruitmentHandlers) updateJobOpening(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req jobOpeningRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.UpdateJobOpening(r.Context(), t.ID, actorOrDefault(r.Context()), id, hr.UpdateJobOpeningInput{
		Title:           req.Title,
		Department:      req.Department,
		Description:     req.Description,
		Requirements:    req.Requirements,
		EmploymentType:  req.EmploymentType,
		Location:        req.Location,
		SalaryRangeMin:  req.SalaryRangeMin,
		SalaryRangeMax:  req.SalaryRangeMax,
		Currency:        req.Currency,
		HiringManagerID: req.HiringManagerID,
		MaxPositions:    req.MaxPositions,
		ClosesAt:        req.ClosesAt,
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *recruitmentHandlers) publishJobOpening(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	out, err := h.store.PublishJobOpening(r.Context(), t.ID, actorOrDefault(r.Context()), id)
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *recruitmentHandlers) closeJobOpening(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	out, err := h.store.CloseJobOpening(r.Context(), t.ID, actorOrDefault(r.Context()), id)
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Applications
// ---------------------------------------------------------------------------

type createApplicationRequest struct {
	JobOpeningID       uuid.UUID  `json:"job_opening_id"`
	ApplicantName      string     `json:"applicant_name"`
	ApplicantEmail     string     `json:"applicant_email,omitempty"`
	Phone              string     `json:"phone,omitempty"`
	ResumeFileID       *uuid.UUID `json:"resume_file_id,omitempty"`
	CoverLetter        string     `json:"cover_letter,omitempty"`
	Source             string     `json:"source,omitempty"`
	ReferrerEmployeeID *uuid.UUID `json:"referrer_employee_id,omitempty"`
	Rating             *int       `json:"rating,omitempty"`
	Notes              string     `json:"notes,omitempty"`
}

func (h *recruitmentHandlers) createApplication(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createApplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.CreateApplication(r.Context(), t.ID, actorOrDefault(r.Context()), hr.CreateApplicationInput{
		JobOpeningID:       req.JobOpeningID,
		ApplicantName:      req.ApplicantName,
		ApplicantEmail:     req.ApplicantEmail,
		Phone:              req.Phone,
		ResumeFileID:       req.ResumeFileID,
		CoverLetter:        req.CoverLetter,
		Source:             req.Source,
		ReferrerEmployeeID: req.ReferrerEmployeeID,
		Rating:             req.Rating,
		Notes:              req.Notes,
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *recruitmentHandlers) listApplications(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var openingID uuid.UUID
	if raw := r.URL.Query().Get("job_opening_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "job_opening_id must be a valid UUID", http.StatusBadRequest)
			return
		}
		openingID = parsed
	}
	out, err := h.store.ListApplications(r.Context(), t.ID, hr.ApplicationFilter{
		JobOpeningID: openingID,
		Status:       r.URL.Query().Get("status"),
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *recruitmentHandlers) getApplication(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	out, err := h.store.GetApplication(r.Context(), t.ID, id)
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type updateApplicationRequest struct {
	ApplicantName      string     `json:"applicant_name"`
	ApplicantEmail     string     `json:"applicant_email,omitempty"`
	Phone              string     `json:"phone,omitempty"`
	ResumeFileID       *uuid.UUID `json:"resume_file_id,omitempty"`
	CoverLetter        string     `json:"cover_letter,omitempty"`
	Source             string     `json:"source,omitempty"`
	ReferrerEmployeeID *uuid.UUID `json:"referrer_employee_id,omitempty"`
	Rating             *int       `json:"rating,omitempty"`
	Notes              string     `json:"notes,omitempty"`
}

func (h *recruitmentHandlers) updateApplication(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req updateApplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.UpdateApplication(r.Context(), t.ID, actorOrDefault(r.Context()), id, hr.UpdateApplicationInput{
		ApplicantName:      req.ApplicantName,
		ApplicantEmail:     req.ApplicantEmail,
		Phone:              req.Phone,
		ResumeFileID:       req.ResumeFileID,
		CoverLetter:        req.CoverLetter,
		Source:             req.Source,
		ReferrerEmployeeID: req.ReferrerEmployeeID,
		Rating:             req.Rating,
		Notes:              req.Notes,
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type advanceApplicationRequest struct {
	Status string `json:"status"`
}

func (h *recruitmentHandlers) advanceApplication(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req advanceApplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.AdvanceApplication(r.Context(), t.ID, id, req.Status, actorOrDefault(r.Context()))
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type rejectApplicationRequest struct {
	Reason string `json:"reason,omitempty"`
}

func (h *recruitmentHandlers) rejectApplication(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req rejectApplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.RejectApplication(r.Context(), t.ID, actorOrDefault(r.Context()), id, req.Reason)
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Interviews
// ---------------------------------------------------------------------------

type createInterviewRequest struct {
	ApplicationID   uuid.UUID  `json:"application_id"`
	InterviewerID   *uuid.UUID `json:"interviewer_id,omitempty"`
	InterviewType   string     `json:"interview_type,omitempty"`
	ScheduledAt     *time.Time `json:"scheduled_at,omitempty"`
	DurationMinutes int        `json:"duration_minutes,omitempty"`
	Location        string     `json:"location,omitempty"`
	MeetingLink     string     `json:"meeting_link,omitempty"`
}

func (h *recruitmentHandlers) createInterview(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createInterviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.CreateInterview(r.Context(), t.ID, actorOrDefault(r.Context()), hr.CreateInterviewInput{
		ApplicationID:   req.ApplicationID,
		InterviewerID:   req.InterviewerID,
		InterviewType:   req.InterviewType,
		ScheduledAt:     req.ScheduledAt,
		DurationMinutes: req.DurationMinutes,
		Location:        req.Location,
		MeetingLink:     req.MeetingLink,
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *recruitmentHandlers) listInterviews(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var appID uuid.UUID
	if raw := r.URL.Query().Get("application_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "application_id must be a valid UUID", http.StatusBadRequest)
			return
		}
		appID = parsed
	}
	out, err := h.store.ListInterviews(r.Context(), t.ID, hr.InterviewFilter{
		ApplicationID: appID,
		Status:        r.URL.Query().Get("status"),
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type completeInterviewRequest struct {
	Rating         *int   `json:"rating,omitempty"`
	Feedback       string `json:"feedback,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
}

func (h *recruitmentHandlers) completeInterview(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req completeInterviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.CompleteInterview(r.Context(), t.ID, actorOrDefault(r.Context()), id, hr.CompleteInterviewInput{
		Rating:         req.Rating,
		Feedback:       req.Feedback,
		Recommendation: req.Recommendation,
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Offer letters
// ---------------------------------------------------------------------------

type createOfferLetterRequest struct {
	ApplicationID      uuid.UUID        `json:"application_id"`
	EmployeeTemplateID *uuid.UUID       `json:"employee_template_id,omitempty"`
	Designation        string           `json:"designation,omitempty"`
	Department         string           `json:"department,omitempty"`
	Salary             *decimal.Decimal `json:"salary,omitempty"`
	Currency           string           `json:"currency,omitempty"`
	JoiningDate        *time.Time       `json:"joining_date,omitempty"`
	ProbationMonths    int              `json:"probation_months,omitempty"`
	Benefits           json.RawMessage  `json:"benefits,omitempty"`
	ValidUntil         *time.Time       `json:"valid_until,omitempty"`
}

func (h *recruitmentHandlers) createOfferLetter(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createOfferLetterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.CreateOfferLetter(r.Context(), t.ID, actorOrDefault(r.Context()), hr.CreateOfferLetterInput{
		ApplicationID:      req.ApplicationID,
		EmployeeTemplateID: req.EmployeeTemplateID,
		Designation:        req.Designation,
		Department:         req.Department,
		Salary:             req.Salary,
		Currency:           req.Currency,
		JoiningDate:        req.JoiningDate,
		ProbationMonths:    req.ProbationMonths,
		Benefits:           req.Benefits,
		ValidUntil:         req.ValidUntil,
	})
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *recruitmentHandlers) listOfferLetters(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var appID uuid.UUID
	if raw := r.URL.Query().Get("application_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "application_id must be a valid UUID", http.StatusBadRequest)
			return
		}
		appID = parsed
	}
	out, err := h.store.ListOfferLetters(r.Context(), t.ID, appID)
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// offerLetterResponse is the send-offer payload. It carries the (possibly
// still-draft) offer and the opened approval — when an approval is
// returned the offer stays in 'draft' until the hiring manager grants it.
type offerLetterResponse struct {
	Offer    any `json:"offer"`
	Approval any `json:"approval,omitempty"`
}

func (h *recruitmentHandlers) sendOfferLetter(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	offer, approval, err := h.store.SendOfferLetter(r.Context(), t.ID, actorOrDefault(r.Context()), id)
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	resp := offerLetterResponse{Offer: offer}
	if approval != nil {
		resp.Approval = approval
	}
	writeJSON(w, http.StatusOK, resp)
}

type respondOfferRequest struct {
	Response string `json:"response"`
}

func (h *recruitmentHandlers) respondOfferLetter(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req respondOfferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.RespondToOffer(r.Context(), t.ID, actorOrDefault(r.Context()), id, req.Response)
	if err != nil {
		writeRecruitmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writeRecruitmentError maps the recruitment package's sentinel errors to
// HTTP status codes consistent with the rest of the API surface.
func writeRecruitmentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hr.ErrRecruitNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, hr.ErrInvalidTransition),
		errors.Is(err, hr.ErrOpeningNotPublishable),
		errors.Is(err, hr.ErrOpeningFull),
		errors.Is(err, hr.ErrOfferNotApproved),
		errors.Is(err, hr.ErrInterviewNotScheduled):
		// State-machine conflicts the client can resolve by retrying
		// from a valid state → 409.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, hr.ErrRecruitInvalidInput),
		errors.Is(err, hr.ErrInvalidStatus):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
