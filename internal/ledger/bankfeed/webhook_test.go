package bankfeed

import (
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
)

// sign returns the hex HMAC-SHA256 of payload under secret — what a
// well-behaved provider would put in its signature header.
func sign(secret string, payload []byte) string {
	return hex.EncodeToString(computeHMAC(secret, payload))
}

func TestVerifyHMACSignature(t *testing.T) {
	secret := "whsec_test"
	payload := []byte(`{"hello":"world"}`)
	good := sign(secret, payload)

	cases := []struct {
		name     string
		secret   string
		provided string
		wantErr  bool
	}{
		{"valid", secret, good, false},
		{"valid with sha256= prefix", secret, "sha256=" + good, false},
		{"valid with SHA256= prefix (case-insensitive)", secret, "SHA256=" + good, false},
		{"empty secret fails closed", "", good, true},
		{"empty signature fails closed", secret, "", true},
		{"whitespace signature fails closed", secret, "   ", true},
		{"wrong secret", "other", good, true},
		{"tampered digest", secret, good[:len(good)-1] + "0", true},
		{"non-hex signature", secret, "zzzz", true},
		{"wrong length digest", secret, "abcd", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyHMACSignature(tc.secret, payload, tc.provided)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyHMACSignature err = %v; wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrWebhookSignature) {
				t.Fatalf("err = %v; want ErrWebhookSignature", err)
			}
		})
	}
}

func TestPlaidVerifyAndParse(t *testing.T) {
	const secret = "whsec_plaid"
	p := NewPlaidProvider(PlaidConfig{
		ClientID: "id", Secret: "s", BaseURL: "https://x", WebhookSecret: secret,
	}, nil)
	if p == nil {
		t.Fatal("provider nil")
	}

	syncBody := []byte(`{"webhook_type":"TRANSACTIONS","webhook_code":"SYNC_UPDATES_AVAILABLE","item_id":"item-123"}`)

	t.Run("valid sync event triggers", func(t *testing.T) {
		h := http.Header{"Plaid-Verification": {sign(secret, syncBody)}}
		ev, err := p.VerifyAndParse(syncBody, h)
		if err != nil {
			t.Fatalf("VerifyAndParse: %v", err)
		}
		if ev.ExternalID != "item-123" || !ev.TriggerSync {
			t.Fatalf("ev = %+v; want item-123 + trigger", ev)
		}
		if ev.Kind != "transactions.sync_updates_available" {
			t.Fatalf("kind = %q", ev.Kind)
		}
	})

	t.Run("X-Webhook-Signature header accepted", func(t *testing.T) {
		h := http.Header{"X-Webhook-Signature": {sign(secret, syncBody)}}
		if _, err := p.VerifyAndParse(syncBody, h); err != nil {
			t.Fatalf("VerifyAndParse: %v", err)
		}
	})

	t.Run("bad signature rejected", func(t *testing.T) {
		h := http.Header{"Plaid-Verification": {sign("wrong", syncBody)}}
		if _, err := p.VerifyAndParse(syncBody, h); !errors.Is(err, ErrWebhookSignature) {
			t.Fatalf("err = %v; want ErrWebhookSignature", err)
		}
	})

	t.Run("missing signature rejected", func(t *testing.T) {
		if _, err := p.VerifyAndParse(syncBody, http.Header{}); !errors.Is(err, ErrWebhookSignature) {
			t.Fatalf("err = %v; want ErrWebhookSignature", err)
		}
	})

	t.Run("non-transaction event parses without trigger", func(t *testing.T) {
		body := []byte(`{"webhook_type":"ITEM","webhook_code":"ERROR","item_id":"item-9"}`)
		h := http.Header{"Plaid-Verification": {sign(secret, body)}}
		ev, err := p.VerifyAndParse(body, h)
		if err != nil {
			t.Fatalf("VerifyAndParse: %v", err)
		}
		if ev.TriggerSync {
			t.Fatalf("ERROR event must not trigger sync: %+v", ev)
		}
		if ev.ExternalID != "item-9" {
			t.Fatalf("ev.ExternalID = %q; want item-9", ev.ExternalID)
		}
	})
}

