package bankfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Condition types, mirroring the bank_reconciliation_rules CHECK. These
// are the legacy single-condition kinds; a rule carrying a non-empty
// Conditions slice uses the compound model below instead and leaves
// ConditionType empty.
const (
	CondDescriptionContains = "description_contains"
	CondDescriptionRegex    = "description_regex"
	CondAmountRange         = "amount_range"
	CondCounterparty        = "counterparty"
)

// Condition fields for the compound model — the transaction attribute a
// condition tests. Payee maps to Counterparty (falling back to
// Description when the provider does not expose a counterparty); Reference
// maps to the structured payment reference (falling back to Description).
const (
	FieldPayee       = "payee"
	FieldReference   = "reference"
	FieldDescription = "description"
	FieldAmount      = "amount"
)

// Condition operators for the compound model. The string operators
// (contains / equals / regex) apply to payee / reference / description;
// the numeric operators (eq / gt / gte / lt / lte / range) apply to
// amount. "regex" is a full RE2 pattern (Go regexp) — safe and linear, no
// catastrophic backtracking — kept "lite" by being matched against a
// single field value rather than the whole row.
const (
	OpContains = "contains"
	OpEquals   = "equals"
	OpRegex    = "regex"
	OpEq       = "eq"
	OpGt       = "gt"
	OpGte      = "gte"
	OpLt       = "lt"
	OpLte      = "lte"
	OpRange    = "range"
)

// Condition-combination modes for a compound rule.
const (
	MatchAll = "all"
	MatchAny = "any"
)

// RuleCondition is one structured predicate in a compound rule: a Field
// tested by an Op against a Value. Amount conditions parse Value as a
// decimal (or, for OpRange, a "min:max" interval); string conditions treat
// Value as the literal / pattern. The shape is persisted as a JSONB array
// element on bank_reconciliation_rules.conditions, so the json tags are
// part of the stored contract.
type RuleCondition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// Rule is one tenant-configurable auto-categorization rule. Rules are
// evaluated in ascending priority order against each freshly-synced
// transaction; the first match decides the target account / cost center
// and whether the categorization auto-approves (skips the suggestion
// queue). A nil BankAccountID applies the rule tenant-wide.
type Rule struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	Priority       int
	ConditionType  string
	ConditionValue string
	// Conditions is the compound, multi-field predicate set (Xero "bank
	// rule" parity). When non-empty it supersedes the legacy
	// ConditionType/ConditionValue pair; ConditionMatch decides whether all
	// or any must hold. Empty means the rule uses the legacy single
	// condition.
	Conditions []RuleCondition
	// ConditionMatch is MatchAll (default) or MatchAny — how Conditions
	// combine. Ignored on the legacy single-condition path.
	ConditionMatch    string
	TargetAccountCode string
	TargetCostCenter  string
	// TargetTaxCode is the tax/VAT code the rule allocates alongside the
	// account + cost-center. Advisory rule configuration consumed by the
	// (separate) auto-posting path, not by the reconciliation matcher.
	TargetTaxCode string
	AutoApprove   bool
	BankAccountID *uuid.UUID
	Enabled       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RuleMatch is the outcome of evaluating the rule set against a
// transaction: which rule fired and the action it dictates.
type RuleMatch struct {
	Rule              Rule
	TargetAccountCode string
	TargetCostCenter  string
	TargetTaxCode     string
	AutoApprove       bool
}

// Validate checks a rule's structural correctness before persistence. A
// regex condition is compiled here so an invalid pattern is rejected at
// write time rather than silently never-matching at sync time. A rule
// carrying a non-empty Conditions slice is validated as a compound rule
// (the legacy ConditionType is ignored and must be empty); otherwise the
// legacy single-condition path is validated.
func (r Rule) Validate() error {
	if len(r.Conditions) > 0 {
		if r.ConditionType != "" {
			return errors.New("bankfeed: a compound rule must not also set condition_type")
		}
		if err := validateConditionMatch(r.ConditionMatch); err != nil {
			return err
		}
		for i := range r.Conditions {
			if err := r.Conditions[i].validate(); err != nil {
				return err
			}
		}
	} else {
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
	}
	if r.TargetAccountCode == "" && r.TargetCostCenter == "" && r.TargetTaxCode == "" && !r.AutoApprove {
		return errors.New("bankfeed: rule must set an account code, cost center, tax code, or auto_approve")
	}
	return nil
}

