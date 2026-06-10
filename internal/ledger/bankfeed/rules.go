package bankfeed

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Condition types, mirroring the bank_reconciliation_rules CHECK.
const (
	CondDescriptionContains = "description_contains"
	CondDescriptionRegex    = "description_regex"
	CondAmountRange         = "amount_range"
	CondCounterparty        = "counterparty"
)

// Rule is one tenant-configurable auto-categorization rule. Rules are
// evaluated in ascending priority order against each freshly-synced
// transaction; the first match decides the target account / cost center
// and whether the categorization auto-approves (skips the suggestion
// queue). A nil BankAccountID applies the rule tenant-wide.
type Rule struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	Priority          int
	ConditionType     string
	ConditionValue    string
	TargetAccountCode string
	TargetCostCenter  string
	AutoApprove       bool
	BankAccountID     *uuid.UUID
	Enabled           bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RuleMatch is the outcome of evaluating the rule set against a
// transaction: which rule fired and the action it dictates.
type RuleMatch struct {
	Rule              Rule
	TargetAccountCode string
	TargetCostCenter  string
	AutoApprove       bool
}

// Validate checks a rule's structural correctness before persistence. A
// regex condition is compiled here so an invalid pattern is rejected at
// write time rather than silently never-matching at sync time.
func (r Rule) Validate() error {
	switch r.ConditionType {
	case CondDescriptionContains, CondCounterparty:
		if strings.TrimSpace(r.ConditionValue) == "" {
			return fmt.Errorf("bankfeed: %s rule requires a non-empty value", r.ConditionType)
		}
	case CondDescriptionRegex:
		if _, err := regexp.Compile(r.ConditionValue); err != nil {
			return fmt.Errorf("bankfeed: invalid regex %q: %w", r.ConditionValue, err)
		}
	case CondAmountRange:
		if _, _, err := parseAmountRange(r.ConditionValue); err != nil {
			return err
		}
	default:
		return fmt.Errorf("bankfeed: unknown condition_type %q", r.ConditionType)
	}
	if r.TargetAccountCode == "" && r.TargetCostCenter == "" && !r.AutoApprove {
		return errors.New("bankfeed: rule must set an account code, cost center, or auto_approve")
	}
	return nil
}

// matches reports whether the rule's condition is satisfied by txn. The
// account-scope filter is applied by the caller (the query) so this only
// evaluates the condition itself.
func (r Rule) matches(txn RawTransaction) bool {
	switch r.ConditionType {
	case CondDescriptionContains:
		return strings.Contains(strings.ToLower(txn.Description), strings.ToLower(r.ConditionValue))
	case CondDescriptionRegex:
		re, err := regexp.Compile(r.ConditionValue)
		if err != nil {
			return false // already rejected at write time; defensive here
		}
		return re.MatchString(txn.Description)
	case CondCounterparty:
		cp := txn.Counterparty
		if cp == "" {
			cp = txn.Description
		}
		return strings.Contains(strings.ToLower(cp), strings.ToLower(r.ConditionValue))
	case CondAmountRange:
		lo, hi, err := parseAmountRange(r.ConditionValue)
		if err != nil {
			return false
		}
		if lo != nil && txn.Amount.LessThan(*lo) {
			return false
		}
		if hi != nil && txn.Amount.GreaterThan(*hi) {
			return false
		}
		return true
	default:
		return false
	}
}

// Evaluate runs the (already priority-ordered) rules against txn and
// returns the first match. The rules slice is assumed sorted ascending by
// priority — RuleStore.ListRules guarantees this. Disabled rules must be
// filtered by the caller (the query does).
func Evaluate(rules []Rule, txn RawTransaction) (RuleMatch, bool) {
	for i := range rules {
		r := &rules[i]
		if r.matches(txn) {
			return RuleMatch{
				Rule:              *r,
				TargetAccountCode: r.TargetAccountCode,
				TargetCostCenter:  r.TargetCostCenter,
				AutoApprove:       r.AutoApprove,
			}, true
		}
	}
	return RuleMatch{}, false
}

// parseAmountRange parses a "min:max" condition value where either bound
// may be empty for an open interval (e.g. "100:" = >=100, ":0" = <=0,
// "-50:50" = between). Bounds are inclusive. Returns (lo, hi) as nil when
// open.
func parseAmountRange(s string) (lo, hi *decimal.Decimal, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("bankfeed: amount_range must be \"min:max\" (got %q)", s)
	}
	parseBound := func(raw string) (*decimal.Decimal, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, nil
		}
		d, err := decimal.NewFromString(raw)
		if err != nil {
			return nil, fmt.Errorf("bankfeed: amount_range bound %q: %w", raw, err)
		}
		return &d, nil
	}
	if lo, err = parseBound(parts[0]); err != nil {
		return nil, nil, err
	}
	if hi, err = parseBound(parts[1]); err != nil {
		return nil, nil, err
	}
	if lo != nil && hi != nil && lo.GreaterThan(*hi) {
		return nil, nil, fmt.Errorf("bankfeed: amount_range min %s exceeds max %s", lo, hi)
	}
	return lo, hi, nil
}

// RuleStore is the CRUD + query surface for bank_reconciliation_rules.
// Every mutation runs under WithTenantTx and emits an audit entry.
type RuleStore struct {
	pool    *pgxpool.Pool
	auditor audit.Logger
	now     func() time.Time
}

