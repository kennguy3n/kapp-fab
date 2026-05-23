//go:build integration

// Integration tests for the migrate CLI.  Run with:
//
//	KAPP_TEST_DB_URL=postgres://kapp:kapp_dev@localhost:5432/kapp?sslmode=disable \
//	  go test -tags=integration -v ./cmd/migrate/...
//
// Each test uses a uniquely-named scratch database created from the
// `kapp` template database so cases run in isolation and a flake in
// one case can't corrupt another.  The test asserts:
//
//   - `up` on a fresh DB applies all 54 migrations and exits 0.
//   - Re-running `up` is a no-op.
//   - `validate` accepts the on-disk migration set.
//   - `force` followed by `version` reports the forced version.
//   - `down N` refuses when the target is a forward-only migration.
//   - `bootstrap` refuses on a populated tracking table.
//   - `bootstrap` on a DB pre-loaded with the Kapp schema (no
//     schema_migrations row) primes the tracking table to the
//     configured highest version.
//
// These tests intentionally exercise the binary rather than the Go API
// because the binary is what operators run, what CI runs, and what
// Makefile targets call.  Driving it through `run()` ensures argv
// parsing, flag wiring, env-var handling, and exit semantics are all
// covered.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const testBaseEnv = "KAPP_TEST_DB_URL"

// runCLI calls main.run() with the given argv and DB_URL.  It captures
// stderr / stdout to a string so tests can assert on user-facing
// output.  Side effect: temporarily mutates os.Stdout/os.Stderr.
func runCLI(t *testing.T, dbURL string, argv ...string) (string, error) {
	t.Helper()
	t.Setenv("DB_URL", dbURL)
	// Force the binary to read migrations from the repo root regardless
	// of where `go test` runs the test binary from.  Resolving relative
	// to this file gives us a stable absolute path.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Setenv("KAPP_MIGRATIONS_DIR", filepath.Join(wd, "..", "..", "migrations"))

	origStdout, origStderr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr
	defer func() { os.Stdout = origStdout; os.Stderr = origStderr }()

	// Drain stdout/stderr concurrently with run().  The OS pipe
	// buffer is small (~64KB on Linux); if we waited for run() to
	// finish before reading, a future subcommand emitting more than
	// the buffer size would block its write call and the test would
	// deadlock.  Concurrent drains keep the buffer empty no matter
	// how much output run() produces.
	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { outCh <- drainPipe(rOut) }()
	go func() { errCh <- drainPipe(rErr) }()

	cliErr := run(argv)
	// Close writers so the drain goroutines see io.EOF and return.
	wOut.Close()
	wErr.Close()
	return <-outCh + <-errCh, cliErr
}

func drainPipe(r *os.File) string {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String()
}

// freshDB clones the `kapp` template DB into a uniquely-named scratch
// DB so each test starts from a known schema.  Returns the DSN of the
// scratch DB and a cleanup that drops it.
func freshDB(t *testing.T) (string, func()) {
	t.Helper()
	base := os.Getenv(testBaseEnv)
	if base == "" {
		t.Skipf("%s not set; skipping integration test", testBaseEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Connect to the `postgres` admin DB to run CREATE / DROP DATABASE.
	// We open a fresh connection inside both the setup and the
	// cleanup closure so the closure can outlive freshDB's stack
	// frame without dangling on a closed *sql.DB.
	adminDSN := rewriteDB(t, base, "postgres")
	setupAdmin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer setupAdmin.Close()

	name := fmt.Sprintf("kapp_test_%d", time.Now().UnixNano())
	// CREATE DATABASE is non-transactional; run plain.
	if _, err := setupAdmin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		t.Fatalf("create db %s: %v", name, err)
	}
	cleanup := func() {
		admin, err := sql.Open("pgx", adminDSN)
		if err != nil {
			t.Logf("cleanup open admin: %v", err)
			return
		}
		defer admin.Close()
		// Disconnect any lingering connections, then drop.  Cancel
		// running queries on the test DB first so DROP DATABASE
		// doesn't block.
		_, _ = admin.Exec(fmt.Sprintf(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s'",
			name,
		))
		if _, err := admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q", name)); err != nil {
			t.Logf("cleanup drop %s: %v", name, err)
		}
	}
	return rewriteDB(t, base, name), cleanup
}

// rewriteDB substitutes the database name in a postgres DSN.  Naive
// string manipulation is fine here because the input comes from CI /
// developer env and is fully under our control.
func rewriteDB(t *testing.T, dsn, name string) string {
	t.Helper()
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		t.Fatalf("rewriteDB: dsn missing '@': %q", dsn)
	}
	tail := dsn[at:]
	q := strings.Index(tail, "?")
	if q < 0 {
		t.Fatalf("rewriteDB: dsn missing '?': %q", dsn)
	}
	slash := strings.LastIndex(tail[:q], "/")
	if slash < 0 {
		t.Fatalf("rewriteDB: dsn missing '/': %q", dsn)
	}
	return dsn[:at] + tail[:slash+1] + name + tail[q:]
}

