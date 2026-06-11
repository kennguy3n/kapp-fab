package bankfeed

import (
	"sort"
	"testing"
)

func TestBuildRegistryCSVAlwaysPresent(t *testing.T) {
	r := BuildRegistry(RegistryConfig{})
	if _, err := r.Get(ProviderCSV); err != nil {
		t.Fatalf("csv provider should always be present: %v", err)
	}
	if _, err := r.Get(ProviderPlaid); err == nil {
		t.Fatal("plaid should be absent when unconfigured")
	}
	if _, err := r.Get(ProviderGoCardless); err == nil {
		t.Fatal("gocardless should be absent when unconfigured")
	}
}

func TestBuildRegistryRegistersConfiguredProviders(t *testing.T) {
	r := BuildRegistry(RegistryConfig{
		PlaidClientID:       "id",
		PlaidSecret:         "sec",
		PlaidEnv:            "sandbox",
		GoCardlessSecretID:  "gid",
		GoCardlessSecretKey: "gkey",
	})
	names := r.Names()
	sort.Strings(names)
	want := []string{ProviderCSV, ProviderGoCardless, ProviderPlaid}
	sort.Strings(want)
	if len(names) != len(want) {
		t.Fatalf("names = %v; want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v; want %v", names, want)
		}
	}
}

func TestRegistryGetUnknown(t *testing.T) {
	var r *Registry
	if _, err := r.Get("anything"); err == nil {
		t.Fatal("nil registry Get should error")
	}
	r2 := NewRegistry(NewCSVProvider())
	if _, err := r2.Get("missing"); err == nil {
		t.Fatal("unknown provider should error")
	}
}
