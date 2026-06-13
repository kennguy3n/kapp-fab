package warehouse

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestResolveSource_KType(t *testing.T) {
	d, err := resolveSource("ktype:crm.contact")
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if d.ktype != "crm.contact" {
		t.Fatalf("ktype = %q, want crm.contact", d.ktype)
	}
	if d.relation != "krecords" {
		t.Fatalf("relation = %q, want krecords", d.relation)
	}
	if d.wmKind != watermarkTimestampUUID {
		t.Fatalf("wmKind = %v, want timestampUUID", d.wmKind)
	}
	if got := d.destTable(); got != "ktype_crm_contact" {
		t.Fatalf("destTable = %q, want ktype_crm_contact", got)
	}
	// ktype must be a bound parameter, never interpolated into the SQL.
	// tenant_id is $1, so the ktype filter binds at $2.
	tid := uuid.New()
	sql, args := buildSelectSQL(tid, d, cursor{})
	if !strings.Contains(sql, "ktype = $2") {
		t.Fatalf("select sql does not bind ktype as a parameter: %s", sql)
	}
	if len(args) != 2 || args[0].(uuid.UUID) != tid || args[1] != "crm.contact" {
		t.Fatalf("args = %v, want [tenant, crm.contact]", args)
	}
}

func TestResolveSource_Ledger(t *testing.T) {
	for _, key := range []string{"ledger.journal_entries", "ledger.journal_lines", "ledger.stock_levels"} {
		d, err := resolveSource(key)
		if err != nil {
			t.Fatalf("resolveSource(%q): %v", key, err)
		}
		if d.key != key {
			t.Fatalf("key = %q, want %q", d.key, key)
		}
		if len(d.columns) == 0 || len(d.pk) == 0 {
			t.Fatalf("descriptor for %q missing columns/pk", key)
		}
		if !strings.HasPrefix(d.destTable(), "ledger_") {
			t.Fatalf("destTable for %q = %q, want ledger_ prefix", key, d.destTable())
		}
	}
}

func TestResolveSource_Rejects(t *testing.T) {
	bad := []string{
		"",
		"ktype:",
		"ktype:Bad.Name",      // uppercase segment
		"ktype:1bad",          // leading digit
		"ledger.gl_accounts",  // not on the allow-list
		"ledger.journal_evil", // not on the allow-list
		"raw:secrets",         // unknown family
		"external:1:t",        // external read prefix is not exportable
	}
	for _, key := range bad {
		if _, err := resolveSource(key); err == nil {
			t.Fatalf("resolveSource(%q): expected error, got nil", key)
		}
	}
}

func TestIsKTypeName_Length(t *testing.T) {
	// 63-byte cap on the assembled "ktype_<name>" identifier.
	long := "a" + strings.Repeat("b", 60) // assembled name exceeds the 63-byte cap
	if isKTypeName(long) {
		t.Fatalf("isKTypeName(%q) = true, want false (exceeds 63-byte table name)", long)
	}
	ok := strings.Repeat("a", 57) // assembled name is exactly 63 bytes
	if !isKTypeName(ok) {
		t.Fatalf("isKTypeName(57 chars) = false, want true")
	}
}

func TestIsIdentifier(t *testing.T) {
	good := []string{"kapp", "warehouse_mirror", "a", "x9_y"}
	bad := []string{"", "9lead", "Upper", "has-dash", "has space", "semi;colon", strings.Repeat("a", 64)}
	for _, s := range good {
		if !isIdentifier(s) {
			t.Errorf("isIdentifier(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isIdentifier(s) {
			t.Errorf("isIdentifier(%q) = true, want false", s)
		}
	}
}
