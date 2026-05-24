package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileProvider_GetSecret_Found(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jwt-primary"), []byte("supersecret\n"), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	p, err := NewFileProvider(dir)
	if err != nil {
		t.Fatalf("NewFileProvider: %v", err)
	}
	v, err := p.GetSecret(context.Background(), "jwt-primary")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(v.Bytes) != "supersecret" {
		t.Fatalf("trailing newline not stripped: got %q", string(v.Bytes))
	}
	if v.Version == "" {
		t.Fatalf("file provider should populate Version with mtime")
	}
}

func TestFileProvider_GetSecret_NestedKey(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "jwt")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "primary"), []byte("nested-secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p, _ := NewFileProvider(dir)
	v, err := p.GetSecret(context.Background(), "jwt/primary")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(v.Bytes) != "nested-secret" {
		t.Fatalf("got %q want nested-secret", string(v.Bytes))
	}
}

func TestFileProvider_GetSecret_Missing(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewFileProvider(dir)
	_, err := p.GetSecret(context.Background(), "absent")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}
}

func TestFileProvider_GetSecret_Empty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty"), []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p, _ := NewFileProvider(dir)
	_, err := p.GetSecret(context.Background(), "empty")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound for empty file, got %v", err)
	}
}

func TestFileProvider_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewFileProvider(dir)
	_, err := p.GetSecret(context.Background(), "../etc/passwd")
	if err == nil {
		t.Fatalf("expected traversal rejection, got nil")
	}
	if !strings.Contains(err.Error(), "..") && !strings.Contains(err.Error(), "escape") {
		t.Errorf("expected traversal error, got: %v", err)
	}
}

func TestFileProvider_VersionChangesOnRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p, _ := NewFileProvider(dir)
	v1, err := p.GetSecret(context.Background(), "key")
	if err != nil {
		t.Fatalf("GetSecret v1: %v", err)
	}
	// Sleep briefly so the mtime advances; some filesystems
	// have second-granularity mtimes.
	time.Sleep(20 * time.Millisecond)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	v2, err := p.GetSecret(context.Background(), "key")
	if err != nil {
		t.Fatalf("GetSecret v2: %v", err)
	}
	if v1.Version == v2.Version {
		t.Fatalf("expected distinct versions, both = %q", v1.Version)
	}
}

func TestFileProvider_RejectsEmptyRoot(t *testing.T) {
	_, err := NewFileProvider("")
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured, got %v", err)
	}
}

// TestFileProvider_VersionMatchesReadContent verifies that the
// reported Version reflects the file generation actually returned
// in Bytes — not a separate stat() that might race with a
// concurrent rotation. The earlier shape called os.Stat THEN
// os.ReadFile, leaving a window where the stat saw v1's mtime
// but ReadFile picked up v2's content; the new shape opens once
// and stats the FD so both come from the same inode generation.
//
// The test simulates the race by populating the file in two
// generations and asserting the version-content tuple is
// internally consistent on every read.
func TestFileProvider_VersionMatchesReadContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotating")
	if err := os.WriteFile(path, []byte("gen1"), 0o600); err != nil {
		t.Fatalf("write gen1: %v", err)
	}
	p, _ := NewFileProvider(dir)
	v1, err := p.GetSecret(context.Background(), "rotating")
	if err != nil {
		t.Fatalf("get gen1: %v", err)
	}
	if string(v1.Bytes) != "gen1" {
		t.Fatalf("gen1 bytes: got %q, want %q", string(v1.Bytes), "gen1")
	}
	time.Sleep(20 * time.Millisecond)
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte("gen2-longer-content"), 0o600); err != nil {
		t.Fatalf("write gen2: %v", err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	v2, err := p.GetSecret(context.Background(), "rotating")
	if err != nil {
		t.Fatalf("get gen2: %v", err)
	}
	if string(v2.Bytes) != "gen2-longer-content" {
		t.Fatalf("gen2 bytes: got %q, want %q", string(v2.Bytes), "gen2-longer-content")
	}
	if v1.Version == v2.Version {
		t.Fatalf("version did not change across generations")
	}
}

// TestFileProvider_LimitsReadSize bounds the read to 1 MiB so a
// misconfigured mount pointing at a multi-gigabyte file doesn't
// exhaust memory before the application notices the wrong path.
// Secrets are conventionally <a few KiB; legitimate JWT keys top
// out around 4 KiB for an RS-4096 PEM. 1 MiB is well above the
// envelope and well below an OOM risk.
func TestFileProvider_LimitsReadSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge")
	// Write 2 MiB so the limiter must clip.
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = 'A'
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatalf("write huge: %v", err)
	}
	p, _ := NewFileProvider(dir)
	v, err := p.GetSecret(context.Background(), "huge")
	if err != nil {
		t.Fatalf("get huge: %v", err)
	}
	if len(v.Bytes) > 1<<20 {
		t.Fatalf("read returned %d bytes; limiter should cap at 1 MiB", len(v.Bytes))
	}
}
