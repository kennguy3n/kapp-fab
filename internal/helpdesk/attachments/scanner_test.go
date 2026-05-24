package attachments

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewScanner_EmptyURLReturnsPermitAll pins the operator-friendly
// default: empty KAPP_HELPDESK_VIRUS_SCAN_URL gives a permit-all
// scanner (with VerdictSkipped on every call) and the bool flag is
// false so the boot wiring can warn.
func TestNewScanner_EmptyURLReturnsPermitAll(t *testing.T) {
	s, configured := NewScanner("", 0)
	if configured {
		t.Fatalf("expected configured=false for empty URL")
	}
	res, err := s.Scan(context.Background(), "a.txt", "text/plain", []byte("hello"))
	if err != nil {
		t.Fatalf("permit-all scanner returned error: %v", err)
	}
	if res.Verdict != VerdictSkipped {
		t.Fatalf("expected VerdictSkipped, got %q", res.Verdict)
	}
	if res.Detail == "" {
		t.Errorf("expected non-empty detail for skipped verdict")
	}
}

// TestNewScanner_WhitespaceURLTreatedAsEmpty pins the "trim before
// check" contract. An operator who sets the env var to "   " by
// accident should get permit-all + the warning, not a scanner that
// POSTs to a whitespace URL and fails on every email.
func TestNewScanner_WhitespaceURLTreatedAsEmpty(t *testing.T) {
	_, configured := NewScanner("   ", 0)
	if configured {
		t.Fatalf("expected configured=false for whitespace URL")
	}
}

// TestNewScanner_ConfiguredURLReturnsHTTP pins the happy-path
// wiring: a real URL yields an httpScanner with a positive timeout.
func TestNewScanner_ConfiguredURLReturnsHTTP(t *testing.T) {
	s, configured := NewScanner("http://example.test/scan", 5*time.Second)
	if !configured {
		t.Fatalf("expected configured=true for non-empty URL")
	}
	hs, ok := s.(*httpScanner)
	if !ok {
		t.Fatalf("expected *httpScanner, got %T", s)
	}
	if hs.client.Timeout != 5*time.Second {
		t.Errorf("expected 5s timeout, got %v", hs.client.Timeout)
	}
}

// TestNewScanner_NonPositiveTimeoutDefaultsTo10s pins the timeout
// fallback. A zero or negative explicit timeout silently falls back
// to 10s rather than being passed through to http.Client (which
// treats zero as "no timeout" — a footgun on the synchronous
// inbound path).
func TestNewScanner_NonPositiveTimeoutDefaultsTo10s(t *testing.T) {
	for _, d := range []time.Duration{0, -1 * time.Second} {
		s, _ := NewScanner("http://example.test/scan", d)
		hs := s.(*httpScanner)
		if hs.client.Timeout != 10*time.Second {
			t.Errorf("timeout %v: expected fallback to 10s, got %v", d, hs.client.Timeout)
		}
	}
}

// TestHTTPScanner_CleanVerdict pins the canonical happy path: 200
// OK + JSON body with verdict=clean returns VerdictClean.
func TestHTTPScanner_CleanVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Helpdesk-Filename") != "report.pdf" {
			t.Errorf("expected filename header, got %q", r.Header.Get("X-Helpdesk-Filename"))
		}
		if r.Header.Get("X-Helpdesk-Content-Type") != "application/pdf" {
			t.Errorf("expected content-type header, got %q", r.Header.Get("X-Helpdesk-Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello world" {
			t.Errorf("expected body 'hello world', got %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verdict":"clean","name":""}`))
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 5*time.Second)
	res, err := s.Scan(context.Background(), "report.pdf", "application/pdf", []byte("hello world"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Verdict != VerdictClean {
		t.Fatalf("expected clean, got %q (detail %q)", res.Verdict, res.Detail)
	}
}

// TestHTTPScanner_InfectedVerdict pins the infected path. The
// signature name from the scanner's response is surfaced on the
// Detail field so the agent UI can render "EICAR-Test-Signature".
func TestHTTPScanner_InfectedVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"verdict":"infected","name":"EICAR-Test-Signature"}`))
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 5*time.Second)
	res, err := s.Scan(context.Background(), "x.txt", "text/plain", []byte("eicar"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Verdict != VerdictInfected {
		t.Fatalf("expected infected, got %q", res.Verdict)
	}
	if res.Detail != "EICAR-Test-Signature" {
		t.Errorf("expected signature in Detail, got %q", res.Detail)
	}
}

// TestHTTPScanner_VirusOrMaliciousVerdictAlias pins the alternate
// verdict strings some scanners return — "virus" and "malicious"
// are both treated as VerdictInfected.
func TestHTTPScanner_VirusOrMaliciousVerdictAlias(t *testing.T) {
	for _, v := range []string{"virus", "malicious", "Virus", "MALICIOUS"} {
		t.Run(v, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"verdict":"` + v + `"}`))
			}))
			defer srv.Close()
			s, _ := NewScanner(srv.URL, 5*time.Second)
			res, err := s.Scan(context.Background(), "x", "", []byte("x"))
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if res.Verdict != VerdictInfected {
				t.Errorf("verdict %q: expected infected, got %q", v, res.Verdict)
			}
		})
	}
}

// TestHTTPScanner_OKVerdictAlias pins the alternate "ok" string for
// clean (some scanner gateways use this).
func TestHTTPScanner_OKVerdictAlias(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"verdict":"ok"}`))
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 5*time.Second)
	res, err := s.Scan(context.Background(), "x", "", []byte("x"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Verdict != VerdictClean {
		t.Errorf("expected clean for ok alias, got %q", res.Verdict)
	}
}

// TestHTTPScanner_5xxFailsClosed pins the security-critical
// fail-closed behaviour: when the scanner returns 5xx, Scan
// returns an error (NOT VerdictClean). An attacker who can DoS the
// scanner cannot use the outage to admit infected payloads.
func TestHTTPScanner_5xxFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`scanner exploded`))
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 5*time.Second)
	_, err := s.Scan(context.Background(), "x", "", []byte("payload"))
	if err == nil {
		t.Fatalf("expected error on 5xx, got nil")
	}
	if !strings.Contains(err.Error(), "scanner status 500") {
		t.Errorf("expected error to mention status code, got %v", err)
	}
}

