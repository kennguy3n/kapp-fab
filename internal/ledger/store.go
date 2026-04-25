package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
)

const (
	// pgUniqueViolation is the SQLSTATE class for unique_violation.
	// Used to detect the (tenant_id, source_ktype, source_id) partial
	// unique index collision on journal_entries inserts.
	pgUniqueViolation = "23505"

	// journalEntriesSourceUniqIndex is the name of the partial unique
	// index installed in migrations/000004_finance_extensions.sql. Only
	// a 23505 on this specific index translates to ErrDuplicateSourceEntry;
	// other unique-violations (e.g. PK collision on journal_lines) keep
	// their generic error shape.
	journalEntriesSourceUniqIndex = "journal_entries_source_uniq"
)

// PGStore implements the double-entry posting engine against PostgreSQL.
// Every mutation runs inside dbutil.WithTenantTx so the tenant GUC is
// established before any RLS-protected table is touched; the typed
// INSERT, the event-outbox emission, and the audit entry all participate
// in the same transaction — mirroring internal/record/store.go.
type PGStore struct {
	pool      *pgxpool.Pool
	publisher events.Publisher
	auditor   audit.Logger
	now       func() time.Time

	// rates resolves exchange rates for foreign-currency journal
	// lines on PostJournalEntry. Optional: when nil, posting falls
	// back to the legacy single-currency behaviour (no auto-convert,
	// base_amount stays NULL on the row).
	rates *ExchangeRateStore
}

