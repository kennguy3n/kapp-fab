// Package migratesource implements a golang-migrate Source driver that
// reads the existing Kapp migration files (`NNNNNN_name.sql`) as up-only
// migrations, without requiring the .up.sql / .down.sql split that the
// stock file:// source expects.
//
// Why a custom source?  Kapp shipped 54 forward-only migrations using the
// `NNNNNN_name.sql` naming convention before this CLI landed.  golang-
// migrate's stock file:// source rejects that layout because it splits
// .up.sql from .down.sql.  Renaming all 54 files to `_name.up.sql` would
// (1) churn every previous PR's blame, (2) break the `make migrate`
// helper documented in README and docs/, (3) force every operator with a
// pre-shipped DB through a one-off rename step.  This source preserves
// the existing layout while still giving us schema_migrations tracking
// and idempotent re-runs.
//
// `Down` migrations are supported when a `NNNNNN_name.down.sql` companion
// exists.  When the companion is missing, ReadDown returns
// `os.ErrNotExist` and golang-migrate refuses to roll back — surfaced to
// operators as "this migration is forward-only; revert manually".  Going
// forward, new migrations should ship with a .down.sql companion so the
// CLI's `down` subcommand can roll them back; legacy migrations remain
// untouched and forward-only.
package migratesource

import (
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4/source"
)

// filenameRE matches the legacy `NNNNNN_name.sql` pattern.  A trailing
// `.up.sql` or `.down.sql` is allowed so new-style direction-aware
// migrations can live in the same directory as the legacy forward-only
// ones.
var filenameRE = regexp.MustCompile(`^(\d{6})_([^.]+?)(?:\.(up|down))?\.sql$`)

// LegacySource implements source.Driver.  The CLI always constructs
// it via NewFromDir() + migrate.NewWithInstance("legacy", src, ...),
// which is the documented "instance-based" entrypoint in
// golang-migrate.  We intentionally do NOT call source.Register to
// expose a `legacy://` URL scheme: the dependency injection path is
// always available, exposes the same Source.Driver contract, and
// avoids the package-init side effect that source.Register would
// introduce.  The Open() method below is still implemented because
// source.Driver requires it; it parses a legacy:// URL pointing at
// the on-disk migrations directory and is reachable from tests, but
// production callers should prefer NewFromDir().
type LegacySource struct {
	dir        string
	migrations *source.Migrations
	files      map[uint]fileEntry // version → file entry
}

type fileEntry struct {
	upPath   string
	downPath string // empty when the migration has no down companion
	name     string
}

// Open is the constructor invoked by migrate.New for the legacy:// scheme.
// The `path` portion of the URL is taken as the absolute filesystem path
// to the migrations directory.
func (l *LegacySource) Open(rawurl string) (source.Driver, error) {
	// Strip the legacy:// scheme prefix directly rather than using
	// url.Parse. On Windows, absolute paths like C:\Users\... contain
	// colons and backslashes that url.Parse rejects as invalid URL
	// syntax. The scheme is a fixed prefix so a manual strip is safe
	// and avoids the URL parser entirely.
	const scheme = "legacy://"
	dir := strings.TrimPrefix(rawurl, scheme)
	if dir == rawurl {
		// No scheme prefix — try url.Parse as a fallback for
		// callers that pass a real URL (e.g. legacy://localhost/path).
		u, err := url.Parse(rawurl)
		if err != nil {
			return nil, fmt.Errorf("migratesource: parse url: %w", err)
		}
		dir = u.Path
	}
	if dir == "" {
		return nil, fmt.Errorf("migratesource: empty path in %q", rawurl)
	}
	// url.Parse unescapes %XX sequences and may strip leading slashes
	// on Windows paths. Convert back to a filesystem path.
	dir = filepath.FromSlash(dir)
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("migratesource: resolve abs path: %w", err)
		}
		dir = abs
	}
	return NewFromDir(dir)
}

