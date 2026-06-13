package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/ledger/bankfeed"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestWriteBankFeedErrorMappings pins the sentinel→HTTP-status contract
// for the bank-feed surface so a refactor of writeBankFeedError fails
// fast without the postgres-backed integration suite. The provider
// layer wraps its sentinels with fmt.Errorf("%w: …"), so the switch must
// keep matching wrapped chains via errors.Is.
func TestWriteBankFeedErrorMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		// leakDetail, when non-empty, must NOT appear in the response body
		// — it is internal error detail that should stay server-side.
		leakDetail string
	}{
		{"unknown provider → 404", bankfeed.ErrUnknownProvider, http.StatusNotFound, ""},
		{"wrapped unknown provider → 404", fmt.Errorf("connect: %w", bankfeed.ErrUnknownProvider), http.StatusNotFound, ""},
		{"provider not configured → 503", bankfeed.ErrProviderNotConfigured, http.StatusServiceUnavailable, ""},
		{"wrapped not configured → 503", fmt.Errorf("plaid: %w", bankfeed.ErrProviderNotConfigured), http.StatusServiceUnavailable, ""},
		{"unsupported → 422", bankfeed.ErrUnsupported, http.StatusUnprocessableEntity, ""},
		{"not found → 404", bankfeed.ErrNotFound, http.StatusNotFound, ""},
		{"wrapped connection not found → 404", fmt.Errorf("bankfeed: connection abc: %w", bankfeed.ErrNotFound), http.StatusNotFound, ""},
		{"suggestion not found → 404", ledger.ErrSuggestionNotFound, http.StatusNotFound, ""},
		{"wrapped suggestion not found → 404", fmt.Errorf("ledger: suggestion x: %w", ledger.ErrSuggestionNotFound), http.StatusNotFound, ""},
		{"suggestion conflict → 409", ledger.ErrSuggestionConflict, http.StatusConflict, ""},
		{"wrapped suggestion conflict → 409", fmt.Errorf("ledger: suggestion already accepted: %w", ledger.ErrSuggestionConflict), http.StatusConflict, ""},
		{"split invalid → 422", ledger.ErrSplitInvalid, http.StatusUnprocessableEntity, ""},
		{"wrapped split invalid → 422", fmt.Errorf("ledger: split sums to -90, expected -100: %w", ledger.ErrSplitInvalid), http.StatusUnprocessableEntity, ""},
		{"unrelated → 500 (detail not leaked)", errors.New("pgx: password=hunter2 host=10.0.0.1 pool exhausted"), http.StatusInternalServerError, "hunter2"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/finance/bank-feeds/connections", http.NoBody)
			writeBankFeedError(rr, req, tc.err)
			if rr.Code != tc.status {
				t.Fatalf("status: want %d, got %d (body=%q)", tc.status, rr.Code, rr.Body.String())
			}
			if tc.leakDetail != "" && strings.Contains(rr.Body.String(), tc.leakDetail) {
				t.Fatalf("internal error detail %q leaked to client body %q", tc.leakDetail, rr.Body.String())
			}
		})
	}
}

