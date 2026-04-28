package main

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/notifications"
)

// TestWebhookBackoffUsesPerWebhookBaseAndAttempt covers the v2
// behavior: the schedule is base * 2^(attempt-1) plus jitter, with
// the base sourced from each webhook so a tenant can opt into
// longer or shorter retry windows.
func TestWebhookBackoffUsesPerWebhookBaseAndAttempt(t *testing.T) {
	cases := []struct {
		name        string
		base        int
		attempt     int
		minExpected time.Duration
		maxExpected time.Duration
	}{
		// Default base 10s, jitter window [0, 5s).
		{"default attempt 1", 0, 1, 10 * time.Second, 15 * time.Second},
		{"default attempt 2", 0, 2, 20 * time.Second, 25 * time.Second},
		{"default attempt 5", 0, 5, 160 * time.Second, 165 * time.Second},
		// Custom base 5s, jitter window [0, 2.5s).
		{"custom base attempt 1", 5, 1, 5 * time.Second, 8 * time.Second},
		{"custom base attempt 3", 5, 3, 20 * time.Second, 23 * time.Second},
		// 24h cap kicks in for very large attempts.
		{"capped attempt", 60, 30, 24 * time.Hour, 24*time.Hour + 31*time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := notifications.Webhook{BackoffBaseSeconds: tc.base}
			got := webhookBackoff(h, tc.attempt)
			if got < tc.minExpected || got > tc.maxExpected {
				t.Fatalf("backoff=%v want [%v,%v]", got, tc.minExpected, tc.maxExpected)
			}
		})
	}
}

// TestScheduleNextRetryRespectsPerWebhookCeiling locks in that a
// webhook with max_retries=3 stops scheduling at attempt 3, while a
// webhook with max_retries=10 keeps going to attempt 10. Without
// per-webhook clamping the v1 hardcoded constant would still apply.
func TestScheduleNextRetryRespectsPerWebhookCeiling(t *testing.T) {
	short := notifications.Webhook{ID: uuid.New(), MaxRetries: 3}
	long := notifications.Webhook{ID: uuid.New(), MaxRetries: 10}

	// short hook: attempt 2 schedules, attempt 3 terminal.
	if got := scheduleNextRetry(short, 2); got == nil {
		t.Fatal("short: attempt 2 should schedule")
	}
	if got := scheduleNextRetry(short, 3); got != nil {
		t.Fatalf("short: attempt 3 should be terminal, got %v", *got)
	}
	// long hook: attempt 9 schedules, attempt 10 terminal.
	if got := scheduleNextRetry(long, 9); got == nil {
		t.Fatal("long: attempt 9 should schedule")
	}
	if got := scheduleNextRetry(long, 10); got != nil {
		t.Fatalf("long: attempt 10 should be terminal, got %v", *got)
	}
}

// TestScheduleNextRetryClampsToPlatformMax guards against a tenant
// submitting an unreasonably large max_retries: even if a webhook
// stores 9999, the platform ceiling of 20 still terminates the
// retry chain.
func TestScheduleNextRetryClampsToPlatformMax(t *testing.T) {
	huge := notifications.Webhook{ID: uuid.New(), MaxRetries: 9999}
	if got := scheduleNextRetry(huge, platformMaxWebhookAttempts); got != nil {
		t.Fatalf("attempt at platform ceiling should be terminal, got %v", *got)
	}
}

// TestScheduleNextRetryDefaultCeiling covers a webhook stored
// before the v2 migration backfill ran: MaxRetries=0 should fall
// back to the platform default of 5.
func TestScheduleNextRetryDefaultCeiling(t *testing.T) {
	legacy := notifications.Webhook{ID: uuid.New()}
	if got := scheduleNextRetry(legacy, 4); got == nil {
		t.Fatal("legacy: attempt 4 should schedule under default ceiling 5")
	}
	if got := scheduleNextRetry(legacy, 5); got != nil {
		t.Fatalf("legacy: attempt 5 should be terminal under default ceiling 5, got %v", *got)
	}
}
