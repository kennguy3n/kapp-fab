package agents

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestRegisterBankFeedToolsNilCollaborators(t *testing.T) {
	t.Parallel()
	x := NewExecutor(nil, nil, nil, nil)
	// Nil collaborators must not panic at registration time — kernel /
	// integration tests that never apply the bank-feed migration still
	// register the tools; commit-mode calls then error cleanly.
	RegisterBankFeedTools(x, nil, nil, nil, nil, nil)
	for _, name := range []string{
		"finance.bank_feed_list_connections",
		"finance.bank_feed_list_suggestions",
		"finance.bank_feed_accept_suggestion",
		"finance.bank_feed_reject_suggestion",
		"finance.bank_feed_trigger_sync",
	} {
		if _, ok := x.handlers[name]; !ok {
			t.Fatalf("tool %q not registered", name)
		}
	}
}

func TestBankFeedToolConfirmationContract(t *testing.T) {
	t.Parallel()
	// The two read tools are non-confirming; the three mutators require
	// confirmation per the agent-tool contract.
	if (&bankFeedListConnectionsTool{}).RequiresConfirmation() {
		t.Fatal("list_connections must not require confirmation")
	}
	if (&bankFeedListSuggestionsTool{}).RequiresConfirmation() {
		t.Fatal("list_suggestions must not require confirmation")
	}
	for _, tool := range []Handler{
		&bankFeedAcceptSuggestionTool{},
		&bankFeedRejectSuggestionTool{},
		&bankFeedTriggerSyncTool{},
	} {
		if !tool.RequiresConfirmation() {
			t.Fatalf("%s must require confirmation", tool.Name())
		}
	}
}

func TestBankFeedListConnectionsToolNilStore(t *testing.T) {
	t.Parallel()
	tool := &bankFeedListConnectionsTool{}
	if tool.Name() != "finance.bank_feed_list_connections" {
		t.Fatalf("Name() = %q", tool.Name())
	}
	// No bank_account_id (optional input) with a nil store must error
	// rather than panic.
	_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, bankFeedListConnectionsInput{}))
	if err == nil {
		t.Fatal("nil store must error")
	}
}

func TestBankFeedListSuggestionsToolValidation(t *testing.T) {
	t.Parallel()
	tool := &bankFeedListSuggestionsTool{}

	t.Run("missing bank_account_id errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, bankFeedListSuggestionsInput{}))
		if err == nil {
			t.Fatal("expected error for missing bank_account_id")
		}
	})

	t.Run("nil matcher errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, bankFeedListSuggestionsInput{BankAccountID: uuid.New()}))
		if err == nil {
			t.Fatal("nil matcher must error")
		}
	})
}

func TestBankFeedAcceptRejectTools(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tool Handler
	}{
		{"accept", &bankFeedAcceptSuggestionTool{}},
		{"reject", &bankFeedRejectSuggestionTool{}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Missing suggestion_id errors even in dry-run.
			if _, err := tc.tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, bankFeedSuggestionInput{})); err == nil {
				t.Fatal("expected error for missing suggestion_id")
			}

			// Dry-run returns a preview without touching the matcher.
			res, err := tc.tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, bankFeedSuggestionInput{SuggestionID: uuid.New()}))
			if err != nil {
				t.Fatalf("dry-run: %v", err)
			}
			if len(res.Preview) == 0 {
				t.Fatal("dry-run must populate Preview")
			}

			// Commit with a nil matcher errors rather than panicking.
			if _, err := tc.tool.Invoke(context.Background(), mkInvocation(t, ModeCommit, bankFeedSuggestionInput{SuggestionID: uuid.New()})); err == nil {
				t.Fatal("commit with nil matcher must error")
			}
		})
	}
}

func TestBankFeedTriggerSyncTool(t *testing.T) {
	t.Parallel()
	tool := &bankFeedTriggerSyncTool{}

	t.Run("missing connection_id errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, bankFeedTriggerSyncInput{}))
		if err == nil {
			t.Fatal("expected error for missing connection_id")
		}
	})

	t.Run("dry-run returns preview", func(t *testing.T) {
		t.Parallel()
		res, err := tool.Invoke(context.Background(), mkInvocation(t, ModeDryRun, bankFeedTriggerSyncInput{ConnectionID: uuid.New()}))
		if err != nil {
			t.Fatalf("dry-run: %v", err)
		}
		if len(res.Preview) == 0 {
			t.Fatal("dry-run must populate Preview")
		}
	})

	t.Run("commit without sync errors", func(t *testing.T) {
		t.Parallel()
		_, err := tool.Invoke(context.Background(), mkInvocation(t, ModeCommit, bankFeedTriggerSyncInput{ConnectionID: uuid.New()}))
		if err == nil {
			t.Fatal("commit with nil sync must error")
		}
	})
}
