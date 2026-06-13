package warehouse

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// cursor is the decoded incremental lower bound for a source read.
// Only the fields relevant to the source's watermark kind are set:
// (ts,id) for watermarkTimestampUUID, seq for watermarkBigint.
type cursor struct {
	hasTime bool
	ts      time.Time
	id      string

	hasSeq bool
	seq    int64
}

// tsCursor / seqCursor are the JSON shapes persisted in
// warehouse_sync_configs.watermarks for each watermark kind. Keeping
// them as named types documents the on-disk contract and keeps the
// parse/serialize sides in lock-step.
type tsCursor struct {
	TS time.Time `json:"ts"`
	ID string    `json:"id"`
}

type seqCursor struct {
	Seq int64 `json:"seq"`
}

// parseCursor decodes a stored watermark into a cursor for the given
// source. An absent (nil/empty) watermark yields the zero cursor — the
// signal to read from the beginning.
func parseCursor(d sourceDescriptor, raw json.RawMessage) (cursor, error) {
	if len(raw) == 0 {
		return cursor{}, nil
	}
	switch d.wmKind {
	case watermarkTimestampUUID:
		var tc tsCursor
		if err := json.Unmarshal(raw, &tc); err != nil {
			return cursor{}, fmt.Errorf("warehouse: decode watermark for %q: %w", d.key, err)
		}
		if tc.ID == "" {
			return cursor{}, nil
		}
		if _, err := uuid.Parse(tc.ID); err != nil {
			return cursor{}, fmt.Errorf("warehouse: invalid watermark id for %q: %w", d.key, err)
		}
		return cursor{hasTime: true, ts: tc.TS, id: tc.ID}, nil
	case watermarkBigint:
		var sc seqCursor
		if err := json.Unmarshal(raw, &sc); err != nil {
			return cursor{}, fmt.Errorf("warehouse: decode watermark for %q: %w", d.key, err)
		}
		return cursor{hasSeq: true, seq: sc.Seq}, nil
	default:
		return cursor{}, nil
	}
}

// runningWatermark accumulates the maximum order key seen while
// streaming a source. Because the source read is ORDER BY the key
// ascending, the last row observed carries the new high-water mark.
type runningWatermark struct {
	ts  time.Time
	id  string
	seq int64
}

// update folds one streamed row's order-key columns into the running
// watermark. vals is the row in descriptor-column order; the relevant
// indices are precomputed on the copyFromSource.
func (w *runningWatermark) update(d sourceDescriptor, vals []any, tsIdx, idIdx, seqIdx int) {
	switch d.wmKind {
	case watermarkTimestampUUID:
		if tsIdx >= 0 && tsIdx < len(vals) {
			if t, ok := vals[tsIdx].(time.Time); ok {
				w.ts = t
			}
		}
		if idIdx >= 0 && idIdx < len(vals) {
			if s, ok := uuidString(vals[idIdx]); ok {
				w.id = s
			}
		}
	case watermarkBigint:
		if seqIdx >= 0 && seqIdx < len(vals) {
			if n, ok := vals[seqIdx].(int64); ok {
				w.seq = n
			}
		}
	}
}

// uuidString extracts the canonical UUID text from whatever concrete
// type the pgx codec decoded the id column into. pgx v5's default UUID
// codec yields [16]byte, but a registered codec extension (e.g.
// pgx-google-uuid) yields uuid.UUID, and a text-mode path yields a
// string. Handling all three means a future codec swap cannot silently
// stall watermark advance — which would otherwise force every
// incremental run to re-read the whole relation. ok is false only for
// a genuinely unexpected type, leaving the prior high-water mark
// untouched.
func uuidString(v any) (string, bool) {
	switch x := v.(type) {
	case [16]byte:
		return uuid.UUID(x).String(), true
	case uuid.UUID:
		return x.String(), true
	case string:
		if _, err := uuid.Parse(x); err != nil {
			return "", false
		}
		return x, true
	case []byte:
		if u, err := uuid.FromBytes(x); err == nil {
			return u.String(), true
		}
		if u, err := uuid.Parse(string(x)); err == nil {
			return u.String(), true
		}
		return "", false
	default:
		return "", false
	}
}

// serialize renders the running watermark into the persisted JSON
// shape, or nil for a source kind that has no incremental cursor.
func (w *runningWatermark) serialize(d sourceDescriptor) json.RawMessage {
	switch d.wmKind {
	case watermarkTimestampUUID:
		b, _ := json.Marshal(tsCursor{TS: w.ts, ID: w.id})
		return b
	case watermarkBigint:
		b, _ := json.Marshal(seqCursor{Seq: w.seq})
		return b
	default:
		return nil
	}
}

// copyFromSource adapts an open source-row cursor to pgx's
// CopyFromSource so the destination COPY pulls rows one at a time
// straight off the source connection — bounded memory regardless of
// relation size. It also counts rows and tracks the advancing
// watermark as a side effect of feeding each row.
type copyFromSource struct {
	rows pgx.Rows
	desc sourceDescriptor

	tsIdx  int
	idIdx  int
	seqIdx int
	inited bool

	count     int64
	sawRow    bool
	watermark runningWatermark
	err       error
}

func (s *copyFromSource) ensureIdx() {
	if s.inited {
		return
	}
	s.tsIdx = s.desc.colIndex(s.desc.wmTimeCol)
	s.idIdx = s.desc.colIndex(s.desc.wmUUIDCol)
	s.seqIdx = s.desc.colIndex(s.desc.wmBigCol)
	s.inited = true
}

// Next advances to the next source row; it satisfies pgx.CopyFromSource.
func (s *copyFromSource) Next() bool { return s.rows.Next() }

// Values returns the current row's column values and, as a side
// effect, counts the row and advances the running watermark.
func (s *copyFromSource) Values() ([]any, error) {
	vals, err := s.rows.Values()
	if err != nil {
		s.err = err
		return nil, err
	}
	s.ensureIdx()
	s.count++
	s.sawRow = true
	s.watermark.update(s.desc, vals, s.tsIdx, s.idIdx, s.seqIdx)
	return vals, nil
}

// Err reports the first error seen while streaming, preferring a
// decode error captured in Values over the cursor's own error.
func (s *copyFromSource) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.rows.Err()
}

// colIndex returns the position of the source column named src in the
// descriptor's projection, or -1 if absent (e.g. an empty wm column
// name for a source with no cursor).
func (d sourceDescriptor) colIndex(src string) int {
	if src == "" {
		return -1
	}
	for i, c := range d.columns {
		if c.src == src {
			return i
		}
	}
	return -1
}
