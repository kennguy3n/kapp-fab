package hr

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRecruitmentKTypesValid pins that all four recruitment KType
// schemas are registered and carry valid JSON (the init() guard would
// panic otherwise, but an explicit test localises a regression).
func TestRecruitmentKTypesValid(t *testing.T) {
	t.Parallel()
	kts := RecruitmentKTypes()
	if len(kts) != 4 {
		t.Fatalf("expected 4 recruitment KTypes, got %d", len(kts))
	}
	wantNames := map[string]bool{
		KTypeJobOpening:     false,
		KTypeJobApplication: false,
		KTypeInterview:      false,
		KTypeOfferLetter:    false,
	}
	for _, kt := range kts {
		if _, ok := wantNames[kt.Name]; !ok {
			t.Fatalf("unexpected KType %q", kt.Name)
		}
		wantNames[kt.Name] = true
		if !json.Valid(kt.Schema) {
			t.Fatalf("KType %q has invalid JSON schema", kt.Name)
		}
		if kt.Version != 1 {
			t.Fatalf("KType %q version = %d, want 1", kt.Name, kt.Version)
		}
	}
	for name, seen := range wantNames {
		if !seen {
			t.Fatalf("KType %q missing from RecruitmentKTypes()", name)
		}
	}
}

func TestValidApplicationStatus(t *testing.T) {
	t.Parallel()
	valid := []string{
		AppStatusApplied, AppStatusScreening, AppStatusShortlisted,
		AppStatusInterview, AppStatusOffered, AppStatusHired,
		AppStatusRejected, AppStatusWithdrawn,
	}
	for _, s := range valid {
		if !validApplicationStatus(s) {
			t.Fatalf("validApplicationStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "unknown", "Applied", "HIRED"} {
		if validApplicationStatus(s) {
			t.Fatalf("validApplicationStatus(%q) = true, want false", s)
		}
	}
}

func TestCanAdvanceApplication(t *testing.T) {
	t.Parallel()
	type tc struct {
		from, to string
		want     bool
	}
	cases := []tc{
		// Idempotent re-assertion always legal.
		{AppStatusApplied, AppStatusApplied, true},
		{AppStatusHired, AppStatusHired, true},
		// Forward pipeline.
		{AppStatusApplied, AppStatusScreening, true},
		{AppStatusScreening, AppStatusShortlisted, true},
		{AppStatusShortlisted, AppStatusInterview, true},
		{AppStatusInterview, AppStatusOffered, true},
		{AppStatusOffered, AppStatusHired, true},
		// Reject / withdraw from any live stage.
		{AppStatusApplied, AppStatusRejected, true},
		{AppStatusOffered, AppStatusWithdrawn, true},
		{AppStatusInterview, AppStatusRejected, true},
		// Illegal skips.
		{AppStatusApplied, AppStatusInterview, false},
		{AppStatusApplied, AppStatusHired, false},
		{AppStatusScreening, AppStatusOffered, false},
		// No moves out of terminal states.
		{AppStatusHired, AppStatusScreening, false},
		{AppStatusRejected, AppStatusApplied, false},
		{AppStatusWithdrawn, AppStatusScreening, false},
		// Backwards is illegal.
		{AppStatusInterview, AppStatusScreening, false},
	}
	for _, c := range cases {
		if got := canAdvanceApplication(c.from, c.to); got != c.want {
			t.Fatalf("canAdvanceApplication(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestCanTransitionOpening(t *testing.T) {
	t.Parallel()
	type tc struct {
		from, to string
		want     bool
	}
	cases := []tc{
		{OpeningStatusDraft, OpeningStatusDraft, true},
		{OpeningStatusDraft, OpeningStatusOpen, true},
		{OpeningStatusDraft, OpeningStatusClosed, true},
		{OpeningStatusOpen, OpeningStatusOnHold, true},
		{OpeningStatusOpen, OpeningStatusFilled, true},
		{OpeningStatusOpen, OpeningStatusClosed, true},
		{OpeningStatusOnHold, OpeningStatusOpen, true},
		{OpeningStatusFilled, OpeningStatusClosed, true},
		// Illegal.
		{OpeningStatusDraft, OpeningStatusFilled, false},
		{OpeningStatusDraft, OpeningStatusOnHold, false},
		{OpeningStatusClosed, OpeningStatusOpen, false},
		{OpeningStatusFilled, OpeningStatusOpen, false},
	}
	for _, c := range cases {
		if got := canTransitionOpening(c.from, c.to); got != c.want {
			t.Fatalf("canTransitionOpening(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestValidRating(t *testing.T) {
	t.Parallel()
	rating := func(n int) *int { return &n }
	if err := validRating(nil); err != nil {
		t.Fatalf("nil rating must be valid: %v", err)
	}
	for _, n := range []int{1, 2, 3, 4, 5} {
		if err := validRating(rating(n)); err != nil {
			t.Fatalf("rating %d must be valid: %v", n, err)
		}
	}
	for _, n := range []int{0, -1, 6, 100} {
		if err := validRating(rating(n)); err == nil {
			t.Fatalf("rating %d must be rejected", n)
		}
	}
}

func TestValidEnumMaps(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"full_time", "part_time", "contract", "intern"} {
		if !validEmploymentType[s] {
			t.Fatalf("employment type %q must be valid", s)
		}
	}
	if validEmploymentType["permanent"] {
		t.Fatal("unknown employment type must be invalid")
	}
	for _, s := range []string{"website", "referral", "linkedin", "agency", "other"} {
		if !validApplicationSource[s] {
			t.Fatalf("application source %q must be valid", s)
		}
	}
	for _, s := range []string{"phone", "video", "in_person", "panel", "technical", "cultural"} {
		if !validInterviewType[s] {
			t.Fatalf("interview type %q must be valid", s)
		}
	}
	for _, s := range []string{"strong_yes", "yes", "neutral", "no", "strong_no"} {
		if !validRecommendation[s] {
			t.Fatalf("recommendation %q must be valid", s)
		}
	}
}

// ----- email template envelopes -----

func TestApplicationReceivedEmail(t *testing.T) {
	t.Parallel()
	app := JobApplication{ApplicantName: "Ada", ApplicantEmail: "ada@example.com"}
	opening := JobOpening{Title: "Engineer", Department: "R&D"}
	env := applicationReceivedEmail(app, opening)
	assertEmail(t, env, "ada@example.com")
	if env["title"] == "" {
		t.Fatal("title must be set")
	}
	// Empty applicant email yields no envelope.
	if got := applicationReceivedEmail(JobApplication{ApplicantName: "Ada"}, opening); got != nil {
		t.Fatal("missing email must yield nil envelope")
	}
}

func TestInterviewScheduledEmail(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, time.January, 2, 15, 4, 0, 0, time.UTC)
	app := JobApplication{ApplicantName: "Ada", ApplicantEmail: "ada@example.com"}
	iv := Interview{InterviewType: "video", ScheduledAt: &when, DurationMinutes: 45, MeetingLink: "https://meet.example/x"}
	env := interviewScheduledEmail(iv, app)
	assertEmail(t, env, "ada@example.com")
	body, _ := env["body"].(string)
	if body == "" {
		t.Fatal("body must be set")
	}
	// No applicant email → nil.
	if got := interviewScheduledEmail(iv, JobApplication{ApplicantName: "Ada"}); got != nil {
		t.Fatal("missing email must yield nil envelope")
	}
}

func TestOfferSentAndAcceptedEmail(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	app := JobApplication{ApplicantName: "Ada", ApplicantEmail: "ada@example.com"}
	offer := OfferLetter{Designation: "Staff Engineer", ValidUntil: &until}
	assertEmail(t, offerSentEmail(offer, app), "ada@example.com")
	assertEmail(t, offerAcceptedEmail(offer, app), "ada@example.com")
	if got := offerSentEmail(offer, JobApplication{ApplicantName: "Ada"}); got != nil {
		t.Fatal("missing email must yield nil envelope")
	}
}

func TestHumanInterviewType(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"in_person": "in-person",
		"":          "scheduled",
		"video":     "video",
		"technical": "technical",
	}
	for in, want := range cases {
		if got := humanInterviewType(in); got != want {
			t.Fatalf("humanInterviewType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDepartmentSuffix(t *testing.T) {
	t.Parallel()
	if got := departmentSuffix(""); got != "" {
		t.Fatalf("empty department suffix = %q, want empty", got)
	}
	if got := departmentSuffix("R&D"); got != " in R&D" {
		t.Fatalf("departmentSuffix(R&D) = %q", got)
	}
}

func assertEmail(t *testing.T, env map[string]any, wantTo string) {
	t.Helper()
	if env == nil {
		t.Fatal("expected non-nil envelope")
	}
	if env["channel"] != "email" {
		t.Fatalf("channel = %v, want email", env["channel"])
	}
	if env["email"] != wantTo {
		t.Fatalf("email = %v, want %q", env["email"], wantTo)
	}
}

// TestNewRecruitmentStoreActorPtr verifies the small actor helper that
// converts the zero UUID into a nil pointer (so system-actor audit
// entries are attributed correctly).
func TestActorPtr(t *testing.T) {
	t.Parallel()
	if actorPtr(uuid.Nil) != nil {
		t.Fatal("nil uuid must map to nil actor pointer")
	}
	id := uuid.New()
	got := actorPtr(id)
	if got == nil || *got != id {
		t.Fatalf("actorPtr(%s) = %v", id, got)
	}
}