// validateConditionMatch accepts the empty string (normalized to MatchAll
// at evaluation time) or one of the two explicit modes.
func validateConditionMatch(m string) error {
	switch m {
	case "", MatchAll, MatchAny:
		return nil
	default:
		return fmt.Errorf("bankfeed: condition_match must be %q or %q (got %q)", MatchAll, MatchAny, m)
	}
}

// validate checks one compound condition: a known field, an operator
// valid for that field's type, and a parseable value.
func (c RuleCondition) validate() error {
	switch c.Field {
	case FieldPayee, FieldReference, FieldDescription:
		switch c.Op {
		case OpContains, OpEquals:
			if strings.TrimSpace(c.Value) == "" {
				return fmt.Errorf("bankfeed: %s %s condition requires a non-empty value", c.Field, c.Op)
			}
		case OpRegex:
			if _, err := regexp.Compile(c.Value); err != nil {
				return fmt.Errorf("bankfeed: invalid regex %q: %w", c.Value, err)
			}
		default:
			return fmt.Errorf("bankfeed: operator %q is not valid for the text field %q", c.Op, c.Field)
		}
	case FieldAmount:
		switch c.Op {
		case OpEq, OpGt, OpGte, OpLt, OpLte:
			if _, err := decimal.NewFromString(strings.TrimSpace(c.Value)); err != nil {
				return fmt.Errorf("bankfeed: amount %s condition value %q: %w", c.Op, c.Value, err)
			}
		case OpRange:
			if _, _, err := parseAmountRange(c.Value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("bankfeed: operator %q is not valid for the amount field", c.Op)
		}
	default:
		return fmt.Errorf("bankfeed: unknown condition field %q", c.Field)
	}
	return nil
}

// regexCache memoizes compiled rule regexes. Rule values are validated at
// write time, so a bounded set of patterns is re-evaluated against every
// line of a sync batch (and across batches/tenants); compiling once per
// distinct pattern avoids recompiling the same RE2 program hundreds of
// times. Only successful compilations are stored — an invalid pattern is
// rejected at write time, and the evaluation paths treat a compile error as
// no-match — so the map size is bounded by the number of distinct valid
// patterns, not by adversarial input.
var regexCache sync.Map // map[string]*regexp.Regexp

// compileRuleRegex returns a cached compiled regex for pattern, compiling
// and memoizing on first use. Concurrency-safe; a benign duplicate compile
// can occur under a race but both callers observe an equivalent program.
func compileRuleRegex(pattern string) (*regexp.Regexp, error) {
	if v, ok := regexCache.Load(pattern); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re, nil
		}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexCache.Store(pattern, re)
	return re, nil
}

