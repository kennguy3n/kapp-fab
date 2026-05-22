package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestSubmitFormRequest_HoneypotDecoysBindAtTopLevel verifies that the
// three top-level decoy fields (url / website / homepage) bind into
// dedicated struct fields rather than into req.Data. The whole point
// of moving the decoys out of req.Data was to keep them from colliding
// with legitimate KType schema fields named url/website/homepage; if a
// refactor accidentally routed any of them through Data again this test
// would catch it.
func TestSubmitFormRequest_HoneypotDecoysBindAtTopLevel(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantURL  string
		wantSite string
		wantHP   string
		dataName string
	}{
		{
			name:     "url decoy",
			body:     `{"data":{"name":"alice"},"url":"http://bot.example"}`,
			wantURL:  "http://bot.example",
			dataName: "alice",
		},
		{
			name:     "website decoy",
			body:     `{"data":{"name":"alice"},"website":"http://bot.example"}`,
			wantSite: "http://bot.example",
			dataName: "alice",
		},
		{
			name:     "homepage decoy",
			body:     `{"data":{"name":"alice"},"homepage":"http://bot.example"}`,
			wantHP:   "http://bot.example",
			dataName: "alice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req submitFormRequest
			if err := json.NewDecoder(bytes.NewReader([]byte(tc.body))).Decode(&req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.HoneypotURL != tc.wantURL {
				t.Errorf("HoneypotURL = %q, want %q", req.HoneypotURL, tc.wantURL)
			}
			if req.HoneypotWebsite != tc.wantSite {
				t.Errorf("HoneypotWebsite = %q, want %q", req.HoneypotWebsite, tc.wantSite)
			}
			if req.HoneypotHomepage != tc.wantHP {
				t.Errorf("HoneypotHomepage = %q, want %q", req.HoneypotHomepage, tc.wantHP)
			}
			if got, ok := req.Data["name"]; !ok || got != tc.dataName {
				t.Errorf("Data[\"name\"] = %v, want %q", got, tc.dataName)
			}
			// Critical: decoy keys must NOT appear inside Data.
			// If they did, the old behavior (which inspected
			// req.Data for these keys) would have silently dropped
			// any legitimate submission that had a schema field
			// named url/website/homepage.
			for _, k := range []string{"url", "website", "homepage"} {
				if _, ok := req.Data[k]; ok {
					t.Errorf("decoy key %q leaked into req.Data", k)
				}
			}
			if !req.isHoneypotTripped() {
				t.Errorf("isHoneypotTripped() = false; want true for body %s", tc.body)
			}
		})
	}
}

// TestSubmitFormRequest_LegitimateURLFieldInDataIsNotHoneypot is the
// regression guard for the bug Devin Review flagged: a KType schema
// with a real "url" field (e.g. a CRM contact whose website lives in
// data.url) must NOT trip the honeypot. The old logic checked
// req.Data["url"] and dropped every such submission as spam.
func TestSubmitFormRequest_LegitimateURLFieldInDataIsNotHoneypot(t *testing.T) {
	cases := []string{
		`{"data":{"url":"https://acme.example","name":"Acme"}}`,
		`{"data":{"website":"https://acme.example","name":"Acme"}}`,
		`{"data":{"homepage":"https://acme.example","name":"Acme"}}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			var req submitFormRequest
			if err := json.NewDecoder(bytes.NewReader([]byte(body))).Decode(&req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.isHoneypotTripped() {
				t.Fatalf("isHoneypotTripped() = true; legitimate schema field treated as spam (body=%s)", body)
			}
		})
	}
}

// TestSubmitFormRequest_NoHoneypotEmptyEnvelope covers the common path
// — no decoy populated and a normal data payload — so the handler
// proceeds to store.Submit.
func TestSubmitFormRequest_NoHoneypotEmptyEnvelope(t *testing.T) {
	body := []byte(`{"data":{"email":"x@example.com"}}`)
	var req submitFormRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.isHoneypotTripped() {
		t.Fatalf("isHoneypotTripped() = true; want false on empty envelope")
	}
}

// TestSubmitFormRequest_WhitespaceOnlyDecoyTripsHoneypot guarantees a
// bot can't dodge the gate by submitting " " in the decoy field. The
// strings.TrimSpace branch is small but worth pinning so a future
// refactor doesn't lose it.
func TestSubmitFormRequest_WhitespaceOnlyDecoyTripsHoneypot(t *testing.T) {
	body := []byte(`{"data":{"name":"alice"},"url":"   "}`)
	var req submitFormRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !req.isHoneypotTripped() {
		t.Fatalf("isHoneypotTripped() = false; whitespace-only decoy should trip the gate")
	}
}
