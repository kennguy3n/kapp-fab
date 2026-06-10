package lms

import (
	"context"
	"encoding/json"
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

// Canonical xAPI verbs we map onto progress. The verb IRI's tail
// (after the last '/') is matched case-insensitively.
const (
	VerbCompleted   = "completed"
	VerbPassed      = "passed"
	VerbFailed      = "failed"
	VerbAttempted   = "attempted"
	VerbExperienced = "experienced"
)

var (
	ErrInvalidStatement = errors.New("lms: invalid xapi statement")
)

// XAPIStatement is the subset of the xAPI statement schema we accept
// and persist. The full statement is retained verbatim in the store's
// `raw` column; these fields drive validation, actor resolution, and
// the verb→progress mapping.
type XAPIStatement struct {
	ID        string      `json:"id,omitempty"`
	Actor     XAPIActor   `json:"actor"`
	Verb      XAPIVerb    `json:"verb"`
	Object    XAPIObject  `json:"object"`
	Result    *XAPIResult `json:"result,omitempty"`
	Timestamp string      `json:"timestamp,omitempty"`
}

// XAPIActor identifies the learner. We support the two most common
// inverse-functional identifiers: mbox (mailto:) and account.
type XAPIActor struct {
	Name    string       `json:"name,omitempty"`
	Mbox    string       `json:"mbox,omitempty"`
	Account *XAPIAccount `json:"account,omitempty"`
}

// XAPIAccount is the account-based actor identifier.
type XAPIAccount struct {
	HomePage string `json:"homePage,omitempty"`
	Name     string `json:"name,omitempty"`
}

// XAPIVerb is the action; ID is the verb IRI.
type XAPIVerb struct {
	ID      string            `json:"id"`
	Display map[string]string `json:"display,omitempty"`
}

// XAPIObject is the activity acted upon; ID is the activity IRI.
type XAPIObject struct {
	ID         string          `json:"id"`
	ObjectType string          `json:"objectType,omitempty"`
	Definition json.RawMessage `json:"definition,omitempty"`
}

// XAPIResult carries score / success / completion.
type XAPIResult struct {
	Score      *XAPIScore `json:"score,omitempty"`
	Success    *bool      `json:"success,omitempty"`
	Completion *bool      `json:"completion,omitempty"`
}

// XAPIScore is the result score; Scaled is -1..1, Raw is absolute.
type XAPIScore struct {
	Scaled *float64 `json:"scaled,omitempty"`
	Raw    *float64 `json:"raw,omitempty"`
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
}

// Validate enforces the xAPI required-property rules we rely on: actor
// (with a resolvable identifier), verb id, and object id. Returns a
// sentinel-wrapped error so the receiver replies 400.
func (st *XAPIStatement) Validate() error {
	if ActorIdentity(st.Actor) == "" {
		return fmt.Errorf("%w: actor must carry mbox or account", ErrInvalidStatement)
	}
	if strings.TrimSpace(st.Verb.ID) == "" {
		return fmt.Errorf("%w: verb.id required", ErrInvalidStatement)
	}
	if strings.TrimSpace(st.Object.ID) == "" {
		return fmt.Errorf("%w: object.id required", ErrInvalidStatement)
	}
	if st.Timestamp != "" {
		if _, err := time.Parse(time.RFC3339, st.Timestamp); err != nil {
			return fmt.Errorf("%w: timestamp not RFC3339", ErrInvalidStatement)
		}
	}
	return nil
}

// CanonicalVerb returns the lower-cased tail of the verb IRI (the
// segment after the last '/' or ':'), e.g.
// "http://adlnet.gov/expapi/verbs/completed" → "completed".
func CanonicalVerb(verbID string) string {
	v := strings.TrimSpace(verbID)
	if i := strings.LastIndexAny(v, "/:"); i >= 0 {
		v = v[i+1:]
	}
	return strings.ToLower(v)
}

// VerbToProgressStatus maps a canonical verb to an lms.progress status.
// The boolean is false for verbs that carry no progress meaning (the
// statement is still stored, but produces no progress write).
func VerbToProgressStatus(verb string) (string, bool) {
	switch verb {
	case VerbCompleted, VerbPassed:
		return ProgressCompleted, true
	case VerbFailed, VerbAttempted, VerbExperienced:
		return ProgressInProgress, true
	default:
		return "", false
	}
}

// ActorIdentity returns the canonical identifier used to resolve an
// xAPI actor to a Kapp user: the mbox email (sans "mailto:") when
// present, else "homePage|name" for an account actor, else "".
func ActorIdentity(a XAPIActor) string {
	if m := strings.TrimSpace(a.Mbox); m != "" {
		return strings.ToLower(strings.TrimPrefix(m, "mailto:"))
	}
	if a.Account != nil {
		hp := strings.TrimSpace(a.Account.HomePage)
		name := strings.TrimSpace(a.Account.Name)
		if hp != "" || name != "" {
			return hp + "|" + name
		}
	}
	return ""
}

// LessonIDFromObject extracts a Kapp lesson UUID from an activity IRI
// when the tail segment parses as a UUID (our SCO/lesson activities are
// minted as ".../lessons/{uuid}"). Returns false when the object is an
// external activity we can't map to a lesson.
func LessonIDFromObject(objectID string) (uuid.UUID, bool) {
	v := strings.TrimSpace(objectID)
	if i := strings.LastIndexAny(v, "/:"); i >= 0 {
		v = v[i+1:]
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// ScoreFromResult projects an xAPI result score onto the 0..100 numeric
// scale lms.progress uses: prefer raw, else scaled×100.
func ScoreFromResult(res *XAPIResult) *float64 {
	if res == nil || res.Score == nil {
		return nil
	}
	if res.Score.Raw != nil {
		return res.Score.Raw
	}
	if res.Score.Scaled != nil {
		v := *res.Score.Scaled * 100
		return &v
	}
	return nil
}

// ---------------------------------------------------------------------------
// Actor resolution + store.
// ---------------------------------------------------------------------------

// ActorResolver maps an xAPI actor identity (see ActorIdentity) to a
// Kapp user id. Implemented at the API layer over the directory/user
// store so the lms package stays decoupled. ok is false when the actor
// is unknown — the statement is still stored, just without a user link.
type ActorResolver interface {
	ResolveActor(ctx context.Context, tenantID uuid.UUID, identity string) (uuid.UUID, bool, error)
}

// StoredStatement is an lms_xapi_statements row.
type StoredStatement struct {
	TenantID     uuid.UUID       `json:"tenant_id"`
	ID           uuid.UUID       `json:"id"`
	ActorUserID  *uuid.UUID      `json:"actor_user_id,omitempty"`
	ActorIdent   string          `json:"actor_ident"`
	VerbID       string          `json:"verb_id"`
	Verb         string          `json:"verb"`
	ObjectID     string          `json:"object_id"`
	LessonID     *uuid.UUID      `json:"lesson_id,omitempty"`
	EnrollmentID *uuid.UUID      `json:"enrollment_id,omitempty"`
	Raw          json.RawMessage `json:"raw"`
	StoredAt     time.Time       `json:"stored_at"`
}

// IngestResult reports what an ingest produced: the stored statement,
// whether it mapped to a progress write, and the resulting status.
type IngestResult struct {
	Statement      StoredStatement `json:"statement"`
	ProgressStatus string          `json:"progress_status,omitempty"`
	WroteProgress  bool            `json:"wrote_progress"`
}

// XAPIStore persists statements and projects them onto lesson_progress.
type XAPIStore struct {
	pool    *pgxpool.Pool
	auditor audit.Logger
	now     func() time.Time
}

// NewXAPIStore wires a store over the shared pool.
func NewXAPIStore(pool *pgxpool.Pool, auditor audit.Logger) *XAPIStore {
	return &XAPIStore{
		pool:    pool,
		auditor: auditor,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithClock substitutes the time source (tests).
func (s *XAPIStore) WithClock(now func() time.Time) *XAPIStore {
	if now != nil {
		s.now = now
	}
	return s
}

// Ingest validates, stores, and (when the verb + actor + lesson all
// resolve) projects a statement onto lesson_progress. The whole
// operation is one transaction so the statement and any progress write
// commit together. Idempotent on the statement id: a repeat of the same
// id is ignored (ON CONFLICT DO NOTHING) per the xAPI immutability rule.
//
// enrollmentResolver maps (lesson, user) → enrollment id; nil disables
// progress projection (statement is stored only).
func (s *XAPIStore) Ingest(
	ctx context.Context,
	tenantID uuid.UUID,
	st XAPIStatement,
	resolver ActorResolver,
	enrollmentFor func(ctx context.Context, tenantID, lessonID, userID uuid.UUID) (uuid.UUID, bool, error),
	actor *uuid.UUID,
) (*IngestResult, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalidStatement)
	}
	if err := st.Validate(); err != nil {
		return nil, err
	}

	stmtID := uuid.New()
	if st.ID != "" {
		if parsed, err := uuid.Parse(st.ID); err == nil {
			stmtID = parsed
		}
	}
	identity := ActorIdentity(st.Actor)
	verb := CanonicalVerb(st.Verb.ID)

	// Resolve actor → user (best effort; unknown actor still stored).
	var userID *uuid.UUID
	if resolver != nil {
		if uid, ok, err := resolver.ResolveActor(ctx, tenantID, identity); err != nil {
			return nil, err
		} else if ok {
			userID = &uid
		}
	}

	// Resolve object → lesson + enrollment when possible.
	var lessonID *uuid.UUID
	if lid, ok := LessonIDFromObject(st.Object.ID); ok {
		lessonID = &lid
	}
	status, statusOK := VerbToProgressStatus(verb)

	raw, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("marshal statement: %w", err)
	}

	result := &IngestResult{}
	err = platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var enrollmentID *uuid.UUID
		if statusOK && lessonID != nil && userID != nil && enrollmentFor != nil {
			if eid, ok, eerr := enrollmentFor(ctx, tenantID, *lessonID, *userID); eerr != nil {
				return eerr
			} else if ok {
				enrollmentID = &eid
			}
		}

		var stored StoredStatement
		row := tx.QueryRow(ctx,
			`INSERT INTO lms_xapi_statements
			    (tenant_id, id, actor_user_id, actor_ident, verb_id, verb,
			     object_id, lesson_id, enrollment_id, raw, stored_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET id = lms_xapi_statements.id
			 RETURNING tenant_id, id, actor_user_id, actor_ident, verb_id, verb,
			           object_id, lesson_id, enrollment_id, raw, stored_at`,
			tenantID, stmtID, userID, identity, st.Verb.ID, verb,
			st.Object.ID, lessonID, enrollmentID, raw, s.now(),
		)
		if err := row.Scan(
			&stored.TenantID, &stored.ID, &stored.ActorUserID, &stored.ActorIdent,
			&stored.VerbID, &stored.Verb, &stored.ObjectID, &stored.LessonID,
			&stored.EnrollmentID, &stored.Raw, &stored.StoredAt,
		); err != nil {
			return err
		}
		result.Statement = stored

		// Project onto lesson_progress when we have a full mapping.
		if statusOK && enrollmentID != nil && lessonID != nil {
			score := ScoreFromResult(st.Result)
			if err := upsertProgressTx(ctx, tx, tenantID, *enrollmentID, *lessonID, status, score, s.now()); err != nil {
				return err
			}
			result.ProgressStatus = status
			result.WroteProgress = true
		}

		if s.auditor == nil {
			return nil
		}
		kind := audit.ActorUser
		if actor == nil {
			kind = audit.ActorSystem
		}
		tid := stored.ID
		return s.auditor.LogTx(ctx, tx, audit.Entry{
			TenantID:    tenantID,
			ActorID:     actor,
			ActorKind:   kind,
			Action:      "lms.xapi.ingest",
			TargetKType: "lms.xapi_statement",
			TargetID:    &tid,
			After:       raw,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("ingest xapi statement: %w", err)
	}
	return result, nil
}

// ListStatements returns the most recent statements for a tenant,
// optionally filtered to one actor user. limit is clamped to [1,500].
func (s *XAPIStore) ListStatements(ctx context.Context, tenantID uuid.UUID, actorUserID *uuid.UUID, limit int) ([]StoredStatement, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []StoredStatement
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, actor_user_id, actor_ident, verb_id, verb,
			        object_id, lesson_id, enrollment_id, raw, stored_at
			   FROM lms_xapi_statements
			  WHERE tenant_id = $1 AND ($2::uuid IS NULL OR actor_user_id = $2)
			  ORDER BY stored_at DESC, id
			  LIMIT $3`,
			tenantID, actorUserID, limit,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st StoredStatement
			if err := rows.Scan(
				&st.TenantID, &st.ID, &st.ActorUserID, &st.ActorIdent, &st.VerbID,
				&st.Verb, &st.ObjectID, &st.LessonID, &st.EnrollmentID, &st.Raw, &st.StoredAt,
			); err != nil {
				return err
			}
			out = append(out, st)
		}
		return rows.Err()
	})
	return out, err
}

// upsertProgressTx writes a lesson_progress row inside an existing
// transaction (shared by the xAPI projection and other in-tx writers).
func upsertProgressTx(ctx context.Context, tx pgx.Tx, tenantID, enrollmentID, lessonID uuid.UUID, status string, scoreRaw *float64, now time.Time) error {
	var completedAt *time.Time
	if status == ProgressCompleted {
		completedAt = &now
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO lesson_progress
		    (tenant_id, enrollment_id, lesson_id, status, score, attempts,
		     started_at, completed_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,1,$6,$7,$6)
		 ON CONFLICT (tenant_id, enrollment_id, lesson_id) DO UPDATE
		    SET status       = EXCLUDED.status,
		        score        = COALESCE(EXCLUDED.score, lesson_progress.score),
		        attempts     = lesson_progress.attempts + 1,
		        started_at   = COALESCE(lesson_progress.started_at, EXCLUDED.started_at),
		        completed_at = COALESCE(lesson_progress.completed_at, EXCLUDED.completed_at),
		        updated_at   = EXCLUDED.updated_at`,
		tenantID, enrollmentID, lessonID, status, scoreRaw, now, completedAt,
	)
	if err != nil {
		return fmt.Errorf("project xapi to lesson_progress: %w", err)
	}
	return nil
}
