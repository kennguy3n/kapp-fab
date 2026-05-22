package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestHasHoneypotValue(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want bool
	}{
		{"empty map", map[string]any{}, false},
		{"benign field", map[string]any{"name": "alice"}, false},
		{"honeypot url empty string", map[string]any{"url": ""}, false},
		{"honeypot url non-empty", map[string]any{"url": "http://bot.example"}, true},
		{"honeypot website non-empty", map[string]any{"website": "x"}, true},
		{"honeypot homepage non-empty", map[string]any{"homepage": "x"}, true},
		{"honeypot non-string value ignored", map[string]any{"url": 42}, false},
		{"unrelated decoy field", map[string]any{"name": "alice", "phone": "555"}, false},
		{"nil map", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasHoneypotValue(tc.in); got != tc.want {
				t.Fatalf("hasHoneypotValue(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSubmitFormRequest_HoneypotTopLevel guards the JSON binding for
// the top-level "url" decoy field. The request struct uses a custom
// json tag — a refactor that breaks the binding would silently turn
// every bot submission into a real record insert.
func TestSubmitFormRequest_HoneypotTopLevel(t *testing.T) {
	body := []byte(`{"data":{"name":"alice"},"url":"http://bot.example"}`)
	var req submitFormRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Honeypot != "http://bot.example" {
		t.Fatalf("Honeypot = %q, want %q", req.Honeypot, "http://bot.example")
	}
	if v, ok := req.Data["name"]; !ok || v != "alice" {
		t.Fatalf("Data[\"name\"] = %v, want alice", v)
	}
}

// TestSubmitFormRequest_NoHoneypot verifies the common path: a real
// submission with no decoy field present leaves Honeypot empty so the
// handler proceeds to store.Submit.
func TestSubmitFormRequest_NoHoneypot(t *testing.T) {
	body := []byte(`{"data":{"email":"x@example.com"}}`)
	var req submitFormRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Honeypot != "" {
		t.Fatalf("Honeypot = %q, want empty", req.Honeypot)
	}
}
