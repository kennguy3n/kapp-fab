package lms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Gamification KType identifiers (mirror of lms_badges / lms_user_badges).
const (
	KTypeBadge     = "lms.badge"
	KTypeUserBadge = "lms.user_badge"
)

// Badge criteria types — the rule the award engine evaluates.
const (
	CriteriaCourseComplete = "course_complete"
	CriteriaPathComplete   = "path_complete"
	CriteriaQuizScore      = "quiz_score"
	CriteriaStreak         = "streak"
)

var (
	// ErrInvalidBadge is returned when a badge definition fails
	// validation (missing name or unknown criteria type).
	ErrInvalidBadge = errors.New("lms: invalid badge")
	// ErrBadgeNotFound is returned when a badge id does not resolve
	// within the tenant.
	ErrBadgeNotFound = errors.New("lms: badge not found")
)

// CriteriaValue is the parameter bag for a badge's criteria, stored as
// JSONB. Only the field relevant to the criteria_type is consulted:
//
//	quiz_score → MinScore (fraction 0..1; award when achieved score ≥ MinScore)
//	streak     → Days (consecutive-day streak length to reach)
//	course_complete / path_complete → no parameter (any completion qualifies)
type CriteriaValue struct {
	MinScore *decimal.Decimal `json:"min_score,omitempty"`
	Days     *int             `json:"days,omitempty"`
}

