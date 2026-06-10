package lms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Discussion KType identifiers (mirror of the lms_discussion_* tables).
const (
	KTypeDiscussionThread = "lms.discussion_thread"
	KTypeDiscussionReply  = "lms.discussion_reply"
)

// Thread lifecycle states.
const (
	ThreadStatusOpen     = "open"
	ThreadStatusResolved = "resolved"
	ThreadStatusClosed   = "closed"
)

// Reply sources.
const (
	ReplySourceWeb   = "web"
	ReplySourceKChat = "kchat"
)

var (
	// ErrInvalidThread is returned when a discussion thread fails
	// validation (e.g. missing course id, title, or body).
	ErrInvalidThread = errors.New("lms: invalid discussion thread")
	// ErrThreadNotFound is returned when a thread id does not resolve
	// within the tenant.
	ErrThreadNotFound = errors.New("lms: discussion thread not found")
	// ErrInvalidReply is returned when a discussion reply fails
	// validation (e.g. empty body).
	ErrInvalidReply = errors.New("lms: invalid discussion reply")
)

// DiscussionThread is an lms_discussion_threads row.
type DiscussionThread struct {
	TenantID   uuid.UUID  `json:"tenant_id"`
	ID         uuid.UUID  `json:"id"`
	CourseID   uuid.UUID  `json:"course_id"`
	LessonID   *uuid.UUID `json:"lesson_id,omitempty"`
	AuthorID   uuid.UUID  `json:"author_id"`
	Title      string     `json:"title"`
	Body       string     `json:"body"`
	Status     string     `json:"status"`
	Pinned     bool       `json:"pinned"`
	ReplyCount int        `json:"reply_count"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// DiscussionReply is an lms_discussion_replies row.
type DiscussionReply struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	ID        uuid.UUID `json:"id"`
	ThreadID  uuid.UUID `json:"thread_id"`
	AuthorID  uuid.UUID `json:"author_id"`
	Body      string    `json:"body"`
	IsAnswer  bool      `json:"is_answer"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DiscussionStore persists threads + replies with the reply_count
// denormalization kept consistent inside each mutating transaction.
type DiscussionStore struct {
	pool    *pgxpool.Pool
	auditor audit.Logger
	now     func() time.Time
}

// NewDiscussionStore wires a store over the shared pool.
func NewDiscussionStore(pool *pgxpool.Pool, auditor audit.Logger) *DiscussionStore {
	return &DiscussionStore{
		pool:    pool,
		auditor: auditor,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithClock substitutes the time source (tests).
func (s *DiscussionStore) WithClock(now func() time.Time) *DiscussionStore {
	if now != nil {
		s.now = now
	}
	return s
}

func validateThread(in *DiscussionThread) error {
	if in.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id required", ErrInvalidThread)
	}
	if in.CourseID == uuid.Nil {
		return fmt.Errorf("%w: course_id required", ErrInvalidThread)
	}
	if in.AuthorID == uuid.Nil {
		return fmt.Errorf("%w: author_id required", ErrInvalidThread)
	}
	if strings.TrimSpace(in.Title) == "" {
		return fmt.Errorf("%w: title required", ErrInvalidThread)
	}
	if in.Status == "" {
		in.Status = ThreadStatusOpen
	}
	switch in.Status {
	case ThreadStatusOpen, ThreadStatusResolved, ThreadStatusClosed:
	default:
		return fmt.Errorf("%w: status %q invalid", ErrInvalidThread, in.Status)
	}
	return nil
}

// CreateThread opens a new discussion thread on a course.
func (s *DiscussionStore) CreateThread(ctx context.Context, in DiscussionThread) (*DiscussionThread, error) {
	if err := validateThread(&in); err != nil {
		return nil, err
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	var out DiscussionThread
	err := platform.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO lms_discussion_threads
			    (tenant_id, id, course_id, lesson_id, author_id, title, body, status, pinned)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 RETURNING tenant_id, id, course_id, lesson_id, author_id, title, body, status, pinned, reply_count, created_at, updated_at`,
			in.TenantID, in.ID, in.CourseID, in.LessonID, in.AuthorID, in.Title, in.Body, in.Status, in.Pinned,
		)
		if err := scanThread(row, &out); err != nil {
			return err
		}
		actor := in.AuthorID
		return s.auditThread(ctx, tx, in.TenantID, &actor, "lms.discussion_thread.create", out.ID, nil, mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetThread returns a single thread by id.
func (s *DiscussionStore) GetThread(ctx context.Context, tenantID, id uuid.UUID) (*DiscussionThread, error) {
	var out *DiscussionThread
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		t, err := getThreadTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListThreads returns the threads for a course, pinned first then most
// recently active.
func (s *DiscussionStore) ListThreads(ctx context.Context, tenantID, courseID uuid.UUID) ([]DiscussionThread, error) {
	var out []DiscussionThread
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, course_id, lesson_id, author_id, title, body, status, pinned, reply_count, created_at, updated_at
			   FROM lms_discussion_threads
			  WHERE tenant_id = $1 AND course_id = $2
			  ORDER BY pinned DESC, updated_at DESC, id`,
			tenantID, courseID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t DiscussionThread
			if err := scanThread(rows, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateThread mutates the mutable thread fields (title, body, status,
// pinned). Used by the author / instructor to edit, resolve, close, or
// pin a thread.
func (s *DiscussionStore) UpdateThread(ctx context.Context, in DiscussionThread, actor *uuid.UUID) (*DiscussionThread, error) {
	if in.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: id required", ErrInvalidThread)
	}
	if err := validateThread(&in); err != nil {
		return nil, err
	}
	var out DiscussionThread
	err := platform.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		before, err := getThreadTx(ctx, tx, in.TenantID, in.ID)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx,
			`UPDATE lms_discussion_threads
			    SET title = $3, body = $4, status = $5, pinned = $6, updated_at = $7
			  WHERE tenant_id = $1 AND id = $2
			 RETURNING tenant_id, id, course_id, lesson_id, author_id, title, body, status, pinned, reply_count, created_at, updated_at`,
			in.TenantID, in.ID, in.Title, in.Body, in.Status, in.Pinned, s.now(),
		)
		if err := scanThread(row, &out); err != nil {
			return err
		}
		return s.auditThread(ctx, tx, in.TenantID, actor, "lms.discussion_thread.update", out.ID, mustJSON(before), mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteThread removes a thread and (via FK cascade) its replies.
func (s *DiscussionStore) DeleteThread(ctx context.Context, tenantID, id uuid.UUID, actor *uuid.UUID) error {
	return platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		before, err := getThreadTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM lms_discussion_threads WHERE tenant_id = $1 AND id = $2`, tenantID, id); err != nil {
			return fmt.Errorf("delete thread: %w", err)
		}
		return s.auditThread(ctx, tx, tenantID, actor, "lms.discussion_thread.delete", id, mustJSON(before), nil)
	})
}

// AddReply appends a reply to a thread and bumps the thread's
// reply_count + updated_at in the same transaction so the denormalized
// count never drifts. `source` is "web" or "kchat" (instructor reply
// synced from KChat).
func (s *DiscussionStore) AddReply(ctx context.Context, in DiscussionReply, actor *uuid.UUID) (*DiscussionReply, error) {
	if in.TenantID == uuid.Nil || in.ThreadID == uuid.Nil || in.AuthorID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id, thread_id, author_id required", ErrInvalidReply)
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, fmt.Errorf("%w: body required", ErrInvalidReply)
	}
	if in.Source == "" {
		in.Source = ReplySourceWeb
	}
	if in.Source != ReplySourceWeb && in.Source != ReplySourceKChat {
		return nil, fmt.Errorf("%w: source %q invalid", ErrInvalidReply, in.Source)
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	var out DiscussionReply
	err := platform.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Ensure the parent thread exists (clean 404 vs raw FK error).
		if _, err := getThreadTx(ctx, tx, in.TenantID, in.ThreadID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx,
			`INSERT INTO lms_discussion_replies
			    (tenant_id, id, thread_id, author_id, body, is_answer, source)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)
			 RETURNING tenant_id, id, thread_id, author_id, body, is_answer, source, created_at, updated_at`,
			in.TenantID, in.ID, in.ThreadID, in.AuthorID, in.Body, in.IsAnswer, in.Source,
		)
		if err := scanReply(row, &out); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE lms_discussion_threads
			    SET reply_count = reply_count + 1, updated_at = $3
			  WHERE tenant_id = $1 AND id = $2`,
			in.TenantID, in.ThreadID, s.now()); err != nil {
			return fmt.Errorf("bump reply_count: %w", err)
		}
		return s.auditReply(ctx, tx, in.TenantID, actor, "lms.discussion_reply.add", out.ID, nil, mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListReplies returns a thread's replies in post order.
func (s *DiscussionStore) ListReplies(ctx context.Context, tenantID, threadID uuid.UUID) ([]DiscussionReply, error) {
	var out []DiscussionReply
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, thread_id, author_id, body, is_answer, source, created_at, updated_at
			   FROM lms_discussion_replies
			  WHERE tenant_id = $1 AND thread_id = $2
			  ORDER BY created_at, id`,
			tenantID, threadID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rep DiscussionReply
			if err := scanReply(rows, &rep); err != nil {
				return err
			}
			out = append(out, rep)
		}
		return rows.Err()
	})
	return out, err
}

// MarkAnswer flags one reply as the accepted answer, clearing the flag
// from any other reply in the same thread (at most one accepted answer)
// and marking the thread resolved. Idempotent.
func (s *DiscussionStore) MarkAnswer(ctx context.Context, tenantID, threadID, replyID uuid.UUID, actor *uuid.UUID) error {
	return platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Verify the reply belongs to the thread.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM lms_discussion_replies
			                WHERE tenant_id = $1 AND thread_id = $2 AND id = $3)`,
			tenantID, threadID, replyID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrInvalidReply
		}
		if _, err := tx.Exec(ctx,
			`UPDATE lms_discussion_replies
			    SET is_answer = (id = $3), updated_at = $4
			  WHERE tenant_id = $1 AND thread_id = $2`,
			tenantID, threadID, replyID, s.now()); err != nil {
			return fmt.Errorf("mark answer: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE lms_discussion_threads
			    SET status = $3, updated_at = $4
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, threadID, ThreadStatusResolved, s.now()); err != nil {
			return fmt.Errorf("resolve thread: %w", err)
		}
		return s.auditReply(ctx, tx, tenantID, actor, "lms.discussion_reply.mark_answer", replyID, nil, nil)
	})
}

func getThreadTx(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (*DiscussionThread, error) {
	row := tx.QueryRow(ctx,
		`SELECT tenant_id, id, course_id, lesson_id, author_id, title, body, status, pinned, reply_count, created_at, updated_at
		   FROM lms_discussion_threads WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
	var t DiscussionThread
	if err := scanThread(row, &t); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrThreadNotFound
		}
		return nil, err
	}
	return &t, nil
}

func scanThread(row rowScanner, t *DiscussionThread) error {
	return row.Scan(
		&t.TenantID, &t.ID, &t.CourseID, &t.LessonID, &t.AuthorID, &t.Title,
		&t.Body, &t.Status, &t.Pinned, &t.ReplyCount, &t.CreatedAt, &t.UpdatedAt,
	)
}

func scanReply(row rowScanner, r *DiscussionReply) error {
	return row.Scan(
		&r.TenantID, &r.ID, &r.ThreadID, &r.AuthorID, &r.Body, &r.IsAnswer,
		&r.Source, &r.CreatedAt, &r.UpdatedAt,
	)
}

func (s *DiscussionStore) auditThread(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, action string, target uuid.UUID, before, after []byte) error {
	return s.auditAny(ctx, tx, tenantID, actor, action, KTypeDiscussionThread, target, before, after)
}

func (s *DiscussionStore) auditReply(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, action string, target uuid.UUID, before, after []byte) error {
	return s.auditAny(ctx, tx, tenantID, actor, action, KTypeDiscussionReply, target, before, after)
}

func (s *DiscussionStore) auditAny(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, action, ktype string, target uuid.UUID, before, after []byte) error {
	if s.auditor == nil {
		return nil
	}
	kind := audit.ActorUser
	if actor == nil {
		kind = audit.ActorSystem
	}
	tid := target
	return s.auditor.LogTx(ctx, tx, audit.Entry{
		TenantID:    tenantID,
		ActorID:     actor,
		ActorKind:   kind,
		Action:      action,
		TargetKType: ktype,
		TargetID:    &tid,
		Before:      before,
		After:       after,
	})
}
