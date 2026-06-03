package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

// signPayload builds a valid Stripe-Signature header for the given
// body + secret at time ts, mirroring Stripe's `t=…,v1=…` scheme so
// the tests exercise the real verification path rather than a stub.
func signPayload(body []byte, secret string, ts time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	tsStr := fmt.Sprintf("%d", ts.Unix())
	mac.Write([]byte(tsStr))
	mac.Write([]byte("."))
	mac.Write(body)
	return fmt.Sprintf("t=%s,v1=%s", tsStr, hex.EncodeToString(mac.Sum(nil)))
}

const testWebhookSecret = "whsec_test_secret"

func TestConstructEventValidSignature(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"invoice.paid","data":{"object":{"id":"in_1"}}}`)
	now := time.Unix(1_700_000_000, 0)
	sig := signPayload(body, testWebhookSecret, now)

	ev, err := ConstructEvent(body, sig, testWebhookSecret, now)
	if err != nil {
		t.Fatalf("ConstructEvent returned error on a valid signature: %v", err)
	}
	if ev.ID != "evt_1" || ev.Type != EventInvoicePaid {
		t.Fatalf("parsed event = %+v, want id=evt_1 type=invoice.paid", ev)
	}
	if string(ev.Data) != `{"id":"in_1"}` {
		t.Fatalf("event data = %s, want the raw data.object", ev.Data)
	}
}

func TestConstructEventEmptySecretFailsClosed(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1_700_000_000, 0)
	// Even a body bearing a header must be rejected when no secret is
	// configured — the handler cannot authenticate it.
	sig := signPayload(body, "anything", now)
	if _, err := ConstructEvent(body, sig, "", now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("empty secret error = %v, want ErrInvalidSignature", err)
	}
}

func TestConstructEventTamperedBody(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"invoice.paid","data":{"object":{}}}`)
	now := time.Unix(1_700_000_000, 0)
	sig := signPayload(body, testWebhookSecret, now)

	tampered := []byte(`{"id":"evt_2","type":"invoice.paid","data":{"object":{}}}`)
	if _, err := ConstructEvent(tampered, sig, testWebhookSecret, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered body error = %v, want ErrInvalidSignature", err)
	}
}

func TestConstructEventStaleTimestamp(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"invoice.paid","data":{"object":{}}}`)
	signedAt := time.Unix(1_700_000_000, 0)
	sig := signPayload(body, testWebhookSecret, signedAt)

	// Verify well outside the tolerance window — replay must be
	// rejected even though the HMAC itself is correct.
	later := signedAt.Add(webhookTolerance + time.Minute)
	if _, err := ConstructEvent(body, sig, testWebhookSecret, later); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("stale timestamp error = %v, want ErrInvalidSignature", err)
	}
}

func TestConstructEventMalformedHeader(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1_700_000_000, 0)
	for _, h := range []string{"", "garbage", "t=123", "v1=abc"} {
		if _, err := ConstructEvent(body, h, testWebhookSecret, now); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("header %q error = %v, want ErrInvalidSignature", h, err)
		}
	}
}
