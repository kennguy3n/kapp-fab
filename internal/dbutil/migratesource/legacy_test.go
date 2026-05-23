package migratesource

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a tiny helper that creates files inside a temporary
// migrations directory so each test case can assemble its own layout.
func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("writeFile %s: %v", name, err)
	}
}

func TestNewFromDir_LegacyUpOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.sql", "SELECT 1;")
	writeFile(t, dir, "000002_extend.sql", "SELECT 2;")

	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	if err := src.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	versions := src.Versions()
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("Versions=%v want [1 2]", versions)
	}
	if src.HasDown(1) || src.HasDown(2) {
		t.Fatalf("legacy files should not advertise a down direction")
	}
}

func TestNewFromDir_UpDownPair(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.up.sql", "CREATE TABLE t (id int);")
	writeFile(t, dir, "000001_init.down.sql", "DROP TABLE t;")
	writeFile(t, dir, "000002_extend.sql", "ALTER TABLE t ADD COLUMN n int;")

	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	if !src.HasDown(1) {
		t.Fatalf("version 1 should expose a down companion")
	}
	if src.HasDown(2) {
		t.Fatalf("version 2 is up-only; HasDown must report false")
	}

	// ReadUp returns the up body.
	rUp, name, err := src.ReadUp(1)
	if err != nil {
		t.Fatalf("ReadUp: %v", err)
	}
	defer rUp.Close()
	if name != "init" {
		t.Fatalf("identifier=%q want %q", name, "init")
	}

	// ReadDown returns the down body for the matched companion.
	rDown, _, err := src.ReadDown(1)
	if err != nil {
		t.Fatalf("ReadDown(1): %v", err)
	}
	defer rDown.Close()

	// ReadDown on the up-only migration returns ErrNotExist.
	if _, _, err := src.ReadDown(2); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadDown(2) err=%v want ErrNotExist", err)
	}
}

func TestNewFromDir_RejectsDuplicateVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.sql", "SELECT 1;")
	writeFile(t, dir, "000001_other.sql", "SELECT 2;")

	if _, err := NewFromDir(dir); err == nil {
		t.Fatalf("expected duplicate-version error, got nil")
	} else if !strings.Contains(err.Error(), "conflicting names") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewFromDir_RejectsDuplicateUpFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.sql", "SELECT 1;")
	writeFile(t, dir, "000001_init.up.sql", "SELECT 2;")

	if _, err := NewFromDir(dir); err == nil {
		t.Fatalf("expected duplicate-up error, got nil")
	} else if !strings.Contains(err.Error(), "duplicate up files") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewFromDir_RejectsDownWithoutUp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.down.sql", "DROP TABLE t;")

	if _, err := NewFromDir(dir); err == nil {
		t.Fatalf("expected down-without-up error, got nil")
	} else if !strings.Contains(err.Error(), "no up file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewFromDir_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.sql", "SELECT 1;")
	writeFile(t, dir, "README.md", "# notes")
	writeFile(t, dir, "000001_extra.txt", "not sql")
	writeFile(t, dir, ".hidden", "ignored")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	if got := src.Versions(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Versions=%v want [1]", got)
	}
}

func TestValidate_RejectsGap(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.sql", "SELECT 1;")
	writeFile(t, dir, "000003_extend.sql", "SELECT 3;")

	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	if err := src.Validate(); err == nil {
		t.Fatalf("expected gap error, got nil")
	} else if !strings.Contains(err.Error(), "non-monotonic") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_RejectsStartNotAtOne(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000002_init.sql", "SELECT 1;")

	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	if err := src.Validate(); err == nil {
		t.Fatalf("expected start-at-001 error, got nil")
	} else if !strings.Contains(err.Error(), "start at 000001") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_RejectsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.sql", "SELECT 1;")
	// Remove the only registered migration to simulate a directory
	// scan that returned no migration files.  We can't use a
	// truly-empty dir because NewFromDir already errors there.
	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	// Empty the internal state to drive the Validate path.
	src.files = map[uint]fileEntry{}
	if err := src.Validate(); err == nil {
		t.Fatalf("expected empty-set error, got nil")
	}
}

func TestFirstNextPrev(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_a.sql", "SELECT 1;")
	writeFile(t, dir, "000002_b.sql", "SELECT 2;")
	writeFile(t, dir, "000003_c.sql", "SELECT 3;")

	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	v, err := src.First()
	if err != nil || v != 1 {
		t.Fatalf("First: v=%d err=%v", v, err)
	}
	v, err = src.Next(1)
	if err != nil || v != 2 {
		t.Fatalf("Next(1): v=%d err=%v", v, err)
	}
	v, err = src.Prev(3)
	if err != nil || v != 2 {
		t.Fatalf("Prev(3): v=%d err=%v", v, err)
	}
	if _, err := src.Next(3); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Next(3) should be ErrNotExist, got %v", err)
	}
	if _, err := src.Prev(1); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Prev(1) should be ErrNotExist, got %v", err)
	}
}

func TestOpenURL(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_init.sql", "SELECT 1;")
	src := &LegacySource{}
	d, err := src.Open("legacy://" + dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := d.First(); err != nil {
		t.Fatalf("driver First: %v", err)
	}
}

// TestHighest exercises both the empty and populated paths so the
// helper's no-data branch is covered.
func TestHighest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_a.sql", "SELECT 1;")
	writeFile(t, dir, "000002_b.sql", "SELECT 2;")

	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	if h := src.Highest(); h != 2 {
		t.Fatalf("Highest=%d want 2", h)
	}
	src.files = map[uint]fileEntry{}
	if h := src.Highest(); h != 0 {
		t.Fatalf("Highest on empty=%d want 0", h)
	}
}

// TestStringContainsAllVersions makes sure the String() debug helper
// surfaces every registered version so operators get a usable summary
// from `migrate version` when the DB is unreachable.
func TestStringContainsAllVersions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "000001_alpha.sql", "SELECT 1;")
	writeFile(t, dir, "000002_beta.up.sql", "SELECT 2;")
	writeFile(t, dir, "000002_beta.down.sql", "DROP TABLE beta;")

	src, err := NewFromDir(dir)
	if err != nil {
		t.Fatalf("NewFromDir: %v", err)
	}
	s := src.String()
	for _, want := range []string{"000001_alpha", "000002_beta", "up-only", "up+down"} {
		if !strings.Contains(s, want) {
			t.Fatalf("String() missing %q in %q", want, s)
		}
	}
}
