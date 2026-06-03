package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Stripe webhook event types the control plane acts on. Stripe sends
// many more; the handler ignores any type not listed here (after
// still recording it for idempotency/audit).
const (
	EventInvoicePaid          = "invoice.paid"
	EventInvoicePaymentFailed = "invoice.payment_failed"
	EventSubscriptionUpdated  = "customer.subscription.updated"
	EventSubscriptionDeleted  = "customer.subscription.deleted"
)

// webhookTolerance bounds the age of a webhook's signature timestamp.
// Stripe recommends 300s; a wider window weakens replay protection,
// a narrower one risks rejecting legitimately-delayed deliveries.
const webhookTolerance = 5 * time.Minute

// Event is a parsed + signature-verified Stripe webhook event. Data
// is the raw `data.object` so the handler can decode it into the
// concrete shape (invoice / subscription) for the event type.
type Event struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"-"`
}

// stripeEnvelope is the on-the-wire event shape. We pull data.object
// out as RawMessage and re-expose it as Event.Data.
type stripeEnvelope struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// ConstructEvent verifies the Stripe-Signature header against the raw
// request body and the configured webhook secret, then parses the
// event. It returns ErrInvalidSignature when the signature is
// missing, malformed, stale (outside webhookTolerance), or does not
// match — the handler maps that to a 400 so Stripe retries without us
// ever trusting an unauthenticated body. now is injectable for tests.
func ConstructEvent(payload []byte, sigHeader, secret string, now time.Time) (Event, error) {
	if secret == "" {
		// Fail closed: with no secret we cannot authenticate the
		// webhook, so we reject rather than trust it.
		return Event{}, ErrInvalidSignature
	}
	if err := verifySignature(payload, sigHeader, secret, now); err != nil {
		return Event{}, err
	}
	var env stripeEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Event{}, fmt.Errorf("billing: decode webhook envelope: %w", err)
	}
	return Event{ID: env.ID, Type: env.Type, Data: env.Data.Object}, nil
}

// verifySignature implements Stripe's webhook signature scheme: the
// header is `t=<unix>,v1=<hex>,v1=<hex>…`; the signed payload is
// `<t>.<body>`; each v1 is HMAC-SHA256(secret, signedPayload) in hex.
// We accept the event if ANY v1 matches (Stripe rotates secrets by
// sending multiple signatures) and the timestamp is fresh.
func verifySignature(payload []byte, sigHeader, secret string, now time.Time) error {
	if sigHeader == "" {
		return ErrInvalidSignature
	}
	var (
		timestamp  string
		signatures []string
	)
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			signatures = append(signatures, kv[1])
		}
	}
	if timestamp == "" || len(signatures) == 0 {
		return ErrInvalidSignature
	}
	tsUnix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	// Reject stale (or future-dated) timestamps to bound replay.
	age := now.UTC().Sub(time.Unix(tsUnix, 0).UTC())
	if age < 0 {
		age = -age
	}
	if age > webhookTolerance {
		return ErrInvalidSignature
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := mac.Sum(nil)

	for _, sig := range signatures {
		got, err := hex.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, expected) {
			return nil
		}
	}
	return ErrInvalidSignature
}
