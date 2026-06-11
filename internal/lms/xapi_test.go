package lms

import (
	"errors"
	"testing"
)

func TestXAPIValidate(t *testing.T) {
	valid := XAPIStatement{
		Actor:  XAPIActor{Mbox: "mailto:jane@acme.test"},
		Verb:   XAPIVerb{ID: "http://adlnet.gov/expapi/verbs/completed"},
		Object: XAPIObject{ID: "https://kapp/lessons/" + "11111111-1111-1111-1111-111111111111"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid statement rejected: %v", err)
	}

	tests := []struct {
		name string
		mut  func(s *XAPIStatement)
	}{
		{"no actor identifier", func(s *XAPIStatement) { s.Actor = XAPIActor{} }},
		{"no verb", func(s *XAPIStatement) { s.Verb.ID = "" }},
		{"no object", func(s *XAPIStatement) { s.Object.ID = "" }},
		{"bad timestamp", func(s *XAPIStatement) { s.Timestamp = "not-a-time" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := valid
			tt.mut(&s)
			if err := s.Validate(); !errors.Is(err, ErrInvalidStatement) {
				t.Fatalf("want ErrInvalidStatement, got %v", err)
			}
		})
	}
}

func TestCanonicalVerb(t *testing.T) {
	cases := map[string]string{
		"http://adlnet.gov/expapi/verbs/completed": "completed",
		"http://adlnet.gov/expapi/verbs/Passed":    "passed",
		"verb:experienced":                         "experienced",
		"attempted":                                "attempted",
	}
	for in, want := range cases {
		if got := CanonicalVerb(in); got != want {
			t.Errorf("CanonicalVerb(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerbToProgressStatus(t *testing.T) {
	cases := []struct {
		verb   string
		status string
		ok     bool
	}{
		{VerbCompleted, ProgressCompleted, true},
		{VerbPassed, ProgressCompleted, true},
		{VerbFailed, ProgressInProgress, true},
		{VerbAttempted, ProgressInProgress, true},
		{VerbExperienced, ProgressInProgress, true},
		{"initialized", "", false},
	}
	for _, c := range cases {
		got, ok := VerbToProgressStatus(c.verb)
		if got != c.status || ok != c.ok {
			t.Errorf("VerbToProgressStatus(%q) = (%q,%v), want (%q,%v)", c.verb, got, ok, c.status, c.ok)
		}
	}
}

func TestActorIdentity(t *testing.T) {
	if got := ActorIdentity(XAPIActor{Mbox: "mailto:Jane@Acme.Test"}); got != "jane@acme.test" {
		t.Errorf("mbox identity = %q", got)
	}
	if got := ActorIdentity(XAPIActor{Account: &XAPIAccount{HomePage: "https://idp", Name: "u123"}}); got != "https://idp|u123" {
		t.Errorf("account identity = %q", got)
	}
	if got := ActorIdentity(XAPIActor{}); got != "" {
		t.Errorf("empty actor identity = %q, want empty", got)
	}
}

func TestLessonIDFromObject(t *testing.T) {
	id, ok := LessonIDFromObject("https://kapp/lessons/11111111-1111-1111-1111-111111111111")
	if !ok || id.String() != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("got (%v,%v)", id, ok)
	}
	if _, ok := LessonIDFromObject("https://example.com/activities/intro"); ok {
		t.Fatal("non-uuid tail should not resolve")
	}
}

func TestScoreFromResult(t *testing.T) {
	if ScoreFromResult(nil) != nil {
		t.Fatal("nil result => nil score")
	}
	if ScoreFromResult(&XAPIResult{}) != nil {
		t.Fatal("no score => nil")
	}
	raw := ScoreFromResult(&XAPIResult{Score: &XAPIScore{Raw: f64(72)}})
	if raw == nil || *raw != 72 {
		t.Fatalf("raw score = %v, want 72", raw)
	}
	scaled := ScoreFromResult(&XAPIResult{Score: &XAPIScore{Scaled: f64(0.5)}})
	if scaled == nil || *scaled != 50 {
		t.Fatalf("scaled score = %v, want 50", scaled)
	}
}
