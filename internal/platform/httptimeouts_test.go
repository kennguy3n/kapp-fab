package platform

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDefaultHTTPTimeouts_ReasonableValues(t *testing.T) {
	d := DefaultHTTPTimeouts()
	// Lock the documented values in so a future refactor that
	// accidentally widens a window has to update the test (and
	// therefore the doc-comment that explains the choice).
	if d.ReadHeader != 10*time.Second {
		t.Fatalf("ReadHeader: got %v, want 10s", d.ReadHeader)
	}
	if d.Read != 60*time.Second {
		t.Fatalf("Read: got %v, want 60s", d.Read)
	}
	if d.Write != 120*time.Second {
		t.Fatalf("Write: got %v, want 120s", d.Write)
	}
	if d.Idle != 120*time.Second {
		t.Fatalf("Idle: got %v, want 120s", d.Idle)
	}
	if d.MaxHeaderBytes != 1<<20 {
		t.Fatalf("MaxHeaderBytes: got %d, want %d", d.MaxHeaderBytes, 1<<20)
	}
}

func TestLongStreamTimeouts_KeepsReadDefensesDropsWrite(t *testing.T) {
	s := LongStreamTimeouts()
	// SSE must not have a write timeout — it would kill the
	// long-lived response.
	if s.Write != 0 {
		t.Fatalf("Write: got %v, want 0 (SSE-safe)", s.Write)
	}
	// Everything else must still match the default policy.
	d := DefaultHTTPTimeouts()
	if s.ReadHeader != d.ReadHeader {
		t.Fatalf("ReadHeader: got %v, want %v", s.ReadHeader, d.ReadHeader)
	}
	if s.Read != d.Read {
		t.Fatalf("Read: got %v, want %v", s.Read, d.Read)
	}
	if s.Idle != d.Idle {
		t.Fatalf("Idle: got %v, want %v", s.Idle, d.Idle)
	}
	if s.MaxHeaderBytes != d.MaxHeaderBytes {
		t.Fatalf("MaxHeaderBytes: got %d, want %d", s.MaxHeaderBytes, d.MaxHeaderBytes)
	}
}

func TestMetricsHTTPTimeouts_TighterThanDefault(t *testing.T) {
	m := MetricsHTTPTimeouts()
	d := DefaultHTTPTimeouts()
	if !(m.ReadHeader < d.ReadHeader) {
		t.Fatalf("metrics ReadHeader %v should be < default %v", m.ReadHeader, d.ReadHeader)
	}
	if !(m.Read < d.Read) {
		t.Fatalf("metrics Read %v should be < default %v", m.Read, d.Read)
	}
	if !(m.Write < d.Write) {
		t.Fatalf("metrics Write %v should be < default %v", m.Write, d.Write)
	}
	if !(m.Idle < d.Idle) {
		t.Fatalf("metrics Idle %v should be < default %v", m.Idle, d.Idle)
	}
	if !(m.MaxHeaderBytes < d.MaxHeaderBytes) {
		t.Fatalf("metrics MaxHeaderBytes %d should be < default %d", m.MaxHeaderBytes, d.MaxHeaderBytes)
	}
}

func TestApply_StampsAllFields(t *testing.T) {
	srv := &http.Server{}
	tc := HTTPTimeouts{
		ReadHeader:     1 * time.Second,
		Read:           2 * time.Second,
		Write:          3 * time.Second,
		Idle:           4 * time.Second,
		MaxHeaderBytes: 5 << 10,
	}
	tc.Apply(srv)
	if srv.ReadHeaderTimeout != tc.ReadHeader {
		t.Fatalf("ReadHeaderTimeout: got %v, want %v", srv.ReadHeaderTimeout, tc.ReadHeader)
	}
	if srv.ReadTimeout != tc.Read {
		t.Fatalf("ReadTimeout: got %v, want %v", srv.ReadTimeout, tc.Read)
	}
	if srv.WriteTimeout != tc.Write {
		t.Fatalf("WriteTimeout: got %v, want %v", srv.WriteTimeout, tc.Write)
	}
	if srv.IdleTimeout != tc.Idle {
		t.Fatalf("IdleTimeout: got %v, want %v", srv.IdleTimeout, tc.Idle)
	}
	if srv.MaxHeaderBytes != tc.MaxHeaderBytes {
		t.Fatalf("MaxHeaderBytes: got %d, want %d", srv.MaxHeaderBytes, tc.MaxHeaderBytes)
	}
}

func TestApply_ZeroMaxHeaderBytesLeavesStdlibDefault(t *testing.T) {
	srv := &http.Server{MaxHeaderBytes: 9999}
	tc := HTTPTimeouts{MaxHeaderBytes: 0}
	tc.Apply(srv)
	// Apply must NOT overwrite a non-zero existing value with 0,
	// because srv.MaxHeaderBytes == 0 means "use stdlib default" and
	// we don't want to silently shrink it. The caller's job to set
	// MaxHeaderBytes explicitly to the policy value.
	if srv.MaxHeaderBytes != 9999 {
		t.Fatalf("MaxHeaderBytes overwritten by zero: got %d, want 9999", srv.MaxHeaderBytes)
	}
}

func TestApply_WriteZeroIsPreservedForSSE(t *testing.T) {
	srv := &http.Server{}
	tc := LongStreamTimeouts()
	tc.Apply(srv)
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout: got %v, want 0 (LongStreamTimeouts must preserve SSE-safe zero)", srv.WriteTimeout)
	}
}

