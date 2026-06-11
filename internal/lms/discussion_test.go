package lms

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestValidateThread(t *testing.T) {
	tid, cid, aid := uuid.New(), uuid.New(), uuid.New()
	base := func() DiscussionThread {
		return DiscussionThread{TenantID: tid, CourseID: cid, AuthorID: aid, Title: "Help"}
	}

	t.Run("ok defaults status open", func(t *testing.T) {
		th := base()
		if err := validateThread(&th); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if th.Status != ThreadStatusOpen {
			t.Fatalf("default status = %q", th.Status)
		}
	})
	t.Run("missing tenant", func(t *testing.T) {
		th := base()
		th.TenantID = uuid.Nil
		if err := validateThread(&th); !errors.Is(err, ErrInvalidThread) {
			t.Fatalf("want ErrInvalidThread, got %v", err)
		}
	})
	t.Run("missing course", func(t *testing.T) {
		th := base()
		th.CourseID = uuid.Nil
		if err := validateThread(&th); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing author", func(t *testing.T) {
		th := base()
		th.AuthorID = uuid.Nil
		if err := validateThread(&th); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("blank title", func(t *testing.T) {
		th := base()
		th.Title = "   "
		if err := validateThread(&th); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("bad status", func(t *testing.T) {
		th := base()
		th.Status = "archived"
		if err := validateThread(&th); err == nil {
			t.Fatal("expected error")
		}
	})
}
