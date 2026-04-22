// Package ledger implements the Phase C finance engine: chart of accounts,
// double-entry journal posting, period lockout, invoice/bill posting,
// credit notes, tax codes, and basic financial reports (trial balance,
// aging, income statement).
//
// Every tenant-scoped operation runs inside dbutil.WithTenantTx so
// `SET LOCAL app.tenant_id` is established before any SQL touches the
// RLS-protected accounts / journal_entries / journal_lines /
// fiscal_periods / tax_codes tables. The same atomicity guarantees that
// internal/record/store.go relies on apply: the typed INSERT, the
// event-outbox emission (via events.PGPublisher.EmitTx), and the audit
// entry (via audit.PGLogger.LogTx) all share the caller's transaction,
// so a failure anywhere rolls the whole posting back. See
// ARCHITECTURE.md §8 rule 7 and internal/record/store.go for the
// canonical pattern the finance engine mirrors.
//
// Financial records are never deleted. Corrections are applied as
// reversal journal entries (see credit_note.go); callers that try to
// mutate a posted entry go through the workflow engine (status =
// draft → posted → reversed) rather than UPDATE.
package ledger
