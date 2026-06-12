package warehouse

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursorRoundTrip_TimestampUUID(t *testing.T) {
	d, _ := resolveSource("ktype:crm.contact")
	id := uuid.New()
	ts := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	wm := runningWatermark{ts: ts, id: id.String()}
	raw := wm.serialize(d)
	if raw == nil {
		t.Fatal("serialize returned nil for timestampUUID source")
	}
	cur, err := parseCursor(d, raw)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if !cur.hasTime || !cur.ts.Equal(ts) || cur.id != id.String() {
		t.Fatalf("round trip mismatch: %+v", cur)
	}
}

func TestCursorRoundTrip_Bigint(t *testing.T) {
	d, _ := resolveSource("ledger.journal_lines")
	wm := runningWatermark{seq: 9001}
	raw := wm.serialize(d)
	cur, err := parseCursor(d, raw)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if !cur.hasSeq || cur.seq != 9001 {
		t.Fatalf("round trip mismatch: %+v", cur)
	}
}

func TestParseCursor_EmptyIsZero(t *testing.T) {
	d, _ := resolveSource("ktype:crm.contact")
	cur, err := parseCursor(d, nil)
	if err != nil {
		t.Fatalf("parseCursor(nil): %v", err)
	}
	if cur.hasTime || cur.hasSeq {
		t.Fatalf("empty watermark must yield zero cursor, got %+v", cur)
	}
}

func TestParseCursor_RejectsBadUUID(t *testing.T) {
	d, _ := resolveSource("ktype:crm.contact")
	if _, err := parseCursor(d, json.RawMessage(`{"ts":"2025-01-01T00:00:00Z","id":"not-a-uuid"}`)); err == nil {
		t.Fatal("expected error for malformed watermark uuid")
	}
}

func TestWatermarkUpdate_TracksLastRow(t *testing.T) {
	d, _ := resolveSource("ledger.journal_lines") // bigint, wmBigCol "id" at index 0
	src := &copyFromSource{desc: d}
	src.ensureIdx()
	// id is the first projected column.
	src.watermark.update(d, []any{int64(5)}, src.tsIdx, src.idIdx, src.seqIdx)
	src.watermark.update(d, []any{int64(17)}, src.tsIdx, src.idIdx, src.seqIdx)
	if src.watermark.seq != 17 {
		t.Fatalf("watermark.seq = %d, want 17 (last row)", src.watermark.seq)
	}
}

func TestNoneWatermark_SerializesNil(t *testing.T) {
	d, _ := resolveSource("ledger.stock_levels")
	wm := runningWatermark{}
	if raw := wm.serialize(d); raw != nil {
		t.Fatalf("watermarkNone must serialize to nil, got %s", raw)
	}
}
