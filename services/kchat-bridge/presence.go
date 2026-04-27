package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// FeatureKeyAttendanceKChatSync is the tenant_features flag that
// gates auto-attendance creation from KChat presence events.
// Tenants without this enabled receive a no-op response (204) so
// upstream KChat isn't repeatedly retrying with backoff.
const FeatureKeyAttendanceKChatSync = "attendance_kchat_sync"

// presenceWebhookPayload is the body KChat POSTs to
// /kchat/presence whenever a user transitions in or out of an
// "online" state. Only state == "online" creates an attendance
// record; other transitions (idle, offline) are ignored. The
// payload is intentionally narrow — the bridge does not depend on
// presence channels, sub-rooms, or any other field KChat may
// extend its envelope with later.
type presenceWebhookPayload struct {
	UserID    string    `json:"user_id"`
	State     string    `json:"state"`
	Timestamp time.Time `json:"timestamp"`
}

// PresenceHandler handles inbound KChat presence webhooks. The
// handler resolves the kchat user → kapp user → tenant memberships,
// then for each membership where the attendance_kchat_sync feature
// flag is enabled, finds the matching hr.employee KRecord (by email
// of the user, case-insensitive) and upserts a per-day hr.attendance
// row with status=present + source=kchat.
//
// The handler is idempotent — repeating the same presence event
// inside the same UTC day re-uses the same attendance record. We
// deliberately avoid a unique constraint on (employee_id, date)
// because attendance records are KRecords (no per-ktype schema in
// SQL), so idempotency is enforced at the application layer via
// ListByField.

// PresenceTenantStore is the subset of *tenant.PGStore the
// presence handler needs to resolve a tenant's wall-clock
// timezone for shift-arithmetic. Defined here as an interface so
// tests can substitute a fixed-string fake without standing up a
// real Postgres tenant row.
type PresenceTenantStore interface {
	Timezone(ctx context.Context, id uuid.UUID) (string, error)
}

type PresenceHandler struct {
	users    *tenant.UserStore
	features *tenant.FeatureStore
	tenants  PresenceTenantStore
	records  *record.PGStore
	now      func() time.Time
}

