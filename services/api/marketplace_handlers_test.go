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

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundle"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestMarketplaceWriteErrorMapping pins the sentinel-error →
// HTTP-status table the handlers contract on. Every documented
// sentinel from the comment block at the top of
// marketplaceHandlers must produce the matching status; any
// future refactor of writeError that drops an arm fails this
// test rather than silently collapsing to 500.
//
// A fresh marketplaceHandlers value is enough to drive
// writeError — it dispatches purely on the error value and does
// not touch the underlying store / engine / resolver. The
// recorder collects the status code; the body content is
// asserted only to confirm the sentinel's message survives the
// http.Error wrap (so a caller can grep their logs for the
// underlying cause).
func TestMarketplaceWriteErrorMapping(t *testing.T) {
	t.Parallel()
	h := &marketplaceHandlers{}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "ErrNotFound", err: marketplace.ErrNotFound, want: http.StatusNotFound},
		{name: "wrapped ErrNotFound", err: fmt.Errorf("%w: extension foo", marketplace.ErrNotFound), want: http.StatusNotFound},
		{name: "ErrConflict", err: marketplace.ErrConflict, want: http.StatusConflict},
		{name: "ErrYanked", err: marketplace.ErrYanked, want: http.StatusConflict},
		{name: "ErrImmutableVersion", err: marketplace.ErrImmutableVersion, want: http.StatusConflict},
		{name: "ErrInvalidManifest", err: marketplace.ErrInvalidManifest, want: http.StatusBadRequest},
		{name: "ErrPermissionScopeUnknown", err: marketplace.ErrPermissionScopeUnknown, want: http.StatusBadRequest},
		{name: "ErrBundleTransportInsecure", err: bundle.ErrBundleTransportInsecure, want: http.StatusBadRequest},
		{name: "ErrBundleTooLarge", err: marketplace.ErrBundleTooLarge, want: http.StatusRequestEntityTooLarge},
		{name: "ErrBundleExceedsLimit", err: bundle.ErrBundleExceedsLimit, want: http.StatusRequestEntityTooLarge},
		{name: "ErrBundleMalformed", err: bundle.ErrBundleMalformed, want: http.StatusUnprocessableEntity},
		{name: "ErrBundleNotFound", err: bundle.ErrBundleNotFound, want: http.StatusBadGateway},
		{name: "ErrBundleFetchFailed", err: bundle.ErrBundleFetchFailed, want: http.StatusBadGateway},
		{name: "ErrBundleHashMismatch", err: marketplace.ErrBundleHashMismatch, want: http.StatusBadGateway},
		{name: "ErrPreInstallRejected", err: runtime.ErrPreInstallRejected, want: http.StatusUnprocessableEntity},
		{name: "ErrPreUninstallRejected", err: runtime.ErrPreUninstallRejected, want: http.StatusUnprocessableEntity},
		{name: "wrapped ErrPreUninstallRejected", err: fmt.Errorf("%w: publisher said no", runtime.ErrPreUninstallRejected), want: http.StatusUnprocessableEntity},
		// B6.1 — Engine.Upgrade sentinels. pre_upgrade
		// rejection mirrors the install/uninstall lifecycle
		// shape (422). Version-mismatch is a precondition
		// failure (409) — the caller observed an old
		// extension_version_id; the engine's in-tx FOR UPDATE
		// found the row had moved. Same-version is a 400
		// because it's a malformed request (from == to is a
		// programmer error, not a state transition).
		{name: "ErrPreUpgradeRejected", err: runtime.ErrPreUpgradeRejected, want: http.StatusUnprocessableEntity},
		{name: "wrapped ErrPreUpgradeRejected", err: fmt.Errorf("%w: extension refused upgrade", runtime.ErrPreUpgradeRejected), want: http.StatusUnprocessableEntity},
		{name: "ErrVersionMismatch", err: runtime.ErrVersionMismatch, want: http.StatusConflict},
		{name: "wrapped ErrVersionMismatch", err: fmt.Errorf("%w: install moved", runtime.ErrVersionMismatch), want: http.StatusConflict},
		{name: "ErrSameVersionUpgrade", err: runtime.ErrSameVersionUpgrade, want: http.StatusBadRequest},
		{name: "wrapped ErrSameVersionUpgrade", err: fmt.Errorf("%w: from==to", runtime.ErrSameVersionUpgrade), want: http.StatusBadRequest},
		// Forward-safety mappings for sentinels declared in
		// types.go that don't yet have a live HTTP raise path
		// in B7 but will once SetAutoApprovePatch / signature
		// verification land in admin handlers. The bot would
		// otherwise fall through to 500 (Devin Review
		// ANALYSIS_0001 on fa364ea); pinning the mapping here
		// keeps the contract intact for future callers.
		{name: "ErrInvalidSignature", err: marketplace.ErrInvalidSignature, want: http.StatusUnprocessableEntity},
		{name: "wrapped ErrInvalidSignature", err: fmt.Errorf("%w: key foo did not verify", marketplace.ErrInvalidSignature), want: http.StatusUnprocessableEntity},
		{name: "ErrPublisherNotVerified", err: marketplace.ErrPublisherNotVerified, want: http.StatusConflict},
		{name: "wrapped ErrPublisherNotVerified", err: fmt.Errorf("%w: cannot enable fast-path", marketplace.ErrPublisherNotVerified), want: http.StatusConflict},
		{name: "unknown error → 500", err: errors.New("boom"), want: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.writeError(rec, tc.err)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			body := rec.Body.String()
			if rec.Code == http.StatusInternalServerError {
				// 500s deliberately do NOT echo the underlying
				// error string — that path was leaking SQL / pgx
				// internals to unauthenticated callers. Assert
				// the generic message AND that the sentinel
				// substring did NOT leak. Devin Review
				// BUG_pr-review-job-...-0002.
				if !strings.Contains(body, "internal server error") {
					t.Fatalf("500 body %q missing generic message", body)
				}
				if strings.Contains(body, tc.err.Error()) && tc.err.Error() != "internal server error" {
					t.Fatalf("500 body %q leaked underlying error %q", body, tc.err.Error())
				}
				return
			}
			if !strings.Contains(body, tc.err.Error()) {
				t.Fatalf("body %q missing sentinel message %q", body, tc.err.Error())
			}
		})
	}
}

