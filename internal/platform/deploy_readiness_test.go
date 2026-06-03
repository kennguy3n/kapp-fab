package platform

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func decodeStatus(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return m
}

func TestReadinessProbe_ReadyByDefault(t *testing.T) {
	p := NewReadinessProbe("")
	if ready, reason := p.Ready(); !ready || reason != "" {
		t.Fatalf("Ready() = (%v, %q), want (true, \"\")", ready, reason)
	}

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := decodeStatus(t, rec.Body.Bytes())["status"]; got != "ready" {
		t.Fatalf("status field = %q, want \"ready\"", got)
	}
}

func TestReadinessProbe_InProcessFlag(t *testing.T) {
	p := NewReadinessProbe("")
	p.BeginMigration()

	if ready, reason := p.Ready(); ready || reason == "" {
		t.Fatalf("Ready() after BeginMigration = (%v, %q), want (false, non-empty)", ready, reason)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status during migration = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	body := decodeStatus(t, rec.Body.Bytes())
	if body["status"] != "draining" {
		t.Fatalf("status field = %q, want \"draining\"", body["status"])
	}
	if body["reason"] == "" {
		t.Fatalf("draining response missing reason")
	}

	p.EndMigration()
	if ready, _ := p.Ready(); !ready {
		t.Fatalf("Ready() after EndMigration = false, want true")
	}
}

func TestReadinessProbe_SentinelFile(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "migrating")
	p := NewReadinessProbe(sentinel)

	// Absent sentinel => ready.
	if ready, _ := p.Ready(); !ready {
		t.Fatalf("Ready() with absent sentinel = false, want true")
	}

	// Present sentinel => draining.
	if err := os.WriteFile(sentinel, []byte("pid 123\n"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	ready, reason := p.Ready()
	if ready {
		t.Fatalf("Ready() with present sentinel = true, want false")
	}
	if reason == "" {
		t.Fatalf("expected a drain reason when sentinel present")
	}

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status with sentinel = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	// Removing it returns to ready.
	if err := os.Remove(sentinel); err != nil {
		t.Fatalf("remove sentinel: %v", err)
	}
	if ready, _ := p.Ready(); !ready {
		t.Fatalf("Ready() after sentinel removed = false, want true")
	}
}

// A non-ErrNotExist stat failure must NOT strand the node in a
// permanently-draining state: the probe treats any stat error as
// "sentinel absent" and reports ready.
func TestReadinessProbe_StatErrorTreatedAsAbsent(t *testing.T) {
	p := NewReadinessProbe("/anything")
	p.statFn = func(string) (os.FileInfo, error) {
		return nil, errors.New("EIO: simulated transient io error")
	}
	if ready, reason := p.Ready(); !ready || reason != "" {
		t.Fatalf("Ready() with stat error = (%v, %q), want (true, \"\")", ready, reason)
	}
}

// A permission-style error (wrapped fs.ErrPermission) is likewise
// treated as absent rather than draining.
func TestReadinessProbe_PermissionErrorTreatedAsAbsent(t *testing.T) {
	p := NewReadinessProbe("/anything")
	p.statFn = func(string) (os.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: "/anything", Err: fs.ErrPermission}
	}
	if ready, _ := p.Ready(); !ready {
		t.Fatalf("Ready() with permission error = false, want true")
	}
}

// The in-process flag and the sentinel are OR-combined: either one
// alone forces draining.
func TestReadinessProbe_FlagAndSentinelCombine(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "migrating")
	p := NewReadinessProbe(sentinel)

	p.BeginMigration()
	if ready, _ := p.Ready(); ready {
		t.Fatalf("flag set but Ready()=true")
	}
	// Clearing the flag while the sentinel is present must still drain.
	if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	p.EndMigration()
	if ready, _ := p.Ready(); ready {
		t.Fatalf("sentinel present but Ready()=true after EndMigration")
	}
}
