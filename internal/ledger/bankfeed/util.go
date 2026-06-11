package bankfeed

import (
	"encoding/json"
	"unicode/utf8"
)

// nullIfEmpty maps an empty string to a SQL NULL so optional TEXT columns
// store NULL rather than ”. Mirrors the helper in internal/ledger.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// truncateRunes bounds s to at most maxLen bytes, backing up to the nearest
// rune boundary so a multi-byte UTF-8 sequence is never split. Splitting
// mid-codepoint yields invalid UTF-8, which Postgres rejects on insert into
// a TEXT column (e.g. bank_feed_connections.last_error) and which mangles
// non-ASCII EU/UK provider messages in logs. An ellipsis marks truncation.
func truncateRunes(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// auditContext builds the credential-free JSON context attached to a
// connection audit entry. Tokens are deliberately excluded — only the
// provider, account, status and (non-secret) external id are recorded.
func auditContext(c Connection) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"provider":        c.Provider,
		"bank_account_id": c.BankAccountID,
		"status":          c.Status,
		"external_id":     c.ExternalID,
	})
	return b
}