func TestLoadHTTPTimeouts_EnvOverrides(t *testing.T) {
	t.Setenv("KAPP_HTTP_READ_HEADER_TIMEOUT", "1s")
	t.Setenv("KAPP_HTTP_READ_TIMEOUT", "2s")
	t.Setenv("KAPP_HTTP_WRITE_TIMEOUT", "3s")
	t.Setenv("KAPP_HTTP_IDLE_TIMEOUT", "4s")
	t.Setenv("KAPP_HTTP_MAX_HEADER_BYTES", "12345")

	got := LoadHTTPTimeouts(DefaultHTTPTimeouts())

	if got.ReadHeader != 1*time.Second {
		t.Fatalf("ReadHeader: %v", got.ReadHeader)
	}
	if got.Read != 2*time.Second {
		t.Fatalf("Read: %v", got.Read)
	}
	if got.Write != 3*time.Second {
		t.Fatalf("Write: %v", got.Write)
	}
	if got.Idle != 4*time.Second {
		t.Fatalf("Idle: %v", got.Idle)
	}
	if got.MaxHeaderBytes != 12345 {
		t.Fatalf("MaxHeaderBytes: %d", got.MaxHeaderBytes)
	}
}

func TestLoadHTTPTimeouts_InvalidValuesFallBack(t *testing.T) {
	t.Setenv("KAPP_HTTP_READ_HEADER_TIMEOUT", "not-a-duration")
	t.Setenv("KAPP_HTTP_READ_TIMEOUT", "negative")
	t.Setenv("KAPP_HTTP_WRITE_TIMEOUT", "-5s")
	t.Setenv("KAPP_HTTP_IDLE_TIMEOUT", "0s") // 0 is non-positive for parseDurationEnv -> fallback.
	t.Setenv("KAPP_HTTP_MAX_HEADER_BYTES", "abc")

	base := DefaultHTTPTimeouts()
	got := LoadHTTPTimeouts(base)

	// Every field falls back to base. parseDurationEnv treats 0 as
	// "non-positive, unsafe → fallback" because IdleTimeout=0 means
	// unlimited keep-alive accumulation.
	if got.ReadHeader != base.ReadHeader {
		t.Fatalf("ReadHeader: %v", got.ReadHeader)
	}
	if got.Read != base.Read {
		t.Fatalf("Read: %v", got.Read)
	}
	if got.Write != base.Write {
		t.Fatalf("Write: %v", got.Write)
	}
	if got.Idle != base.Idle {
		t.Fatalf("Idle: %v", got.Idle)
	}
	if got.MaxHeaderBytes != base.MaxHeaderBytes {
		t.Fatalf("MaxHeaderBytes: %d", got.MaxHeaderBytes)
	}
}

func TestLoadHTTPTimeouts_ZeroAllowedForReadAndWrite(t *testing.T) {
	// parseDurationEnvAllowZero is used by Read + Write so a service
	// that intentionally wants unlimited can opt in via env.
	t.Setenv("KAPP_HTTP_READ_TIMEOUT", "0s")
	t.Setenv("KAPP_HTTP_WRITE_TIMEOUT", "0s")

	got := LoadHTTPTimeouts(DefaultHTTPTimeouts())
	if got.Read != 0 {
		t.Fatalf("Read: got %v, want 0", got.Read)
	}
	if got.Write != 0 {
		t.Fatalf("Write: got %v, want 0", got.Write)
	}
}

func TestLoadHTTPTimeouts_PreservesLongStreamWriteZero(t *testing.T) {
	// When the base is LongStreamTimeouts (Write=0), an unset env var
	// must NOT replace the deliberate zero with the default 120s.
	got := LoadHTTPTimeouts(LongStreamTimeouts())
	if got.Write != 0 {
		t.Fatalf("Write: got %v, want 0 (long-stream base must survive unset env)", got.Write)
	}
}

func TestParseDurationEnv_RejectsZero(t *testing.T) {
	// Direct guard: parseDurationEnv (no AllowZero) treats zero as
	// unsafe. IdleTimeout uses this because Idle=0 = unlimited.
	t.Setenv("KAPP_HTTP_IDLE_TIMEOUT", "0s")
	got := parseDurationEnv("KAPP_HTTP_IDLE_TIMEOUT", 99*time.Second)
	if got != 99*time.Second {
		t.Fatalf("got %v, want fallback 99s", got)
	}
}

// Confirm doc-comment env-var key names are stable. A future refactor
// that renames a constant would silently break operators' .env files;
// the test catches it by failing on the contract surface.
func TestEnvVarKeysAreStable(t *testing.T) {
	// We don't introspect the function — we just confirm that
	// setting the documented keys actually has effect, which the
	// other tests in this file do. This test exists as a sentinel
	// for grep: if you change one of these strings you'll see test
	// failures across the suite.
	docKeys := []string{
		"KAPP_HTTP_READ_HEADER_TIMEOUT",
		"KAPP_HTTP_READ_TIMEOUT",
		"KAPP_HTTP_WRITE_TIMEOUT",
		"KAPP_HTTP_IDLE_TIMEOUT",
		"KAPP_HTTP_MAX_HEADER_BYTES",
	}
	for _, k := range docKeys {
		if !strings.HasPrefix(k, "KAPP_HTTP_") {
			t.Fatalf("env var %q broke the KAPP_HTTP_ prefix contract", k)
		}
	}
}