// NewPresenceHandler constructs a handler with sensible defaults.
// `now` is injectable so the integration test fixes the clock and
// asserts the date partition deterministically. `tenants` may be
// nil (older callers / smoke tests); the late-arrival path then
// falls back to UTC for shift-start parsing.
func NewPresenceHandler(users *tenant.UserStore, features *tenant.FeatureStore, tenants PresenceTenantStore, records *record.PGStore) *PresenceHandler {
	return &PresenceHandler{
		users:    users,
		features: features,
		tenants:  tenants,
		records:  records,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// HandleHTTP is the chi-compatible handler wired into the bridge's
// router at POST /kchat/presence. Returns 204 on a successful
// no-op, 200 with a JSON summary on a successful upsert, and 400
// for malformed payloads. Internal errors return 500 so KChat's
// retry queue picks them up.
func (h *PresenceHandler) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	if r == nil || h == nil {
		http.Error(w, "presence handler not configured", http.StatusInternalServerError)
		return
	}
	var p presenceWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, fmt.Sprintf("decode presence payload: %v", err), http.StatusBadRequest)
		return
	}
	if p.UserID == "" {
		http.Error(w, "user_id required", http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(p.State, "online") {
		// Only online transitions trigger attendance. We swallow
		// idle/offline so the upstream caller does not see a 4xx
		// and retry — it's an accepted outcome of the policy.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	when := p.Timestamp
	if when.IsZero() {
		when = h.now()
	}

	summaries, err := h.process(r.Context(), p.UserID, when)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(summaries) == 0 {
		// No matching tenant/employee/feature — successful no-op.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"upserts": summaries})
}

// presenceUpsert is the per-tenant outcome the handler returns to
// the caller for observability. Used by the integration test as
// well as by KChat operators inspecting webhook delivery logs.
type presenceUpsert struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	EmployeeID uuid.UUID `json:"employee_id"`
	RecordID   uuid.UUID `json:"record_id"`
	Date       string    `json:"date"`
	Created    bool      `json:"created"`
}

// process is the testable core of HandleHTTP: given a kchat user id
// and a timestamp, walk every tenant the user belongs to and upsert
// the attendance record where the flag is enabled. Errors from a
// single tenant are NOT fatal — we keep going so a misconfigured
// tenant doesn't deny presence-sync to its peers.
func (h *PresenceHandler) process(ctx context.Context, kchatUserID string, when time.Time) ([]presenceUpsert, error) {
	if h.users == nil || h.features == nil || h.records == nil {
		return nil, errors.New("presence: handler not wired")
	}
	user, err := h.users.GetUserByKChatID(ctx, kchatUserID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("presence: lookup user: %w", err)
	}
	if user.Email == "" {
		// Without an email we can't match an employee — bail out
		// rather than guess. Operators can manually attach the
		// email via /api/v1/admin/users.
		return nil, nil
	}
	memberships, err := h.users.GetUserTenants(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("presence: load memberships: %w", err)
	}
	if len(memberships) == 0 {
		return nil, nil
	}
	dateKey := when.UTC().Format("2006-01-02")
	out := make([]presenceUpsert, 0, len(memberships))
	for _, m := range memberships {
		if m.Status != "active" {
			continue
		}
		on, err := h.features.IsEnabled(ctx, m.TenantID, FeatureKeyAttendanceKChatSync)
		if err != nil || !on {
			continue
		}
		emp, err := h.findEmployee(ctx, m.TenantID, user.Email)
		if err != nil || emp == nil {
			continue
		}
		rec, created, err := h.upsertAttendance(ctx, m.TenantID, *emp, user.ID, when, dateKey)
		if err != nil {
			continue
		}
		out = append(out, presenceUpsert{
			TenantID:   m.TenantID,
			EmployeeID: *emp,
			RecordID:   rec,
			Date:       dateKey,
			Created:    created,
		})
	}
	return out, nil
}

// findEmployee resolves the hr.employee KRecord whose email matches
// the supplied address. Returns nil when no employee exists for the
// tenant — that's a normal case (e.g. tenants who have not yet
// onboarded an HR module) so the caller treats it as a skip.
func (h *PresenceHandler) findEmployee(ctx context.Context, tenantID uuid.UUID, email string) (*uuid.UUID, error) {
	rows, err := h.records.ListByField(ctx, tenantID, record.ListFilter{KType: hr.KTypeEmployee}, "email", email)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	id := rows[0].ID
	return &id, nil
}

// upsertAttendance ensures an hr.attendance KRecord exists for the
// (employee, date) tuple. Returns the resulting record id and a
// `created` flag so the caller can distinguish a new record from
// an idempotent re-application. Re-applying never updates the
// existing record's check_in — the first online event of the day
// wins.
func (h *PresenceHandler) upsertAttendance(ctx context.Context, tenantID uuid.UUID, employeeID, actorID uuid.UUID, when time.Time, dateKey string) (uuid.UUID, bool, error) {
	existing, err := h.records.ListByField(ctx, tenantID, record.ListFilter{KType: hr.KTypeAttendance}, "employee_id", employeeID.String())
	if err != nil {
		return uuid.Nil, false, err
	}
	for _, e := range existing {
		var data map[string]any
		if e.Data != nil {
			_ = json.Unmarshal(e.Data, &data)
		}
		if data == nil {
			continue
		}
		if d, _ := data["date"].(string); d == dateKey {
			return e.ID, false, nil
		}
	}
	body := map[string]any{
		"employee_id": employeeID.String(),
		"date":        dateKey,
		"status":      "present",
		"source":      "kchat",
		"check_in":    when.UTC().Format(time.RFC3339),
	}
	// Phase M shift cross-reference. If the employee has a
	// shift_assignment for this date, decorate the attendance
	// record with the expected start_time + a `late` flag the
	// scheduling UI can highlight. A missing assignment, missing
	// shift_type, or unparseable start_time is non-fatal — we
	// fall back to a plain `present` row so the existing flow
	// keeps working for tenants that haven't enabled shift
	// scheduling yet.
	if late, expectedStart, tardyMins, found := h.evaluateLateArrival(ctx, tenantID, employeeID, dateKey, when); found {
		body["expected_start"] = expectedStart
		body["late"] = late
		if late {
			body["tardy_minutes"] = tardyMins
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return uuid.Nil, false, err
	}
	created, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     hr.KTypeAttendance,
		Data:      raw,
		CreatedBy: actorID,
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	return created.ID, true, nil
}

// evaluateLateArrival looks up the employee's shift_assignment for
// dateKey, resolves the matching shift_type, and decides whether
// `when` constitutes a late arrival relative to the shift's
// start_time.
//
// `found=false` means there is no assignment, the assignment
// references a missing shift_type, or the start_time field can't
// be parsed — all non-fatal cases the caller silently skips.
//
// `late=true` requires `when` (UTC) to be strictly later than the
// shift start at the assignment's calendar date. We use a 5-minute
// grace window so a check-in at exactly the shift start counts as
// on-time. tardyMinutes returns the integer minutes past start.
//
// The presence sweeper invokes this once per attendance upsert so
// the cost stays bounded by employee count, not shift count.
func (h *PresenceHandler) evaluateLateArrival(
	ctx context.Context,
	tenantID uuid.UUID,
	employeeID uuid.UUID,
	dateKey string,
	when time.Time,
) (late bool, expectedStart string, tardyMins int, found bool) {
	const graceMinutes = 5

	// Resolve tenant timezone up-front. We use it twice:
	//   1) to derive `localDateKey` for the shift_date comparison,
	//      since shift_date stores the tenant's local calendar
	//      date (e.g. "2026-04-15" = April 15 wall-clock) but the
	//      `dateKey` parameter is UTC-derived from upsertAttendance
	//      and would mis-match for non-UTC tenants near day
	//      boundaries (a NY 23:30 ET check-in resolves to UTC
	//      April 16, missing the April 15 assignment);
	//   2) to interpret shift_type.start_time (also wall-clock)
	//      via parseShiftStart below.
	// Falling back to UTC keeps the legacy behaviour for tenants
	// that never set a timezone or whose timezone string is bad.
	tz := "UTC"
	if h.tenants != nil {
		if got, terr := h.tenants.Timezone(ctx, tenantID); terr == nil && got != "" {
			tz = got
		}
	}
	localDateKey := localCalendarDate(when, tz)

	assignments, err := h.records.ListByField(ctx, tenantID, record.ListFilter{KType: hr.KTypeShiftAssignment}, "employee_id", employeeID.String())
	if err != nil || len(assignments) == 0 {
		return false, "", 0, false
	}
	// Collect every non-cancelled assignment for this date. A
	// single match is the common case (single shift); split-shift
	// employees may have two or three (Morning + Evening + on-call
	// in the pathological case). We deliberately do NOT break on
	// first match because ListByField returns rows ordered by
	// updated_at DESC, which means re-saving Morning would steal
	// match priority away from a still-relevant Evening shift.
	var shiftTypeIDs []string
	for _, a := range assignments {
		if a.Data == nil {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(a.Data, &data); err != nil {
			continue
		}
		if d, _ := data["shift_date"].(string); d != localDateKey {
			continue
		}
		if status, _ := data["status"].(string); status == "cancelled" {
			continue
		}
		if id, _ := data["shift_type_id"].(string); id != "" {
			shiftTypeIDs = append(shiftTypeIDs, id)
		}
	}
	if len(shiftTypeIDs) == 0 {
		return false, "", 0, false
	}

	// Resolve every candidate's start_time and pick the assignment
	// whose start is the latest value <= the local check-in time
	// (the "shift the employee just clocked in for"). If no shift
	// has started yet — e.g. a 05:55 check-in for the day's first
	// 06:00 Morning slot — fall through to the earliest upcoming
	// shift so the standard grace-window late-arrival logic still
	// fires correctly. This mirrors the calendar UI's
	// indexAssignments sort (by start_time) so server-side
	// detection and the operator-facing grid agree on which shift
	// is "the" shift for a given check-in.
	candidates := make([]shiftCandidate, 0, len(shiftTypeIDs))
	for _, id := range shiftTypeIDs {
		stID, perr := uuid.Parse(id)
		if perr != nil {
			continue
		}
		stRec, gerr := h.records.Get(ctx, tenantID, stID)
		if gerr != nil || stRec == nil {
			continue
		}
		var stData map[string]any
		if jerr := json.Unmarshal(stRec.Data, &stData); jerr != nil {
			continue
		}
		startStr, _ := stData["start_time"].(string)
		if startStr == "" {
			continue
		}
		shiftStart, sok := parseShiftStart(localDateKey, startStr, tz)
		if !sok {
			continue
		}
		candidates = append(candidates, shiftCandidate{StartStr: startStr, StartUTC: shiftStart})
	}
	pick, ok := pickShiftForCheckIn(candidates, when.UTC())
	if !ok {
		return false, "", 0, false
	}
	startStr := pick.StartStr
	shiftStart := pick.StartUTC
	delta := when.UTC().Sub(shiftStart)
	if delta <= time.Duration(graceMinutes)*time.Minute {
		return false, startStr, 0, true
	}
	mins := int(delta / time.Minute)
	return true, startStr, mins, true
}

// shiftCandidate carries the resolved start info for one of the
// employee's same-day assignments. We collect every candidate up
// front and let pickShiftForCheckIn choose the one closest to the
// check-in time, rather than letting ListByField's updated_at DESC
// ordering decide for us.
type shiftCandidate struct {
	StartStr string
	StartUTC time.Time
}

// pickShiftForCheckIn returns the candidate the employee is most
// likely clocking in for, given multiple same-day shifts:
//
//   - If any shift has already started (StartUTC <= checkInUTC),
//     pick the most recent of those — this matches the "Evening
//     started 5 min ago" intent over a Morning shift that ended
//     hours earlier. tardyMinutes then computes against that
//     start, which is what an HR operator expects to see on a
//     split-shift employee.
//
//   - If no shift has started yet — i.e. the employee is early
//     for the day's first shift — pick the earliest upcoming so
//     the standard 5-minute grace window still triggers correctly
//     for early arrivals.
//
// Returns ok=false on an empty input.
func pickShiftForCheckIn(candidates []shiftCandidate, checkInUTC time.Time) (shiftCandidate, bool) {
	var (
		best     shiftCandidate
		bestKind int // 0 = none yet, 1 = upcoming, 2 = started
	)
	for _, c := range candidates {
		started := !c.StartUTC.After(checkInUTC)
		switch {
		case started && (bestKind != 2 || c.StartUTC.After(best.StartUTC)):
			best = c
			bestKind = 2
		case !started && bestKind == 0:
			best = c
			bestKind = 1
		case !started && bestKind == 1 && c.StartUTC.Before(best.StartUTC):
			best = c
		}
	}
	if bestKind == 0 {
		return shiftCandidate{}, false
	}
	return best, true
}

// localCalendarDate returns the YYYY-MM-DD wall-clock date that
// `when` falls on in the supplied IANA timezone. Used by
// evaluateLateArrival to compare against the tenant-local
// shift_date field on hr.shift_assignment records, which would
// otherwise mismatch for non-UTC tenants whose check-in time
// crosses a UTC day boundary. Falls back to UTC when the
// timezone string isn't recognised so a misconfigured
// tenants.timezone can't break the late-arrival path entirely.
func localCalendarDate(when time.Time, tz string) string {
	loc := time.UTC
	if tz != "" {
		if parsed, err := time.LoadLocation(tz); err == nil {
			loc = parsed
		}
	}
	return when.In(loc).Format("2006-01-02")
}

// parseShiftStart parses a (YYYY-MM-DD, HH:MM) pair in the supplied
// IANA timezone and returns the UTC instant. Falls back to UTC if
// the timezone string isn't recognised by time.LoadLocation so an
// operator-misconfigured tenant.timezone can't cause attendance
// upserts to error out — the late-arrival window just becomes less
// accurate. Returns ok=false on a malformed date or HH:MM.
func parseShiftStart(dateKey, startStr, tz string) (time.Time, bool) {
	if strings.Count(dateKey, "-") != 2 {
		return time.Time{}, false
	}
	loc := time.UTC
	if tz != "" {
		if parsed, err := time.LoadLocation(tz); err == nil {
			loc = parsed
		}
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", dateKey+" "+startStr, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