func TestUp_FreshDB(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()

	out, err := runCLI(t, dsn, "up")
	if err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	if !strings.Contains(out, "applied; current version=000054") {
		t.Fatalf("expected current version=000054 in output, got:\n%s", out)
	}

	// Idempotency: second `up` should be a no-op.
	out2, err := runCLI(t, dsn, "up")
	if err != nil {
		t.Fatalf("up again: %v\n%s", err, out2)
	}
	if !strings.Contains(out2, "no migrations to apply") {
		t.Fatalf("expected idempotent no-op, got:\n%s", out2)
	}
}

func TestValidate(t *testing.T) {
	// Validate is read-only and doesn't touch the DB; we can pass an
	// arbitrary DSN.  But the helper requires KAPP_TEST_DB_URL anyway
	// to enforce the integration-only build tag, so reuse freshDB
	// without exercising it.
	if os.Getenv(testBaseEnv) == "" {
		t.Skipf("%s not set", testBaseEnv)
	}
	out, err := runCLI(t, "postgres://stub", "validate")
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sequence well-formed") {
		t.Fatalf("expected well-formed in output, got:\n%s", out)
	}
}

func TestForceAndVersion(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()

	if _, err := runCLI(t, dsn, "up"); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := runCLI(t, dsn, "force", "10"); err != nil {
		t.Fatalf("force: %v", err)
	}
	out, err := runCLI(t, dsn, "version")
	if err != nil {
		t.Fatalf("version: %v\n%s", err, out)
	}
	if !strings.Contains(out, "current version=000010") {
		t.Fatalf("expected version=000010, got:\n%s", out)
	}
}

func TestDown_RefusesForwardOnly(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()

	if _, err := runCLI(t, dsn, "up"); err != nil {
		t.Fatalf("up: %v", err)
	}
	out, err := runCLI(t, dsn, "down", "1")
	if err == nil {
		t.Fatalf("expected error on forward-only down, got nil; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "forward-only") {
		t.Fatalf("expected forward-only message, got: %v", err)
	}
}

func TestBootstrap_RefusesWhenPopulated(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()

	if _, err := runCLI(t, dsn, "up"); err != nil {
		t.Fatalf("up: %v", err)
	}
	out, err := runCLI(t, dsn, "bootstrap")
	if err == nil {
		t.Fatalf("expected bootstrap refusal on populated DB, got nil; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "already has applied migrations") {
		t.Fatalf("expected committed-state refusal, got: %v", err)
	}
}

func TestBootstrap_RefusesFreshDB(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()

	// Don't run up.  The fresh DB lacks the `tenants` sentinel, so
	// bootstrap should refuse with the "looks like a fresh DB" hint.
	out, err := runCLI(t, dsn, "bootstrap")
	if err == nil {
		t.Fatalf("expected bootstrap refusal on fresh DB, got nil; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "fresh DB") {
		t.Fatalf("expected fresh-DB hint, got: %v", err)
	}
}

func TestBootstrap_PrimesLegacyDB(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()

	// Step 1: apply migrations the way the legacy psql-loop would,
	// then manually drop schema_migrations to mimic a DB provisioned
	// by docker-entrypoint-initdb.d or the previous Makefile target.
	if _, err := runCLI(t, dsn, "up"); err != nil {
		t.Fatalf("up: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("DROP TABLE schema_migrations"); err != nil {
		t.Fatalf("drop tracking: %v", err)
	}
	db.Close()

	// Step 2: bootstrap.  Should detect the legacy-DB case and prime
	// the tracking table to the highest version.
	out, err := runCLI(t, dsn, "bootstrap")
	if err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out)
	}
	if !strings.Contains(out, "version=000054") {
		t.Fatalf("expected version=000054 after bootstrap, got:\n%s", out)
	}

	// Step 3: up should now be a no-op.
	out2, err := runCLI(t, dsn, "up")
	if err != nil {
		t.Fatalf("up after bootstrap: %v\n%s", err, out2)
	}
	if !strings.Contains(out2, "no migrations to apply") {
		t.Fatalf("expected no-op after bootstrap, got:\n%s", out2)
	}
}

func TestUp_RefusesLegacyDBWithoutBootstrap(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()

	// Apply via the CLI, then drop the tracking table to mimic the
	// legacy-DB shape.
	if _, err := runCLI(t, dsn, "up"); err != nil {
		t.Fatalf("up: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("DROP TABLE schema_migrations"); err != nil {
		t.Fatalf("drop tracking: %v", err)
	}
	db.Close()

	_, err = runCLI(t, dsn, "up")
	if err == nil {
		t.Fatalf("expected up refusal on legacy DB")
	}
	if !strings.Contains(err.Error(), "legacy DB detected") {
		t.Fatalf("expected legacy-DB refusal, got: %v", err)
	}
}

// TestInvalidSubcommand makes sure the CLI surfaces a clear error on
// typo'd subcommands rather than silently doing nothing.
func TestInvalidSubcommand(t *testing.T) {
	_, err := runCLI(t, "postgres://stub", "uppppp")
	if err == nil {
		t.Fatalf("expected error on bogus subcommand")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("expected 'unknown subcommand', got: %v", err)
	}
}
