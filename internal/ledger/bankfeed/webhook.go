package bankfeed

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// webhook.go defines the provider-agnostic inbound-webhook capability that
// complements the hourly scheduled sync with near-real-time updates. A
// provider that can receive push notifications (Plaid
// SYNC_UPDATES_AVAILABLE, GoCardless requisition events) implements
// WebhookVerifier; the HTTP ingress (services/api) verifies the signature
// and, on a sync-worthy event, resolves the affected connection and runs
// the same SyncOne pipeline the scheduler drives.
//
// Signature scheme: the verifier checks an HMAC-SHA256 of the raw request
// body, hex-encoded, supplied in the provider's signature header, against a
// per-deployment webhook secret configured out of band with the provider.
// The comparison is constant-time. This mirrors the outbound webhook
// signing the notifications worker already uses (services/worker/
// notifications.go) and keeps verification self-contained and testable
// without live provider credentials. The WebhookVerifier interface is
// deliberately scheme-agnostic: a provider that needs its own scheme
// (e.g. Plaid's production JWT/JWK verification) can implement it without
// touching the ingress.

// WebhookEvent is the provider-neutral result of verifying and parsing an
// inbound webhook. ExternalID identifies the affected provider handle
// (Plaid item_id, GoCardless requisition id) that maps to a
// bank_feed_connections row via its external_id column. TriggerSync is true
// when the event means new data is available and the connection should be
// synced; informational events (e.g. an error notification) parse cleanly
// with TriggerSync false so the ingress acknowledges them without work.
type WebhookEvent struct {
	// ExternalID is the provider handle used to resolve the connection.
	ExternalID string
	// Kind is a coarse, non-sensitive classification of the event for
	// logging / metrics (e.g. "transactions.sync", "requisition.linked").
	Kind string
	// TriggerSync indicates the event warrants a sync of the connection.
	TriggerSync bool
}

// WebhookVerifier is the optional capability a Provider implements when it
// receives push notifications. VerifyAndParse is handed the raw request
// body and the request headers; it verifies the signature against the
// provider's configured webhook secret and returns the provider-neutral
// event, or ErrWebhookSignature when verification fails. A provider with
// no configured webhook secret returns ErrWebhookSignature (fail-closed):
// an unverifiable endpoint must never trigger work.
type WebhookVerifier interface {
	VerifyAndParse(payload []byte, headers http.Header) (WebhookEvent, error)
}

// verifyHMACSignature checks that provided is the hex-encoded HMAC-SHA256 of
// payload under secret, in constant time. An empty secret or an empty /
// malformed signature fails closed. Exposed package-internally so each
// provider's VerifyAndParse shares one audited comparison.
func verifyHMACSignature(secret string, payload []byte, provided string) error {
	provided = strings.TrimSpace(provided)
	// A common convention prefixes the digest with the algorithm
	// ("sha256="); accept and strip it so operators can configure either
	// form at the provider.
	if i := strings.IndexByte(provided, '='); i >= 0 && strings.EqualFold(provided[:i], "sha256") {
		provided = provided[i+1:]
	}
	if secret == "" || provided == "" {
		return ErrWebhookSignature
	}
	want := computeHMAC(secret, payload)
	got, err := hex.DecodeString(provided)
	if err != nil {
		return ErrWebhookSignature
	}
	// hmac.Equal is constant-time and length-safe.
	if !hmac.Equal(want, got) {
		return ErrWebhookSignature
	}
	return nil
}

// computeHMAC returns the raw HMAC-SHA256 of payload under secret.
func computeHMAC(secret string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return mac.Sum(nil)
}

// firstHeader returns the first non-empty value among the candidate header
// names (case-insensitive via http.Header.Get). Providers differ on the
// exact header name, so each verifier passes its accepted set.
func firstHeader(headers http.Header, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(headers.Get(n)); v != "" {
			return v
		}
	}
	return ""
}