// TestNewMarketplaceHandlersNilDeps ensures the constructor
// returns nil when any required dependency is missing. The
// route registrar checks `d.mph != nil` before mounting routes;
// a constructor that returned a half-built struct would skip
// the guard and panic later when a handler dereferenced a nil
// store or engine.
func TestNewMarketplaceHandlersNilDeps(t *testing.T) {
	t.Parallel()
	if h := newMarketplaceHandlers(nil, nil, nil); h != nil {
		t.Fatalf("nil store/engine/resolver: handler = %+v, want nil", h)
	}
	store := &marketplace.Store{}
	engine := &runtime.Engine{}
	resolver := bundle.NewInMemoryResolver()
	if h := newMarketplaceHandlers(nil, engine, resolver); h != nil {
		t.Fatalf("nil store: handler = %+v, want nil", h)
	}
	if h := newMarketplaceHandlers(store, nil, resolver); h != nil {
		t.Fatalf("nil engine: handler = %+v, want nil", h)
	}
	if h := newMarketplaceHandlers(store, engine, nil); h != nil {
		t.Fatalf("nil resolver: handler = %+v, want nil", h)
	}
	got := newMarketplaceHandlers(store, engine, resolver)
	if got == nil {
		t.Fatalf("all-non-nil deps: handler = nil, want non-nil")
	}
}

