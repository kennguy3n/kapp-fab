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
	"math"
	"os"
	"path/filepath"
	"strconv"
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

Environment:
  DB_URL                    PostgreSQL DSN (required for DB ops).
  KAPP_MIGRATIONS_DIR       Migrations directory (default ./migrations).
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
// can be in.  The state controls whether `up` proceeds normally,
// whether `bootstrap` is the correct next step, and whether
// `bootstrap` can safely run.
type schemaMigrationsStatus int

const (
	// schemaMigrationsAbsent: table does not exist.  Fresh DB —
	// `up` proceeds and WithInstance creates the table.
	schemaMigrationsAbsent schemaMigrationsStatus = iota
	// schemaMigrationsEmpty: table exists with zero rows.  Either a
	// previous `up` attempt aborted after WithInstance's CREATE TABLE
	// but before any migration committed, or a previous `bootstrap`
	// invocation crashed.  `up` still proceeds; `bootstrap` is allowed.
	schemaMigrationsEmpty
	// schemaMigrationsPopulated: table has at least one row.  `up`
	// proceeds normally; `bootstrap` is refused so it cannot clobber
	// committed state.
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
	v, dirty, _ := m.Version()
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
