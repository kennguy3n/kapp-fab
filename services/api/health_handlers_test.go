package main

import "testing"

// TestPublicComponentName_NeverLeaksRawProbeName asserts the public
// surface maps every internal probe name to a generic, technology-
// agnostic label and never echoes the raw name (which would let a
// scrape fingerprint the stack). The keys here are the probe names
// registered in platform.HealthChecker.probes().
func TestPublicComponentName_NeverLeaksRawProbeName(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"postgres":         "database",
		"redis":            "cache",
		"nats":             "event_bus",
		"zk_object_fabric": "object_storage",
		"outbox":           "event_delivery",
		"worker":           "background_jobs",
	}
	for raw, label := range want {
		got := publicComponentName(raw)
		if got != label {
			t.Errorf("publicComponentName(%q) = %q, want %q", raw, got, label)
		}
		if got == raw {
			t.Errorf("publicComponentName(%q) leaked the raw probe name", raw)
		}
	}
}

// TestPublicComponentName_UnknownCollapsesToService ensures a probe
// added in the future without a mapping cannot leak its raw name on
// the public surface — it shows up as the generic "service".
func TestPublicComponentName_UnknownCollapsesToService(t *testing.T) {
	t.Parallel()
	if got := publicComponentName("kafka"); got != "service" {
		t.Errorf("publicComponentName(unknown) = %q, want \"service\"", got)
	}
}
