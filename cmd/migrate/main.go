// Command migrate is the Kapp database migration CLI.  It wraps
// github.com/golang-migrate/migrate/v4 with a custom source driver
// (internal/dbutil/migratesource) that reads the existing legacy
// `NNNNNN_name.sql` files without forcing the .up.sql / .down.sql split
// that the stock file:// driver requires.
//
// Subcommands:
//
//	migrate up [N]            Apply pending migrations.  N is optional;
//	                          when omitted, applies all pending.
//	migrate down N            Roll back the last N migrations.  Each
//	                          migration must have a .down.sql companion
//	                          or the command errors out.
//	migrate force V           Set the current version to V without
//	                          running it.  Use only to recover from a
//	                          dirty state (e.g. after a partial apply).
//	migrate version           Print the current applied version and
//	                          whether the schema_migrations row is
//	                          marked dirty.
//	migrate validate          Check that the migrations directory has
//	                          a contiguous sequence starting at 000001
//	                          with no gaps or duplicates.
//	migrate bootstrap [V]     Initialize schema_migrations on a legacy
//	                          DB that already has Kapp tables but no
//	                          tracking table.  V defaults to the highest
//	                          version found on disk.  Refuses to run if
//	                          schema_migrations already has rows; an
//	                          empty schema_migrations table (e.g. left
//	                          behind by a crashed `up` after
//	                          WithInstance's CREATE TABLE) is treated
//	                          as safe to bootstrap into.
//	migrate pre-check         Parse every pending migration's SQL and
//	                          refuse the deploy if any is NOT backward-
//	                          compatible (column/table drops, table or
//	                          column renames, NOT NULL additions without
//	                          a DEFAULT, SET NOT NULL on an existing
//	                          column).  With DB_URL set the scope is
//	                          migrations above the applied version; with
//	                          -all (or no DB_URL) every on-disk migration
//	                          is checked.  Used as the first gate in
//	                          scripts/deploy.sh.
//	migrate apply [--canary]  Apply pending migrations across every
//	                          cell (KAPP_CELL_DSNS, falling back to
//	                          DB_URL as a single cell).  With --canary,
//	                          apply to the first cell, wait for its
//	                          health check to pass, then proceed to the
//	                          rest.  -readiness-sentinel writes a drain
//	                          file for the duration of the apply so the
//	                          LB readiness probe
//	                          (internal/platform.ReadinessProbe) sheds
//	                          traffic while the schema changes.
//	migrate rollback [N]      Roll back the last N applied migrations
//	                          (default 1) on every cell using their
//	                          .down.sql companions.  Refuses when a
//	                          target migration is forward-only.
//
// Configuration:
//
//	DB_URL                    PostgreSQL DSN.  Required for every
//	                          subcommand except `validate`.
//	KAPP_MIGRATIONS_DIR       Override the migrations directory.
//	                          Defaults to ./migrations relative to the
//	                          current working directory.
//
// Idempotency: re-running `up` after every migration has applied is a
// no-op (golang-migrate returns ErrNoChange, which the CLI maps to
// exit-0 with "no migrations to apply").
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver registration

	"github.com/kennguy3n/kapp-fab/internal/dbutil/migratesource"
)

const (
	// schemaMigrationsTable is golang-migrate's default tracking table.
	// We name it explicitly so the bootstrap subcommand can probe for
	// it via information_schema without hard-coding the literal in two
	// places.
	schemaMigrationsTable = "schema_migrations"

	// kappSentinelTable is one of the always-created Kapp tables from
	// 000001_initial_schema.sql.  The bootstrap subcommand uses its
	// presence (alongside the absence of schema_migrations) as the
	// signal that the DB was provisioned by the legacy psql-loop and
	// needs its tracking table primed.
	kappSentinelTable = "tenants"

	// connectTimeout caps the time we wait for the initial DB
	// connection.  Migration operations themselves can take much
	// longer (CREATE INDEX CONCURRENTLY, etc.), so we do not impose a
	// cap on the migration call itself.
	connectTimeout = 30 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("missing subcommand")
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "up":
		return cmdUp(rest)
	case "down":
		return cmdDown(rest)
	case "force":
		return cmdForce(rest)
	case "version":
		return cmdVersion(rest)
	case "validate":
		return cmdValidate(rest)
	case "bootstrap":
		return cmdBootstrap(rest)
	case "pre-check":
		return cmdPreCheck(rest)
	case "apply":
		return cmdApply(rest)
	case "rollback":
		return cmdRollback(rest)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `migrate — Kapp database migration CLI

Usage:
  migrate up [N]            Apply pending migrations (N optional).
  migrate down N            Roll back the last N migrations.
  migrate force V           Set current version to V (no-op on schema).
  migrate version           Print current version and dirty flag.
  migrate validate          Check on-disk numbering invariants.
  migrate bootstrap [V]     Prime schema_migrations on a legacy DB.
  migrate pre-check [-all]  Refuse non-backward-compatible migrations.
  migrate apply [--canary]  Apply pending migrations across all cells.
  migrate rollback [N]      Roll back last N migrations on all cells.

Environment:
  DB_URL                    PostgreSQL DSN (required for DB ops).
  KAPP_MIGRATIONS_DIR       Migrations directory (default ./migrations).
  KAPP_CELL_DSNS            Comma-separated per-cell DSNs for apply/
                            rollback (defaults to DB_URL as one cell).
  KAPP_CELL_HEALTH_URLS     Comma-separated health-check URLs, index-
                            aligned with KAPP_CELL_DSNS (used by
                            apply --canary).
`)
}

