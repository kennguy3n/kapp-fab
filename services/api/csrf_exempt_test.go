package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsPublicCSRFExempt_MatchesFormSubmit(t *testing.T) {
	patterns := publicCSRFExemptPathSet()

	cases := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"POST form submit", "POST", "/api/v1/forms/abc-123/submit", true},
		{"POST form submit with uuid", "POST", "/api/v1/forms/550e8400-e29b-41d4-a716-446655440000/submit", true},
		// Non-POST methods are never exempt — only the form submit
		// endpoint is a public-mutating route from third-party
		// origins; everything else under /forms/ goes through the
		// tenant chain.
		{"GET form public", "GET", "/api/v1/forms/abc-123/submit", false},
		{"DELETE form submit", "DELETE", "/api/v1/forms/abc-123/submit", false},
		// Missing {id} segment — the exempt pattern requires an
		// identifier between the prefix and suffix; bare
		// /api/v1/forms/submit (no id) shouldn't match because no
		// such handler exists.
		{"missing id", "POST", "/api/v1/forms//submit", false},
		{"flat path no id", "POST", "/api/v1/forms/submit", false},
		// Nested path — the {id} placeholder is strictly one
		// segment; multi-segment ids aren't a real route shape.
		{"multi-segment id", "POST", "/api/v1/forms/a/b/submit", false},
		// Suffix-only match without prefix.
		{"different prefix", "POST", "/api/v2/forms/abc/submit", false},
		// Other public endpoints are NOT exempt yet (they may be
		// added later with their own line in the exempt set).
		{"sso", "POST", "/api/v1/auth/sso", false},
		{"portal request", "POST", "/api/v1/portal/auth/request", false},
		{"captcha challenge", "GET", "/api/v1/captcha/challenge", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, http.NoBody)
			if got := isPublicCSRFExempt(r, patterns); got != tc.want {
				t.Errorf("isPublicCSRFExempt(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestIsPublicCSRFExempt_EmptyPatternSet(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/v1/forms/abc/submit", http.NoBody)
	if got := isPublicCSRFExempt(r, nil); got {
		t.Error("empty pattern set should never match")
	}
}