// TestHTTPScanner_4xxAlsoFails pins that 4xx from the scanner is
// NOT silently treated as clean. The scanner rejected the request
// shape — that's an operator config error, not a clean payload.
func TestHTTPScanner_4xxAlsoFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 5*time.Second)
	_, err := s.Scan(context.Background(), "x", "", []byte("p"))
	if err == nil {
		t.Fatalf("expected error on 4xx, got nil")
	}
}

// TestHTTPScanner_UnknownVerdictFails pins that an unrecognised
// verdict string is treated as scanner failure, NOT silently
// admitted. If a scanner returns "maybe" or "review_required"
// without prior contract, we'd rather 5xx than guess.
func TestHTTPScanner_UnknownVerdictFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"verdict":"maybe"}`))
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 5*time.Second)
	_, err := s.Scan(context.Background(), "x", "", []byte("p"))
	if err == nil {
		t.Fatalf("expected error on unknown verdict, got nil")
	}
	if !strings.Contains(err.Error(), "unknown verdict") {
		t.Errorf("expected unknown-verdict message, got %v", err)
	}
}

// TestHTTPScanner_MalformedJSONFails pins the parser-error path.
// The scanner returned 200 but a body we can't decode — treat as
// scanner failure rather than guessing.
func TestHTTPScanner_MalformedJSONFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 5*time.Second)
	_, err := s.Scan(context.Background(), "x", "", []byte("p"))
	if err == nil {
		t.Fatalf("expected error on malformed JSON")
	}
}

// TestHTTPScanner_NetworkErrorPropagates pins that a transport-
// level failure (DNS, connection refused) surfaces as an error so
// the caller can fail closed.
func TestHTTPScanner_NetworkErrorPropagates(t *testing.T) {
	// Use a port that's almost certainly not listening. The
	// transport error happens before we send any bytes.
	s, _ := NewScanner("http://127.0.0.1:1/scan", 1*time.Second)
	_, err := s.Scan(context.Background(), "x", "", []byte("p"))
	if err == nil {
		t.Fatalf("expected network error, got nil")
	}
}

// TestHTTPScanner_ContextCancellation pins that a cancelled context
// aborts the scan request — important on the synchronous inbound
// path where the parent request may time out.
func TestHTTPScanner_ContextCancellation(t *testing.T) {
	// A server that sleeps so the request is in flight when
	// the context cancels.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Scan(ctx, "x", "", []byte("p"))
	if err == nil {
		t.Fatalf("expected context cancellation error")
	}
}

// TestHTTPScanner_DefaultsContentType pins that an empty content
// type still produces a sane Content-Type request header so the
// scanner doesn't reject the request shape.
func TestHTTPScanner_DefaultsContentType(t *testing.T) {
	var gotCT, gotXCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotXCT = r.Header.Get("X-Helpdesk-Content-Type")
		_, _ = w.Write([]byte(`{"verdict":"clean"}`))
	}))
	defer srv.Close()
	s, _ := NewScanner(srv.URL, 5*time.Second)
	_, err := s.Scan(context.Background(), "x.bin", "", []byte("p"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("expected request Content-Type application/octet-stream, got %q", gotCT)
	}
	if gotXCT != "application/octet-stream" {
		t.Errorf("expected X-Helpdesk-Content-Type fallback, got %q", gotXCT)
	}
}

// TestErrInfected pins the sentinel error. The Attacher wraps this
// so the caller can `errors.Is(err, ErrInfected)` to distinguish a
// virus-detected outcome from a scanner-unavailable outcome (which
// is a transient error and the relay retries).
func TestErrInfected(t *testing.T) {
	if ErrInfected == nil {
		t.Fatal("ErrInfected sentinel must not be nil")
	}
	wrapped := errors.New("wrap: " + ErrInfected.Error())
	if errors.Is(wrapped, ErrInfected) {
		t.Errorf("unwrap: only %%w should propagate; plain string wrap should NOT match")
	}
	wrapped2 := errors.Join(ErrInfected, errors.New("with detail"))
	if !errors.Is(wrapped2, ErrInfected) {
		t.Errorf("errors.Join should preserve ErrInfected identity")
	}
}
