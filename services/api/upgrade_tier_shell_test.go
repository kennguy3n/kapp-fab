package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestUpgradeTierShellTablesMatchGoList enforces the third leg of the
// lock-step invariant called out in internal/tenant/tier.go and
// scripts/upgrade_tier.sh: the bash array TABLES=(...) in
// scripts/upgrade_tier.sh must hold the same identifiers in the same
// order as tenant.TenantScopedTables.
//
// The two existing checks already cover the Go-side copies:
//
//   - TestTierUpgradeTablesMatchBackupSourceList (this package)
//     compares tenant.TenantScopedTables against
//     services/kapp-backup/main.go::TenantScopedTables.
//
//   - The dedicated-schema integration tests round-trip the full
//     slice end-to-end against PostgreSQL.
//
// Until now the bash copy in scripts/upgrade_tier.sh::TABLES has been
// maintained by hand, which was flagged in the PR #47 review as the
// remaining drift risk. This test closes that gap by parsing the
// shell file at compile-time-of-test and comparing element-by-element
// against tenant.TenantScopedTables. It runs in the default
// `go test ./...` lane (no //go:build integration), so a desync in
// either direction fails CI immediately.
func TestUpgradeTierShellTablesMatchGoList(t *testing.T) {
	const path = "../../scripts/upgrade_tier.sh"
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(body)

	const marker = "TABLES=("
	idx := strings.Index(src, marker)
	if idx == -1 {
		t.Fatalf("%s: marker %q not found; did the bash array name change?", path, marker)
	}
	rest := src[idx+len(marker):]
	end := strings.Index(rest, ")")
	if end == -1 {
		t.Fatalf("%s: closing %q for TABLES=(...) not found", path, ")")
	}
	block := rest[:end]

	// Strip line comments (`#…`) so that future operator notes inside
	// the array body don't confuse the tokenizer.
	commentRE := regexp.MustCompile(`(?m)#.*$`)
	block = commentRE.ReplaceAllString(block, "")

	// Tokenize on any whitespace. Bash arrays accept space- or
	// newline-separated identifiers, and the canonical layout in
	// scripts/upgrade_tier.sh uses both.
	var shell []string
	for _, tok := range strings.Fields(block) {
		tok = strings.Trim(tok, "'\"")
		if tok == "" {
			continue
		}
		shell = append(shell, tok)
	}

	if len(shell) == 0 {
		t.Fatalf("%s: TABLES=(...) parsed as empty slice; check parser regex", path)
	}

	// Element-by-element comparison: order matters because the FK
	// dependency walk in promote_tenant_to_schema follows array order
	// and a reorder against the Go list would mask a real bug.
	if len(shell) != len(tenant.TenantScopedTables) {
		t.Fatalf("scripts/upgrade_tier.sh::TABLES drifted from tenant.TenantScopedTables\n"+
			"shell  (%d): %s\nGo     (%d): %s",
			len(shell), strings.Join(shell, ", "),
			len(tenant.TenantScopedTables), strings.Join(tenant.TenantScopedTables, ", "),
		)
	}
	for i := range shell {
		if shell[i] != tenant.TenantScopedTables[i] {
			t.Fatalf("scripts/upgrade_tier.sh::TABLES[%d] = %q, tenant.TenantScopedTables[%d] = %q",
				i, shell[i], i, tenant.TenantScopedTables[i])
		}
	}

	// Sorted-set equality is implied by the element-wise check above,
	// but the explicit assertion makes a duplicate or missing entry
	// fail with a clearer message in CI logs.
	sortedShell := append([]string{}, shell...)
	sortedGo := append([]string{}, tenant.TenantScopedTables...)
	sort.Strings(sortedShell)
	sort.Strings(sortedGo)
	if strings.Join(sortedShell, ",") != strings.Join(sortedGo, ",") {
		t.Fatalf("set mismatch between shell TABLES and tenant.TenantScopedTables\n"+
			"shell:\n  %s\nGo:\n  %s",
			strings.Join(sortedShell, "\n  "),
			strings.Join(sortedGo, "\n  "),
		)
	}
}
