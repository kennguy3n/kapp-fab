package inventory

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsPartitionedInventoryMovesViolation pins the prefix+suffix
// matcher behind isInventoryMovesSourceUniqViolation and
// isInventoryMovesReversalOfUniqViolation. The shape of the matcher
// is load-bearing: it must accept the parent-level index name AND any
// partition's child index whose name follows the convention set by
// migrations/000063_manufacturing.sql
// (`inventory_moves_<partition>_<suffix>`), while rejecting unrelated
// constraint names that happen to share a fragment.
func TestIsPartitionedInventoryMovesViolation(t *testing.T) {
	type tc struct {
		name           string
		code           string
		constraintName string
		want           bool
	}

	sourceCases := []tc{
		// Parent-level name (exact match).
		{"parent source uniq", pgUniqueViolation, "inventory_moves_source_uniq", true},
		// Default partition child (the only one that exists today).
		{"default partition child", pgUniqueViolation, "inventory_moves_default_source_uniq", true},
		// Forward-looking: a yearly partition added later.
		{"yearly partition child", pgUniqueViolation, "inventory_moves_2026_source_uniq", true},
		// Forward-looking: a tenant-sharded partition.
		{"sharded partition child", pgUniqueViolation, "inventory_moves_tenant_shard3_source_uniq", true},
		// Wrong suffix — same prefix, different constraint family.
		{"wrong suffix - reversal", pgUniqueViolation, "inventory_moves_default_reversal_of_uniq", false},
		// Wrong prefix — different table entirely.
		{"wrong prefix", pgUniqueViolation, "other_table_source_uniq", false},
		// Not a unique-violation code.
		{"non-unique violation", "23502", "inventory_moves_source_uniq", false},
		// Empty constraint name.
		{"empty constraint", pgUniqueViolation, "", false},
	}
	for _, c := range sourceCases {
		t.Run("source/"+c.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: c.code, ConstraintName: c.constraintName}
			if got := isInventoryMovesSourceUniqViolation(pgErr); got != c.want {
				t.Fatalf("isInventoryMovesSourceUniqViolation(%q, code=%s) = %v, want %v",
					c.constraintName, c.code, got, c.want)
			}
		})
	}

	reversalCases := []tc{
		{"parent reversal uniq", pgUniqueViolation, "inventory_moves_reversal_of_uniq", true},
		{"default partition child", pgUniqueViolation, "inventory_moves_default_reversal_of_uniq", true},
		{"yearly partition child", pgUniqueViolation, "inventory_moves_2026_reversal_of_uniq", true},
		// Wrong suffix — should not flip the reversal matcher just
		// because the prefix happens to match.
		{"wrong suffix - source", pgUniqueViolation, "inventory_moves_default_source_uniq", false},
		{"wrong prefix", pgUniqueViolation, "other_table_reversal_of_uniq", false},
		{"non-unique violation", "23502", "inventory_moves_reversal_of_uniq", false},
	}
	for _, c := range reversalCases {
		t.Run("reversal/"+c.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: c.code, ConstraintName: c.constraintName}
			if got := isInventoryMovesReversalOfUniqViolation(pgErr); got != c.want {
				t.Fatalf("isInventoryMovesReversalOfUniqViolation(%q, code=%s) = %v, want %v",
					c.constraintName, c.code, got, c.want)
			}
		})
	}

	// Nil error must never panic and must return false on both
	// matchers — the call sites assume it's safe to pass any
	// *pgconn.PgError including the zero-value case.
	if isInventoryMovesSourceUniqViolation(nil) {
		t.Fatal("isInventoryMovesSourceUniqViolation(nil) = true, want false")
	}
	if isInventoryMovesReversalOfUniqViolation(nil) {
		t.Fatal("isInventoryMovesReversalOfUniqViolation(nil) = true, want false")
	}
}
