package runtime

import (
	"strings"
	"testing"
)

// TestResolveEndpoint asserts the registrar resolver mirrors the
// manifest validator's two-form endpoint contract. Devin Review
// BUG_0001 on PR #128 caught the previous single-form
// implementation that rejected fully-qualified HTTPS URLs even
// though the validator had accepted them at manifest-load time.
//
// The validator (validateEndpoint in manifest.go §3.1) accepts:
//
//  1. ${EXTENSION_WEBHOOK_BASE} optionally followed by a `/`-rooted
//     path; OR
//  2. A fully-qualified `https://` URL with no embedded `${...}`
//     tokens.
//
// resolveEndpoint must accept both shapes, substituting webhook_base
// only for the placeholder form and passing the direct URL through
// unchanged. A regression here would silently fail every install
// whose manifest declared a direct-HTTPS endpoint — a strictly
// worse user experience than rejecting the manifest at submit time.
func TestResolveEndpoint(t *testing.T) {
	const webhookBase = "https://tenant-42.kapp.io/extwh"

	tests := []struct {
		name        string
		endpoint    string
		base        string
		want        string
		wantErrSub  string // non-empty: expect error whose message contains this substring
	}{
		{
			name:     "form1_placeholder_only_substitutes_to_base",
			endpoint: "${EXTENSION_WEBHOOK_BASE}",
			base:     webhookBase,
			want:     webhookBase,
		},
		{
			name:     "form1_placeholder_with_path_substitutes_and_concatenates",
			endpoint: "${EXTENSION_WEBHOOK_BASE}/lifecycle/pre_install",
			base:     webhookBase,
			want:     webhookBase + "/lifecycle/pre_install",
		},
		{
			name:     "form2_direct_https_passthrough",
			endpoint: "https://publisher.example.com/callback",
			base:     webhookBase,
			want:     "https://publisher.example.com/callback",
		},
		{
			name:     "form2_direct_https_with_query_passthrough",
			endpoint: "https://hooks.publisher.example/v1/handle?ver=2",
			base:     webhookBase,
			want:     "https://hooks.publisher.example/v1/handle?ver=2",
		},
		{
			name:     "form1_whitespace_trimmed_before_resolution",
			endpoint: "  ${EXTENSION_WEBHOOK_BASE}/x  ",
			base:     webhookBase,
			want:     webhookBase + "/x",
		},
		{
			name:       "empty_endpoint_rejected",
			endpoint:   "",
			base:       webhookBase,
			wantErrSub: "empty endpoint",
		},
		{
			name:       "whitespace_only_endpoint_rejected",
			endpoint:   "   ",
			base:       webhookBase,
			wantErrSub: "empty endpoint",
		},
		{
			name:       "plain_http_direct_url_rejected",
			endpoint:   "http://publisher.example.com/cb",
			base:       webhookBase,
			wantErrSub: "https://",
		},
		{
			name:       "non_url_string_rejected",
			endpoint:   "publisher.example.com/cb",
			base:       webhookBase,
			wantErrSub: "fully-qualified https://",
		},
		{
			name:       "placeholder_with_plain_http_base_rejected",
			endpoint:   "${EXTENSION_WEBHOOK_BASE}/lifecycle",
			base:       "http://tenant-42.kapp.io/extwh",
			wantErrSub: "must use https://",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveEndpoint(tt.endpoint, tt.base)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("resolveEndpoint(%q, %q) = %q, nil; want error containing %q", tt.endpoint, tt.base, got, tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("resolveEndpoint(%q, %q) error = %v; want substring %q", tt.endpoint, tt.base, err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEndpoint(%q, %q) unexpected error: %v", tt.endpoint, tt.base, err)
			}
			if got != tt.want {
				t.Fatalf("resolveEndpoint(%q, %q) = %q; want %q", tt.endpoint, tt.base, got, tt.want)
			}
		})
	}
}
