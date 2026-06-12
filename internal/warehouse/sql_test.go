package warehouse

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBuildSelectSQL_IncrementalKeyset(t *testing.T) {
	d, _ := resolveSource("ledger.journal_lines") // bigint watermark
	cur := cursor{hasSeq: true, seq: 42}
	sql, args := buildSelectSQL(d, cur)
	if !strings.Contains(sql, "id > $1") {
		t.Fatalf("missing bigint keyset bound: %s", sql)
	}
	if !strings.HasSuffix(sql, "ORDER BY id") {
		t.Fatalf("missing order by watermark: %s", sql)
	}
	if len(args) != 1 || args[0].(int64) != 42 {
		t.Fatalf("args = %v, want [42]", args)
	}
}

func TestBuildSelectSQL_TimestampUUIDKeyset(t *testing.T) {
	d, _ := resolveSource("ktype:sales.order")
	ts := time.Now().UTC()
	id := uuid.NewString()
	sql, args := buildSelectSQL(d, cursor{hasTime: true, ts: ts, id: id})
	// ktype filter is $1, the keyset tuple is ($2,$3).
	if !strings.Contains(sql, "(updated_at, id) > ($2, $3::uuid)") {
		t.Fatalf("missing tuple keyset bound: %s", sql)
	}
	if len(args) != 3 {
		t.Fatalf("args = %v, want 3 (ktype, ts, id)", args)
	}
}

func TestBuildSelectSQL_FullNoCursor(t *testing.T) {
	d, _ := resolveSource("ledger.stock_levels") // watermarkNone
	sql, args := buildSelectSQL(d, cursor{})
	if strings.Contains(sql, ">") {
		t.Fatalf("full read must have no keyset predicate: %s", sql)
	}
	if len(args) != 0 {
		t.Fatalf("full read must bind no args, got %v", args)
	}
	// Ordered by the PK so the read is deterministic.
	if !strings.Contains(sql, "ORDER BY item_id, warehouse_id") {
		t.Fatalf("missing pk order: %s", sql)
	}
}

func TestBuildCreateTableSQL_Sanitised(t *testing.T) {
	d, _ := resolveSource("ktype:crm.contact")
	sql := buildCreateTableSQL("kapp", "ktype_crm_contact", d)
	if !strings.HasPrefix(sql, `CREATE TABLE IF NOT EXISTS "kapp"."ktype_crm_contact"`) {
		t.Fatalf("unexpected DDL prefix: %s", sql)
	}
	if !strings.Contains(sql, `PRIMARY KEY ("id")`) {
		t.Fatalf("missing primary key: %s", sql)
	}
	if !strings.Contains(sql, `"data" jsonb`) {
		t.Fatalf("missing jsonb data column: %s", sql)
	}
}

func TestBuildUpsertSQL_MergesNonPK(t *testing.T) {
	d, _ := resolveSource("ktype:crm.contact")
	sql := buildUpsertSQL("kapp", "ktype_crm_contact", "_wh_stage_ktype_crm_contact", d)
	if !strings.Contains(sql, `ON CONFLICT ("id") DO UPDATE SET`) {
		t.Fatalf("missing upsert conflict clause: %s", sql)
	}
	if strings.Contains(sql, `"id" = EXCLUDED."id"`) {
		t.Fatalf("pk column must not be in the SET list: %s", sql)
	}
	if !strings.Contains(sql, `"data" = EXCLUDED."data"`) {
		t.Fatalf("non-pk column must be merged from EXCLUDED: %s", sql)
	}
}

func TestBuildUpsertSQL_CompositePKDoNothing(t *testing.T) {
	d, _ := resolveSource("ledger.stock_levels") // pk (item_id, warehouse_id), no non-pk besides qty
	sql := buildUpsertSQL("kapp", "ledger_stock_levels", "_wh_stage_ledger_stock_levels", d)
	if !strings.Contains(sql, `ON CONFLICT ("item_id", "warehouse_id")`) {
		t.Fatalf("missing composite conflict target: %s", sql)
	}
	// qty IS a non-pk column, so this one does update.
	if !strings.Contains(sql, `"qty" = EXCLUDED."qty"`) {
		t.Fatalf("expected qty merge: %s", sql)
	}
}
