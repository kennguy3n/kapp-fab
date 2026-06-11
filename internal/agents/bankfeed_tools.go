package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/ledger/bankfeed"
)

// RegisterBankFeedTools wires the Session 15 bank-feed + smart-
// reconciliation agent tools onto an executor. Nil collaborators are
// tolerated so kernel / integration tests that never apply the bank-feed
// migration still register the tools — commit-mode calls then return a
// clear error rather than panicking, matching RegisterRecruitmentTools'
// contract.
//
// The five tools mirror the operator's headline reconciliation actions:
//   - finance.bank_feed_list_connections  — read active feeds
//   - finance.bank_feed_list_suggestions  — read the match queue
//   - finance.bank_feed_accept_suggestion — reconcile a line
//   - finance.bank_feed_reject_suggestion — dismiss a candidate
//   - finance.bank_feed_trigger_sync      — pull a feed on demand
//
// All mutating tools require confirmation and support dry-run preview per
// the ARCHITECTURE.md §11 agent-tool contract; the two list tools are
// read-only.
func RegisterBankFeedTools(x *Executor, conns *bankfeed.ConnectionStore, rules *bankfeed.RuleStore, registry *bankfeed.Registry, sync *bankfeed.SyncHandler, matcher *ledger.SmartMatcher) {
	x.Register(&bankFeedListConnectionsTool{conns: conns})
	x.Register(&bankFeedListSuggestionsTool{matcher: matcher})
	x.Register(&bankFeedAcceptSuggestionTool{matcher: matcher})
	x.Register(&bankFeedRejectSuggestionTool{matcher: matcher})
	x.Register(&bankFeedTriggerSyncTool{conns: conns, sync: sync})
	_ = rules
	_ = registry
}

// ----- finance.bank_feed_list_connections -----

type bankFeedListConnectionsInput struct {
	BankAccountID *uuid.UUID `json:"bank_account_id,omitempty"`
}

type bankFeedListConnectionsTool struct {
	conns *bankfeed.ConnectionStore
}

// Name is the agent-facing identifier for the list-connections tool.
func (t *bankFeedListConnectionsTool) Name() string { return "finance.bank_feed_list_connections" }

// RequiresConfirmation is false: listing connections is read-only.
func (t *bankFeedListConnectionsTool) RequiresConfirmation() bool { return false }

// Invoke lists the tenant's bank-feed connections (credential-free).
func (t *bankFeedListConnectionsTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in bankFeedListConnectionsInput
	// Inputs are optional for this read tool — an empty body lists every
	// active feed; a bank_account_id narrows to one account.
	if len(inv.Inputs) > 0 {
		if err := json.Unmarshal(inv.Inputs, &in); err != nil {
			return nil, fmt.Errorf("agents: tool %q: decode inputs: %w", inv.ToolName, err)
		}
	}
	if t.conns == nil {
		return nil, errors.New("finance.bank_feed_list_connections: connection store not configured")
	}
	var (
		conns []bankfeed.Connection
		err   error
	)
	if in.BankAccountID != nil {
		conns, err = t.conns.ListConnectionsByAccount(ctx, inv.TenantID, *in.BankAccountID)
	} else {
		conns, err = t.conns.ListActiveConnections(ctx, inv.TenantID)
	}
	if err != nil {
		return nil, err
	}
	// Project to a credential-free view so no token material is ever
	// returned through the agent surface.
	view := make([]map[string]any, 0, len(conns))
	for i := range conns {
		c := &conns[i]
		entry := map[string]any{
			"id":              c.ID,
			"bank_account_id": c.BankAccountID,
			"provider":        c.Provider,
			"status":          c.Status,
		}
		if c.LastSyncAt != nil {
			entry["last_sync_at"] = c.LastSyncAt.UTC()
		}
		if c.LastError != "" {
			entry["last_error"] = c.LastError
		}
		view = append(view, entry)
	}
	return &Result{
		Summary: fmt.Sprintf("%d bank-feed connection(s)", len(view)),
		Extra:   map[string]any{"connections": view},
	}, nil
}

// ----- finance.bank_feed_list_suggestions -----

type bankFeedListSuggestionsInput struct {
	BankAccountID uuid.UUID `json:"bank_account_id"`
}

type bankFeedListSuggestionsTool struct {
	matcher *ledger.SmartMatcher
}

// Name is the agent-facing identifier for the list-suggestions tool.
func (t *bankFeedListSuggestionsTool) Name() string { return "finance.bank_feed_list_suggestions" }

// RequiresConfirmation is false: listing suggestions is read-only.
func (t *bankFeedListSuggestionsTool) RequiresConfirmation() bool { return false }

// Invoke lists the open match suggestions for one bank account.
func (t *bankFeedListSuggestionsTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in bankFeedListSuggestionsInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.BankAccountID == uuid.Nil {
		return nil, errors.New("finance.bank_feed_list_suggestions: bank_account_id required")
	}
	if t.matcher == nil {
		return nil, errors.New("finance.bank_feed_list_suggestions: matcher not configured")
	}
	sugs, err := t.matcher.ListSuggestions(ctx, inv.TenantID, in.BankAccountID)
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("%d open match suggestion(s) for account %s", len(sugs), in.BankAccountID),
		Extra:   map[string]any{"suggestions": sugs},
	}, nil
}