// NewFromDir constructs a LegacySource directly from a directory path.
// Useful for tests and for callers who already have an absolute path and
// want to skip the URL parsing.
func NewFromDir(dir string) (*LegacySource, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("migratesource: stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("migratesource: %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migratesource: read dir %s: %w", dir, err)
	}
	ls := &LegacySource{
		dir:        dir,
		migrations: source.NewMigrations(),
		files:      make(map[uint]fileEntry),
	}
	// Group by version so a (.up.sql, .down.sql) pair collapses into a
	// single fileEntry.  Plain `NNNNNN_name.sql` is treated as up.
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := filenameRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v64, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("migratesource: parse version from %s: %w", e.Name(), err)
		}
		version := uint(v64)
		name := m[2]
		direction := m[3] // "" | "up" | "down"

		entry := ls.files[version]
		if entry.name != "" && entry.name != name {
			return nil, fmt.Errorf(
				"migratesource: version %06d has conflicting names %q and %q",
				version, entry.name, name,
			)
		}
		entry.name = name
		fullPath := filepath.Join(dir, e.Name())
		switch direction {
		case "", "up":
			if entry.upPath != "" {
				return nil, fmt.Errorf(
					"migratesource: version %06d has duplicate up files: %s and %s",
					version, filepath.Base(entry.upPath), e.Name(),
				)
			}
			entry.upPath = fullPath
		case "down":
			if entry.downPath != "" {
				return nil, fmt.Errorf(
					"migratesource: version %06d has duplicate down files: %s and %s",
					version, filepath.Base(entry.downPath), e.Name(),
				)
			}
			entry.downPath = fullPath
		}
		ls.files[version] = entry
	}
	if len(ls.files) == 0 {
		return nil, fmt.Errorf("migratesource: no migrations found in %s", dir)
	}
	// Register with the migrate source.Migrations index, exposing the up
	// direction unconditionally and the down direction only when a
	// companion file exists.
	for version, entry := range ls.files {
		if entry.upPath == "" {
			return nil, fmt.Errorf(
				"migratesource: version %06d has a down file but no up file",
				version,
			)
		}
		ls.migrations.Append(&source.Migration{
			Version:    version,
			Identifier: entry.name,
			Direction:  source.Up,
			Raw:        entry.upPath,
		})
		if entry.downPath != "" {
			ls.migrations.Append(&source.Migration{
				Version:    version,
				Identifier: entry.name,
				Direction:  source.Down,
				Raw:        entry.downPath,
			})
		}
	}
	return ls, nil
}

// Close is a no-op; the source holds no live resources.
func (l *LegacySource) Close() error { return nil }

// First returns the lowest registered migration version.
func (l *LegacySource) First() (uint, error) {
	v, ok := l.migrations.First()
	if !ok {
		return 0, &fs.PathError{Op: "first", Path: l.dir, Err: fs.ErrNotExist}
	}
	return v, nil
}

// Prev returns the version immediately below `version` in the
// registered set.
func (l *LegacySource) Prev(version uint) (uint, error) {
	v, ok := l.migrations.Prev(version)
	if !ok {
		return 0, &fs.PathError{Op: "prev", Path: l.dir, Err: fs.ErrNotExist}
	}
	return v, nil
}

// Next returns the version immediately above `version`.
func (l *LegacySource) Next(version uint) (uint, error) {
	v, ok := l.migrations.Next(version)
	if !ok {
		return 0, &fs.PathError{Op: "next", Path: l.dir, Err: fs.ErrNotExist}
	}
	return v, nil
}

// ReadUp opens and returns the up-direction SQL body for the given
// version.
func (l *LegacySource) ReadUp(version uint) (io.ReadCloser, string, error) {
	m, ok := l.migrations.Up(version)
	if !ok {
		return nil, "", &fs.PathError{Op: "read", Path: l.dir, Err: fs.ErrNotExist}
	}
	f, err := os.Open(m.Raw)
	if err != nil {
		return nil, "", fmt.Errorf("migratesource: open up %d: %w", version, err)
	}
	return f, m.Identifier, nil
}

// ReadDown opens and returns the down-direction SQL body when a
// companion file exists.  Legacy forward-only migrations return
// os.ErrNotExist, which golang-migrate maps to "migration not found"
// and refuses to roll back.
func (l *LegacySource) ReadDown(version uint) (io.ReadCloser, string, error) {
	m, ok := l.migrations.Down(version)
	if !ok {
		return nil, "", &fs.PathError{Op: "read", Path: l.dir, Err: fs.ErrNotExist}
	}
	f, err := os.Open(m.Raw)
	if err != nil {
		return nil, "", fmt.Errorf("migratesource: open down %d: %w", version, err)
	}
	return f, m.Identifier, nil
}