// migrationsDir resolves the migrations directory, honoring the
// KAPP_MIGRATIONS_DIR override.  We resolve to an absolute path so the
// custom source's filesystem walks remain stable even if the CLI is
// invoked from a different working directory than the repo root.
func migrationsDir() (string, error) {
	dir := os.Getenv("KAPP_MIGRATIONS_DIR")
	if dir == "" {
		dir = "migrations"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve migrations dir: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat migrations dir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

// openSource constructs the LegacySource for the configured migrations
// directory.  Called by every subcommand that needs to inspect on-disk
// migrations.
func openSource() (*migratesource.LegacySource, error) {
	dir, err := migrationsDir()
	if err != nil {
		return nil, err
	}
	return migratesource.NewFromDir(dir)
}

// openSourceValidated returns the legacy source after running
// Validate() on it.  Factored out so every DB-touching subcommand can
// fail fast on a malformed migrations directory before opening a
// connection.
func openSourceValidated() (*migratesource.LegacySource, error) {
	src, err := openSource()
	if err != nil {
		return nil, err
	}
	if err := src.Validate(); err != nil {
		return nil, fmt.Errorf("on-disk migrations invalid: %w", err)
	}
	return src, nil
}

// openDB opens a *sql.DB to DB_URL and pings it.  Callers MUST close
// the returned DB themselves — unlike openMigrate, this helper does
// not transfer ownership to a migrate driver.
func openDB() (*sql.DB, error) {
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		return nil, errors.New("DB_URL is required")
	}
	return openDBURL(dbURL)
}

// openDBURL is openDB for an explicit DSN.  Used by the multi-cell
// apply / rollback paths, where each cell has its own DSN drawn from
// KAPP_CELL_DSNS rather than the single DB_URL env var.
func openDBURL(dbURL string) (*sql.DB, error) {
	if dbURL == "" {
		return nil, errors.New("empty DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return db, nil
}

// openMigrate constructs the golang-migrate Migrate instance bound to
// the configured DB and the LegacySource.  Important: golang-migrate's
// postgres driver eagerly CREATEs the schema_migrations table inside
// WithInstance, so callers that need to probe schema state *before*
// that side effect (specifically `up`, which must distinguish a fresh
// DB from a legacy psql-loop DB) MUST run their probes against the
// returned *sql.DB BEFORE invoking openMigrate.
//
// Ownership: on success, the returned *migrate.Migrate takes ownership
// of `db` — calling migrate.Close() will close the DB.  The caller
// MUST NOT also defer db.Close() in this case.  On error, ownership
// stays with the caller and the caller is responsible for closing db.
//
// Note on the NewWithInstance failure path: we intentionally do NOT
// call driver.Close() if NewWithInstance returns an error.  The
// postgres driver wraps the same *sql.DB the caller passed in; its
// Close() would close that DB, breaking the documented contract
// that on error the caller still owns db.  The driver itself
// allocates no extra connections beyond the wrapped *sql.DB, so
// dropping it without Close() is not a leak.
func openMigrate(src *migratesource.LegacySource, db *sql.DB) (*migrate.Migrate, error) {
	driver, err := migratepg.WithInstance(db, &migratepg.Config{
		MigrationsTable: schemaMigrationsTable,
	})
	if err != nil {
		return nil, fmt.Errorf("init postgres driver: %w", err)
	}
	m, err := migrate.NewWithInstance("legacy", src, "postgres", driver)
	if err != nil {
		// See doc comment above: do not close driver here.
		return nil, fmt.Errorf("init migrate: %w", err)
	}
	return m, nil
}

// schemaMigrationsStatus enumerates the three states schema_migrations
// can be in.  The state alone does NOT determine `up`'s behaviour —
// cmdUp combines this with a sentinel-table probe to distinguish a
// fresh DB from a legacy psql-loop DB (see cmdUp for the truth table).
// The state controls:
//
//   - whether cmdBootstrap can safely run (Populated => refuse, anything
//     else => allow);
//   - the operator-facing wording in cmdUp's legacy-DB error message
//     (Absent => "missing", Empty => "empty").
type schemaMigrationsStatus int

const (
	// schemaMigrationsAbsent: table does not exist.  Fresh DB by
	// itself; legacy psql-loop DB if the sentinel table is also
	// present.  cmdUp branches on the sentinel probe.
	schemaMigrationsAbsent schemaMigrationsStatus = iota
	// schemaMigrationsEmpty: table exists with zero rows.  This can
	// happen if (a) a previous `up` aborted after WithInstance's
	// CREATE TABLE but before any migration committed, or (b) a
	// previous `bootstrap` crashed after WithInstance's CREATE TABLE
	// but before Force() committed the version row.  In both cases,
	// re-running `bootstrap` is the recovery path; cmdUp itself
	// refuses to proceed when the sentinel is also present, because
	// applying migrations on top of a legacy DB without first
	// recording prior versions would re-run every 000001..N
	// migration and fail.
	schemaMigrationsEmpty
	// schemaMigrationsPopulated: table has at least one row.  cmdUp
	// proceeds normally (golang-migrate will pick up where the
	// existing rows leave off); cmdBootstrap is refused so it cannot
	// clobber committed state.
	schemaMigrationsPopulated
)

// inspectSchemaMigrations returns the current status without modifying
// state.  It must be called BEFORE openMigrate on the same DB if the
// schemaMigrationsAbsent state matters to the caller.
func inspectSchemaMigrations(ctx context.Context, db *sql.DB) (schemaMigrationsStatus, error) {
	exists, err := tableExists(ctx, db, schemaMigrationsTable)
	if err != nil {
		return 0, err
	}
	if !exists {
		return schemaMigrationsAbsent, nil
	}
	var n int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s", schemaMigrationsTable)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s rows: %w", schemaMigrationsTable, err)
	}
	if n == 0 {
		return schemaMigrationsEmpty, nil
	}
	return schemaMigrationsPopulated, nil
}

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	src, err := openSourceValidated()
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	owned := true
	defer func() {
		if owned {
			_ = db.Close()
		}
	}()

	// Pre-check: detect the legacy-DB case BEFORE WithInstance ever
	// touches schema_migrations.  WithInstance unconditionally CREATEs
	// the table inside its own constructor; if we let it run first
	// we'd lose the ability to distinguish "fresh DB" from "legacy DB
	// where someone ran psql-loop migrations directly".  The fix is
	// structural: do the probe on the raw *sql.DB first, decide
	// whether to proceed, then hand the DB to WithInstance.
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	status, err := inspectSchemaMigrations(ctx, db)
	if err != nil {
		return fmt.Errorf("up: inspect %s: %w", schemaMigrationsTable, err)
	}
	if status != schemaMigrationsPopulated {
		hasSentinel, perr := tableExists(ctx, db, kappSentinelTable)
		if perr != nil {
			return fmt.Errorf("up: probe %s: %w", kappSentinelTable, perr)
		}
		if hasSentinel {
			// Describe schema_migrations precisely so the operator
			// can correlate the message with what they see in psql.
			// status here is either schemaMigrationsAbsent (no
			// table at all) or schemaMigrationsEmpty (table exists
			// but no rows).  Both states need bootstrap, but
			// saying "empty" when the table is absent is
			// factually misleading.
			//
			// Multi-line guidance is returned via fmt.Fprintf
			// after the short error string so staticcheck's
			// ST1005 (no punctuation/newlines in error strings)
			// is honored.
			var stateDesc string
			switch status {
			case schemaMigrationsAbsent:
				stateDesc = "missing"
			case schemaMigrationsEmpty:
				stateDesc = "empty"
			case schemaMigrationsPopulated:
				// Unreachable: outer `if status != schemaMigrationsPopulated`
				// already excludes this case.  Defensive default for
				// future enum additions.
				stateDesc = "in an unexpected state"
			}
			fmt.Fprintf(os.Stderr,
				"%s exists but %s is %s; this DB was provisioned by the legacy psql-loop\n\n"+
					"Run:\n\n    go run ./cmd/migrate bootstrap\n\n"+
					"to mark existing migrations as applied without re-running them.\n",
				kappSentinelTable, schemaMigrationsTable, stateDesc,
			)
			return fmt.Errorf("up: legacy DB detected (%s present, %s %s)",
				kappSentinelTable, schemaMigrationsTable, stateDesc)
		}
	}

	m, err := openMigrate(src, db)
	if err != nil {
		return err
	}
	owned = false // migrate now owns db
	defer closeMigrate(m)

	switch len(rest) {
	case 0:
		err = m.Up()
	case 1:
		n, perr := strconv.Atoi(rest[0])
		if perr != nil {
			return fmt.Errorf("up: invalid N %q: %w", rest[0], perr)
		}
		if n < 1 {
			return fmt.Errorf("up: N must be >= 1, got %d", n)
		}
		err = m.Steps(n)
	default:
		return fmt.Errorf("up: too many arguments (want [N], got %d)", len(rest))
	}
	if errors.Is(err, migrate.ErrNoChange) {
		fmt.Println("no migrations to apply")
		return nil
	}
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}
	// Best-effort Version() readback for the completion message.
	// If Up() succeeded the schema_migrations row is committed, so
	// Version() should always return cleanly; surface any error in
	// the output rather than silently printing v=000000 dirty=false
	// (which would look like a forced rollback).  We do not bubble
	// the error up because the migration itself succeeded.
	v, dirty, vErr := m.Version()
	if vErr != nil {
		fmt.Printf("applied; (could not read schema_migrations afterwards: %v)\n", vErr)
		return nil
	}
	fmt.Printf("applied; current version=%06d dirty=%v\n", v, dirty)
	return nil
}

func cmdDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("down: requires N (number of steps to roll back)")
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		return fmt.Errorf("down: invalid N %q: %w", rest[0], err)
	}
	if n < 1 {
		return fmt.Errorf("down: N must be >= 1, got %d", n)
	}
	src, err := openSourceValidated()
	if err != nil {
		return err
	}
	// Pre-check: the legacy 54 migrations were shipped without down
	// companions.  Trying to roll them back via golang-migrate would
	// surface as a generic ErrNotExist that doesn't explain the
	// situation.  We probe the current version and refuse early when
	// the rollback target lacks a .down.sql.  The source is threaded
	// into openMigrateForDBWithSource so the pre-check and the migrate
	// instance share a single directory scan.
	m, closeFn, err := openMigrateForDBWithSource(src)
	if err != nil {
		return err
	}
	defer closeFn()
	current, _, vErr := m.Version()
	if errors.Is(vErr, migrate.ErrNilVersion) {
		return errors.New("down: no migrations applied; nothing to roll back")
	}
	if vErr != nil {
		return fmt.Errorf("down: read version: %w", vErr)
	}
	// Bounds check: refuse up front when n exceeds the number of
	// applied migrations.  golang-migrate's Steps(-n) would surface a
	// generic error in that case; this gives operators a clear
	// message and lets the HasDown loop below assume probe>=1 so its
	// arithmetic is straightforward.
	//
	// uint(n) is safe here: n is checked >= 1 above so the cast does
	// not wrap, and current is a uint so the comparison is exact.
	nu := uint(n) //nolint:gosec // n is bounded >=1 above; no sign change
	if nu > current {
		return fmt.Errorf(
			"down: N=%d exceeds current version %06d (only %d migration(s) applied)",
			n, current, current,
		)
	}
	// Probe every rollback target's HasDown.  We walk i in [0, n)
	// using the bounded uint computed above, so the loop counter is
	// always representable and there is no dead overflow guard.
	//
	// Coupling note: this arithmetic (probe := current - step)
	// assumes migration versions are strictly contiguous starting at
	// 000001.  That invariant is enforced by
	// migratesource.LegacySource.Validate(), which the
	// openSourceValidated() call above runs before we get here.  If
	// the contiguity rule is ever relaxed (e.g. allowing gaps in the
	// numbering), this loop must be reworked to walk the sorted
	// applied-versions list returned by golang-migrate instead of
	// computing positions arithmetically.
	for step := uint(0); step < nu; step++ {
		probe := current - step
		if !src.HasDown(probe) {
			return fmt.Errorf(
				"down: version %06d is forward-only (no .down.sql companion); "+
					"manual rollback required",
				probe,
			)
		}
	}
	if err := m.Steps(-n); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			fmt.Println("nothing to roll back")
			return nil
		}
		return fmt.Errorf("down: %w", err)
	}
	v, dirty, vErr := m.Version()
	if errors.Is(vErr, migrate.ErrNilVersion) {
		fmt.Println("rolled back; database is now at the pre-migration baseline")
		return nil
	}
	if vErr != nil {
		return fmt.Errorf("down: post-rollback version: %w", vErr)
	}
	fmt.Printf("rolled back %d step(s); current version=%06d dirty=%v\n", n, v, dirty)
	return nil
}

