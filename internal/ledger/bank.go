// Package ledger already hosts the typed double-entry primitives; this
// file adds the bank-account and reconciliation KTypes + typed-row
// helpers used by the reconciliation tool. The raw bank statement
// lines live in a dedicated `bank_transactions` table (see
// migrations/000011_sales_procurement_bank.sql) so we can index them
// by (tenant_id, bank_account_id, value_date) for the matcher; the
// bank_accounts metadata itself is both a typed row and a KRecord so
// the UI + agent tools can drive off a single KType.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KType identifiers for the bank-reconciliation surface.
const (
	KTypeBankAccount     = "finance.bank_account"
	KTypeBankTransaction = "finance.bank_transaction"
)

// bankAccountSchema — the KRecord mirror of the typed `bank_accounts`
// table. `account_code` refs the GL account (finance.account.code)
// that bank activity posts against, so a statement import can fan
// out to the right ledger leg without a lookup per row.
var bankAccountSchema = []byte(`{
  "name": "finance.bank_account",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "account_code", "type": "string", "required": true, "max_length": 32},
    {"name": "bank_name", "type": "string", "max_length": 200},
    {"name": "account_number", "type": "string", "max_length": 64},
    {"name": "iban", "type": "string", "max_length": 64},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "required": true},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["name", "bank_name", "account_number", "currency", "active"]},
    "form": {"sections": [
      {"title": "Bank Account", "fields": ["name", "bank_name", "account_code", "account_number", "iban", "currency", "active"]}
    ]}
  },
  "cards": {"summary": "{{name}} — {{bank_name}} ({{currency}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]}
}`)

// bankTransactionSchema — a single statement line. `matched_entry_id`
// is populated when the reconciliation matcher pairs the statement
// line to a journal entry; an empty value means "unreconciled".
var bankTransactionSchema = []byte(`{
  "name": "finance.bank_transaction",
  "version": 1,
  "fields": [
    {"name": "bank_account_id", "type": "ref", "ktype": "finance.bank_account", "required": true},
    {"name": "value_date", "type": "date", "required": true},
    {"name": "description", "type": "text"},
    {"name": "amount", "type": "number", "required": true},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "required": true},
    {"name": "status", "type": "enum", "values": ["unreconciled", "matched", "ignored"], "default": "unreconciled"},
    {"name": "matched_entry_id", "type": "string"},
    {"name": "external_ref", "type": "string", "max_length": 128}
  ],
  "views": {
    "list": {"columns": ["value_date", "description", "amount", "currency", "status", "matched_entry_id"]},
    "form": {"sections": [
      {"title": "Transaction", "fields": ["bank_account_id", "value_date", "description", "amount", "currency", "external_ref"]},
      {"title": "Reconciliation", "fields": ["status", "matched_entry_id"]}
    ]}
  },
  "cards": {"summary": "{{value_date}} {{description}} — {{amount}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]}
}`)

