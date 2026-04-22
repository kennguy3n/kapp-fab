package ledger

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Account-type constants. Matches the CHECK constraint on accounts.type
// in migrations/000001_initial_schema.sql lines 190-198.
const (
	AccountTypeAsset     = "asset"
	AccountTypeLiability = "liability"
	AccountTypeEquity    = "equity"
	AccountTypeRevenue   = "revenue"
	AccountTypeExpense   = "expense"
)

// Journal-entry statuses (surfaced in the finance.journal_entry KType
// workflow). The typed ledger itself does not store a status column —
// `status` is tracked on the mirror KRecord. A "reversed" entry is
// represented in the ledger as a separate contra-entry rather than a
// mutation of the original; see credit_note.go.
const (
	JournalStatusDraft    = "draft"
	JournalStatusPosted   = "posted"
	JournalStatusReversed = "reversed"
)

// Tax-code types. Inclusive = price already contains tax; exclusive = tax
// added on top. Matches the CHECK in migrations/000004_finance_extensions.sql.
const (
	TaxTypeInclusive = "inclusive"
	TaxTypeExclusive = "exclusive"
)

// Account is a chart-of-accounts entry. One row per (tenant_id, code).
type Account struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	Code       string    `json:"code"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	ParentCode string    `json:"parent_code,omitempty"`
	Active     bool      `json:"active"`
}

// JournalEntry is a posted journal-entry header with its balanced lines.
// SourceKType / SourceID optionally link the entry back to the business
// record that triggered it (e.g. finance.ar_invoice + invoice UUID).
type JournalEntry struct {
	ID          uuid.UUID     `json:"id"`
	TenantID    uuid.UUID     `json:"tenant_id"`
	PostedAt    time.Time     `json:"posted_at"`
	Memo        string        `json:"memo,omitempty"`
	SourceKType string        `json:"source_ktype,omitempty"`
	SourceID    *uuid.UUID    `json:"source_id,omitempty"`
	CreatedBy   uuid.UUID     `json:"created_by"`
	CreatedAt   time.Time     `json:"created_at"`
	Lines       []JournalLine `json:"lines"`
}

// JournalLine is a single leg of a double-entry posting. Exactly one of
// Debit / Credit is > 0 on a valid line; the SQL CHECK constraint
// (migrations/000001_initial_schema.sql lines 223-225) enforces this.
type JournalLine struct {
	ID          int64           `json:"id,omitempty"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	EntryID     uuid.UUID       `json:"entry_id"`
	AccountCode string          `json:"account_code"`
	Debit       decimal.Decimal `json:"debit"`
	Credit      decimal.Decimal `json:"credit"`
	Currency    string          `json:"currency"`
	Memo        string          `json:"memo,omitempty"`
}

// FiscalPeriod represents a contiguous accounting window that may be
// locked to prevent further postings.
type FiscalPeriod struct {
	TenantID    uuid.UUID  `json:"tenant_id"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	Locked      bool       `json:"locked"`
	LockedAt    *time.Time `json:"locked_at,omitempty"`
	LockedBy    *uuid.UUID `json:"locked_by,omitempty"`
}

// TaxCode is a simple VAT/GST registry entry.
type TaxCode struct {
	TenantID uuid.UUID       `json:"tenant_id"`
	Code     string          `json:"code"`
	Name     string          `json:"name"`
	Rate     decimal.Decimal `json:"rate"`
	Type     string          `json:"type"`
	Active   bool            `json:"active"`
}

// AccountFilter narrows a ListAccounts call.
type AccountFilter struct {
	Type   string
	Active *bool
	Limit  int
	Offset int
}

// JournalEntryFilter narrows a ListJournalEntries call.
type JournalEntryFilter struct {
	From        *time.Time
	To          *time.Time
	SourceKType string
	SourceID    *uuid.UUID
	AccountCode string
	Limit       int
	Offset      int
}

// Sentinel errors the API layer surfaces as 4xx.
var (
	ErrAccountNotFound     = errors.New("ledger: account not found")
	ErrEntryNotFound       = errors.New("ledger: journal entry not found")
	ErrUnbalancedEntry     = errors.New("ledger: debits and credits must balance")
	ErrEmptyEntry          = errors.New("ledger: journal entry requires at least one line")
	ErrInactiveAccount     = errors.New("ledger: account inactive")
	ErrInvalidLine         = errors.New("ledger: invalid journal line")
	ErrPeriodLocked        = errors.New("ledger: fiscal period is locked")
	ErrPeriodNotFound      = errors.New("ledger: fiscal period not found")
	ErrCurrencyMismatch    = errors.New("ledger: journal lines must share currency")
	ErrSourceMismatch      = errors.New("ledger: source record kind mismatch")
	ErrTaxCodeNotFound     = errors.New("ledger: tax code not found")
	ErrInvoiceNotPostable  = errors.New("ledger: invoice not postable from current status")
	ErrInvoiceAlreadyPosted = errors.New("ledger: invoice already posted")
	// ErrDuplicateSourceEntry is surfaced when a caller tries to post
	// a second journal entry that references the same (source_ktype,
	// source_id). A concurrent poster or a retry-after-partial-failure
	// would otherwise double-post the ledger; the DB unique index on
	// journal_entries(tenant_id, source_ktype, source_id) WHERE
	// source_id IS NOT NULL is the hard guarantee, and this sentinel
	// lets callers detect the race and reuse the existing entry.
	ErrDuplicateSourceEntry = errors.New("ledger: journal entry already exists for source record")
)