// ----- finance.bank_feed_accept_suggestion -----

type bankFeedSuggestionInput struct {
	SuggestionID uuid.UUID `json:"suggestion_id"`
}

type bankFeedAcceptSuggestionTool struct {
	matcher *ledger.SmartMatcher
}

// Name is the agent-facing identifier for the accept-suggestion tool.
func (t *bankFeedAcceptSuggestionTool) Name() string { return "finance.bank_feed_accept_suggestion" }

// RequiresConfirmation is true: accepting reconciles a journal entry.
func (t *bankFeedAcceptSuggestionTool) RequiresConfirmation() bool { return true }

// Invoke previews (dry-run) or commits acceptance of a match suggestion.
func (t *bankFeedAcceptSuggestionTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in bankFeedSuggestionInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.SuggestionID == uuid.Nil {
		return nil, errors.New("finance.bank_feed_accept_suggestion: suggestion_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would accept match suggestion %s", in.SuggestionID),
			Preview: preview,
		}, nil
	}
	if t.matcher == nil {
		return nil, errors.New("finance.bank_feed_accept_suggestion: matcher not configured")
	}
	out, err := t.matcher.AcceptSuggestion(ctx, inv.TenantID, in.SuggestionID, inv.ActorID)
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Accepted suggestion %s (reconciled transaction %s)", out.ID, out.TransactionID),
		Extra: map[string]any{
			"suggestion_id":    out.ID,
			"transaction_id":   out.TransactionID,
			"journal_entry_id": out.JournalEntryID,
			"status":           out.Status,
		},
	}, nil
}

// ----- finance.bank_feed_reject_suggestion -----

type bankFeedRejectSuggestionTool struct {
	matcher *ledger.SmartMatcher
}

// Name is the agent-facing identifier for the reject-suggestion tool.
func (t *bankFeedRejectSuggestionTool) Name() string { return "finance.bank_feed_reject_suggestion" }

// RequiresConfirmation is true: rejecting mutates suggestion state.
func (t *bankFeedRejectSuggestionTool) RequiresConfirmation() bool { return true }

// Invoke previews (dry-run) or commits rejection of a match suggestion.
func (t *bankFeedRejectSuggestionTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in bankFeedSuggestionInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.SuggestionID == uuid.Nil {
		return nil, errors.New("finance.bank_feed_reject_suggestion: suggestion_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would reject match suggestion %s", in.SuggestionID),
			Preview: preview,
		}, nil
	}
	if t.matcher == nil {
		return nil, errors.New("finance.bank_feed_reject_suggestion: matcher not configured")
	}
	if err := t.matcher.RejectSuggestion(ctx, inv.TenantID, in.SuggestionID, inv.ActorID); err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Rejected suggestion %s", in.SuggestionID),
		Extra:   map[string]any{"suggestion_id": in.SuggestionID, "status": ledger.SuggestionRejected},
	}, nil
}

// ----- finance.bank_feed_trigger_sync -----

type bankFeedTriggerSyncInput struct {
	ConnectionID uuid.UUID `json:"connection_id"`
}

type bankFeedTriggerSyncTool struct {
	conns *bankfeed.ConnectionStore
	sync  *bankfeed.SyncHandler
}

// Name is the agent-facing identifier for the trigger-sync tool.
func (t *bankFeedTriggerSyncTool) Name() string { return "finance.bank_feed_trigger_sync" }

// RequiresConfirmation is true: a sync pulls and posts new ledger lines.
func (t *bankFeedTriggerSyncTool) RequiresConfirmation() bool { return true }

// Invoke previews (dry-run) or commits an on-demand sync of one connection.
func (t *bankFeedTriggerSyncTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in bankFeedTriggerSyncInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.ConnectionID == uuid.Nil {
		return nil, errors.New("finance.bank_feed_trigger_sync: connection_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would sync bank-feed connection %s now", in.ConnectionID),
			Preview: preview,
		}, nil
	}
	if t.conns == nil || t.sync == nil {
		return nil, errors.New("finance.bank_feed_trigger_sync: bank-feed sync not configured")
	}
	conn, err := t.conns.GetConnection(ctx, inv.TenantID, in.ConnectionID)
	if err != nil {
		return nil, err
	}
	res, err := t.sync.SyncOne(ctx, inv.TenantID, conn)
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Synced connection %s: %d fetched, %d new, %d suggested, %d auto-matched",
			in.ConnectionID, res.Fetched, res.Inserted, res.Suggested, res.AutoMatched),
		Extra: map[string]any{
			"connection_id": in.ConnectionID,
			"fetched":       res.Fetched,
			"inserted":      res.Inserted,
			"suggested":     res.Suggested,
			"auto_matched":  res.AutoMatched,
			"updated":       res.Updated,
			"voided":        res.Voided,
			"skipped":       res.Skipped,
		},
	}, nil
}