// Badge is an lms_badges row — an award definition.
type Badge struct {
	TenantID      uuid.UUID     `json:"tenant_id"`
	ID            uuid.UUID     `json:"id"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	Icon          string        `json:"icon"`
	CriteriaType  string        `json:"criteria_type"`
	CriteriaValue CriteriaValue `json:"criteria_value"`
	Active        bool          `json:"active"`
	CreatedBy     *uuid.UUID    `json:"created_by,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// UserBadge is an lms_user_badges row — a badge awarded to a user.
type UserBadge struct {
	TenantID uuid.UUID       `json:"tenant_id"`
	ID       uuid.UUID       `json:"id"`
	UserID   uuid.UUID       `json:"user_id"`
	BadgeID  uuid.UUID       `json:"badge_id"`
	EarnedAt time.Time       `json:"earned_at"`
	Context  json.RawMessage `json:"context,omitempty"`
}

// Milestone is a learning event evaluated against badge criteria. The
// award engine matches a milestone's Type to a badge's CriteriaType and
// then applies the type-specific threshold.
type Milestone struct {
	Type       string
	QuizScore  *decimal.Decimal // for quiz_score
	StreakDays int              // for streak
	// Context is persisted with the award (course_id, score, …) for the
	// UI tooltip and audit.
	Context json.RawMessage
}

// BadgeQualifies reports whether a milestone satisfies a badge's
// criteria. Pure — the award rule is unit-tested in isolation. An
// inactive badge or a type mismatch never qualifies.
func BadgeQualifies(b Badge, m Milestone) bool {
	if !b.Active || b.CriteriaType != m.Type {
		return false
	}
	switch b.CriteriaType {
	case CriteriaCourseComplete, CriteriaPathComplete:
		return true
	case CriteriaQuizScore:
		if m.QuizScore == nil {
			return false
		}
		threshold := b.CriteriaValue.MinScore
		if threshold == nil {
			// No threshold configured: any scored quiz qualifies.
			return true
		}
		return m.QuizScore.GreaterThanOrEqual(*threshold)
	case CriteriaStreak:
		days := b.CriteriaValue.Days
		if days == nil {
			return false
		}
		return m.StreakDays >= *days
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Store.
// ---------------------------------------------------------------------------

// GamificationStore persists badges + awards and computes leaderboards.
type GamificationStore struct {
	pool    *pgxpool.Pool
	auditor audit.Logger
	now     func() time.Time
}

// NewGamificationStore wires a store over the shared pool.
func NewGamificationStore(pool *pgxpool.Pool, auditor audit.Logger) *GamificationStore {
	return &GamificationStore{
		pool:    pool,
		auditor: auditor,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithClock substitutes the time source (tests).
func (s *GamificationStore) WithClock(now func() time.Time) *GamificationStore {
	if now != nil {
		s.now = now
	}
	return s
}

func validateBadge(b *Badge) error {
	if b.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id required", ErrInvalidBadge)
	}
	if strings.TrimSpace(b.Name) == "" {
		return fmt.Errorf("%w: name required", ErrInvalidBadge)
	}
	switch b.CriteriaType {
	case CriteriaCourseComplete, CriteriaPathComplete, CriteriaQuizScore, CriteriaStreak:
	default:
		return fmt.Errorf("%w: criteria_type %q invalid", ErrInvalidBadge, b.CriteriaType)
	}
	if b.CriteriaType == CriteriaQuizScore && b.CriteriaValue.MinScore != nil {
		if b.CriteriaValue.MinScore.IsNegative() || b.CriteriaValue.MinScore.GreaterThan(decimal.NewFromInt(1)) {
			return fmt.Errorf("%w: quiz_score min_score must be within [0,1]", ErrInvalidBadge)
		}
	}
	if b.CriteriaType == CriteriaStreak {
		if b.CriteriaValue.Days == nil || *b.CriteriaValue.Days <= 0 {
			return fmt.Errorf("%w: streak badge requires criteria_value.days > 0", ErrInvalidBadge)
		}
	}
	return nil
}

// CreateBadge inserts a badge definition.
func (s *GamificationStore) CreateBadge(ctx context.Context, in Badge) (*Badge, error) {
	if err := validateBadge(&in); err != nil {
		return nil, err
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	cv, err := json.Marshal(in.CriteriaValue)
	if err != nil {
		return nil, fmt.Errorf("marshal criteria_value: %w", err)
	}
	var out Badge
	err = platform.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO lms_badges
			    (tenant_id, id, name, description, icon, criteria_type, criteria_value, active, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 RETURNING tenant_id, id, name, description, icon, criteria_type, criteria_value, active, created_by, created_at, updated_at`,
			in.TenantID, in.ID, in.Name, in.Description, in.Icon, in.CriteriaType, cv, in.Active, in.CreatedBy,
		)
		if err := scanBadge(row, &out); err != nil {
			return err
		}
		return s.auditBadge(ctx, tx, in.TenantID, in.CreatedBy, "lms.badge.create", out.ID, nil, mustJSON(out))
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListBadges returns a tenant's badges. When activeOnly is true only
// active badges are returned (the award engine's candidate set).
func (s *GamificationStore) ListBadges(ctx context.Context, tenantID uuid.UUID, activeOnly bool) ([]Badge, error) {
	var out []Badge
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, name, description, icon, criteria_type, criteria_value, active, created_by, created_at, updated_at
			   FROM lms_badges
			  WHERE tenant_id = $1 AND ($2 = false OR active = true)
			  ORDER BY name`,
			tenantID, activeOnly,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b Badge
			if err := scanBadge(rows, &b); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// AwardBadge grants a badge to a user. Idempotent via the UNIQUE
// (tenant, user, badge) constraint: a repeat award is a no-op and
// returns awarded=false so callers can suppress duplicate notifications.
func (s *GamificationStore) AwardBadge(ctx context.Context, tenantID, userID, badgeID uuid.UUID, awardContext json.RawMessage, actor *uuid.UUID) (*UserBadge, bool, error) {
	if tenantID == uuid.Nil || userID == uuid.Nil || badgeID == uuid.Nil {
		return nil, false, fmt.Errorf("%w: tenant_id, user_id, badge_id required", ErrInvalidBadge)
	}
	if len(awardContext) == 0 {
		awardContext = json.RawMessage(`{}`)
	}
	var (
		out     UserBadge
		awarded bool
	)
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO lms_user_badges (tenant_id, id, user_id, badge_id, earned_at, context)
			 VALUES ($1,$2,$3,$4,$5,$6)
			 ON CONFLICT (tenant_id, user_id, badge_id) DO NOTHING`,
			tenantID, uuid.New(), userID, badgeID, s.now(), awardContext,
		)
		if err != nil {
			return fmt.Errorf("award badge: %w", err)
		}
		awarded = tag.RowsAffected() == 1
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, id, user_id, badge_id, earned_at, context
			   FROM lms_user_badges
			  WHERE tenant_id = $1 AND user_id = $2 AND badge_id = $3`,
			tenantID, userID, badgeID,
		)
		if err := row.Scan(&out.TenantID, &out.ID, &out.UserID, &out.BadgeID, &out.EarnedAt, &out.Context); err != nil {
			return err
		}
		if !awarded {
			return nil
		}
		return s.auditBadge(ctx, tx, tenantID, actor, "lms.badge.award", out.ID, nil, mustJSON(out))
	})
	if err != nil {
		return nil, false, err
	}
	return &out, awarded, nil
}

// ListUserBadges returns the badges a user holds, newest first.
func (s *GamificationStore) ListUserBadges(ctx context.Context, tenantID, userID uuid.UUID) ([]UserBadge, error) {
	var out []UserBadge
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, user_id, badge_id, earned_at, context
			   FROM lms_user_badges
			  WHERE tenant_id = $1 AND user_id = $2
			  ORDER BY earned_at DESC, id`,
			tenantID, userID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ub UserBadge
			if err := rows.Scan(&ub.TenantID, &ub.ID, &ub.UserID, &ub.BadgeID, &ub.EarnedAt, &ub.Context); err != nil {
				return err
			}
			out = append(out, ub)
		}
		return rows.Err()
	})
	return out, err
}

// BadgeCountsByUser returns the number of badges each user holds within
// a tenant. Feeds the leaderboard's "badges" dimension.
func (s *GamificationStore) BadgeCountsByUser(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]int, error) {
	out := make(map[uuid.UUID]int)
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT user_id, COUNT(*) FROM lms_user_badges WHERE tenant_id = $1 GROUP BY user_id`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var uid uuid.UUID
			var n int
			if err := rows.Scan(&uid, &n); err != nil {
				return err
			}
			out[uid] = n
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Leaderboard.
// ---------------------------------------------------------------------------

// LeaderboardEntry is one ranked learner.
type LeaderboardEntry struct {
	UserID           uuid.UUID       `json:"user_id"`
	CoursesCompleted int             `json:"courses_completed"`
	TotalScore       decimal.Decimal `json:"total_score"`
	Badges           int             `json:"badges"`
	Rank             int             `json:"rank"`
}

// RankLeaderboard orders entries by courses completed desc, then total
// score desc, then badge count desc, then user id for determinism, and
// assigns competition ranks (1,2,2,4 — ties share a rank, the next rank
// skips). Pure so the ranking is unit-tested without a database.
func RankLeaderboard(entries []LeaderboardEntry) []LeaderboardEntry {
	out := make([]LeaderboardEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.CoursesCompleted != b.CoursesCompleted {
			return a.CoursesCompleted > b.CoursesCompleted
		}
		if !a.TotalScore.Equal(b.TotalScore) {
			return a.TotalScore.GreaterThan(b.TotalScore)
		}
		if a.Badges != b.Badges {
			return a.Badges > b.Badges
		}
		return a.UserID.String() < b.UserID.String()
	})
	for i := range out {
		if i > 0 && tiedLeaderboard(out[i-1], out[i]) {
			out[i].Rank = out[i-1].Rank
		} else {
			out[i].Rank = i + 1
		}
	}
	return out
}

func tiedLeaderboard(a, b LeaderboardEntry) bool {
	return a.CoursesCompleted == b.CoursesCompleted &&
		a.TotalScore.Equal(b.TotalScore) &&
		a.Badges == b.Badges
}

func scanBadge(row rowScanner, b *Badge) error {
	var cv []byte
	if err := row.Scan(
		&b.TenantID, &b.ID, &b.Name, &b.Description, &b.Icon, &b.CriteriaType,
		&cv, &b.Active, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadgeNotFound
		}
		return err
	}
	if len(cv) > 0 {
		if err := json.Unmarshal(cv, &b.CriteriaValue); err != nil {
			return fmt.Errorf("unmarshal criteria_value: %w", err)
		}
	}
	return nil
}

func (s *GamificationStore) auditBadge(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, action string, target uuid.UUID, before, after []byte) error {
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
		TargetKType: KTypeBadge,
		TargetID:    &tid,
		Before:      before,
		After:       after,
	})
}
