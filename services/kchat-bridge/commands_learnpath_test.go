package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/lms"
)

// TestLearnPathGuards covers the /learn-path argument + configuration
// guards that short-circuit before any database access, so they can be
// verified without a live pool.
func TestLearnPathGuards(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	user := uuid.New()

	t.Run("store not configured", func(t *testing.T) {
		d := &CommandDispatcher{}
		res, err := d.learnPath(ctx, CommandRequest{TenantID: tenant, UserID: user, Args: []string{"list"}})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(res.Text, "not configured") {
			t.Fatalf("text = %q", res.Text)
		}
	})

	// A store with a nil pool is sufficient for the branches that return
	// before touching the database (usage + uuid parse validation).
	d := &CommandDispatcher{learningPaths: lms.NewLearningPathStore(nil, nil)}

	t.Run("missing tenant", func(t *testing.T) {
		res, _ := d.learnPath(ctx, CommandRequest{UserID: user, Args: []string{"list"}})
		if !strings.Contains(res.Text, "tenant_id required") {
			t.Fatalf("text = %q", res.Text)
		}
	})

	t.Run("enroll usage without path_id", func(t *testing.T) {
		res, _ := d.learnPath(ctx, CommandRequest{TenantID: tenant, UserID: user, Args: []string{"enroll"}})
		if !strings.Contains(res.Text, "Usage: /learn-path enroll") {
			t.Fatalf("text = %q", res.Text)
		}
	})

	t.Run("enroll rejects invalid path_id", func(t *testing.T) {
		res, _ := d.learnPath(ctx, CommandRequest{TenantID: tenant, UserID: user, Args: []string{"enroll", "not-a-uuid"}})
		if !strings.Contains(res.Text, "invalid path_id") {
			t.Fatalf("text = %q", res.Text)
		}
	})

	t.Run("unknown subcommand shows usage", func(t *testing.T) {
		res, _ := d.learnPath(ctx, CommandRequest{TenantID: tenant, UserID: user, Args: []string{"frobnicate"}})
		if !strings.Contains(res.Text, "Usage: /learn-path") {
			t.Fatalf("text = %q", res.Text)
		}
	})
}
