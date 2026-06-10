package bankfeed

import (
	"encoding/json"
)

// nullIfEmpty maps an empty string to a SQL NULL so optional TEXT columns
// store NULL rather than ”. Mirrors the helper in internal/ledger.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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
