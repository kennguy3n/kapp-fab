package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/hr"
)

// recruitDispatcher returns a dispatcher whose recruitment store is
// constructed with a nil pool. Argument-validation paths return before
// any store method is called, so they never touch the pool; reaching a
// store call would panic and thereby flag a missing guard.
func recruitDispatcher() *CommandDispatcher {
	return &CommandDispatcher{recruitment: hr.NewRecruitmentStore(nil, nil, nil, nil, nil)}
}

func TestRecruitNotConfigured(t *testing.T) {
	t.Parallel()
	d := &CommandDispatcher{} // recruitment == nil
	resp, err := d.recruit(context.Background(), CommandRequest{TenantID: uuid.New(), UserID: uuid.New()})
	if err != nil {
		t.Fatalf("recruit: %v", err)
	}
	if !strings.Contains(resp.Text, "not configured") {
		t.Fatalf("want 'not configured', got %q", resp.Text)
	}
}

func TestRecruitRequiresTenant(t *testing.T) {
	t.Parallel()
	resp, err := recruitDispatcher().recruit(context.Background(), CommandRequest{})
	if err != nil {
		t.Fatalf("recruit: %v", err)
	}
	if !strings.Contains(resp.Text, "tenant_id required") {
		t.Fatalf("want 'tenant_id required', got %q", resp.Text)
	}
}

func TestRecruitArgValidation(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	user := uuid.New()
	d := recruitDispatcher()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown subcommand", []string{"frobnicate"}, "Usage: /recruit"},
		{"applications missing id", []string{"applications"}, "Usage: /recruit applications"},
		{"applications bad id", []string{"applications", "not-a-uuid"}, "invalid job_opening_id"},
		{"advance too few args", []string{"advance", uuid.New().String()}, "Usage: /recruit advance"},
		{"advance bad id", []string{"advance", "nope", "screening"}, "invalid application_id"},
		{"schedule too few args", []string{"schedule", uuid.New().String(), uuid.New().String()}, "Usage: /recruit schedule"},
		{"schedule bad app id", []string{"schedule", "nope", uuid.New().String(), "2026-01-02T15:04:05Z"}, "invalid application_id"},
		{"schedule bad interviewer id", []string{"schedule", uuid.New().String(), "nope", "2026-01-02T15:04:05Z"}, "invalid interviewer_id"},
		{"schedule bad datetime", []string{"schedule", uuid.New().String(), uuid.New().String(), "yesterday"}, "invalid datetime"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			resp, err := d.recruit(context.Background(), CommandRequest{TenantID: tenant, UserID: user, Args: c.args})
			if err != nil {
				t.Fatalf("recruit: %v", err)
			}
			if !strings.Contains(resp.Text, c.want) {
				t.Fatalf("args=%v: want text containing %q, got %q", c.args, c.want, resp.Text)
			}
		})
	}
}