func TestPlaidVerifyAndParseFailsClosedWithoutSecret(t *testing.T) {
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, nil)
	body := []byte(`{"webhook_type":"TRANSACTIONS","webhook_code":"SYNC_UPDATES_AVAILABLE","item_id":"i"}`)
	// Even a "correct" signature for an empty secret must be rejected: an
	// unconfigured webhook endpoint never triggers work.
	h := http.Header{"Plaid-Verification": {sign("", body)}}
	if _, err := p.VerifyAndParse(body, h); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("err = %v; want ErrWebhookSignature", err)
	}
}

func TestGoCardlessVerifyAndParse(t *testing.T) {
	const secret = "whsec_gc"
	p := NewGoCardlessProvider(GoCardlessConfig{
		SecretID: "i", SecretKey: "k", BaseURL: "https://x", WebhookSecret: secret,
	}, nil)
	if p == nil {
		t.Fatal("provider nil")
	}

	t.Run("linked event triggers, keyed by requisition id", func(t *testing.T) {
		body := []byte(`{"requisition_id":"req-77","event":"LINKED"}`)
		h := http.Header{"Webhook-Signature": {sign(secret, body)}}
		ev, err := p.VerifyAndParse(body, h)
		if err != nil {
			t.Fatalf("VerifyAndParse: %v", err)
		}
		if ev.ExternalID != "req-77" || !ev.TriggerSync {
			t.Fatalf("ev = %+v; want req-77 + trigger", ev)
		}
	})

	t.Run("id fallback when requisition_id absent", func(t *testing.T) {
		body := []byte(`{"id":"req-88","type":"ACCOUNT_REFRESHED"}`)
		h := http.Header{"X-Webhook-Signature": {sign(secret, body)}}
		ev, err := p.VerifyAndParse(body, h)
		if err != nil {
			t.Fatalf("VerifyAndParse: %v", err)
		}
		if ev.ExternalID != "req-88" || !ev.TriggerSync {
			t.Fatalf("ev = %+v; want req-88 + trigger", ev)
		}
	})

	t.Run("unknown event parses without trigger", func(t *testing.T) {
		body := []byte(`{"requisition_id":"req-77","event":"EXPIRED"}`)
		h := http.Header{"Webhook-Signature": {sign(secret, body)}}
		ev, err := p.VerifyAndParse(body, h)
		if err != nil {
			t.Fatalf("VerifyAndParse: %v", err)
		}
		if ev.TriggerSync {
			t.Fatalf("EXPIRED must not trigger: %+v", ev)
		}
	})

	t.Run("bad signature rejected", func(t *testing.T) {
		body := []byte(`{"requisition_id":"req-77","event":"LINKED"}`)
		h := http.Header{"Webhook-Signature": {sign("wrong", body)}}
		if _, err := p.VerifyAndParse(body, h); !errors.Is(err, ErrWebhookSignature) {
			t.Fatalf("err = %v; want ErrWebhookSignature", err)
		}
	})

	t.Run("enveloped events array, requisition under links, triggers", func(t *testing.T) {
		body := []byte(`{"events":[{"action":"linked","resource_type":"requisitions","links":{"requisition":"req-env"}}]}`)
		h := http.Header{"Webhook-Signature": {sign(secret, body)}}
		ev, err := p.VerifyAndParse(body, h)
		if err != nil {
			t.Fatalf("VerifyAndParse: %v", err)
		}
		if ev.ExternalID != "req-env" || !ev.TriggerSync {
			t.Fatalf("enveloped event = %+v; want req-env + trigger", ev)
		}
	})

	t.Run("enveloped events array with no sync-worthy action acks without trigger", func(t *testing.T) {
		body := []byte(`{"events":[{"action":"expired","links":{"requisition":"req-exp"}}]}`)
		h := http.Header{"Webhook-Signature": {sign(secret, body)}}
		ev, err := p.VerifyAndParse(body, h)
		if err != nil {
			t.Fatalf("VerifyAndParse: %v", err)
		}
		if ev.TriggerSync {
			t.Fatalf("expired enveloped event must not trigger: %+v", ev)
		}
		if ev.ExternalID != "req-exp" {
			t.Fatalf("ExternalID = %q; want req-exp (first seen)", ev.ExternalID)
		}
	})
}