// matches reports whether the rule's condition is satisfied by txn. The
// account-scope filter is applied by the caller (the query) so this only
// evaluates the condition itself. A compound rule (non-empty Conditions)
// combines its conditions per ConditionMatch; otherwise the legacy single
// condition is evaluated.
func (r Rule) matches(txn RawTransaction) bool {
	if len(r.Conditions) > 0 {
		return r.matchesCompound(txn)
	}
	switch r.ConditionType {
	case CondDescriptionContains:
		return strings.Contains(strings.ToLower(txn.Description), strings.ToLower(r.ConditionValue))
	case CondDescriptionRegex:
		re, err := compileRuleRegex(r.ConditionValue)
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

// matchesCompound evaluates the compound Conditions per ConditionMatch:
// MatchAny fires on the first condition that holds; MatchAll (the default,
// including the empty string) requires every condition to hold. An empty
// Conditions slice never reaches here (matches() routes to the legacy
// path), so MatchAll over zero conditions is not a concern.
func (r Rule) matchesCompound(txn RawTransaction) bool {
	matchAny := r.ConditionMatch == MatchAny
	for i := range r.Conditions {
		ok := r.Conditions[i].matches(txn)
		if matchAny && ok {
			return true
		}
		if !matchAny && !ok {
			return false
		}
	}
	// MatchAny fell through with no hit -> false; MatchAll fell through with
	// no miss -> true.
	return !matchAny
}

// matches reports whether one compound condition holds for txn. An
// unparseable amount / regex value returns false (it was rejected at write
// time; this is defensive only).
func (c RuleCondition) matches(txn RawTransaction) bool {
	switch c.Field {
	case FieldAmount:
		return c.matchesAmount(txn.Amount)
	case FieldPayee:
		return c.matchesText(fallback(txn.Counterparty, txn.Description))
	case FieldReference:
		return c.matchesText(fallback(txn.Reference, txn.Description))
	case FieldDescription:
		return c.matchesText(txn.Description)
	default:
		return false
	}
}

// matchesText applies a string operator case-insensitively. equals is a
// full (trimmed) match; contains is a substring; regex is an RE2 search.
func (c RuleCondition) matchesText(field string) bool {
	switch c.Op {
	case OpContains:
		return strings.Contains(strings.ToLower(field), strings.ToLower(c.Value))
	case OpEquals:
		return strings.EqualFold(strings.TrimSpace(field), strings.TrimSpace(c.Value))
	case OpRegex:
		re, err := compileRuleRegex(c.Value)
		if err != nil {
			return false
		}
		return re.MatchString(field)
	default:
		return false
	}
}

// matchesAmount applies a numeric operator. The signed amount is compared
// as-is so a rule can target money-out (negative) or money-in (positive)
// explicitly; a range with both bounds matches the inclusive interval.
func (c RuleCondition) matchesAmount(amount decimal.Decimal) bool {
	if c.Op == OpRange {
		lo, hi, err := parseAmountRange(c.Value)
		if err != nil {
			return false
		}
		if lo != nil && amount.LessThan(*lo) {
			return false
		}
		if hi != nil && amount.GreaterThan(*hi) {
			return false
		}
		return true
	}
	v, err := decimal.NewFromString(strings.TrimSpace(c.Value))
	if err != nil {
		return false
	}
	switch c.Op {
	case OpEq:
		return amount.Equal(v)
	case OpGt:
		return amount.GreaterThan(v)
	case OpGte:
		return amount.GreaterThanOrEqual(v)
	case OpLt:
		return amount.LessThan(v)
	case OpLte:
		return amount.LessThanOrEqual(v)
	default:
		return false
	}
}

// fallback returns primary when non-empty, else secondary.
func fallback(primary, secondary string) string {
	if primary != "" {
		return primary
	}
	return secondary
}

// Evaluate runs the (already priority-ordered) rules against txn and
// returns the first match. The rules slice is assumed sorted ascending by
// priority — RuleStore.ListRules guarantees this. Disabled rules must be
// filtered by the caller (the query does).
func Evaluate(rules []Rule, txn RawTransaction) (RuleMatch, bool) {
	m, _, ok := EvaluateIndexed(rules, txn)
	return m, ok
}

// EvaluateIndexed is Evaluate that also returns the zero-based position of
// the matching rule in the slice (-1 when nothing matched), so the rule
// preview surface can tell the operator which rule fired without
// re-deriving it by (fragile) field comparison.
func EvaluateIndexed(rules []Rule, txn RawTransaction) (RuleMatch, int, bool) {
	for i := range rules {
		r := &rules[i]
		if r.matches(txn) {
			return RuleMatch{
				Rule:              *r,
				TargetAccountCode: r.TargetAccountCode,
				TargetCostCenter:  r.TargetCostCenter,
				TargetTaxCode:     r.TargetTaxCode,
				AutoApprove:       r.AutoApprove,
			}, i, true
		}
	}
	return RuleMatch{}, -1, false
}

// ruleSelect is the column list shared by the rule reads. Column order is
// the contract queryRules' scan relies on.
const ruleSelect = `SELECT id, tenant_id, priority, condition_type, condition_value,
	               conditions, condition_match, target_account_code,
	               target_cost_center, target_tax_code, auto_approve,
	               bank_account_id, enabled, created_at, updated_at
	          FROM bank_reconciliation_rules`

// marshalConditions serializes the compound conditions for the JSONB
// column, returning a SQL NULL when there are none so a legacy rule's
// column stays NULL rather than an empty-array literal.
func marshalConditions(conds []RuleCondition) (any, error) {
	if len(conds) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(conds)
	if err != nil {
		return nil, fmt.Errorf("bankfeed: marshal rule conditions: %w", err)
	}
	return b, nil
}

// unmarshalConditions parses the JSONB conditions column. A NULL / empty
// column yields a nil slice (the legacy single-condition path).
func unmarshalConditions(raw []byte) ([]RuleCondition, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var conds []RuleCondition
	if err := json.Unmarshal(raw, &conds); err != nil {
		return nil, fmt.Errorf("bankfeed: unmarshal rule conditions: %w", err)
	}
	if len(conds) == 0 {
		return nil, nil
	}
	return conds, nil
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
		// RETURNING created_at/updated_at so the update path reports the
		// true persisted created_at (the DB keeps the original; it is not
		// in the SET clause) rather than the current time.
		conds, err := marshalConditions(r.Conditions)
		if err != nil {
			return err
		}
		// Normalize a blank match mode to MatchAll for every rule, compound
		// or legacy. The column is NOT NULL with a CHECK in ('all','any'),
		// and the INSERT supplies condition_match explicitly (so the column
		// DEFAULT never applies); passing "" would fail the constraint on a
		// legacy single-condition rule, which leaves ConditionMatch unset.
		condMatch := r.ConditionMatch
		if condMatch == "" {
			condMatch = MatchAll
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO bank_reconciliation_rules
			     (tenant_id, id, priority, condition_type, condition_value,
			      conditions, condition_match, target_account_code,
			      target_cost_center, target_tax_code, auto_approve,
			      bank_account_id, enabled, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET
			     priority            = EXCLUDED.priority,
			     condition_type      = EXCLUDED.condition_type,
			     condition_value     = EXCLUDED.condition_value,
			     conditions          = EXCLUDED.conditions,
			     condition_match     = EXCLUDED.condition_match,
			     target_account_code = EXCLUDED.target_account_code,
			     target_cost_center  = EXCLUDED.target_cost_center,
			     target_tax_code     = EXCLUDED.target_tax_code,
			     auto_approve        = EXCLUDED.auto_approve,
			     bank_account_id     = EXCLUDED.bank_account_id,
			     enabled             = EXCLUDED.enabled,
			     updated_at          = EXCLUDED.updated_at
			 RETURNING created_at, updated_at`,
			r.TenantID, r.ID, r.Priority, nullIfEmpty(r.ConditionType), nullIfEmpty(r.ConditionValue),
			conds, condMatch, nullIfEmpty(r.TargetAccountCode),
			nullIfEmpty(r.TargetCostCenter), nullIfEmpty(r.TargetTaxCode), r.AutoApprove,
			r.BankAccountID, r.Enabled, now,
		).Scan(&out.CreatedAt, &out.UpdatedAt); err != nil {
			return fmt.Errorf("bankfeed: upsert rule: %w", err)
		}
		return s.auditRule(ctx, tx, r, "finance.bank_feed.rule.upsert")
	})
	if err != nil {
		return nil, err
	}
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
			return fmt.Errorf("bankfeed: rule %s: %w", id, ErrNotFound)
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
	sql := ruleSelect + `
	         WHERE tenant_id = $1 AND enabled
	           AND (bank_account_id IS NULL OR bank_account_id = $2)
	         ORDER BY priority ASC, created_at ASC`
	return s.queryRules(ctx, tenantID, sql, tenantID, bankAccountID)
}

// ListAllRules returns every rule (enabled or not) for the tenant, used
// by the rule-editor UI. Priority ascending.
func (s *RuleStore) ListAllRules(ctx context.Context, tenantID uuid.UUID) ([]Rule, error) {
	sql := ruleSelect + `
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
				r         Rule
				condType  *string
				condValue *string
				conds     []byte
				acct      *string
				cc        *string
				taxCode   *string
				account   *uuid.UUID
			)
			if err := rows.Scan(&r.ID, &r.TenantID, &r.Priority, &condType, &condValue,
				&conds, &r.ConditionMatch, &acct, &cc, &taxCode,
				&r.AutoApprove, &account, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
				return err
			}
			if condType != nil {
				r.ConditionType = *condType
			}
			if condValue != nil {
				r.ConditionValue = *condValue
			}
			if r.Conditions, err = unmarshalConditions(conds); err != nil {
				return err
			}
			if acct != nil {
				r.TargetAccountCode = *acct
			}
			if cc != nil {
				r.TargetCostCenter = *cc
			}
			if taxCode != nil {
				r.TargetTaxCode = *taxCode
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
