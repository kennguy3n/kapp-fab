package platform

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// HTTPTimeouts centralises the slow-loris / slow-write / idle-connection
// defenses every Kapp HTTP server must apply. Go's net/http leaves every
// timeout at zero (i.e. unlimited) by default, which is unsafe for any
// service that terminates connections from the public internet OR shares
// a pod with a service that does. Phase 5 introduces this helper so all
// five services (api, worker, importer, agent-tools, kchat-bridge) share
// one tuning surface and a single regression-tested defaults table.
//
// Defaults reflect a typical Kapp-API workload mix:
//
//   - ReadHeader: 10s caps slow-loris on TLS handshake completion + the
//     header line. Headers are bounded by MaxHeaderBytes so 10s is
//     generous for a 1 MiB header on any non-pathological link.
//   - Read: 60s caps slow-loris on body upload. Importer accepts large
//     CSVs; 60s is the realistic upper bound for a 50 MiB body over a
//     slow consumer link. Tunable up for bigger imports.
//   - Write: 120s caps slow-write on the response. Insights / report
//     handlers can take a while; long-streaming routes (SSE) must opt
//     out of the timeout by constructing a server with Long=true (see
//     LongStreamTimeouts).
//   - Idle: 120s reaps idle h2 / keep-alive connections. Go's default
//     is zero (unlimited) which lets connection pools accumulate
//     without bound under load-test or attacker traffic.
//   - MaxHeaderBytes: 1 MiB caps the JWT + cookie + custom-header
//     total. The default (`http.DefaultMaxHeaderBytes = 1 MiB`) is
//     fine but we encode it explicitly so a future Go-stdlib change
//     to the default doesn't silently widen Kapp's attack surface.
//
// Tuning. Every field is overridable via env var (KAPP_HTTP_*). All
// values parse as time.Duration (e.g. "60s", "2m", "1500ms") or, for
// MaxHeaderBytes, as an int. Invalid values fall back to the default
// AND emit a structured log warning at boot via the calling service's
// logger.
type HTTPTimeouts struct {
	// ReadHeader bounds the time from connection accept to end of
	// request headers. A programmatic zero disables (Go stdlib
	// behaviour), but the KAPP_HTTP_READ_HEADER_TIMEOUT env
	// override REJECTS zero / negative values as a defensive
	// measure (an operator who fat-fingers "0" should not
	// silently disable slow-loris protection); see
	// parseDurationEnv.
	ReadHeader time.Duration
	// Read bounds the time from connection accept to end of
	// request body. A programmatic zero allows arbitrarily slow
	// uploads. The KAPP_HTTP_READ_TIMEOUT env override DOES
	// accept zero so importer / agent-tools can opt out for
	// large-CSV imports; see parseDurationEnvAllowZero.
	Read time.Duration
	// Write bounds the time from end of request headers to end
	// of response body. A programmatic zero is required for SSE
	// / long-lived streams. The KAPP_HTTP_WRITE_TIMEOUT env
	// override DOES accept zero so an operator can disable the
	// timeout for a streaming workload; see
	// parseDurationEnvAllowZero.
	Write time.Duration
	// Idle bounds the time a keep-alive connection sits between
	// requests before the server closes it. A programmatic zero
	// falls back to the stdlib default (also zero, i.e.
	// unlimited), which is unsafe for any production server;
	// DefaultHTTPTimeouts sets 120s. The KAPP_HTTP_IDLE_TIMEOUT
	// env override REJECTS zero / negative values for the same
	// reason as ReadHeader; see parseDurationEnv.
	Idle time.Duration
	// MaxHeaderBytes caps the total header size accepted from a
	// request. A programmatic zero falls back to
	// http.DefaultMaxHeaderBytes (1 MiB). The
	// KAPP_HTTP_MAX_HEADER_BYTES env override accepts only
	// strictly positive values.
	MaxHeaderBytes int
}

// DefaultHTTPTimeouts returns the standard timeout policy for a
// "request-response" Kapp HTTP server (no long-lived streams). All five
// services start from this and override individual fields where their
// workload demands.
func DefaultHTTPTimeouts() HTTPTimeouts {
	return HTTPTimeouts{
		ReadHeader:     10 * time.Second,
		Read:           60 * time.Second,
		Write:          120 * time.Second,
		Idle:           120 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MiB
	}
}