func cmdForce(args []string) error {
	fs := flag.NewFlagSet("force", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("force: requires V (target version)")
	}
	v, err := strconv.Atoi(rest[0])
	if err != nil {
		return fmt.Errorf("force: invalid V %q: %w", rest[0], err)
	}
	if v < 0 {
		return fmt.Errorf("force: V must be >= 0, got %d", v)
	}
	m, closeFn, err := openMigrateForDB()
	if err != nil {
		return err
	}
	defer closeFn()
	if err := m.Force(v); err != nil {
		return fmt.Errorf("force: %w", err)
	}
	fmt.Printf("forced; current version=%06d (dirty cleared)\n", v)
	return nil
}

func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("version: takes no arguments")
	}
	m, closeFn, err := openMigrateForDB()
	if err != nil {
		return err
	}
	defer closeFn()
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		fmt.Println("version: <nil> (no migrations applied)")
		return nil
	}
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	fmt.Printf("current version=%06d dirty=%v\n", v, dirty)
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("validate: takes no arguments")
	}
	src, err := openSource()
	if err != nil {
		return err
	}
	if err := src.Validate(); err != nil {
		return err
	}
	versions := src.Versions()
	fmt.Printf("validate: %d migrations (%06d → %06d), sequence well-formed\n",
		len(versions), versions[0], versions[len(versions)-1])
	return nil
}

func cmdBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	src, err := openSource()
	if err != nil {
		return err
	}
	if err := src.Validate(); err != nil {
		return err
	}
	highest := src.Highest()
	target := highest
	if len(rest) == 1 {
		n, perr := strconv.Atoi(rest[0])
		if perr != nil {
			return fmt.Errorf("bootstrap: invalid V %q: %w", rest[0], perr)
		}
		if n < 1 {
			return fmt.Errorf("bootstrap: V must be >= 1, got %d", n)
		}
		if uint(n) > highest {
			return fmt.Errorf(
				"bootstrap: V=%d exceeds highest on-disk migration %06d",
				n, highest,
			)
		}
		target = uint(n)
	} else if len(rest) > 1 {
		return fmt.Errorf("bootstrap: too many arguments")
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	owned := true
	defer func() {
		if owned {
			_ = db.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	status, err := inspectSchemaMigrations(ctx, db)
	if err != nil {
		return fmt.Errorf("probe %s: %w", schemaMigrationsTable, err)
	}
	if status == schemaMigrationsPopulated {
		return fmt.Errorf(
			"bootstrap: %s already has applied migrations; refusing to overwrite committed state",
			schemaMigrationsTable,
		)
	}
	hasSentinel, err := tableExists(ctx, db, kappSentinelTable)
	if err != nil {
		return fmt.Errorf("probe %s: %w", kappSentinelTable, err)
	}
	if !hasSentinel {
		return fmt.Errorf(
			"bootstrap: %s does not exist; this looks like a fresh DB — run `migrate up` instead",
			kappSentinelTable,
		)
	}
	m, err := openMigrate(src, db)
	if err != nil {
		return err
	}
	owned = false // migrate now owns db
	defer closeMigrate(m)
	if target > uint(math.MaxInt32) {
		return fmt.Errorf("bootstrap: target version %d exceeds int32 range", target)
	}
	if err := m.Force(int(target)); err != nil { //nolint:gosec // bounded check above
		return fmt.Errorf("bootstrap force: %w", err)
	}
	fmt.Printf("bootstrapped; %s now reports version=%06d dirty=false\n",
		schemaMigrationsTable, target)
	return nil
}

// tableExists checks information_schema.tables for a table in the
// public schema.  Used by `bootstrap` to detect the legacy-DB case.
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	const q = `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = current_schema()
      AND table_name   = $1
)`
	var exists bool
	if err := db.QueryRowContext(ctx, q, name).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// openMigrateForDB is the convenience wrapper used by subcommands that
// have no need to probe schema state before WithInstance runs (down,
// force, version).  It opens the source + db, constructs the migrate
// instance, and returns a single cleanup function the caller defers.
//
// Callers that already hold a validated *migratesource.LegacySource
// (e.g. cmdDown, which needs the source for its HasDown pre-check
// before opening the migrate instance) should call
// openMigrateForDBWithSource instead to avoid a second filesystem
// scan of the migrations directory.
func openMigrateForDB() (*migrate.Migrate, func(), error) {
	src, err := openSourceValidated()
	if err != nil {
		return nil, nil, err
	}
	return openMigrateForDBWithSource(src)
}

// openMigrateForDBWithSource is identical to openMigrateForDB but
// reuses an already-validated LegacySource.  Threading the source
// through the call sites lets cmdDown (which validates the source
// once for its HasDown pre-check) avoid re-scanning the migrations
// directory inside this helper.
func openMigrateForDBWithSource(src *migratesource.LegacySource) (*migrate.Migrate, func(), error) {
	db, err := openDB()
	if err != nil {
		return nil, nil, err
	}
	m, err := openMigrate(src, db)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return m, func() { closeMigrate(m) }, nil
}

// closeMigrate runs the standard golang-migrate Close idiom, joining
// source + database errors into a single output line so a flake during
// shutdown doesn't mask the operation's real exit status.
func closeMigrate(m *migrate.Migrate) {
	srcErr, dbErr := m.Close()
	if srcErr != nil {
		fmt.Fprintf(os.Stderr, "migrate: close source: %v\n", srcErr)
	}
	if dbErr != nil {
		fmt.Fprintf(os.Stderr, "migrate: close db: %v\n", dbErr)
	}
}

// ---------------------------------------------------------------------
// Workstream 7: zero-downtime deploy support (pre-check / apply / rollback)
// ---------------------------------------------------------------------

// unsafeRule pairs a backward-incompatible DDL class with a matcher.
// Rules are evaluated per statement after comments and string/dollar-
// quoted literals are stripped (see splitStatements), so a keyword that
// only appears in a migration's comment header or a string literal is
// never mistaken for the real operation.
type unsafeRule struct {
	label string
	re    *regexp.Regexp
}

// unsafeRules enumerates the operations that break a rolling deploy: an
// old binary keeps serving against the migrated schema for the duration
// of the rollout, so any change that the old binary cannot tolerate
// (dropped/renamed columns or tables, a newly-required column) must go
// through a maintenance window instead. The matchers intentionally err
// toward flagging; a false positive is downgraded by moving the change
// to a breaking-release window, whereas a false negative ships an
// outage.
//
// Note the deliberate gaps: DROP CONSTRAINT / DROP INDEX / DROP DEFAULT
// and `DROP NOT NULL` (relaxing a constraint) are NOT flagged because
// they are backward-compatible for a still-running old binary.
var unsafeRules = []unsafeRule{
	{"DROP TABLE", regexp.MustCompile(`(?is)\bDROP\s+TABLE\b`)},
	{"DROP COLUMN", regexp.MustCompile(`(?is)\bDROP\s+COLUMN\b`)},
	{"table rename (ALTER TABLE ... RENAME TO)", regexp.MustCompile(`(?is)\bALTER\s+TABLE\b.*?\bRENAME\s+TO\b`)},
	{"column rename (RENAME COLUMN)", regexp.MustCompile(`(?is)\bRENAME\s+COLUMN\b`)},
	{"SET NOT NULL on existing column", regexp.MustCompile(`(?is)\bSET\s+NOT\s+NULL\b`)},
}

var (
	// addColumnNotNullRE catches `ADD COLUMN ... NOT NULL`; combined
	// with the absence of a DEFAULT clause it is backward-incompatible
	// (the old binary INSERTs rows without the column, and on a
	// populated table the ALTER rewrites every row).
	addColumnNotNullRE = regexp.MustCompile(`(?is)\bADD\s+COLUMN\b.*?\bNOT\s+NULL\b`)
	defaultClauseRE    = regexp.MustCompile(`(?is)\bDEFAULT\b`)
	// generatedRE excludes GENERATED ALWAYS AS (...) STORED columns:
	// Postgres always populates them, so NOT NULL without an explicit
	// DEFAULT is safe for those.
	generatedRE = regexp.MustCompile(`(?is)\bGENERATED\s+ALWAYS\b`)
)

// precheckFinding is one backward-incompatible operation located in a
// pending migration.
type precheckFinding struct {
	version uint
	name    string
	rule    string
	snippet string
}

// dollarTag reports whether sql[i:] opens a Postgres dollar-quoted
// string (`$tag$` / `$$`) and returns the tag (without delimiters). The
// scanner uses this to skip function bodies whole, so a `;` or a
// keyword inside one is never treated as DDL.
func dollarTag(sqlText string, i int) (string, bool) {
	if i >= len(sqlText) || sqlText[i] != '$' {
		return "", false
	}
	for j := i + 1; j < len(sqlText); j++ {
		ch := sqlText[j]
		if ch == '$' {
			return sqlText[i+1 : j], true
		}
		if ch != '_' && (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			return "", false
		}
	}
	return "", false
}

// splitStatements strips SQL comments and string/dollar-quoted literals
// and splits the remaining text into statements on top-level `;`. The
// returned statements contain only DDL/keyword text (literals collapsed
// to a single space), which is exactly what unsafeRules need to match
// against without tripping on prose in comment headers.
func splitStatements(sqlText string) []string {
	var stmts []string
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			stmts = append(stmts, s)
		}
		b.Reset()
	}
	n := len(sqlText)
	for i := 0; i < n; {
		c := sqlText[i]
		switch {
		case c == '-' && i+1 < n && sqlText[i+1] == '-':
			if j := strings.IndexByte(sqlText[i:], '\n'); j < 0 {
				i = n
			} else {
				i += j + 1
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < n && sqlText[i+1] == '*':
			if j := strings.Index(sqlText[i+2:], "*/"); j < 0 {
				i = n
			} else {
				i += 2 + j + 2
			}
			b.WriteByte(' ')
		case c == '\'':
			i++
			for i < n {
				if sqlText[i] == '\'' {
					if i+1 < n && sqlText[i+1] == '\'' { // '' escape
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteByte(' ')
		case c == '$':
			if tag, ok := dollarTag(sqlText, i); ok {
				delim := "$" + tag + "$"
				start := i + len(delim)
				if j := strings.Index(sqlText[start:], delim); j < 0 {
					i = n
				} else {
					i = start + j + len(delim)
				}
				b.WriteByte(' ')
			} else {
				b.WriteByte(c)
				i++
			}
		case c == ';':
			flush()
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	flush()
	return stmts
}

// analyzeSQL returns every backward-incompatible operation found in a
// single migration's up SQL.
func analyzeSQL(version uint, name, sqlText string) []precheckFinding {
	var out []precheckFinding
	for _, stmt := range splitStatements(sqlText) {
		for _, r := range unsafeRules {
			if r.re.MatchString(stmt) {
				out = append(out, precheckFinding{version, name, r.label, snippet(stmt)})
			}
		}
		if addColumnNotNullRE.MatchString(stmt) &&
			!defaultClauseRE.MatchString(stmt) &&
			!generatedRE.MatchString(stmt) {
			out = append(out, precheckFinding{
				version, name, "ADD COLUMN ... NOT NULL without DEFAULT", snippet(stmt),
			})
		}
	}
	return out
}

// snippet collapses a statement's internal whitespace and truncates it
// for a one-line report entry.
func snippet(stmt string) string {
	collapsed := strings.Join(strings.Fields(stmt), " ")
	const maxLen = 120
	if len(collapsed) > maxLen {
		return collapsed[:maxLen] + "…"
	}
	return collapsed
}

// readMigrationUp reads a migration's up body via the source driver.
func readMigrationUp(src *migratesource.LegacySource, v uint) (body, name string, err error) {
	rc, ident, rErr := src.ReadUp(v)
	if rErr != nil {
		return "", "", fmt.Errorf("read migration %06d: %w", v, rErr)
	}
	defer func() { _ = rc.Close() }()
	raw, rdErr := io.ReadAll(rc)
	if rdErr != nil {
		return "", "", fmt.Errorf("read migration %06d body: %w", v, rdErr)
	}
	return string(raw), ident, nil
}

// currentDBVersion returns the applied schema_migrations version (0 when
// the table is absent or empty). It refuses to report a version when the
// row is marked dirty so callers fail fast instead of deploying on top
// of a half-applied migration.
func currentDBVersion(ctx context.Context, db *sql.DB) (uint, error) {
	exists, err := tableExists(ctx, db, schemaMigrationsTable)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var version sql.NullInt64
	var dirty sql.NullBool
	q := fmt.Sprintf("SELECT version, dirty FROM %s LIMIT 1", schemaMigrationsTable)
	switch err := db.QueryRowContext(ctx, q).Scan(&version, &dirty); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read %s: %w", schemaMigrationsTable, err)
	}
	if dirty.Valid && dirty.Bool {
		return 0, fmt.Errorf(
			"%s is dirty at version %d; resolve with `migrate force` before deploying",
			schemaMigrationsTable, version.Int64,
		)
	}
	if !version.Valid || version.Int64 < 0 {
		return 0, nil
	}
	return uint(version.Int64), nil //nolint:gosec // schema_migrations.version is a small non-negative bigint
}

// pendingVersions returns the on-disk migration versions that the
// pre-check should inspect: every version above the applied DB version,
// or — when -all is set or DB_URL is unset — every version on disk.
func pendingVersions(src *migratesource.LegacySource, all bool) ([]uint, error) {
	versions := src.Versions()
	if all || os.Getenv("DB_URL") == "" {
		return versions, nil
	}
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	current, err := currentDBVersion(ctx, db)
	if err != nil {
		return nil, err
	}
	var pending []uint
	for _, v := range versions {
		if v > current {
			pending = append(pending, v)
		}
	}
	return pending, nil
}

func cmdPreCheck(args []string) error {
	fs := flag.NewFlagSet("pre-check", flag.ContinueOnError)
	all := fs.Bool("all", false, "check every on-disk migration, not just those above the applied version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("pre-check: takes no positional arguments")
	}

	src, err := openSource()
	if err != nil {
		return err
	}

	// Numbering contiguity is surfaced as a WARNING, not a hard
	// failure. During a stacked/parallel rollout a migration number
	// can be legitimately reserved by a sibling workstream that has
	// not merged yet (see migrations/000079_db_maintenance.sql), and
	// pre-check must not block deploying the migrations that ARE
	// present. `migrate validate` remains the strict numbering gate.
	if vErr := src.Validate(); vErr != nil {
		fmt.Fprintf(os.Stderr, "pre-check: warning: numbering not contiguous: %v\n", vErr)
	}

	pending, err := pendingVersions(src, *all)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Println("pre-check: no pending migrations to check")
		return nil
	}

	var findings []precheckFinding
	for _, v := range pending {
		body, name, rErr := readMigrationUp(src, v)
		if rErr != nil {
			return rErr
		}
		findings = append(findings, analyzeSQL(v, name, body)...)
	}
	if len(findings) == 0 {
		fmt.Printf("pre-check: %d pending migration(s) are all backward-compatible\n", len(pending))
		return nil
	}

	fmt.Fprintf(os.Stderr, "pre-check: %d backward-incompatible operation(s) found:\n\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "  %06d_%s: %s\n      %s\n", f.version, f.name, f.rule, f.snippet)
	}
	fmt.Fprintf(os.Stderr,
		"\nThese cannot be applied during a zero-downtime rolling deploy. Move them to a\n"+
			"maintenance-window release (docs/UPGRADE_RUNBOOK.md) or rework them to be\n"+
			"backward-compatible (nullable/defaulted columns, soft-deprecate instead of drop).\n")
	return fmt.Errorf("pre-check: %d backward-incompatible migration operation(s)", len(findings))
}

// cell is one deploy target: a DB DSN and an optional health-check URL.
type cell struct {
	dsn       string
	healthURL string
}

// parseCells builds the ordered cell list from KAPP_CELL_DSNS (comma-
// separated), falling back to DB_URL as a single cell. Health URLs come
// from KAPP_CELL_HEALTH_URLS, index-aligned with the DSN list.
func parseCells() ([]cell, error) {
	var dsns []string
	if raw := strings.TrimSpace(os.Getenv("KAPP_CELL_DSNS")); raw != "" {
		for _, d := range strings.Split(raw, ",") {
			if d = strings.TrimSpace(d); d != "" {
				dsns = append(dsns, d)
			}
		}
		if len(dsns) == 0 {
			return nil, errors.New("KAPP_CELL_DSNS is set but contains no DSNs")
		}
	} else if single := os.Getenv("DB_URL"); single != "" {
		dsns = []string{single}
	} else {
		return nil, errors.New("no cells configured: set KAPP_CELL_DSNS or DB_URL")
	}

	var healthURLs []string
	if raw := strings.TrimSpace(os.Getenv("KAPP_CELL_HEALTH_URLS")); raw != "" {
		for _, u := range strings.Split(raw, ",") {
			healthURLs = append(healthURLs, strings.TrimSpace(u))
		}
	}
	cells := make([]cell, len(dsns))
	for i, d := range dsns {
		cells[i] = cell{dsn: d}
		if i < len(healthURLs) {
			cells[i].healthURL = healthURLs[i]
		}
	}
	return cells, nil
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	canary := fs.Bool("canary", false, "apply to the first cell and wait for its health check before applying to the rest")
	sentinel := fs.String("readiness-sentinel", "", "path to a drain sentinel file created for the duration of the apply")
	healthTimeout := fs.Duration("health-timeout", 60*time.Second, "max time to wait for a cell health check to pass")
	healthInterval := fs.Duration("health-interval", 3*time.Second, "polling interval between cell health checks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("apply: takes no positional arguments")
	}
	if *healthInterval <= 0 {
		return errors.New("apply: -health-interval must be > 0")
	}

	src, err := openSource()
	if err != nil {
		return err
	}
	cells, err := parseCells()
	if err != nil {
		return err
	}
	if *canary && len(cells) > 1 && cells[0].healthURL == "" {
		return errors.New("apply --canary needs a health URL for the first cell (set KAPP_CELL_HEALTH_URLS)")
	}

	if *sentinel != "" {
		if err := writeSentinel(*sentinel); err != nil {
			return err
		}
		defer removeSentinel(*sentinel)
	}

	for i, c := range cells {
		label := fmt.Sprintf("cell %d/%d", i+1, len(cells))
		fmt.Printf("apply: %s: applying migrations\n", label)
		if err := applyUp(src, c.dsn); err != nil {
			return fmt.Errorf("apply: %s: %w", label, err)
		}
		if *canary && i == 0 && len(cells) > 1 {
			fmt.Printf("apply: %s is the canary; waiting for health check %s\n", label, c.healthURL)
			if err := waitHealthy(c.healthURL, *healthTimeout, *healthInterval); err != nil {
				return fmt.Errorf("apply: canary %s failed health check: %w", label, err)
			}
			fmt.Printf("apply: canary healthy; proceeding to remaining %d cell(s)\n", len(cells)-1)
		}
	}
	fmt.Printf("apply: completed across %d cell(s)\n", len(cells))
	return nil
}

// applyUp runs `up` against a single cell DSN, mapping ErrNoChange to a
// successful no-op so re-running apply is idempotent.
func applyUp(src *migratesource.LegacySource, dsn string) error {
	db, err := openDBURL(dsn)
	if err != nil {
		return err
	}
	m, err := openMigrate(src, db)
	if err != nil {
		_ = db.Close()
		return err
	}
	defer closeMigrate(m)

	switch err := m.Up(); {
	case errors.Is(err, migrate.ErrNoChange):
		fmt.Println("  no migrations to apply")
		return nil
	case err != nil:
		return err
	}
	v, dirty, vErr := m.Version()
	if vErr != nil {
		fmt.Printf("  applied; (could not read version afterwards: %v)\n", vErr)
		return nil
	}
	fmt.Printf("  applied; current version=%06d dirty=%v\n", v, dirty)
	return nil
}

func cmdRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	n := 1
	switch len(rest) {
	case 0:
	case 1:
		v, perr := strconv.Atoi(rest[0])
		if perr != nil {
			return fmt.Errorf("rollback: invalid N %q: %w", rest[0], perr)
		}
		n = v
	default:
		return errors.New("rollback: too many arguments (want [N])")
	}
	if n < 1 {
		return fmt.Errorf("rollback: N must be >= 1, got %d", n)
	}

	src, err := openSource()
	if err != nil {
		return err
	}
	cells, err := parseCells()
	if err != nil {
		return err
	}

	// Roll back in reverse apply order so the canary (applied first)
	// unwinds last, mirroring a careful manual unwind.
	for i := len(cells) - 1; i >= 0; i-- {
		label := fmt.Sprintf("cell %d/%d", i+1, len(cells))
		fmt.Printf("rollback: %s: rolling back %d migration(s)\n", label, n)
		if err := rollbackDown(src, cells[i].dsn, n); err != nil {
			return fmt.Errorf("rollback: %s: %w", label, err)
		}
	}
	fmt.Printf("rollback: completed across %d cell(s)\n", len(cells))
	return nil
}

// rollbackDown reverts the last n applied migrations on one cell. Before
// touching the DB it walks the actual applied versions downward via the
// source's Prev() — NOT by arithmetic on the version number — so a
// numbering gap from a sibling workstream cannot make a present
// migration look forward-only. Every target must have a .down.sql
// companion or the rollback is refused.
func rollbackDown(src *migratesource.LegacySource, dsn string, n int) error {
	db, err := openDBURL(dsn)
	if err != nil {
		return err
	}
	m, err := openMigrate(src, db)
	if err != nil {
		_ = db.Close()
		return err
	}
	defer closeMigrate(m)

	current, _, vErr := m.Version()
	if errors.Is(vErr, migrate.ErrNilVersion) {
		fmt.Println("  no migrations applied; nothing to roll back")
		return nil
	}
	if vErr != nil {
		return fmt.Errorf("read version: %w", vErr)
	}

	probe := current
	for k := 0; k < n; k++ {
		if !src.HasDown(probe) {
			return fmt.Errorf(
				"version %06d is forward-only (no .down.sql companion); manual rollback required",
				probe,
			)
		}
		if k == n-1 {
			break
		}
		prev, pErr := src.Prev(probe)
		if pErr != nil {
			return fmt.Errorf("cannot roll back %d migration(s): only %d applied above baseline", n, k+1)
		}
		probe = prev
	}

	switch err := m.Steps(-n); {
	case errors.Is(err, migrate.ErrNoChange):
		fmt.Println("  nothing to roll back")
		return nil
	case err != nil:
		return err
	}
	v, dirty, pvErr := m.Version()
	if errors.Is(pvErr, migrate.ErrNilVersion) {
		fmt.Println("  rolled back to the pre-migration baseline")
		return nil
	}
	if pvErr != nil {
		return fmt.Errorf("post-rollback version: %w", pvErr)
	}
	fmt.Printf("  rolled back %d step(s); current version=%06d dirty=%v\n", n, v, dirty)
	return nil
}

// writeSentinel creates the readiness drain file. While it exists, the
// API replicas' platform.ReadinessProbe reports 503 so the LB drains
// connections for the duration of the migration apply.
func writeSentinel(path string) error {
	content := fmt.Sprintf("migrate apply pid=%d started=%s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write readiness sentinel %s: %w", path, err)
	}
	fmt.Printf("apply: wrote readiness sentinel %s (LB will drain)\n", path)
	return nil
}

// removeSentinel deletes the drain file. A missing file is not an error
// (the apply may have failed before creating it); any other failure is
// logged but not fatal so it can't mask the apply's real exit status.
func removeSentinel(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "apply: warning: could not remove readiness sentinel %s: %v\n", path, err)
		return
	}
	fmt.Printf("apply: removed readiness sentinel %s\n", path)
}

// waitHealthy polls url until it returns 200 or timeout elapses.
func waitHealthy(url string, timeout, interval time.Duration) error {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for attempt := 1; ; attempt++ {
		err := healthOnce(client, url)
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("health check did not pass within %s (last error: %w)", timeout, lastErr)
		}
		fmt.Printf("  health attempt %d not ready (%v); retrying in %s\n", attempt, lastErr, interval)
		time.Sleep(interval)
	}
}

// healthOnce performs a single GET and reports success on HTTP 200.
func healthOnce(client *http.Client, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