// TestConnectionResponseOmitsCredentials is the security regression
// guard for the most sensitive field on the surface: a Connection
// carries the field-encrypted AccessToken / RefreshToken, and those must
// never reach the wire. The DTO has no token fields at all, so marshalling
// a fully-populated connection must not surface either secret.
func TestConnectionResponseOmitsCredentials(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	last := now.Add(-time.Hour)
	c := bankfeed.Connection{
		ID:            uuid.New(),
		TenantID:      uuid.New(),
		BankAccountID: uuid.New(),
		Provider:      bankfeed.ProviderCSV,
		AccessToken:   "super-secret-access-token",
		RefreshToken:  "super-secret-refresh-token",
		Cursor:        "cursor-123",
		ExternalID:    "ext-1",
		Status:        bankfeed.StatusActive,
		LastSyncAt:    &last,
		LastError:     "",
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	blob, err := json.Marshal(toConnectionResponse(&c))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(blob)

	for _, secret := range []string{
		"super-secret-access-token",
		"super-secret-refresh-token",
		"access_token", "accessToken", "AccessToken",
		"refresh_token", "refreshToken", "RefreshToken",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("connection DTO leaked credential token %q: %s", secret, out)
		}
	}

	// Sanity: the non-secret operator fields are present and snake_cased.
	for _, want := range []string{`"bank_account_id"`, `"last_sync_at"`, `"provider":"csv"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("connection DTO missing %q: %s", want, out)
		}
	}
}

// TestSuggestionResponseProjectsExactContract pins the wire shape of the
// match-suggestion surface to a dedicated DTO so a field added to the
// domain ledger.Suggestion later (e.g. an internal scoring detail) cannot
// silently leak to a tenant. The marshalled object must contain exactly
// the published key set — no more, no fewer.
func TestSuggestionResponseProjectsExactContract(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := ledger.Suggestion{
		ID:             uuid.New(),
		TenantID:       uuid.New(),
		TransactionID:  uuid.New(),
		JournalEntryID: uuid.New(),
		Confidence:     0.92,
		MatchReason:    "amount+date",
		Status:         "pending",
		CreatedAt:      now,
	}
	blob, err := json.Marshal(toSuggestionResponse(&s))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]struct{}{
		"id": {}, "tenant_id": {}, "transaction_id": {}, "journal_entry_id": {},
		"confidence": {}, "match_reason": {}, "status": {}, "created_at": {},
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("suggestion DTO exposed unexpected key %q (possible leak): %s", k, blob)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("suggestion DTO missing expected key %q: %s", k, blob)
		}
	}
}

// TestSyncResultResponseOmitsCursor verifies the sync DTO exposes the
// operator-facing counts under snake_case keys and drops the provider's
// internal pagination cursor (an implementation detail the client must
// not depend on).
func TestSyncResultResponseOmitsCursor(t *testing.T) {
	t.Parallel()
	res := &bankfeed.SyncResult{
		Fetched:     10,
		Skipped:     1,
		Inserted:    7,
		Updated:     2,
		Voided:      1,
		Unwound:     1,
		Suggested:   3,
		AutoMatched: 2,
		Transfers:   4,
		Duplicates:  5,
		Cursor:      "provider-internal-cursor",
	}
	blob, err := json.Marshal(toSyncResultResponse(res))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(blob)
	if strings.Contains(out, "provider-internal-cursor") || strings.Contains(strings.ToLower(out), "cursor") {
		t.Fatalf("sync DTO leaked cursor: %s", out)
	}
	for _, want := range []string{`"fetched":10`, `"inserted":7`, `"auto_matched":2`, `"suggested":3`, `"transfers":4`, `"duplicates":5`} {
		if !strings.Contains(out, want) {
			t.Fatalf("sync DTO missing %q: %s", want, out)
		}
	}
}

// TestRuleResponseShape checks the rule DTO emits snake_case keys and
// preserves the optional account scope (a nil BankAccountID is omitted,
// a set one is surfaced).
func TestRuleResponseShape(t *testing.T) {
	t.Parallel()
	acct := uuid.New()
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	scoped := bankfeed.Rule{
		ID:                uuid.New(),
		Priority:          50,
		ConditionType:     "description_contains",
		ConditionValue:    "STRIPE",
		TargetAccountCode: "4000",
		AutoApprove:       true,
		BankAccountID:     &acct,
		Enabled:           true,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	blob, err := json.Marshal(toRuleResponse(&scoped))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(blob)
	for _, want := range []string{
		`"condition_type":"description_contains"`,
		`"condition_value":"STRIPE"`,
		`"target_account_code":"4000"`,
		`"auto_approve":true`,
		`"enabled":true`,
		`"bank_account_id":"` + acct.String() + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rule DTO missing %q: %s", want, out)
		}
	}

	// A tenant-wide rule (nil account) omits the bank_account_id key.
	global := scoped
	global.BankAccountID = nil
	blob, err = json.Marshal(toRuleResponse(&global))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "bank_account_id") {
		t.Fatalf("global rule should omit bank_account_id: %s", string(blob))
	}
}

// TestPreviewRuleDefaultsEmptyAmountToZero guards the preview UX fix: an
// operator drafting a purely textual rule (payee/reference/description)
// supplies no amount, and the handler must treat an omitted sample.amount
// as zero rather than 400-ing on "must be a decimal string". A non-text
// rule path is unaffected because the rule here keys on description only.
func TestPreviewRuleDefaultsEmptyAmountToZero(t *testing.T) {
	t.Parallel()
	h := &bankfeedHandlers{}

	// Supplied rules path → no DB access. Empty Amount must default to 0.
	body := `{"sample":{"description":"ACME PAYROLL"},"rules":[` +
		`{"priority":1,"condition_type":"description_contains",` +
		`"condition_value":"payroll","target_account_code":"6000"}]}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/finance/bank-feeds/rules/preview", strings.NewReader(body))
	ctx := platform.WithTenant(req.Context(), &tenant.Tenant{ID: uuid.New()})
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	h.previewRule(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("empty amount preview: want 200, got %d (body=%q)", rr.Code, rr.Body.String())
	}
	var resp previewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if !resp.Matched {
		t.Fatalf("text rule should fire on zero-amount sample: %+v", resp)
	}
	if resp.TargetAccountCode != "6000" {
		t.Fatalf("TargetAccountCode = %q; want 6000", resp.TargetAccountCode)
	}
}

// TestPreviewRuleRejectsNonDecimalAmount keeps the explicit-bad-value path:
// a present but non-numeric amount is still a client 400, so the zero
// default does not mask a genuine typo.
func TestPreviewRuleRejectsNonDecimalAmount(t *testing.T) {
	t.Parallel()
	h := &bankfeedHandlers{}
	body := `{"sample":{"description":"x","amount":"not-a-number"},"rules":[` +
		`{"priority":1,"condition_type":"description_contains","condition_value":"x"}]}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/finance/bank-feeds/rules/preview", strings.NewReader(body))
	ctx := platform.WithTenant(req.Context(), &tenant.Tenant{ID: uuid.New()})
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	h.previewRule(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("non-decimal amount: want 400, got %d (body=%q)", rr.Code, rr.Body.String())
	}
}

