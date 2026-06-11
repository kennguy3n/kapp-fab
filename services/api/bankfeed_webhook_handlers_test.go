package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/kapp-fab/internal/ledger/bankfeed"
)

// webhookSign is the hex HMAC-SHA256 a well-behaved provider would send.
func webhookSign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// newWebhookHandlers wires a handler bundle with a registry holding a
// signature-verifying Plaid provider plus the (webhook-less) CSV provider.
// conns/sync are zero-value but non-nil so the nil-guard passes; the paths
// exercised here all return before either is dereferenced.
func newWebhookHandlers(secret string) *bankfeedWebhookHandlers {
	plaid := bankfeed.NewPlaidProvider(bankfeed.PlaidConfig{
		ClientID: "id", Secret: "s", BaseURL: "https://x", WebhookSecret: secret,
	}, nil)
	return &bankfeedWebhookHandlers{
		registry: bankfeed.NewRegistry(plaid, bankfeed.NewCSVProvider()),
		conns:    &bankfeed.ConnectionStore{},
		sync:     &bankfeed.SyncHandler{},
	}
}

// postWebhook drives receive() with a chi URL param for {provider}.
func postWebhook(t *testing.T, h *bankfeedWebhookHandlers, provider string, body []byte, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/finance/bank-feeds/webhooks/"+provider, strings.NewReader(string(body)))
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("provider", provider)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rr := httptest.NewRecorder()
	h.receive(rr, req)
	return rr
}

func TestWebhookReceiveUnknownProvider(t *testing.T) {
	h := newWebhookHandlers("whsec")
	rr := postWebhook(t, h, "monzo", []byte(`{}`), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rr.Code)
	}
}

func TestWebhookReceiveProviderWithoutWebhookSupport(t *testing.T) {
	h := newWebhookHandlers("whsec")
	// CSV is registered but does not implement WebhookVerifier.
	rr := postWebhook(t, h, "csv", []byte(`{}`), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rr.Code)
	}
}

func TestWebhookReceiveBadSignature(t *testing.T) {
	h := newWebhookHandlers("whsec")
	body := []byte(`{"webhook_type":"TRANSACTIONS","webhook_code":"SYNC_UPDATES_AVAILABLE","item_id":"i"}`)
	hdr := http.Header{"Plaid-Verification": {webhookSign("wrong-secret", body)}}
	rr := postWebhook(t, h, "plaid", body, hdr)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rr.Code)
	}
}

func TestWebhookReceiveMissingSignature(t *testing.T) {
	h := newWebhookHandlers("whsec")
	body := []byte(`{"webhook_type":"TRANSACTIONS","webhook_code":"SYNC_UPDATES_AVAILABLE","item_id":"i"}`)
	rr := postWebhook(t, h, "plaid", body, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rr.Code)
	}
}

// An oversize body is rejected with 413 before any signature check, so an
// operator sees the real cause rather than a misleading "signature failed".
func TestWebhookReceiveOversizeBodyRejected(t *testing.T) {
	const secret = "whsec"
	h := newWebhookHandlers(secret)
	body := make([]byte, maxWebhookBytes+1)
	for i := range body {
		body[i] = 'a'
	}
	hdr := http.Header{"Plaid-Verification": {webhookSign(secret, body)}}
	rr := postWebhook(t, h, "plaid", body, hdr)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; want 413", rr.Code)
	}
}

// A verified but non-sync event (e.g. an item ERROR) is acknowledged with
// 202 and never reaches connection resolution.
func TestWebhookReceiveVerifiedNonSyncEventAcks(t *testing.T) {
	const secret = "whsec"
	h := newWebhookHandlers(secret)
	body := []byte(`{"webhook_type":"ITEM","webhook_code":"ERROR","item_id":"i"}`)
	hdr := http.Header{"Plaid-Verification": {webhookSign(secret, body)}}
	rr := postWebhook(t, h, "plaid", body, hdr)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rr.Code)
	}
}

// A verified sync event whose item id is empty is acknowledged without
// resolution (nothing to look up), so it never dereferences conns.
func TestWebhookReceiveVerifiedSyncEventEmptyIDAcks(t *testing.T) {
	const secret = "whsec"
	h := newWebhookHandlers(secret)
	body := []byte(`{"webhook_type":"TRANSACTIONS","webhook_code":"SYNC_UPDATES_AVAILABLE","item_id":""}`)
	hdr := http.Header{"Plaid-Verification": {webhookSign(secret, body)}}
	rr := postWebhook(t, h, "plaid", body, hdr)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rr.Code)
	}
}

func TestWebhookReceiveNotWired(t *testing.T) {
	var h *bankfeedWebhookHandlers
	rr := postWebhook(t, h, "plaid", []byte(`{}`), nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rr.Code)
	}
}

// The response body must be content-free regardless of outcome: a provider
// caller must learn nothing about which connections exist.
func TestWebhookReceiveResponseBodyIsTerse(t *testing.T) {
	const secret = "whsec"
	h := newWebhookHandlers(secret)
	body := []byte(`{"webhook_type":"ITEM","webhook_code":"ERROR","item_id":"i"}`)
	hdr := http.Header{"Plaid-Verification": {webhookSign(secret, body)}}
	rr := postWebhook(t, h, "plaid", body, hdr)
	if got := strings.TrimSpace(rr.Body.String()); got != "" {
		t.Fatalf("202 body = %q; want empty", got)
	}
}

// The webhook route must bypass the global CSRF middleware: a server-to-
// server provider POST carries no Origin/Referer/Bearer, so without an
// exemption it would be rejected with 403 in production (CSRFAllowedOrigins
// set). Guards the fix that adds the path to publicCSRFExemptPathSet.
func TestWebhookPathIsCSRFExempt(t *testing.T) {
	patterns := publicCSRFExemptPathSet()
	for _, provider := range []string{"plaid", "gocardless"} {
		path := "/api/v1/finance/bank-feeds/webhooks/" + provider
		r := httptest.NewRequest(http.MethodPost, path, http.NoBody)
		if !isPublicCSRFExempt(r, patterns) {
			t.Errorf("POST %s not CSRF-exempt; provider webhooks would 403 in prod", path)
		}
	}
	// The bare subtree (no provider segment) and non-POST methods must NOT
	// be exempt — the exemption is for the concrete signed-POST route only.
	bare := httptest.NewRequest(http.MethodPost, "/api/v1/finance/bank-feeds/webhooks/", http.NoBody)
	if isPublicCSRFExempt(bare, patterns) {
		t.Error("bare webhooks/ path must not be CSRF-exempt")
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/finance/bank-feeds/webhooks/plaid", http.NoBody)
	if isPublicCSRFExempt(get, patterns) {
		t.Error("GET on webhook path must not be CSRF-exempt")
	}
}
