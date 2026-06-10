package main

import "testing"

// TestSPACallbackPath_DefaultsToCallback locks the post-review fix: with
// no IAM_CORE_POST_LOGIN_REDIRECT configured the callback must deliver
// the token fragment to "/callback" (the SPA route that reads the
// fragment), not "/", which matched the AppShell catch-all and silently
// dropped the tokens.
func TestSPACallbackPath_DefaultsToCallback(t *testing.T) {
	h := &authHandlers{}
	if got := h.spaCallbackPath(); got != "/callback" {
		t.Fatalf("spaCallbackPath default = %q, want %q", got, "/callback")
	}
}

// TestSPACallbackPath_HonoursOverride confirms operators can still mount
// the SPA callback elsewhere via IAM_CORE_POST_LOGIN_REDIRECT, as long
// as it is a safe same-site path.
func TestSPACallbackPath_HonoursOverride(t *testing.T) {
	h := &authHandlers{postLoginRedirect: "/auth/finish"}
	if got := h.spaCallbackPath(); got != "/auth/finish" {
		t.Fatalf("spaCallbackPath override = %q, want %q", got, "/auth/finish")
	}
}

// TestSPACallbackPath_RejectsUnsafeOverride confirms an open-redirect
// style override is ignored and the safe default is used instead.
func TestSPACallbackPath_RejectsUnsafeOverride(t *testing.T) {
	for _, bad := range []string{"//evil.example", "https://evil.example", "no-leading-slash"} {
		h := &authHandlers{postLoginRedirect: bad}
		if got := h.spaCallbackPath(); got != "/callback" {
			t.Fatalf("spaCallbackPath(%q) = %q, want safe default %q", bad, got, "/callback")
		}
	}
}
