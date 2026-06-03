package billing

import (
	"testing"
	"time"
)

// TestGraceExpired pins the suspension policy. The key regression it
// guards: a past-due subscription that never had a trial (TrialEnd nil)
// must still be suspended once its current period + grace has elapsed —
// anchoring solely on TrialEnd previously let it evade suspension.
func TestGraceExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	ptr := func(d time.Duration) *time.Time { tt := now.Add(d); return &tt }

	cases := []struct {
		name string
		sub  Subscription
		want bool
	}{
		{"active never suspends", Subscription{Status: SubStatusActive, TrialEnd: ptr(-365 * 24 * time.Hour)}, false},
		{"canceled never suspends", Subscription{Status: SubStatusCanceled, CurrentPeriodEnd: ptr(-365 * 24 * time.Hour)}, false},
		{"trialing within grace stays", Subscription{Status: SubStatusTrialing, TrialEnd: ptr(-1 * time.Hour)}, false},
		{"trialing past grace suspends", Subscription{Status: SubStatusTrialing, TrialEnd: ptr(-trialGrace - time.Hour)}, true},
		{"past_due no trial within grace stays", Subscription{Status: SubStatusPastDue, CurrentPeriodEnd: ptr(-1 * time.Hour)}, false},
		{"past_due no trial past grace suspends", Subscription{Status: SubStatusPastDue, CurrentPeriodEnd: ptr(-trialGrace - time.Hour)}, true},
		{"past_due no anchors never suspends", Subscription{Status: SubStatusPastDue}, false},
		{"past_due falls back to trial end", Subscription{Status: SubStatusPastDue, TrialEnd: ptr(-trialGrace - time.Hour)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := tc.sub
			if got := graceExpired(&sub, now); got != tc.want {
				t.Fatalf("graceExpired(%+v) = %v, want %v", tc.sub, got, tc.want)
			}
		})
	}
}

// TestSanitizeReturnURL pins the post-checkout redirect guard: only a
// same-origin absolute http(s) URL is honored; anything else falls back
// to the configured ReturnURL so a replayed JWT can't steer the victim
// to an attacker-controlled origin after checkout.
func TestSanitizeReturnURL(t *testing.T) {
	const base = "https://app.kapp.example/billing"
	svc := NewService(ServiceDeps{Config: Config{ReturnURL: base}})

	cases := []struct {
		name      string
		candidate string
		want      string
	}{
		{"empty falls back to configured", "", base},
		{"same origin path accepted", "https://app.kapp.example/billing?status=ok", "https://app.kapp.example/billing?status=ok"},
		{"foreign host rejected", "https://evil.example/phish", base},
		{"scheme downgrade rejected", "http://app.kapp.example/billing", base},
		{"non-http scheme rejected", "javascript:alert(1)", base},
		{"garbage rejected", "://nope", base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.sanitizeReturnURL(tc.candidate); got != tc.want {
				t.Fatalf("sanitizeReturnURL(%q) = %q, want %q", tc.candidate, got, tc.want)
			}
		})
	}

	// With no configured base there is nothing to pin against, so any
	// client value is refused (Stripe falls back to the account default).
	noBase := NewService(ServiceDeps{Config: Config{}})
	if got := noBase.sanitizeReturnURL("https://app.kapp.example/billing"); got != "" {
		t.Fatalf("sanitizeReturnURL with empty base = %q, want empty", got)
	}
}
