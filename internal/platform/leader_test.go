package platform

import "testing"

// TestLeaderElection_LockKeyDeterministic pins the contract that a
// given namespace string ALWAYS maps to the same int64 advisory-lock
// key. Two LeaderElection values configured with the same namespace
// must contest the same lock, and a value's lock key must survive
// process restart so a freshly-elected leader claims the same lock
// the previous leader released.
//
// If this test starts failing the hash function has changed silently,
// which would cause a rolling deploy to elect two leaders concurrently
// (old replicas hashing to key A, new replicas hashing to key B) — a
// correctness incident worse than no leader election.
func TestLeaderElection_LockKeyDeterministic(t *testing.T) {
	cases := []struct {
		namespace string
	}{
		{"outbox-drain"},
		{"export-queue"},
		{"autoscaler"},
		{""},
		{"namespace with spaces and / slashes"},
		{"long-namespace-" + "abcdefghijklmnopqrstuvwxyz" + "0123456789"},
	}
	for _, tc := range cases {
		t.Run(tc.namespace, func(t *testing.T) {
			a := NewLeaderElection("", tc.namespace, "test-a").LockKey()
			b := NewLeaderElection("", tc.namespace, "test-b").LockKey()
			if a != b {
				t.Fatalf("two LeaderElection values for namespace %q produced different lock keys: %d vs %d", tc.namespace, a, b)
			}
			if a != keyForNamespace(tc.namespace) {
				t.Fatalf("LeaderElection.LockKey diverges from keyForNamespace for %q: %d vs %d", tc.namespace, a, keyForNamespace(tc.namespace))
			}
		})
	}
}

// TestLeaderElection_LockKeyDistinct ensures distinct namespaces map
// to distinct lock keys. Without this guarantee, "outbox-drain" and
// "export-queue" would race on the same advisory lock and starve
// each other.
//
// FNV-64a's collision rate on short ASCII strings is astronomically
// low, so this test exercises the common cases (different namespaces
// the worker actually uses) rather than the abstract collision
// resistance.
func TestLeaderElection_LockKeyDistinct(t *testing.T) {
	keys := map[int64]string{}
	namespaces := []string{
		"outbox-drain",
		"export-queue",
		"autoscaler",
		"scheduler",
		"webhook-retry",
	}
	for _, ns := range namespaces {
		k := NewLeaderElection("", ns, "test").LockKey()
		if other, ok := keys[k]; ok {
			t.Fatalf("namespaces %q and %q hash to the same lock key %d", ns, other, k)
		}
		keys[k] = ns
	}
}

// TestLeaderElection_Builders pins the With* builder contracts: zero
// or negative durations are ignored (defaults stay in place), positive
// durations override.
func TestLeaderElection_Builders(t *testing.T) {
	le := NewLeaderElection("", "ns", "id")
	origPoll := le.pollInterval
	origHeartbeat := le.heartbeat

	if got := le.WithPollInterval(0); got.pollInterval != origPoll {
		t.Fatalf("WithPollInterval(0) should be a no-op; got %s want %s", got.pollInterval, origPoll)
	}
	if got := le.WithPollInterval(-1); got.pollInterval != origPoll {
		t.Fatalf("WithPollInterval(-1) should be a no-op; got %s want %s", got.pollInterval, origPoll)
	}
	if got := le.WithHeartbeat(0); got.heartbeat != origHeartbeat {
		t.Fatalf("WithHeartbeat(0) should be a no-op; got %s want %s", got.heartbeat, origHeartbeat)
	}

	// Positive values override.
	le2 := NewLeaderElection("", "ns", "id").WithPollInterval(1).WithHeartbeat(2)
	if le2.pollInterval != 1 {
		t.Fatalf("WithPollInterval(1) did not override; got %s", le2.pollInterval)
	}
	if le2.heartbeat != 2 {
		t.Fatalf("WithHeartbeat(2) did not override; got %s", le2.heartbeat)
	}
}
