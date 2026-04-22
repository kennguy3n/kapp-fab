package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ReverseJournalEntry posts a contra-entry that reverses every leg of
// the supplied entry (debits become credits and vice versa). The
// reversal links back to the original via SourceKType =
// "finance.journal_entry" and SourceID = <original id>.
//
// Financial records are never deleted (ARCHITECTURE.md §C-1 design
// decision 2); corrections ALWAYS post a new reversal entry so the
// audit trail stays linear and immutable.
func (s *PGStore) ReverseJournalEntry(ctx context.Context, tenantID, entryID, actorID uuid.UUID, memo string) (*JournalEntry, error) {
	if tenantID == uuid.Nil || entryID == uuid.Nil {
		return nil, errors.New("ledger: tenant id and entry id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("ledger: actor id required")
	}
	original, err := s.GetJournalEntry(ctx, tenantID, entryID)
	if err != nil {
		return nil, err
	}
	if len(original.Lines) == 0 {
		return nil, ErrEmptyEntry
	}

	reversal := make([]JournalLine, 0, len(original.Lines))
	for _, line := range original.Lines {
		reversal = append(reversal, JournalLine{
			AccountCode: line.AccountCode,
			Debit:       line.Credit,
			Credit:      line.Debit,
			Currency:    line.Currency,
			Memo:        fmt.Sprintf("Reversal of %s", shortID(entryID)),
		})
	}
	if memo == "" {
		memo = fmt.Sprintf("Reversal of journal entry %s", shortID(entryID))
	}

	sourceID := entryID
	return s.PostJournalEntry(ctx, JournalEntry{
		TenantID:    tenantID,
		PostedAt:    s.now(),
		Memo:        memo,
		SourceKType: "finance.journal_entry",
		SourceID:    &sourceID,
		CreatedBy:   actorID,
		Lines:       reversal,
	})
}

func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}