// NewPGStore wires a PGStore from the shared pool and its collaborators.
// A nil publisher or auditor is tolerated so unit tests can run the store
// without the outbox/audit pipeline — every production caller should
// pass non-nil values so the ledger, outbox, and audit log stay
// authoritative (ARCHITECTURE.md §8 rule 7).
func NewPGStore(pool *pgxpool.Pool, publisher events.Publisher, auditor audit.Logger) *PGStore {
	return &PGStore{
		pool:      pool,
		publisher: publisher,
		auditor:   auditor,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// WithNow lets tests pin the posting clock to a deterministic value.
func (s *PGStore) WithNow(now func() time.Time) *PGStore {
	s.now = now
	return s
}

// WithExchangeRates wires the exchange-rate store so PostJournalEntry
// can convert foreign-currency lines into the tenant's base currency.
// Returns the same store for fluent chaining; passing a nil rates is
// equivalent to leaving conversion off.
func (s *PGStore) WithExchangeRates(rates *ExchangeRateStore) *PGStore {
	s.rates = rates
	return s
}

// ---------------------------------------------------------------------------
// Chart of accounts
// ---------------------------------------------------------------------------

// CreateAccount upserts a single chart-of-accounts entry. The `accounts`
// table has a composite PK on (tenant_id, code); existing codes are
// updated in place so registering a standard chart multiple times is
// idempotent.
func (s *PGStore) CreateAccount(ctx context.Context, a Account) (*Account, error) {
	if a.TenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if a.Code == "" || a.Name == "" {
		return nil, errors.New("ledger: account code and name required")
	}
	if !validAccountType(a.Type) {
		return nil, fmt.Errorf("ledger: invalid account type %q", a.Type)
	}

	out := a
	err := dbutil.WithTenantTx(ctx, s.pool, a.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var parent any
		if a.ParentCode != "" {
			parent = a.ParentCode
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO accounts (tenant_id, code, name, type, parent_code, active)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id, code) DO UPDATE SET
			     name = EXCLUDED.name,
			     type = EXCLUDED.type,
			     parent_code = EXCLUDED.parent_code,
			     active = EXCLUDED.active`,
			a.TenantID, a.Code, a.Name, a.Type, parent, a.Active,
		)
		if err != nil {
			return fmt.Errorf("ledger: upsert account: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAccount loads a single account by (tenant_id, code).
func (s *PGStore) GetAccount(ctx context.Context, tenantID uuid.UUID, code string) (*Account, error) {
	if tenantID == uuid.Nil || code == "" {
		return nil, errors.New("ledger: tenant id and code required")
	}
	var out Account
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var parent *string
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, code, name, type, parent_code, active
			 FROM accounts WHERE tenant_id = $1 AND code = $2`,
			tenantID, code,
		).Scan(&out.TenantID, &out.Code, &out.Name, &out.Type, &parent, &out.Active)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAccountNotFound
			}
			return fmt.Errorf("ledger: load account: %w", err)
		}
		if parent != nil {
			out.ParentCode = *parent
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAccounts returns the tenant's chart of accounts, optionally filtered
// by type and active flag. The result is ordered by code for stable UI
// rendering and report assembly.
func (s *PGStore) ListAccounts(ctx context.Context, tenantID uuid.UUID, filter AccountFilter) ([]Account, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 500
	}
	out := make([]Account, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			conds  []string
			args   []any
			nextID = 1
		)
		conds = append(conds, fmt.Sprintf("tenant_id = $%d", nextID))
		args = append(args, tenantID)
		nextID++
		if filter.Type != "" {
			conds = append(conds, fmt.Sprintf("type = $%d", nextID))
			args = append(args, filter.Type)
			nextID++
		}
		if filter.Active != nil {
			conds = append(conds, fmt.Sprintf("active = $%d", nextID))
			args = append(args, *filter.Active)
			nextID++
		}
		args = append(args, filter.Limit, filter.Offset)
		query := fmt.Sprintf(
			`SELECT tenant_id, code, name, type, parent_code, active
			 FROM accounts WHERE %s
			 ORDER BY code
			 LIMIT $%d OFFSET $%d`,
			strings.Join(conds, " AND "), nextID, nextID+1,
		)
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("ledger: list accounts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a Account
			var parent *string
			if err := rows.Scan(&a.TenantID, &a.Code, &a.Name, &a.Type, &parent, &a.Active); err != nil {
				return fmt.Errorf("ledger: scan account: %w", err)
			}
			if parent != nil {
				a.ParentCode = *parent
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Journal entries
// ---------------------------------------------------------------------------

// PostJournalEntry is the core double-entry posting function. It
// validates the entry is balanced, every referenced account exists and
// is active, and the posting date does not fall inside a locked fiscal
// period. On success it writes the header + lines, emits
// `finance.journal.posted` on the outbox, and logs an audit entry —
// all in a single tenant-scoped transaction.
func (s *PGStore) PostJournalEntry(ctx context.Context, entry JournalEntry) (*JournalEntry, error) {
	if entry.TenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if entry.CreatedBy == uuid.Nil {
		return nil, errors.New("ledger: created_by required")
	}
	if len(entry.Lines) == 0 {
		return nil, ErrEmptyEntry
	}
	if entry.ID == uuid.Nil {
		entry.ID = uuid.New()
	}
	if entry.PostedAt.IsZero() {
		entry.PostedAt = s.now()
	}
	entry.CreatedAt = s.now()

	if err := validateLines(entry.Lines); err != nil {
		return nil, err
	}
	currency := entry.Lines[0].Currency

	var out JournalEntry
	err := dbutil.WithTenantTx(ctx, s.pool, entry.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Period lockout — reject postings whose date falls inside a
		// closed period so finalised books can't be edited.
		locked, err := isPeriodLockedTx(ctx, tx, entry.TenantID, entry.PostedAt)
		if err != nil {
			return err
		}
		if locked {
			return fmt.Errorf("%w: %s", ErrPeriodLocked, entry.PostedAt.Format("2006-01-02"))
		}

		// Verify every referenced account exists and is active. This
		// guards against typos on agent-tool inputs and against posting
		// to retired accounts.
		codes := distinctCodes(entry.Lines)
		for _, code := range codes {
			var active bool
			err := tx.QueryRow(ctx,
				`SELECT active FROM accounts WHERE tenant_id = $1 AND code = $2`,
				entry.TenantID, code,
			).Scan(&active)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: %s", ErrAccountNotFound, code)
				}
				return fmt.Errorf("ledger: verify account: %w", err)
			}
			if !active {
				return fmt.Errorf("%w: %s", ErrInactiveAccount, code)
			}
		}

		// Insert the header. journal_entries.PRIMARY KEY = (tenant_id, id).
		var srcKType any
		var srcID any
		if entry.SourceKType != "" {
			srcKType = entry.SourceKType
		}
		if entry.SourceID != nil {
			srcID = *entry.SourceID
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO journal_entries
			     (id, tenant_id, posted_at, memo, source_ktype, source_id, created_by, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			entry.ID, entry.TenantID, entry.PostedAt, nullIfEmpty(entry.Memo),
			srcKType, srcID, entry.CreatedBy, entry.CreatedAt,
		)
		if err != nil {
			// A collision on the source-row partial unique index means
			// some other poster (concurrent or a retried caller) already
			// persisted a JE for (source_ktype, source_id). Surface
			// ErrDuplicateSourceEntry so the invoice/bill poster can
			// reuse the existing entry rather than double-post.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) &&
				pgErr.Code == pgUniqueViolation &&
				pgErr.ConstraintName == journalEntriesSourceUniqIndex {
				return ErrDuplicateSourceEntry
			}
			return fmt.Errorf("ledger: insert journal entry: %w", err)
		}

		// Resolve the tenant's base currency once per entry so the
		// per-line conversion below can compare line.Currency against
		// it. Falls back to the entry's own currency when the lookup
		// fails (e.g. tenants table not on this connection or rates
		// store not wired) — preserves legacy single-currency posts.
		baseCurrency := s.resolveBaseCurrencyTx(ctx, tx, entry.TenantID)
		if baseCurrency == "" {
			baseCurrency = currency
		}

		// Insert each line. The DB CHECK (debit >= 0 AND credit >= 0)
		// and (NOT (debit > 0 AND credit > 0)) backstop validateLines.
		insertedLines := make([]JournalLine, 0, len(entry.Lines))
		for _, line := range entry.Lines {
			line.TenantID = entry.TenantID
			line.EntryID = entry.ID
			if line.Currency == "" {
				line.Currency = currency
			}
			baseAmount, err := s.computeBaseAmount(ctx, entry.TenantID, line, baseCurrency, entry.PostedAt)
			if err != nil {
				return fmt.Errorf("ledger: convert line currency: %w", err)
			}
			var id int64
			err = tx.QueryRow(ctx,
				`INSERT INTO journal_lines
				     (tenant_id, entry_id, account_code, debit, credit, currency, memo, base_amount)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				 RETURNING id`,
				line.TenantID, line.EntryID, line.AccountCode,
				line.Debit, line.Credit, line.Currency, nullIfEmpty(line.Memo), baseAmount,
			).Scan(&id)
			if err != nil {
				return fmt.Errorf("ledger: insert journal line: %w", err)
			}
			line.ID = id
			if baseAmount != nil {
				ba := *baseAmount
				line.BaseAmount = &ba
			}
			insertedLines = append(insertedLines, line)
		}
		entry.Lines = insertedLines
		out = entry

		// Outbox event + audit entry — atomic with the ledger write.
		if s.publisher != nil {
			payload, _ := json.Marshal(map[string]any{
				"entry_id":     entry.ID,
				"posted_at":    entry.PostedAt,
				"memo":         entry.Memo,
				"source_ktype": entry.SourceKType,
				"source_id":    entry.SourceID,
				"total":        sumDebits(entry.Lines).String(),
				"currency":     currency,
				"line_count":   len(entry.Lines),
				"actor":        entry.CreatedBy,
			})
			if err := s.publisher.EmitTx(ctx, tx, events.Event{
				TenantID: entry.TenantID,
				Type:     "finance.journal.posted",
				Payload:  payload,
			}); err != nil {
				return err
			}
		}
		if s.auditor != nil {
			after, _ := json.Marshal(map[string]any{
				"entry_id":     entry.ID,
				"posted_at":    entry.PostedAt,
				"total":        sumDebits(entry.Lines).String(),
				"currency":     currency,
				"line_count":   len(entry.Lines),
				"source_ktype": entry.SourceKType,
				"source_id":    entry.SourceID,
			})
			actor := entry.CreatedBy
			if err := s.auditor.LogTx(ctx, tx, audit.Entry{
				TenantID:    entry.TenantID,
				ActorID:     &actor,
				ActorKind:   audit.ActorUser,
				Action:      "finance.journal.posted",
				TargetKType: "finance.journal_entry",
				TargetID:    &entry.ID,
				After:       after,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetJournalEntry loads a single entry with its lines ordered by line id.
func (s *PGStore) GetJournalEntry(ctx context.Context, tenantID, id uuid.UUID) (*JournalEntry, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("ledger: tenant id and entry id required")
	}
	var out JournalEntry
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			srcKType *string
			srcID    *uuid.UUID
			memo     *string
		)
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, posted_at, memo, source_ktype, source_id, created_by, created_at
			 FROM journal_entries WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(
			&out.ID, &out.TenantID, &out.PostedAt, &memo,
			&srcKType, &srcID, &out.CreatedBy, &out.CreatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrEntryNotFound
			}
			return fmt.Errorf("ledger: load journal entry: %w", err)
		}
		if memo != nil {
			out.Memo = *memo
		}
		if srcKType != nil {
			out.SourceKType = *srcKType
		}
		out.SourceID = srcID

		lines, err := loadLinesTx(ctx, tx, tenantID, out.ID)
		if err != nil {
			return err
		}
		out.Lines = lines
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetJournalEntryBySource loads the journal entry previously posted
// for the given (source_ktype, source_id). Returns ErrEntryNotFound
// when no matching entry exists. Used by InvoicePoster to make
// PostSalesInvoice / PostPurchaseBill idempotent across retries: if a
// prior posting committed the JE but crashed before patching the
// source KRecord, the next call finds the original entry instead of
// creating a duplicate.
func (s *PGStore) GetJournalEntryBySource(ctx context.Context, tenantID uuid.UUID, sourceKType string, sourceID uuid.UUID) (*JournalEntry, error) {
	if tenantID == uuid.Nil || sourceKType == "" || sourceID == uuid.Nil {
		return nil, errors.New("ledger: tenant id, source_ktype, and source_id required")
	}
	var out JournalEntry
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			srcKType *string
			srcID    *uuid.UUID
			memo     *string
		)
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, posted_at, memo, source_ktype, source_id, created_by, created_at
			 FROM journal_entries
			 WHERE tenant_id = $1 AND source_ktype = $2 AND source_id = $3
			 ORDER BY created_at ASC
			 LIMIT 1`,
			tenantID, sourceKType, sourceID,
		).Scan(
			&out.ID, &out.TenantID, &out.PostedAt, &memo,
			&srcKType, &srcID, &out.CreatedBy, &out.CreatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrEntryNotFound
			}
			return fmt.Errorf("ledger: load journal entry by source: %w", err)
		}
		if memo != nil {
			out.Memo = *memo
		}
		if srcKType != nil {
			out.SourceKType = *srcKType
		}
		out.SourceID = srcID

		lines, err := loadLinesTx(ctx, tx, tenantID, out.ID)
		if err != nil {
			return err
		}
		out.Lines = lines
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListJournalEntries returns posted entries ordered by posted_at DESC.
// The line-level AccountCode filter joins back to journal_lines so the
// UI can drill into a single account's activity.
func (s *PGStore) ListJournalEntries(ctx context.Context, tenantID uuid.UUID, filter JournalEntryFilter) ([]JournalEntry, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	out := make([]JournalEntry, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			conds  = []string{"je.tenant_id = $1"}
			args   = []any{tenantID}
			nextID = 2
			join   = ""
		)
		if filter.From != nil {
			conds = append(conds, fmt.Sprintf("je.posted_at >= $%d", nextID))
			args = append(args, *filter.From)
			nextID++
		}
		if filter.To != nil {
			conds = append(conds, fmt.Sprintf("je.posted_at <= $%d", nextID))
			args = append(args, *filter.To)
			nextID++
		}
		if filter.SourceKType != "" {
			conds = append(conds, fmt.Sprintf("je.source_ktype = $%d", nextID))
			args = append(args, filter.SourceKType)
			nextID++
		}
		if filter.SourceID != nil {
			conds = append(conds, fmt.Sprintf("je.source_id = $%d", nextID))
			args = append(args, *filter.SourceID)
			nextID++
		}
		if filter.AccountCode != "" {
			join = `JOIN journal_lines jl ON jl.tenant_id = je.tenant_id AND jl.entry_id = je.id`
			conds = append(conds, fmt.Sprintf("jl.account_code = $%d", nextID))
			args = append(args, filter.AccountCode)
			nextID++
		}
		args = append(args, filter.Limit, filter.Offset)
		query := fmt.Sprintf(
			`SELECT DISTINCT je.id, je.tenant_id, je.posted_at, je.memo,
			                 je.source_ktype, je.source_id, je.created_by, je.created_at
			 FROM journal_entries je %s
			 WHERE %s
			 ORDER BY je.posted_at DESC, je.id DESC
			 LIMIT $%d OFFSET $%d`,
			join, strings.Join(conds, " AND "), nextID, nextID+1,
		)
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("ledger: list journal entries: %w", err)
		}
		headers := make([]JournalEntry, 0)
		for rows.Next() {
			var (
				h        JournalEntry
				memo     *string
				srcKType *string
				srcID    *uuid.UUID
			)
			if err := rows.Scan(
				&h.ID, &h.TenantID, &h.PostedAt, &memo,
				&srcKType, &srcID, &h.CreatedBy, &h.CreatedAt,
			); err != nil {
				rows.Close()
				return fmt.Errorf("ledger: scan journal entry: %w", err)
			}
			if memo != nil {
				h.Memo = *memo
			}
			if srcKType != nil {
				h.SourceKType = *srcKType
			}
			h.SourceID = srcID
			headers = append(headers, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("ledger: rows: %w", err)
		}

		// Hydrate lines. N+1 is fine at the current scale (per-tenant
		// reports render a few hundred entries at most); a batched
		// join can replace this when we hit pagination limits.
		for i := range headers {
			lines, err := loadLinesTx(ctx, tx, tenantID, headers[i].ID)
			if err != nil {
				return err
			}
			headers[i].Lines = lines
		}
		out = headers
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers (unexported)
// ---------------------------------------------------------------------------

func validateLines(lines []JournalLine) error {
	if len(lines) == 0 {
		return ErrEmptyEntry
	}
	var debit, credit decimal.Decimal
	currency := ""
	for i, line := range lines {
		if line.AccountCode == "" {
			return fmt.Errorf("%w: line %d missing account_code", ErrInvalidLine, i)
		}
		if line.Currency == "" {
			return fmt.Errorf("%w: line %d missing currency", ErrInvalidLine, i)
		}
		if currency == "" {
			currency = line.Currency
		} else if line.Currency != currency {
			return fmt.Errorf("%w: line %d currency %q != %q", ErrCurrencyMismatch, i, line.Currency, currency)
		}
		if line.Debit.IsNegative() || line.Credit.IsNegative() {
			return fmt.Errorf("%w: line %d has negative debit/credit", ErrInvalidLine, i)
		}
		if line.Debit.IsPositive() && line.Credit.IsPositive() {
			return fmt.Errorf("%w: line %d has both debit and credit", ErrInvalidLine, i)
		}
		if line.Debit.IsZero() && line.Credit.IsZero() {
			return fmt.Errorf("%w: line %d has zero debit and credit", ErrInvalidLine, i)
		}
		debit = debit.Add(line.Debit)
		credit = credit.Add(line.Credit)
	}
	if !debit.Equal(credit) {
		return fmt.Errorf("%w: debits=%s credits=%s", ErrUnbalancedEntry, debit, credit)
	}
	return nil
}

func sumDebits(lines []JournalLine) decimal.Decimal {
	total := decimal.Zero
	for _, l := range lines {
		total = total.Add(l.Debit)
	}
	return total
}

func distinctCodes(lines []JournalLine) []string {
	seen := make(map[string]struct{}, len(lines))
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if _, ok := seen[l.AccountCode]; ok {
			continue
		}
		seen[l.AccountCode] = struct{}{}
		out = append(out, l.AccountCode)
	}
	return out
}

func loadLinesTx(ctx context.Context, tx pgx.Tx, tenantID, entryID uuid.UUID) ([]JournalLine, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tenant_id, entry_id, account_code, debit, credit, currency, memo
		 FROM journal_lines
		 WHERE tenant_id = $1 AND entry_id = $2
		 ORDER BY id`,
		tenantID, entryID,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: load lines: %w", err)
	}
	defer rows.Close()
	out := make([]JournalLine, 0)
	for rows.Next() {
		var (
			line JournalLine
			memo *string
		)
		if err := rows.Scan(
			&line.ID, &line.TenantID, &line.EntryID, &line.AccountCode,
			&line.Debit, &line.Credit, &line.Currency, &memo,
		); err != nil {
			return nil, fmt.Errorf("ledger: scan line: %w", err)
		}
		if memo != nil {
			line.Memo = *memo
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: rows: %w", err)
	}
	return out, nil
}

func validAccountType(t string) bool {
	switch t {
	case AccountTypeAsset, AccountTypeLiability, AccountTypeEquity, AccountTypeRevenue, AccountTypeExpense:
		return true
	default:
		return false
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// resolveBaseCurrencyTx looks up the tenant's functional currency for
// the active transaction. The tenants row is read inside the existing
// tenant-scoped tx so RLS still applies. Returns an empty string when
// the lookup fails (e.g. control-plane connection on a different
// pool, mock pool in tests) — callers fall back to the entry's
// declared currency in that case.
func (s *PGStore) resolveBaseCurrencyTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) string {
	var code string
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(base_currency, 'USD') FROM tenants WHERE id = $1`,
		tenantID,
	).Scan(&code)
	if err != nil {
		return ""
	}
	return code
}

// computeBaseAmount converts the line's net (debit − credit) into the
// tenant's base currency. Returns nil + nil when no conversion is
// needed (currency match, no rates store wired, or a zero-amount
// line). Returns an error when a rate lookup fails — callers should
// surface it so the entry rolls back rather than posting an
// inaccurate base figure.
func (s *PGStore) computeBaseAmount(ctx context.Context, tenantID uuid.UUID, line JournalLine, baseCurrency string, asOf time.Time) (*decimal.Decimal, error) {
	if line.Currency == "" || line.Currency == baseCurrency {
		return nil, nil
	}
	net := line.Debit.Sub(line.Credit)
	if net.IsZero() {
		zero := decimal.Zero
		return &zero, nil
	}
	if s.rates == nil {
		// Conversion store not wired — leave base_amount NULL so
		// reports fall back to the line currency. This is the
		// pre-000029 behaviour and keeps unit tests with mock
		// pools working without an exchange-rate dependency.
		return nil, nil
	}
	if asOf.IsZero() {
		asOf = s.now()
	}
	converted, err := s.rates.Convert(ctx, tenantID, net, line.Currency, baseCurrency, asOf)
	if err != nil {
		return nil, err
	}
	return &converted, nil
}
