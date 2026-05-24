package spam

import (
	"strings"
	"testing"
)

// TestScorer_AuthMatrix pins the Authentication-Results header
// parsing across the full SPF/DKIM/DMARC × pass/fail/softfail/none
// matrix. Each row is a representative line the upstream relay
// emits; the test verifies the score + decision land where the
// design doc says they should.
func TestScorer_AuthMatrix(t *testing.T) {
	cases := []struct {
		name      string
		headers   Headers
		wantScore int
		wantRules []string // rule names that must appear
	}{
		{
			name: "all_pass",
			headers: Headers{{
				Name:  "Authentication-Results",
				Value: "mx.acme; spf=pass smtp.mailfrom=a@b.com; dkim=pass header.d=b.com; dmarc=pass action=none",
			}},
			wantScore: 0,
			wantRules: []string{},
		},
		{
			name: "spf_fail_alone",
			headers: Headers{{
				Name:  "Authentication-Results",
				Value: "mx.acme; spf=fail; dkim=pass; dmarc=pass",
			}},
			wantScore: 5,
			wantRules: []string{"spf_fail"},
		},
		{
			name: "spf_softfail",
			headers: Headers{{
				Name:  "Authentication-Results",
				Value: "mx.acme; spf=softfail; dkim=pass; dmarc=pass",
			}},
			wantScore: 2,
			wantRules: []string{"spf_softfail"},
		},
		{
			name: "spf_none_dkim_none",
			headers: Headers{{
				Name:  "Authentication-Results",
				Value: "mx.acme; spf=none; dkim=none; dmarc=pass",
			}},
			wantScore: 2,
			wantRules: []string{"spf_none", "dkim_none"},
		},
		{
			name: "all_fail",
			headers: Headers{{
				Name:  "Authentication-Results",
				Value: "mx.acme; spf=fail; dkim=fail; dmarc=fail",
			}},
			wantScore: 15,
			wantRules: []string{"spf_fail", "dkim_fail", "dmarc_fail"},
		},
		{
			name:      "no_auth_header",
			headers:   Headers{},
			wantScore: 0,
			wantRules: []string{},
		},
	}
	s := NewScorer()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := s.Score(Email{
				From:     "alice@example.com",
				BodyText: "hello, I have a question about my account please help me",
				Subject:  "support request",
				Headers:  tc.headers,
			})
			if res.Score != tc.wantScore {
				t.Fatalf("score = %d, want %d (reasons: %+v)", res.Score, tc.wantScore, res.Reasons)
			}
			for _, want := range tc.wantRules {
				found := false
				for _, r := range res.Reasons {
					if r.Rule == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected rule %q to fire (reasons: %+v)", want, res.Reasons)
				}
			}
		})
	}
}

// TestScorer_ChainedAuthResultsWorstWins pins the
// "multiple Authentication-Results lines, worst wins" reducer.
// Real chained MTAs prepend their own header — a downstream
// trusted relay seeing "pass" must not paper over an upstream
// edge MTA's "fail".
func TestScorer_ChainedAuthResultsWorstWins(t *testing.T) {
	headers := Headers{
		{Name: "Authentication-Results", Value: "downstream-relay; spf=pass; dkim=pass; dmarc=pass"},
		{Name: "Authentication-Results", Value: "upstream-edge; spf=fail; dkim=fail; dmarc=fail"},
	}
	s := NewScorer()
	res := s.Score(Email{
		From:     "alice@spam.test",
		BodyText: "hello world there",
		Subject:  "hello",
		Headers:  headers,
	})
	if res.Score != 15 {
		t.Fatalf("expected score 15 (3×fail at +5), got %d (reasons: %+v)", res.Score, res.Reasons)
	}
	if res.Decision != BucketJunk {
		t.Fatalf("expected junk decision, got %q", res.Decision)
	}
}

// TestScorer_ReplyToMismatch pins the canonical spammer pattern:
// From: looks legitimate but Reply-To redirects to a throwaway.
func TestScorer_ReplyToMismatch(t *testing.T) {
	s := NewScorer()
	res := s.Score(Email{
		From:     "billing@bank-co.example",
		BodyText: "click here to verify your account",
		Subject:  "support request",
		Headers: Headers{
			{Name: "Reply-To", Value: "evil@throwaway.test"},
		},
	})
	found := false
	for _, r := range res.Reasons {
		if r.Rule == "reply_to_mismatch" {
			found = true
			if r.Points != 3 {
				t.Errorf("expected +3, got +%d", r.Points)
			}
		}
	}
	if !found {
		t.Fatalf("expected reply_to_mismatch rule to fire (reasons: %+v)", res.Reasons)
	}
}