// LongStreamTimeouts returns a timeout policy suitable for a server
// that hosts long-lived response streams (Server-Sent Events,
// WebSockets-upgraded handlers, large file downloads). It keeps every
// defense EXCEPT WriteTimeout, which would kill the stream mid-flight.
// The main API server uses this because /api/v1/events/stream is SSE.
// Worker / importer / agent-tools / kchat-bridge do NOT host streams
// and should use DefaultHTTPTimeouts.
//
// Read-side slow-loris and Idle reaping remain in force, so the only
// attack vector this opens is a slow-write consumer holding a response
// goroutine. That risk is bounded by chi's per-route middleware.Timeout
// (mounted on every non-streaming route) and by the connection's
// underlying TCP socket buffer back-pressure.
func LongStreamTimeouts() HTTPTimeouts {
	t := DefaultHTTPTimeouts()
	t.Write = 0 // unlimited: SSE clients hold the response open.
	return t
}

// MetricsHTTPTimeouts returns the timeout policy for the dedicated
// /metrics scrape endpoint. Scrapes are short request-response cycles
// from Prometheus, never long-lived streams, never large payloads, so
// the timeouts can be tighter than the user-facing API.
func MetricsHTTPTimeouts() HTTPTimeouts {
	return HTTPTimeouts{
		ReadHeader:     5 * time.Second,
		Read:           10 * time.Second,
		Write:          30 * time.Second,
		Idle:           60 * time.Second,
		MaxHeaderBytes: 64 << 10, // 64 KiB: scrapers send tiny headers.
	}
}

// LoadHTTPTimeouts builds an HTTPTimeouts from environment variable
// overrides on top of the supplied base. Designed to be called as:
//
//	cfg.HTTPTimeouts = platform.LoadHTTPTimeouts(platform.DefaultHTTPTimeouts())
//
// Recognised env vars:
//
//	KAPP_HTTP_READ_HEADER_TIMEOUT  -> ReadHeader  (time.Duration)
//	KAPP_HTTP_READ_TIMEOUT         -> Read        (time.Duration)
//	KAPP_HTTP_WRITE_TIMEOUT        -> Write       (time.Duration)
//	KAPP_HTTP_IDLE_TIMEOUT         -> Idle        (time.Duration)
//	KAPP_HTTP_MAX_HEADER_BYTES     -> MaxHeaderBytes (int, bytes)
//
// Duration values are parsed with time.ParseDuration ("60s", "2m",
// "1500ms"). MaxHeaderBytes is a plain integer count of bytes. Invalid
// values fall back to the base and emit a stderr warning so an operator
// can see the override was rejected at boot time.
func LoadHTTPTimeouts(base HTTPTimeouts) HTTPTimeouts {
	out := base
	out.ReadHeader = parseDurationEnv("KAPP_HTTP_READ_HEADER_TIMEOUT", base.ReadHeader)
	// Read/Write can be intentionally set to 0 by a long-stream
	// server, so we honour an explicit "0" or "0s" value. Anything
	// negative or unparseable falls back to base.
	out.Read = parseDurationEnvAllowZero("KAPP_HTTP_READ_TIMEOUT", base.Read)
	out.Write = parseDurationEnvAllowZero("KAPP_HTTP_WRITE_TIMEOUT", base.Write)
	out.Idle = parseDurationEnv("KAPP_HTTP_IDLE_TIMEOUT", base.Idle)
	if raw := os.Getenv("KAPP_HTTP_MAX_HEADER_BYTES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			out.MaxHeaderBytes = n
		} else {
			fmt.Fprintf(os.Stderr, "platform: KAPP_HTTP_MAX_HEADER_BYTES=%q invalid; using %d\n", raw, base.MaxHeaderBytes)
		}
	}
	return out
}

// Apply stamps the timeout fields onto the supplied http.Server. Call
// this BEFORE srv.ListenAndServe — net/http reads these fields on
// every accepted connection so a post-Serve mutation is racy. The
// existing http.Handler / Addr / TLSConfig fields are left untouched.
func (t HTTPTimeouts) Apply(srv *http.Server) {
	srv.ReadHeaderTimeout = t.ReadHeader
	srv.ReadTimeout = t.Read
	srv.WriteTimeout = t.Write
	srv.IdleTimeout = t.Idle
	if t.MaxHeaderBytes > 0 {
		srv.MaxHeaderBytes = t.MaxHeaderBytes
	}
}

// parseDurationEnv returns the parsed duration for the named env var,
// or the fallback when the var is unset, empty, or invalid. A non-
// positive parsed value also falls back: most timeouts are unsafe at
// zero (would let slow-loris hold the connection forever), so the
// caller signals "unlimited is fine" via parseDurationEnvAllowZero.
func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "platform: %s=%q invalid duration; using %s\n", key, raw, fallback)
		return fallback
	}
	return d
}

// parseDurationEnvAllowZero is parseDurationEnv but accepts zero (used
// by long-stream servers that explicitly want WriteTimeout=0). Negative
// values still fall back so a typo like "-5s" doesn't silently disable
// the timeout.
func parseDurationEnvAllowZero(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "platform: %s=%q invalid duration; using %s\n", key, raw, fallback)
		return fallback
	}
	return d
}
