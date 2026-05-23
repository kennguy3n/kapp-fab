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
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("migratesource: parse url: %w", err)
	}
	dir := u.Path
	if dir == "" {
		return nil, fmt.Errorf("migratesource: empty path in %q", rawurl)
	}
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
//  2. Versions must form a contiguous sequence starting at 1.
//  3. No duplicate versions.
//  4. Every version must have an up file.
//
// (Duplicates are already prevented at NewFromDir; we re-check here
// because the rules contract is the source of truth.)
func (l *LegacySource) Validate() error {
	versions := l.Versions()
	if len(versions) == 0 {
		return fmt.Errorf("migratesource: no migrations registered")
	}
	if versions[0] != 1 {
		return fmt.Errorf("migratesource: sequence must start at 000001 (got %06d)", versions[0])
	}
	for i := 1; i < len(versions); i++ {
		if versions[i] == versions[i-1] {
			return fmt.Errorf("migratesource: duplicate version %06d", versions[i])
		}
		if versions[i] != versions[i-1]+1 {
			return fmt.Errorf(
				"migratesource: non-monotonic sequence; expected %06d after %06d, got %06d",
				versions[i-1]+1, versions[i-1], versions[i],
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

// Highest returns the highest registered version.  Equivalent to the
// last element of Versions(); kept as a convenience accessor that
// avoids a copy of the version slice in hot paths.
func (l *LegacySource) Highest() uint {
	vs := l.Versions()
	if len(vs) == 0 {
		return 0
	}
	return vs[len(vs)-1]
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