// TestUploadCSVRejectsOversizeBody proves the statement-upload guard
// returns 413 for a body past maxCSVUploadBytes instead of silently
// ingesting a truncated prefix. The size check runs before any store or
// provider access, so an oversize body is rejected even on a handler
// with no wired dependencies — which is exactly what this test exercises.
func TestUploadCSVRejectsOversizeBody(t *testing.T) {
	t.Parallel()
	h := &bankfeedHandlers{}
	acct := uuid.New()

	// One byte past the cap is enough to trip the guard; the handler must
	// not read (or buffer) more than maxCSVUploadBytes+1.
	body := strings.Repeat("a", maxCSVUploadBytes+1)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/finance/bank-feeds/accounts/"+acct.String()+"/upload",
		strings.NewReader(body),
	)
	ctx := platform.WithTenant(req.Context(), &tenant.Tenant{ID: uuid.New()})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("bank_account_id", acct.String())
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	h.uploadCSV(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload: want 413, got %d (body=%q)", rr.Code, rr.Body.String())
	}
}

// TestBankTransactionResponseShape pins the split endpoint's response DTO:
// snake_case keys, amount as an exact decimal string, and matched_entry_id
// omitted for a split (it is NULL — the allocations table is the source of
// truth) but surfaced for a 1:1 match.
func TestBankTransactionResponseShape(t *testing.T) {
	t.Parallel()
	split := ledger.BankTransaction{
		ID:            uuid.New(),
		TenantID:      uuid.New(),
		BankAccountID: uuid.New(),
		Amount:        decimal.RequireFromString("-100.00"),
		Currency:      "USD",
		Status:        ledger.BankTxnMatched,
	}
	blob, err := json.Marshal(toBankTransactionResponse(&split))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(blob)
	for _, want := range []string{`"amount":"-100"`, `"currency":"USD"`, `"status":"matched"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("txn DTO missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "matched_entry_id") {
		t.Fatalf("split DTO should omit matched_entry_id (NULL): %s", out)
	}
	// A 1:1 match surfaces matched_entry_id.
	entry := uuid.New()
	one := split
	one.MatchedEntryID = &entry
	blob, _ = json.Marshal(toBankTransactionResponse(&one))
	if !strings.Contains(string(blob), `"matched_entry_id":"`+entry.String()+`"`) {
		t.Fatalf("1:1 DTO should surface matched_entry_id: %s", string(blob))
	}
}

// TestAcceptSplitRejectsMalformedRequest covers the handler's client-input
// validation, all of which runs before any matcher/DB access (so a handler
// with no wired matcher still exercises every branch). A well-formed but
// semantically unbalanced split is the matcher's job (422, integration-
// tested); here we pin the 400s for malformed wire input.
func TestAcceptSplitRejectsMalformedRequest(t *testing.T) {
	t.Parallel()
	txn := uuid.New()
	entry := uuid.New()
	cases := []struct {
		name string
		body string
	}{
		{"empty allocations", `{"allocations":[]}`},
		{"single allocation", `{"allocations":[{"journal_entry_id":"` + entry.String() + `","amount":"-100.00"}]}`},
		{"missing allocations", `{}`},
		{"invalid json", `{`},
		{"bad entry uuid", `{"allocations":[{"journal_entry_id":"not-a-uuid","amount":"-100.00"}]}`},
		{"bad amount", `{"allocations":[{"journal_entry_id":"` + entry.String() + `","amount":"NaN"}]}`},
		{"bad suggestion uuid", `{"allocations":[{"journal_entry_id":"` + entry.String() + `","amount":"-100.00","suggestion_id":"nope"}]}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &bankfeedHandlers{}
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/finance/bank-feeds/bank-transactions/"+txn.String()+"/split",
				strings.NewReader(tc.body))
			ctx := platform.WithTenant(req.Context(), &tenant.Tenant{ID: uuid.New()})
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("transaction_id", txn.String())
			ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
			req = req.WithContext(ctx)

			rr := httptest.NewRecorder()
			h.acceptSplit(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: want 400, got %d (body=%q)", tc.name, rr.Code, rr.Body.String())
			}
		})
	}
}