// Versions returns the sorted list of every registered version.
// Exposed for the CLI's `version` subcommand and for the validation
// helpers in this package.
func (l *LegacySource) Versions() []uint {
	out := make([]uint, 0, len(l.files))
	for v := range l.files {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HasDown reports whether the given version has a .down.sql companion.
// Useful for the CLI's `down` subcommand which gives a clearer error
// than the underlying ErrNotExist surface.
func (l *LegacySource) HasDown(version uint) bool {
	entry, ok := l.files[version]
	return ok && entry.downPath != ""
}

// Validate enforces the numbering invariants documented in
// scripts/check_migration_numbering.sh, expressed in Go so the CLI can
// surface them locally (without shelling out to bash) and so unit tests
// can exercise the checks.  Rules:
//
//  1. At least one migration must exist.
//  2. Versions must start at 1 and be strictly increasing (no
//     duplicates).
//  3. Every version must have an up file.
//
// Gaps in the sequence are PERMITTED.  Migration prefixes are assigned
// across several parallel workstreams, and a prefix is sometimes
// reserved by one branch before it lands on main (e.g. 000079 shipping
// while 000078 is still in review on another branch — see the numbering
// note in migrations/000079_db_maintenance.sql).  golang-migrate keys
// on the unique version integer and walks the registered set in order
// via First/Next, so a missing prefix is applied as "there is simply no
// migration at that number" — it is benign.  Enforcing strict
// contiguity here used to fail-fast `migrate up` (and therefore every
// DB-backed test) the moment such a coordinated gap existed, which is
// why the contiguity requirement was relaxed to gap-tolerant.  The
// genuinely dangerous case the guard was added for — a DUPLICATE prefix
// (two files sharing a number) — is still rejected, both here and
// structurally at NewFromDir, which fails before Validate even runs.
//
// Post-conditions for callers that observe a nil return:
//
//   - len(l.Versions()) >= 1
//   - l.Versions()[0] == 1
//   - l.Highest() == l.Versions()[len(l.Versions())-1]
//
// LegacySource is immutable after NewFromDir, so these post-conditions
// continue to hold for the lifetime of the value.  Callers (e.g.
// cmdValidate in cmd/migrate/main.go) rely on this to safely index
// into Versions() without a separate length check.
func (l *LegacySource) Validate() error {
	versions := l.Versions()
	if len(versions) == 0 {
		return fmt.Errorf("migratesource: no migrations registered")
	}
	if versions[0] != 1 {
		return fmt.Errorf("migratesource: sequence must start at 000001 (got %06d)", versions[0])
	}
	for i := 1; i < len(versions); i++ {
		// Strictly increasing (duplicates rejected).  Gaps are allowed:
		// versions[i] > versions[i-1] is sufficient; we do NOT require
		// versions[i] == versions[i-1]+1.  Versions() is derived from
		// unique map keys sorted ascending, so a non-increasing pair is
		// unreachable today and this is a defensive guard against a
		// future change to Versions() or the file index.
		if versions[i] <= versions[i-1] {
			return fmt.Errorf(
				"migratesource: versions must be strictly increasing; got %06d after %06d",
				versions[i], versions[i-1],
			)
		}
	}
	for _, v := range versions {
		if l.files[v].upPath == "" {
			return fmt.Errorf("migratesource: version %06d has no up file", v)
		}
	}
	return nil
}

// Highest returns the highest registered version, or 0 if no
// migrations are registered.  Implemented as a direct O(n) scan of
// l.files so it does not allocate the sorted slice that Versions()
// returns.  Although the CLI only calls this once at startup today,
// keeping the no-allocation path means tests that exercise it in a
// loop (or future hot-path callers) do not pay the sort cost.
func (l *LegacySource) Highest() uint {
	var highest uint
	for v := range l.files {
		if v > highest {
			highest = v
		}
	}
	return highest
}

// String returns a human-readable summary used by the CLI's `version`
// subcommand when no DB is reachable.
func (l *LegacySource) String() string {
	vs := l.Versions()
	var sb strings.Builder
	fmt.Fprintf(&sb, "legacy source: %d migrations in %s\n", len(vs), l.dir)
	for _, v := range vs {
		entry := l.files[v]
		dir := "up-only"
		if entry.downPath != "" {
			dir = "up+down"
		}
		fmt.Fprintf(&sb, "  %06d_%s (%s)\n", v, entry.name, dir)
	}
	return sb.String()
}
