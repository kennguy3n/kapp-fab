// Package warehouse implements Workstream 4 — the reporting →
// warehouse/BI export bridge. Where internal/insights reads FROM
// external Postgres warehouses, this package writes a tenant's data
// INTO one: a scheduled, incremental mirror of selected sources
// (krecords KTypes and the allowed ledger.* relations) into a
// destination schema the tenant's own BI tool then queries.
//
// The destination is not a new connection type: it reuses the
// insights external-datasource model (encrypted connection string +
// per-tenant pool cache). The export reads every source under tenant
// RLS via the same typed-table / krecords access the reporting engine
// uses, and writes through bounded, parameterised COPY + upsert so an
// untrusted identifier never reaches a SQL string.
package warehouse

import (
	"fmt"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/reporting"
)

// watermarkKind classifies how a source supports incremental export.
//
//   - watermarkNone: the source has no stable per-row cursor (e.g. an
//     aggregate view). Such a source is always exported in full —
//     incremental mode degrades to a full table replace for it.
//   - watermarkTimestampUUID: rows carry a monotonic timestamp plus a
//     UUID tiebreaker (krecords.updated_at/id, journal_entries.
//     created_at/id). The keyset walks (ts, id) ascending and the
//     stored cursor is the last (ts, id) that landed.
//   - watermarkBigint: rows carry a monotonic BIGINT id
//     (journal_lines.id). The keyset walks id ascending.
type watermarkKind int

const (
	watermarkNone watermarkKind = iota
	watermarkTimestampUUID
	watermarkBigint
)

// column is one projected column of a source: the source-side SQL
// expression (always a fixed, code-defined identifier — never user
// input) and the destination column name + Postgres type the mirror
// table is created with.
type column struct {
	src  string
	dest string
	typ  string
}

// sourceDescriptor is the resolved, code-defined shape of one
// exportable source. Every field is derived from a closed allow-list,
// so nothing here is attacker-controlled: the only tenant input is the
// source KEY, which is matched against the allow-list before a
// descriptor is produced. ktype is non-empty for krecords sources and
// is bound as a query PARAMETER (never interpolated).
type sourceDescriptor struct {
	key       string
	relation  string
	ktype     string
	columns   []column
	pk        []string
	wmKind    watermarkKind
	wmTimeCol string
	wmUUIDCol string
	wmBigCol  string
}

// destTable is the sanitised destination table name for the source.
// Dots in a KType name (e.g. "crm.contact") collapse to underscores so
// the result is a bare SQL identifier; the "ktype_" / "ledger_" prefix
// keeps the two source families from colliding and namespaces the
// mirror inside the destination schema.
func (d sourceDescriptor) destTable() string {
	switch {
	case d.ktype != "":
		return "ktype_" + strings.ReplaceAll(d.ktype, ".", "_")
	default:
		return "ledger_" + d.relation
	}
}

// destColumns returns the ordered destination column names — the COPY
// / upsert column list. Kept in one place so the reader projection and
// the writer DDL never drift.
func (d sourceDescriptor) destColumns() []string {
	out := make([]string, len(d.columns))
	for i, c := range d.columns {
		out[i] = c.dest
	}
	return out
}

// ktypeColumns is the krecords envelope projected for every KType
// source. The heterogeneous business fields live in `data` (JSONB) —
// a per-field column layout is impossible across KTypes and a BI tool
// on Postgres expands JSONB natively — so the mirror preserves the
// full document plus the lifecycle envelope. status is the krecords
// lifecycle flag ('active'/'deleted'); soft-deleted rows are mirrored
// (not filtered) so a delete propagates to the warehouse as a row
// whose deleted_at is set.
var ktypeColumns = []column{
	{src: "id", dest: "id", typ: "uuid"},
	{src: "ktype", dest: "ktype", typ: "text"},
	{src: "ktype_version", dest: "ktype_version", typ: "integer"},
	{src: "status", dest: "status", typ: "text"},
	{src: "version", dest: "version", typ: "integer"},
	{src: "data", dest: "data", typ: "jsonb"},
	{src: "created_by", dest: "created_by", typ: "uuid"},
	{src: "created_at", dest: "created_at", typ: "timestamptz"},
	{src: "updated_by", dest: "updated_by", typ: "uuid"},
	{src: "updated_at", dest: "updated_at", typ: "timestamptz"},
	{src: "deleted_at", dest: "deleted_at", typ: "timestamptz"},
}