// NewRuleStore wires a rule store.
func NewRuleStore(pool *pgxpool.Pool, auditor audit.Logger) *RuleStore {
	return &RuleStore{pool: pool, auditor: auditor, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock pins the clock for deterministic tests.
func (s *RuleStore) WithClock(now func() time.Time) *RuleStore {
	if now != nil {
		s.now = now
	}
	return s
}

// UpsertRule inserts or updates a rule by (tenant_id, id), validating it
// first so an invalid regex/range never lands in the table.
func (s *RuleStore) UpsertRule(ctx context.Context, r Rule) (*Rule, error) {
	if r.TenantID == uuid.Nil {
		return nil, errors.New("bankfeed: tenant id required")
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	now := s.now()
	out := r
	err := dbutil.WithTenantTx(ctx, s.pool, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO bank_reconciliation_rules
			     (tenant_id, id, priority, condition_type, condition_value,
			      target_account_code, target_cost_center, auto_approve,
			      bank_account_id, enabled, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET
			     priority            = EXCLUDED.priority,
			     condition_type      = EXCLUDED.condition_type,
			     condition_value     = EXCLUDED.condition_value,
			     target_account_code = EXCLUDED.target_account_code,
			     target_cost_center  = EXCLUDED.target_cost_center,
			     auto_approve        = EXCLUDED.auto_approve,
			     bank_account_id     = EXCLUDED.bank_account_id,
			     enabled             = EXCLUDED.enabled,
			     updated_at          = EXCLUDED.updated_at`,
			r.TenantID, r.ID, r.Priority, r.ConditionType, r.ConditionValue,
			nullIfEmpty(r.TargetAccountCode), nullIfEmpty(r.TargetCostCenter), r.AutoApprove,
			r.BankAccountID, r.Enabled, now,
		); err != nil {
			return fmt.Errorf("bankfeed: upsert rule: %w", err)
		}
		return s.auditRule(ctx, tx, r, "finance.bank_feed.rule.upsert")
	})
	if err != nil {
		return nil, err
	}
	out.CreatedAt = now
	out.UpdatedAt = now
	return &out, nil
}

// DeleteRule removes a rule by id. Emits an audit entry.
func (s *RuleStore) DeleteRule(ctx context.Context, tenantID, id uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`DELETE FROM bank_reconciliation_rules WHERE tenant_id = $1 AND id = $2`,
			tenantID, id)
		if err != nil {
			return fmt.Errorf("bankfeed: delete rule: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("bankfeed: rule %s not found", id)
		}
		return s.auditRule(ctx, tx, Rule{TenantID: tenantID, ID: id}, "finance.bank_feed.rule.delete")
	})
}

// ListRules returns enabled rules applicable to bankAccountID (account-
// scoped plus tenant-wide rules), priority ascending. A nil bankAccountID
// returns only the tenant-wide rules. The ordering is the evaluation
// order Evaluate relies on; ties break by created_at so behaviour is
// deterministic across syncs.
func (s *RuleStore) ListRules(ctx context.Context, tenantID uuid.UUID, bankAccountID *uuid.UUID) ([]Rule, error) {
	sql := `SELECT id, tenant_id, priority, condition_type, condition_value,
	               target_account_code, target_cost_center, auto_approve,
	               bank_account_id, enabled, created_at, updated_at
	          FROM bank_reconciliation_rules
	         WHERE tenant_id = $1 AND enabled
	           AND (bank_account_id IS NULL OR bank_account_id = $2)
	         ORDER BY priority ASC, created_at ASC`
	return s.queryRules(ctx, tenantID, sql, tenantID, bankAccountID)
}

// ListAllRules returns every rule (enabled or not) for the tenant, used
// by the rule-editor UI. Priority ascending.
func (s *RuleStore) ListAllRules(ctx context.Context, tenantID uuid.UUID) ([]Rule, error) {
	sql := `SELECT id, tenant_id, priority, condition_type, condition_value,
	               target_account_code, target_cost_center, auto_approve,
	               bank_account_id, enabled, created_at, updated_at
	          FROM bank_reconciliation_rules
	         WHERE tenant_id = $1
	         ORDER BY priority ASC, created_at ASC`
	return s.queryRules(ctx, tenantID, sql, tenantID)
}

func (s *RuleStore) queryRules(ctx context.Context, tenantID uuid.UUID, sql string, args ...any) ([]Rule, error) {
	var out []Rule
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("bankfeed: query rules: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				r       Rule
				acct    *string
				cc      *string
				account *uuid.UUID
			)
			if err := rows.Scan(&r.ID, &r.TenantID, &r.Priority, &r.ConditionType, &r.ConditionValue,
				&acct, &cc, &r.AutoApprove, &account, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
				return err
			}
			if acct != nil {
				r.TargetAccountCode = *acct
			}
			if cc != nil {
				r.TargetCostCenter = *cc
			}
			r.BankAccountID = account
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *RuleStore) auditRule(ctx context.Context, tx pgx.Tx, r Rule, action string) error {
	if s.auditor == nil {
		return nil
	}
	id := r.ID
	return s.auditor.LogTx(ctx, tx, audit.Entry{
		TenantID:    r.TenantID,
		ActorKind:   audit.ActorSystem,
		Action:      action,
		TargetKType: "finance.bank_reconciliation_rule",
		TargetID:    &id,
	})
}