// TestScorer_ReplyToSameDomain pins the negative case: when Reply-To
// matches From, the rule does NOT fire. This is the common case for
// legitimate senders using a no-reply alias.
func TestScorer_ReplyToSameDomain(t *testing.T) {
	s := NewScorer()
	res := s.Score(Email{
		From:     "alice@acme.com",
		BodyText: "I'd like to follow up on my ticket from last week please",
		Subject:  "follow up",
		Headers: Headers{
			{Name: "Reply-To", Value: "no-reply@acme.com"},
		},
	})
	for _, r := range res.Reasons {
		if r.Rule == "reply_to_mismatch" {
			t.Fatalf("expected no reply_to_mismatch (same domain), got %+v", res.Reasons)
		}
	}
}

// TestSenderHost_FallbackStripsTrailingAngleBracket pins the
// non-RFC-5322-parseable display-name path: when net/mail rejects
// the input (display-name + angle brackets without proper
// quoting), the fallback substring-after-last-@ extraction must
// NOT carry a trailing '>' into the host. Otherwise the whitelist
// lookup misses (`"example.com>"` != `"example.com"`) and the
// reply-to mismatch rule false-positives.
func TestSenderHost_FallbackStripsTrailingAngleBracket(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		// net/mail.ParseAddress rejects unquoted-display +
		// angle-bracketed addresses with no space between
		// display and bracket. The fallback path runs.
		{
			name:  "display_no_space_angle",
			input: `display<alice@example.com>`,
			want:  "example.com",
		},
		{
			name:  "display_no_space_angle_uppercase",
			input: `Display<Alice@EXAMPLE.COM>`,
			want:  "example.com",
		},
		// Well-formed inputs go through net/mail and the
		// fallback path is never reached.
		{
			name:  "wellformed_bracketed",
			input: `Alice <alice@example.com>`,
			want:  "example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := senderHost(tc.input)
			if got != tc.want {
				t.Errorf("senderHost(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestScorer_WhitelistedSenderViaFallback pins the user-visible
// effect of TestSenderHost_FallbackStripsTrailingAngleBracket:
// a whitelist entry for `example.com` actually matches an inbound
// `From: "display<alice@example.com>"` even when net/mail can't
// parse it.
func TestScorer_WhitelistedSenderViaFallback(t *testing.T) {
	s := NewScorer(WithWhitelist([]string{"example.com"}))
	res := s.Score(Email{
		From:     `display<alice@example.com>`,
		BodyText: "long enough body content to avoid thin-body trigger",
		Subject:  "billing question",
	})
	if res.Score != 0 {
		t.Errorf("expected Score=0 (whitelist hit), got %d (reasons=%+v)", res.Score, res.Reasons)
	}
	if res.Decision != BucketOpen {
		t.Errorf("expected BucketOpen, got %q", res.Decision)
	}
}

// TestScorer_SuspiciousSubject pins the subject-pattern rule. The
// regex is narrow — broad patterns FP too often on legit support
// traffic.
func TestScorer_SuspiciousSubject(t *testing.T) {
	s := NewScorer()
	cases := []struct {
		subject string
		want    bool
	}{
		{"You are a winner! Click here to claim your prize", true},
		{"Free Viagra for everyone", true},
		{"My order #12345 is delayed", false},
		{"How do I reset my password?", false},
		{"Congratulations on your inheritance from a Nigerian prince", true},
	}
	for _, tc := range cases {
		t.Run(tc.subject, func(t *testing.T) {
			res := s.Score(Email{
				From:     "test@example.com",
				BodyText: "this is the body of the message please help",
				Subject:  tc.subject,
			})
			fired := false
			for _, r := range res.Reasons {
				if r.Rule == "suspicious_subject" {
					fired = true
				}
			}
			if fired != tc.want {
				t.Fatalf("suspicious_subject fired=%v, want %v (subject=%q, reasons=%+v)", fired, tc.want, tc.subject, res.Reasons)
			}
		})
	}
}

// TestScorer_ThinBody pins the structural-thinness rule. A legit
// support email always carries a description; sub-5-char bodies
// without attachments are almost always misfires.
func TestScorer_ThinBody(t *testing.T) {
	s := NewScorer()
	cases := []struct {
		name           string
		body           string
		hasAttachments bool
		wantRule       string
	}{
		{"empty", "", false, "thin_body"},
		{"one_char", "a", false, "thin_body"},
		{"empty_with_attachment", "", true, "attachment_only"},
		{"normal", "I have a problem with my account, please help me reset it", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := s.Score(Email{
				From:           "alice@example.com",
				Subject:        "support",
				BodyText:       tc.body,
				HasAttachments: tc.hasAttachments,
			})
			if tc.wantRule == "" {
				for _, r := range res.Reasons {
					if r.Rule == "thin_body" || r.Rule == "attachment_only" {
						t.Fatalf("did not expect thin/attachment rules to fire, got %+v", res.Reasons)
					}
				}
				return
			}
			found := false
			for _, r := range res.Reasons {
				if r.Rule == tc.wantRule {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %q to fire, got %+v", tc.wantRule, res.Reasons)
			}
		})
	}
}

// TestScorer_WhitelistShortCircuit pins the per-tenant whitelist
// escape hatch. A whitelisted From domain must score 0 even if
// every other rule would fire — operators use this to rescue
// false-positives on legitimate but anomalous senders.
func TestScorer_WhitelistShortCircuit(t *testing.T) {
	s := NewScorer(WithWhitelist([]string{"trusted-partner.com"}))
	res := s.Score(Email{
		From:    "alice@TRUSTED-PARTNER.COM", // case-insensitive
		Subject: "FREE VIAGRA WINNER",        // would otherwise fire
		Headers: Headers{
			{Name: "Authentication-Results", Value: "mx; spf=fail; dkim=fail; dmarc=fail"},
		},
		BodyText: "",
	})
	if res.Score != 0 {
		t.Fatalf("expected whitelist short-circuit to zero the score, got %d (reasons %+v)", res.Score, res.Reasons)
	}
	if res.Decision != BucketOpen {
		t.Fatalf("expected whitelist to land in open bucket, got %q", res.Decision)
	}
	if len(res.Reasons) != 0 {
		t.Fatalf("expected no reasons for whitelisted sender, got %+v", res.Reasons)
	}
}

// TestScorer_ThresholdTunable pins the WithThreshold option. The
// caller can raise the bar so a borderline message (single auth
// failure, mild subject) stays open; or lower it for stricter
// triage.
func TestScorer_ThresholdTunable(t *testing.T) {
	headers := Headers{{
		Name:  "Authentication-Results",
		Value: "mx; spf=fail; dkim=pass; dmarc=pass",
	}}
	email := Email{
		From:     "alice@example.com",
		BodyText: "I need help with my account thanks",
		Subject:  "support request",
		Headers:  headers,
	}
	// At threshold 7 (default), spf=fail alone (+5) is below
	// — stays open.
	dflt := NewScorer().Score(email)
	if dflt.Decision != BucketOpen {
		t.Fatalf("default threshold: expected open, got %q (score %d)", dflt.Decision, dflt.Score)
	}
	// At threshold 5, the same email is junk.
	strict := NewScorer(WithThreshold(5)).Score(email)
	if strict.Decision != BucketJunk {
		t.Fatalf("strict threshold: expected junk, got %q (score %d)", strict.Decision, strict.Score)
	}
}

// TestScorer_BoundaryAtThreshold pins the exact boundary. A score
// EQUAL to threshold lands in junk (the comparison is >=), not
// open. This is the conservative choice — a borderline message
// goes to triage rather than the active queue.
func TestScorer_BoundaryAtThreshold(t *testing.T) {
	// Build a score exactly equal to default 7: spf=fail (+5)
	// + suspicious subject (+2) = 7.
	res := NewScorer().Score(Email{
		From:    "alice@example.com",
		Subject: "free shipping on your order",
		Headers: Headers{{
			Name:  "Authentication-Results",
			Value: "mx; spf=fail; dkim=pass; dmarc=pass",
		}},
		BodyText: "I have a question about my recent purchase, please help",
	})
	if res.Score != 7 {
		t.Fatalf("expected boundary score 7, got %d (reasons %+v)", res.Score, res.Reasons)
	}
	if res.Decision != BucketJunk {
		t.Fatalf("expected score at threshold to land in junk, got %q", res.Decision)
	}
}

// TestScorer_ZeroOrNegativeThresholdIgnored pins the WithThreshold
// guard. An operator who passes 0 (or negative) shouldn't disable
// the bucket entirely; the option silently falls back to the
// default. Disabling junk-bucketing is achievable by setting an
// arbitrarily high integer (e.g. math.MaxInt32).
func TestScorer_ZeroOrNegativeThresholdIgnored(t *testing.T) {
	for _, t0 := range []int{0, -1, -100} {
		s := NewScorer(WithThreshold(t0))
		if s.threshold != DefaultThreshold {
			t.Errorf("WithThreshold(%d): expected fallback to %d, got %d", t0, DefaultThreshold, s.threshold)
		}
	}
}

// TestExtractAuthResult pins the permissive Authentication-Results
// parser against real-world header variations.
func TestExtractAuthResult(t *testing.T) {
	cases := []struct {
		name, line, mech, want string
	}{
		{"simple", "mx; spf=pass smtp.mailfrom=a@b.com", "spf", "pass"},
		{"middle", "mx; dkim=pass; spf=fail; dmarc=pass", "spf", "fail"},
		{"end", "mx; spf=pass; dkim=pass; dmarc=softfail", "dmarc", "softfail"},
		{"absent", "mx; spf=pass; dkim=pass", "dmarc", ""},
		{"case_insensitive_mech", "mx; SPF=Pass", "spf", "pass"},
		{"with_value_comments", "mx; spf=fail (reason: hard fail) smtp.mailfrom=a@b.com", "spf", "fail"},
		{"empty", "", "spf", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractAuthResult(tc.line, tc.mech)
			if got != tc.want {
				t.Errorf("extractAuthResult(%q, %q) = %q, want %q", tc.line, tc.mech, got, tc.want)
			}
		})
	}
}

// TestSenderHost pins the RFC-5322 address-parsing helper across
// the address forms the inbound mail path actually sees.
func TestSenderHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"alice@example.com", "example.com"},
		{"Alice <alice@example.com>", "example.com"},
		{"  alice@EXAMPLE.COM  ", "example.com"},
		{"<alice@example.com>", "example.com"},
		// Permissive fallback for net/mail-rejecting addresses.
		{"日本語<alice@example.com>", "example.com"},
		{"no-at-sign", ""},
		{"", ""},
		{"alice@", ""}, // empty host after @ → empty result
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := senderHost(tc.in)
			if got != tc.want {
				t.Errorf("senderHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHeaders_GetAll pins the case-insensitive header lookup.
// RFC-5322 mandates case-insensitive header names but the upstream
// MTA's canonicalisation is not under our control.
func TestHeaders_GetAll(t *testing.T) {
	h := Headers{
		{Name: "Authentication-Results", Value: "a"},
		{Name: "authentication-results", Value: "b"},
		{Name: "X-Foo", Value: "c"},
	}
	got := h.Get("authentication-results")
	if got != "a" {
		t.Errorf("Get: expected first match 'a', got %q", got)
	}
	all := h.All("Authentication-Results")
	if len(all) != 2 || all[0] != "a" || all[1] != "b" {
		t.Errorf("All: expected [a b], got %v", all)
	}
	if h.Get("not-present") != "" {
		t.Errorf("Get(absent): expected empty, got %q", h.Get("not-present"))
	}
}

// TestScorer_DetailRecorded pins that the Reasons slice carries
// enough context for the agent UI to render a breakdown.
func TestScorer_DetailRecorded(t *testing.T) {
	s := NewScorer()
	res := s.Score(Email{
		From:    "spammer@evil.test",
		Subject: "free winner click here",
		Headers: Headers{
			{Name: "Authentication-Results", Value: "mx; spf=fail; dkim=pass; dmarc=pass"},
			{Name: "Reply-To", Value: "redirect@other.test"},
		},
		BodyText: "I have a question about my account",
	})
	for _, r := range res.Reasons {
		if r.Detail == "" {
			t.Errorf("expected Detail to be populated for rule %q", r.Rule)
		}
		if !strings.Contains(strings.ToLower(r.Detail), strings.ToLower(strings.Split(r.Rule, "_")[0])) &&
			r.Rule != "suspicious_subject" {
			// Looser check: details should mention something
			// related to the rule. The suspicious_subject
			// rule's detail is generic ("matched suspicious-
			// pattern") so it's exempted from this check.
			t.Logf("note: rule %q detail %q does not mention rule keyword", r.Rule, r.Detail)
		}
	}
}