// ledgerDescriptors holds the static, typed projection for each
// allowed ledger.* source. The set of keys is kept in lock-step with
// reporting's allow-list via reporting.IsAllowedLedgerSource (checked
// in resolveSource) so the export can never reach a ledger relation
// the reporting engine would itself refuse.
var ledgerDescriptors = map[string]sourceDescriptor{
	"ledger.journal_entries": {
		key:      "ledger.journal_entries",
		relation: "journal_entries",
		columns: []column{
			{src: "id", dest: "id", typ: "uuid"},
			{src: "posted_at", dest: "posted_at", typ: "timestamptz"},
			{src: "memo", dest: "memo", typ: "text"},
			{src: "source_ktype", dest: "source_ktype", typ: "text"},
			{src: "source_id", dest: "source_id", typ: "uuid"},
			{src: "created_by", dest: "created_by", typ: "uuid"},
			{src: "created_at", dest: "created_at", typ: "timestamptz"},
		},
		pk:        []string{"id"},
		wmKind:    watermarkTimestampUUID,
		wmTimeCol: "created_at",
		wmUUIDCol: "id",
	},
	"ledger.journal_lines": {
		key:      "ledger.journal_lines",
		relation: "journal_lines",
		columns: []column{
			{src: "id", dest: "id", typ: "bigint"},
			{src: "entry_id", dest: "entry_id", typ: "uuid"},
			{src: "account_code", dest: "account_code", typ: "text"},
			{src: "debit", dest: "debit", typ: "numeric(20,4)"},
			{src: "credit", dest: "credit", typ: "numeric(20,4)"},
			{src: "currency", dest: "currency", typ: "text"},
			{src: "memo", dest: "memo", typ: "text"},
		},
		pk:       []string{"id"},
		wmKind:   watermarkBigint,
		wmBigCol: "id",
	},
	"ledger.stock_levels": {
		key:      "ledger.stock_levels",
		relation: "stock_levels",
		columns: []column{
			{src: "item_id", dest: "item_id", typ: "uuid"},
			{src: "warehouse_id", dest: "warehouse_id", typ: "uuid"},
			{src: "qty", dest: "qty", typ: "numeric(20,4)"},
		},
		pk:     []string{"item_id", "warehouse_id"},
		wmKind: watermarkNone,
	},
}

// resolveSource turns a tenant-supplied source key into the
// code-defined descriptor, or an error if the key is not exportable.
// KType sources accept any validly-named KType (the name is bound as a
// parameter, never interpolated); ledger sources must clear
// reporting's allow-list AND have a descriptor here.
func resolveSource(key string) (sourceDescriptor, error) {
	if strings.HasPrefix(key, reporting.SourceKTypePrefix) {
		name := strings.TrimPrefix(key, reporting.SourceKTypePrefix)
		if !isKTypeName(name) {
			return sourceDescriptor{}, fmt.Errorf("warehouse: invalid ktype name %q", name)
		}
		d := sourceDescriptor{
			key:       key,
			relation:  "krecords",
			ktype:     name,
			columns:   ktypeColumns,
			pk:        []string{"id"},
			wmKind:    watermarkTimestampUUID,
			wmTimeCol: "updated_at",
			wmUUIDCol: "id",
		}
		return d, nil
	}
	if strings.HasPrefix(key, reporting.SourceLedger) {
		if !reporting.IsAllowedLedgerSource(key) {
			return sourceDescriptor{}, fmt.Errorf("warehouse: unsupported ledger source %q", key)
		}
		d, ok := ledgerDescriptors[key]
		if !ok {
			return sourceDescriptor{}, fmt.Errorf("warehouse: no descriptor for ledger source %q", key)
		}
		return d, nil
	}
	return sourceDescriptor{}, fmt.Errorf("warehouse: unsupported source %q", key)
}

// isKTypeName accepts the dotted KType naming convention
// ("crm.contact", "sales.order", "custom.widget", "test_ktype"): one
// or more dot-separated segments, each starting with a lowercase
// letter and continuing in [a-z0-9_]. The full name must fit a SQL
// identifier once dots collapse to underscores and the "ktype_" prefix
// is added, so the assembled destination table stays <= 63 bytes.
func isKTypeName(s string) bool {
	if s == "" {
		return false
	}
	if len("ktype_")+len(strings.ReplaceAll(s, ".", "_")) > 63 {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if !isSegment(seg) {
			return false
		}
	}
	return true
}

func isSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for i, c := range seg {
		switch {
		case c >= 'a' && c <= 'z':
		case c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// isIdentifier guards the few identifiers the export DOES interpolate
// into DDL (the destination schema name and assembled table names).
// Mirrors reporting.isIdentifier / tenant.IsSafeIdentifier: a bare
// [a-z_][a-z0-9_]* token of at most 63 bytes. Values still flow as
// parameters; this is the identifier-safety net.
func isIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