// BankAccount is the typed row stored in `bank_accounts`. The KRecord
// mirror is the same shape so callers only need one model to reason
// about either.
type BankAccount struct {
	ID            uuid.UUID `json:"id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	Name          string    `json:"name"`
	AccountCode   string    `json:"account_code"`
	BankName      string    `json:"bank_name,omitempty"`
	AccountNumber string    `json:"account_number,omitempty"`
	IBAN          string    `json:"iban,omitempty"`
	Currency      string    `json:"currency"`
	Active        bool      `json:"active"`
}

// BankTransaction is a single imported statement line.
type BankTransaction struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	BankAccountID  uuid.UUID       `json:"bank_account_id"`
	ValueDate      time.Time       `json:"value_date"`
	Description    string          `json:"description"`
	Amount         decimal.Decimal `json:"amount"`
	Currency       string          `json:"currency"`
	Status         string          `json:"status"`
	MatchedEntryID *uuid.UUID      `json:"matched_entry_id,omitempty"`
	ExternalRef    string          `json:"external_ref,omitempty"`
}

// BankTransaction statuses.
const (
	BankTxnUnreconciled = "unreconciled"
	BankTxnMatched      = "matched"
	BankTxnIgnored      = "ignored"
)

// MatchWindow is the date window the default matcher uses to accept a
// journal-entry candidate. ERPNext uses ±7 days by default; we copy
// that with an override on the import call.
const DefaultMatchWindow = 7 * 24 * time.Hour

// BankKTypes returns the bank-reconciliation KTypes for registration.
// Kept separate from the generic finance catalog so callers can opt
// into the whole bank-rec surface or just the bookkeeping primitives.
func BankKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeBankAccount, Version: 1, Schema: bankAccountSchema},
		{Name: KTypeBankTransaction, Version: 1, Schema: bankTransactionSchema},
	}
}

// ErrNoMatch is returned by ReconcileTransaction when no journal
// entry is close enough to the statement line to accept.
var ErrNoMatch = errors.New("ledger: no journal-entry match within window")

// UpsertBankAccount inserts or updates a bank_accounts row. The typed
// table sits behind an RLS policy so tenant_id is both validated by
// the app and enforced by the DB.
func (s *PGStore) UpsertBankAccount(ctx context.Context, a BankAccount) (*BankAccount, error) {
	if a.TenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if a.Name == "" || a.AccountCode == "" || a.Currency == "" {
		return nil, errors.New("ledger: bank_account name/account_code/currency required")
	}
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	out := a
	err := dbutil.WithTenantTx(ctx, s.pool, a.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bank_accounts (id, tenant_id, name, account_code, bank_name, account_number, iban, currency, active)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET
			     name = EXCLUDED.name,
			     account_code = EXCLUDED.account_code,
			     bank_name = EXCLUDED.bank_name,
			     account_number = EXCLUDED.account_number,
			     iban = EXCLUDED.iban,
			     currency = EXCLUDED.currency,
			     active = EXCLUDED.active`,
			a.ID, a.TenantID, a.Name, a.AccountCode, nullIfEmpty(a.BankName),
			nullIfEmpty(a.AccountNumber), nullIfEmpty(a.IBAN), a.Currency, a.Active,
		)
		if err != nil {
			return fmt.Errorf("ledger: upsert bank_account: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ImportBankStatement inserts a batch of statement lines in a single
// tenant-scoped transaction. Callers typically decode a CSV into
// BankTransaction values and hand the slice in — the method enforces
// that every line shares the same bank account.
func (s *PGStore) ImportBankStatement(ctx context.Context, tenantID, bankAccountID uuid.UUID, lines []BankTransaction) ([]BankTransaction, error) {
	if tenantID == uuid.Nil || bankAccountID == uuid.Nil {
		return nil, errors.New("ledger: tenant_id and bank_account_id required")
	}
	if len(lines) == 0 {
		return nil, nil
	}
	out := make([]BankTransaction, 0, len(lines))
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, ln := range lines {
			if ln.ID == uuid.Nil {
				ln.ID = uuid.New()
			}
			ln.TenantID = tenantID
			ln.BankAccountID = bankAccountID
			if ln.Status == "" {
				ln.Status = BankTxnUnreconciled
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO bank_transactions
				     (id, tenant_id, bank_account_id, value_date, description,
				      amount, currency, status, matched_entry_id, external_ref)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				 ON CONFLICT (tenant_id, id) DO NOTHING`,
				ln.ID, ln.TenantID, ln.BankAccountID, ln.ValueDate,
				nullIfEmpty(ln.Description), ln.Amount, ln.Currency, ln.Status,
				ln.MatchedEntryID, nullIfEmpty(ln.ExternalRef),
			)
			if err != nil {
				return fmt.Errorf("ledger: insert bank_transaction: %w", err)
			}
			out = append(out, ln)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReconcileTransaction searches for a journal entry whose total
// debit/credit matches the statement-line amount (absolute value) and
// whose posted_at falls within `window` of the statement's value_date.
// On success the bank_transactions row is updated to `matched`
// referencing the chosen journal_entry.id. Callers get the matched
// entry back so they can render a confirmation card in KChat.
//
// The matcher is deliberately conservative: it only returns a match
// when exactly one candidate journal entry fits the window and amount.
// Multiple candidates (e.g. two $1,000 invoices posted the same day)
// return nil + a ErrNoMatch-like sentinel so a human has to break the
// tie.
func (s *PGStore) ReconcileTransaction(ctx context.Context, tenantID, txnID uuid.UUID, window time.Duration) (*JournalEntry, error) {
	if tenantID == uuid.Nil || txnID == uuid.Nil {
		return nil, errors.New("ledger: tenant_id and txn_id required")
	}
	if window <= 0 {
		window = DefaultMatchWindow
	}
	// The outer tx does the statement-line read, candidate search, and
	// UPDATE under one pool connection. Loading the matched journal
	// entry happens after commit so we don't nest a second
	// WithTenantTx inside the callback — doing so would grab a second
	// pool connection per reconcile and deadlock a batch run once the
	// pool saturates. Ledger rows are immutable after posting, so the
	// post-commit read is safe.
	var matchedID uuid.UUID
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			amount    decimal.Decimal
			valueDate time.Time
			currency  string
			status    string
		)
		err := tx.QueryRow(ctx,
			`SELECT amount, value_date, currency, status FROM bank_transactions
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, txnID,
		).Scan(&amount, &valueDate, &currency, &status)
		if err != nil {
			return fmt.Errorf("ledger: load bank_transaction: %w", err)
		}
		if status != BankTxnUnreconciled {
			return fmt.Errorf("ledger: bank_transaction already %s", status)
		}
		absAmount := amount.Abs()
		rows, err := tx.Query(ctx,
			`SELECT je.id
			   FROM journal_entries je
			   JOIN journal_lines jl ON jl.tenant_id = je.tenant_id AND jl.entry_id = je.id
			  WHERE je.tenant_id = $1
			    AND je.posted_at BETWEEN $2 AND $3
			    AND jl.currency = $4
			    AND (jl.debit = $5 OR jl.credit = $5)
			  GROUP BY je.id`,
			tenantID, valueDate.Add(-window), valueDate.Add(window), currency, absAmount,
		)
		if err != nil {
			return fmt.Errorf("ledger: scan candidate entries: %w", err)
		}
		defer rows.Close()
		var candidates []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("ledger: scan candidate: %w", err)
			}
			candidates = append(candidates, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(candidates) == 0 {
			return ErrNoMatch
		}
		if len(candidates) > 1 {
			// Ambiguous match; refuse to auto-reconcile. Surface the
			// situation via ErrNoMatch so the caller prompts a human.
			return fmt.Errorf("%w: %d candidates", ErrNoMatch, len(candidates))
		}
		entryID := candidates[0]
		if _, err := tx.Exec(ctx,
			`UPDATE bank_transactions
			    SET status = $3, matched_entry_id = $4
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, txnID, BankTxnMatched, entryID,
		); err != nil {
			return fmt.Errorf("ledger: update bank_transaction: %w", err)
		}
		matchedID = entryID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetJournalEntry(ctx, tenantID, matchedID)
}

// ParseBankStatementCSV is a light helper: it turns a comma-separated
// byte slice with headers [value_date, description, amount, currency,
// external_ref] into a slice of BankTransaction. Quoting is not
// supported — callers handle quoted CSVs with the stdlib encoding/csv
// and call InsertBankStatement directly. Kept here so the typical
// HTTP handler has a one-line way to ingest a simple statement.
func ParseBankStatementCSV(data []byte) ([]BankTransaction, error) {
	// Implementation is intentionally small — real-world adapters
	// should use encoding/csv with the account currency injected from
	// context. The helper is retained for unit tests that want a
	// fixture-driven round trip without reaching for the stdlib.
	var raw []struct {
		ValueDate   string `json:"value_date"`
		Description string `json:"description"`
		Amount      string `json:"amount"`
		Currency    string `json:"currency"`
		ExternalRef string `json:"external_ref"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse statement: %w", err)
	}
	out := make([]BankTransaction, 0, len(raw))
	for _, r := range raw {
		vd, err := time.Parse("2006-01-02", r.ValueDate)
		if err != nil {
			return nil, fmt.Errorf("value_date %q: %w", r.ValueDate, err)
		}
		amt, err := decimal.NewFromString(r.Amount)
		if err != nil {
			return nil, fmt.Errorf("amount %q: %w", r.Amount, err)
		}
		out = append(out, BankTransaction{
			ValueDate:   vd,
			Description: r.Description,
			Amount:      amt,
			Currency:    r.Currency,
			ExternalRef: r.ExternalRef,
		})
	}
	return out, nil
}