// TestInstallationToViewEmptySettings exercises the JSON
// settings normalisation path. The DB column defaults to '{}'
// when InstallInput.Settings is empty (see store.Install), so
// the view should always present a non-nil settings map for
// JSON consumers — never `null`.
func TestInstallationToViewEmptySettings(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	in := &marketplace.Installation{
		ID:                 uuid.New(),
		TenantID:           uuid.New(),
		ExtensionID:        uuid.New(),
		ExtensionVersionID: uuid.New(),
		Status:             marketplace.InstallStatusActive,
		Settings:           nil,
		WebhookBase:        "https://example.com",
		InstalledAt:        now,
		UpdatedAt:          now,
	}
	v := installationToView(in)
	if v.Settings == nil {
		t.Fatalf("settings map should be non-nil for nil input bytes")
	}
	if len(v.Settings) != 0 {
		t.Fatalf("settings = %+v, want empty map", v.Settings)
	}
	// Verify the JSON marshal surfaces {} not null — the
	// frontend depends on this for the settings form initial
	// state.
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"settings":{}`) {
		t.Fatalf("marshalled view %q missing settings:{}", string(body))
	}
}

// TestInstallationToViewParsesSettings round-trips a real
// settings blob through installationToView. The handler reads
// the JSONB column as a []byte; we need to make sure a typical
// "object of typed leaves" survives the unmarshal intact (no
// number→float64 conversion ambiguity for typed fields the UI
// might key off of).
func TestInstallationToViewParsesSettings(t *testing.T) {
	t.Parallel()
	in := &marketplace.Installation{
		Settings: []byte(`{"theme":"dark","retries":3,"feature_flags":{"beta":true}}`),
	}
	v := installationToView(in)
	if v.Settings["theme"] != "dark" {
		t.Fatalf("theme = %v, want dark", v.Settings["theme"])
	}
	// JSON numbers round-trip as float64 by default — the
	// JSON.Decoder we'd use to enforce json.Number is a
	// project-wide decision; for v1 we accept the float64
	// idiom. Pin the expectation here so a future change to
	// json.Decoder.UseNumber elsewhere doesn't silently
	// reshape the response wire format.
	if got, _ := v.Settings["retries"].(float64); got != 3 {
		t.Fatalf("retries = %v, want 3", v.Settings["retries"])
	}
}

// TestInstallationToViewCorruptSettingsCollapsesToEmpty ensures
// a corrupt JSONB blob (which should never happen — DB has a
// CHECK that the column is valid JSON) doesn't panic the
// response. Instead the view's settings field collapses to an
// empty map so the caller still sees the rest of the row.
func TestInstallationToViewCorruptSettingsCollapsesToEmpty(t *testing.T) {
	t.Parallel()
	in := &marketplace.Installation{
		Settings: []byte("not-json"),
	}
	v := installationToView(in)
	if v.Settings == nil || len(v.Settings) != 0 {
		t.Fatalf("settings = %+v, want empty map", v.Settings)
	}
}

// TestValidateInstallSettingsAcceptsNilSchema asserts that an
// extension that declares no settings_schema accepts any
// settings document (including nil + arbitrary keys).
func TestValidateInstallSettingsAcceptsNilSchema(t *testing.T) {
	t.Parallel()
	if err := validateInstallSettings(nil, nil); err != nil {
		t.Fatalf("nil schema + nil settings: err = %v, want nil", err)
	}
	if err := validateInstallSettings(nil, map[string]any{"anything": 1}); err != nil {
		t.Fatalf("nil schema + arbitrary settings: err = %v, want nil", err)
	}
}

// TestValidateInstallSettingsEnforcesSchema runs a real schema
// through the validator to make sure the handler-side gate
// rejects an invalid document. Uses a minimal schema with a
// required string field so the test isn't coupled to the full
// validator's surface — that's tested directly in
// internal/marketplace/settings/validator_test.go.
func TestValidateInstallSettingsEnforcesSchema(t *testing.T) {
	t.Parallel()
	schema := []byte(`{"type":"object","required":["api_key"],"properties":{"api_key":{"type":"string"}}}`)
	if err := validateInstallSettings(schema, map[string]any{"api_key": "k-1"}); err != nil {
		t.Fatalf("valid settings: err = %v, want nil", err)
	}
	if err := validateInstallSettings(schema, map[string]any{}); err == nil {
		t.Fatalf("missing required field: err = nil, want non-nil")
	}
	if err := validateInstallSettings(schema, map[string]any{"api_key": 42}); err == nil {
		t.Fatalf("wrong-type field: err = nil, want non-nil")
	}
}

// TestReviewStateToItemReviewedAt covers the timestamp
// formatting branch for the reviewed_at field. The store
// stores reviewed_at as *time.Time so a nil pointer must
// produce an empty string (which the omitempty JSON tag then
// strips); a non-nil pointer must produce a UTC RFC3339 string.
func TestReviewStateToItemReviewedAt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	s := marketplace.ReviewState{
		ExtensionVersionID: uuid.New(),
		Status:             marketplace.ReviewStatusApproved,
		ReviewedAt:         &at,
		CreatedAt:          at,
		UpdatedAt:          at,
	}
	item := reviewStateToItem(s)
	if item.ReviewedAt != "2026-05-30T12:00:00Z" {
		t.Fatalf("reviewed_at = %q, want 2026-05-30T12:00:00Z", item.ReviewedAt)
	}
	s.ReviewedAt = nil
	item = reviewStateToItem(s)
	if item.ReviewedAt != "" {
		t.Fatalf("nil reviewed_at: got %q, want empty", item.ReviewedAt)
	}
}

// TestUpgradeRequestBodyUnmarshal pins the tri-state settings
// decoder: "settings absent" / "settings: null" / "settings: {}"
// produce three distinct (Settings, SettingsProvided) outcomes
// that drive different engine branches downstream
// (default-keep / explicit-keep / explicit-write). A regression
// here would silently swap "preserve existing settings" with
// "wipe to empty document" — a data-loss bug for the publisher's
// settings document. The unit shape is small enough to test
// without a full HTTP harness; see
// marketplace_runtime_upgrade_test.go for end-to-end coverage
// of the same paths through the live engine.
func TestUpgradeRequestBodyUnmarshal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		body             string
		wantSettings     map[string]any
		wantProvided     bool
		wantKeepSettings bool
	}{
		{
			name: "settings absent",
			body: `{"from_version_id":"a","to_version_id":"b"}`,
			// SettingsProvided false → engine default-keep branch.
			wantProvided: false,
		},
		{
			name:         "settings null",
			body:         `{"from_version_id":"a","to_version_id":"b","settings":null}`,
			wantProvided: true,
			// nil map + provided=true → engine treats as keep.
		},
		{
			name:         "settings empty object",
			body:         `{"from_version_id":"a","to_version_id":"b","settings":{}}`,
			wantProvided: true,
			wantSettings: map[string]any{},
			// non-nil map + provided=true → engine writes
			// (in this case wipes to {}).
		},
		{
			name:         "settings populated",
			body:         `{"from_version_id":"a","to_version_id":"b","settings":{"foo":"bar"}}`,
			wantProvided: true,
			wantSettings: map[string]any{"foo": "bar"},
		},
		{
			name:             "keep_settings true",
			body:             `{"from_version_id":"a","to_version_id":"b","keep_settings":true}`,
			wantProvided:     false,
			wantKeepSettings: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got upgradeRequestBody
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.SettingsProvided != tc.wantProvided {
				t.Fatalf("SettingsProvided = %v, want %v", got.SettingsProvided, tc.wantProvided)
			}
			if got.KeepSettings != tc.wantKeepSettings {
				t.Fatalf("KeepSettings = %v, want %v", got.KeepSettings, tc.wantKeepSettings)
			}
			if tc.wantSettings == nil {
				if got.Settings != nil {
					t.Fatalf("Settings = %v, want nil", got.Settings)
				}
				return
			}
			if len(got.Settings) != len(tc.wantSettings) {
				t.Fatalf("Settings len = %d, want %d (got=%v)", len(got.Settings), len(tc.wantSettings), got.Settings)
			}
			for k, want := range tc.wantSettings {
				if got.Settings[k] != want {
					t.Fatalf("Settings[%q] = %v, want %v", k, got.Settings[k], want)
				}
			}
		})
	}
}

// TestUpgradeHandlerContradictoryBodyRejected pins the handler's
// 400 guard against {keep_settings: true, settings: {populated}}.
// The wire contract says the two fields are mutually exclusive;
// previously the switch would silently let keep_settings win and
// the caller's settings document would be dropped without
// feedback. This test reproduces the contradictory body and
// verifies a 400 lands before the handler touches the store /
// resolver / engine — so a misconfigured handler with nil deps
// still produces the correct rejection. "settings: null" +
// keep_settings is the legitimate equivalent (both mean
// preserve), so that combination must NOT trip the guard.
func TestUpgradeHandlerContradictoryBodyRejected(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	installID := uuid.New()

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "keep_settings + populated settings rejected",
			body:       fmt.Sprintf(`{"from_version_id":%q,"to_version_id":%q,"keep_settings":true,"settings":{"foo":"bar"}}`, uuid.New(), uuid.New()),
			wantStatus: http.StatusBadRequest,
			wantSubstr: "mutually exclusive",
		},
		{
			name:       "keep_settings + empty settings object rejected",
			body:       fmt.Sprintf(`{"from_version_id":%q,"to_version_id":%q,"keep_settings":true,"settings":{}}`, uuid.New(), uuid.New()),
			wantStatus: http.StatusBadRequest,
			wantSubstr: "mutually exclusive",
		},
		{
			name: "keep_settings + settings:null is allowed",
			// Both express "preserve existing"; the guard must
			// not fire. A 400 still lands downstream because
			// the handler's nil-store dereferences on
			// GetInstallation — but the response body must
			// NOT carry the mutual-exclusivity message, which
			// proves the wire-contract guard let this through.
			body:       fmt.Sprintf(`{"from_version_id":%q,"to_version_id":%q,"keep_settings":true,"settings":null}`, uuid.New(), uuid.New()),
			wantStatus: 0, // unchecked — we only assert NOT the rejection message
			wantSubstr: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A fresh marketplaceHandlers with nil deps is
			// enough because the contradiction guard fires
			// BEFORE the first store / resolver call. The
			// legitimate body case will dereference past the
			// guard and panic in the test harness, which we
			// recover from explicitly.
			h := &marketplaceHandlers{}

			// chi.RouteContext carries the {install_id} URL
			// param; production routing populates this via
			// chi.URLParam.
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("install_id", installID.String())

			ctx := context.WithValue(context.Background(), chi.RouteCtxKey, rctx)
			ctx = platform.WithTenant(ctx, &tenant.Tenant{ID: tenantID, Status: tenant.StatusActive})

			req := httptest.NewRequest(http.MethodPost,
				"/installations/"+installID.String()+"/upgrade",
				strings.NewReader(tc.body)).WithContext(ctx)
			rec := httptest.NewRecorder()

			func() {
				defer func() {
					// Recover from the nil-store panic on
					// the legitimate-body case so we can
					// still assert on the response body
					// captured up to that point.
					_ = recover()
				}()
				h.upgrade(rec, req)
			}()

			if tc.wantStatus != 0 && rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			if tc.wantSubstr != "" {
				if !strings.Contains(body, tc.wantSubstr) {
					t.Fatalf("body = %q, want substring %q", body, tc.wantSubstr)
				}
			} else {
				// Legitimate body: must NOT carry the
				// rejection message. The handler may have
				// progressed and panicked / errored
				// downstream; we only care that the guard
				// itself didn't fire.
				if strings.Contains(body, "mutually exclusive") {
					t.Fatalf("body = %q unexpectedly carried mutual-exclusivity message", body)
				}
			}
		})
	}
}


