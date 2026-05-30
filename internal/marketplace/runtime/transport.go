package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Transport executes a single HTTP attempt against the target URL
// with the dispatcher's headers attached. The retry loop lives in
// the Dispatcher — Transport.Send is per-attempt.
//
// The interface is small enough that tests can supply a stub
// without httptest.Server overhead (see InMemoryTransport below).
// Production wiring uses HTTPTransport which round-trips through
// net/http with timeout + body-size cap enforced.
type Transport interface {
	Send(ctx context.Context, target string, body []byte, headers map[string]string, timeout time.Duration) (*DispatchResponse, error)
}

// MaxResponseBodyBytes is the cap the HTTPTransport enforces on
// the size of the body it reads back from the extension. The cap
// is INVISIBLE to the dispatcher's caller — it sees a body that
// has been truncated to MaxResponseBodyBytes and (optionally) a
// flag in Header["X-Kapp-Truncated"]. 1 MiB is plenty for agent
// tool responses (a single PDF URL or a structured JSON reply
// fits easily); larger payloads should be uploaded to object
// storage and referenced by URL in the response body.
const MaxResponseBodyBytes = 1 << 20

// HTTPTransport is the production Transport. Wraps an *http.Client
// with a per-attempt timeout context, a 1-MiB response body cap,
// and request-body buffering so the audit log can include the
// hash on every attempt (avoiding a TeeReader for byte-counting).
type HTTPTransport struct {
	// Client is the underlying HTTP client. Tests can supply a
	// client with a TLS-skipping transport for self-signed
	// extension webhook servers. Production wiring uses
	// http.DefaultClient with a system root pool.
	Client *http.Client
}

// NewHTTPTransport returns an HTTPTransport with a sensible default
// client (system roots, http/2 enabled, redirect chain disabled —
// the dispatcher signs the configured URL, not a redirected target,
// so redirects would break signature verification on the receiver
// side).
func NewHTTPTransport() *HTTPTransport {
	return &HTTPTransport{
		Client: &http.Client{
			// Per-attempt timeout is enforced via context, not
			// here, so Client.Timeout stays 0 (unlimited). Setting
			// both would create confusing "which one fires first"
			// semantics.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Refuse redirects. A 301/302 from the extension
				// would force the dispatcher to re-sign for the
				// new URL OR replay the signature against a URL
				// that wasn't signed — both unsafe.
				return http.ErrUseLastResponse
			},
		},
	}
}

// Send issues a POST with the supplied headers and body. Returns a
// DispatchResponse on any HTTP-level outcome (even non-2xx);
// returns an error only on transport-level failures (DNS, TCP,
// TLS, timeout, malformed response).
func (t *HTTPTransport) Send(ctx context.Context, target string, body []byte, headers map[string]string, timeout time.Duration) (*DispatchResponse, error) {
	if t == nil || t.Client == nil {
		return nil, errors.New("runtime: nil HTTPTransport")
	}
	if timeout <= 0 {
		return nil, errors.New("runtime: non-positive timeout")
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("runtime: build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := t.Client.Do(req)
	latency := time.Since(start)
	if err != nil {
		// Context-deadline-exceeded surfaces here. The dispatcher
		// retry loop classifies this as a retryable transport
		// error.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", ErrDispatchTimeout, err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	// Read response with a hard cap. Anything beyond the cap is
	// discarded.
	limited := io.LimitReader(resp.Body, MaxResponseBodyBytes+1)
	respBody, readErr := io.ReadAll(limited)
	if readErr != nil {
		return nil, fmt.Errorf("runtime: read response: %w", readErr)
	}
	truncated := false
	if len(respBody) > MaxResponseBodyBytes {
		respBody = respBody[:MaxResponseBodyBytes]
		truncated = true
	}
	header := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			header[k] = vs[0]
		}
	}
	if truncated {
		header["X-Kapp-Truncated"] = "true"
	}
	return &DispatchResponse{
		Status:  resp.StatusCode,
		Body:    respBody,
		Header:  header,
		Latency: latency,
	}, nil
}

// InMemoryTransport is a Transport for tests. Holds a Handler that
// the caller supplies + a per-call audit log of the requests it
// saw. Tests assert on the audit log (header presence, body
// contents) without spinning up a httptest.Server.
//
// Safe for concurrent use by multiple goroutines. The mu mutex
// serialises audit appends and reads — see Snapshot. Devin Review
// round-5 on PR #127 caught a data race where the round-4 TOCTOU
// regression test (TestMarketplaceRuntime_Uninstall_ConcurrentRace)
// fanned two goroutines through a single InMemoryTransport's Send
// path, racing the previous unguarded `t.audit = append(...)` line.
// The fix here serialises the append; tests inspect the audit log
// via Snapshot (deep-copied view) or Len (counter) only — the
// underlying slice is unexported so direct field access cannot
// race a concurrent Send, and the type system enforces the
// contract rather than relying on a comment. Devin Review round-10
// ANALYSIS_0008 on PR #127 escalated the footgun risk to a true
// encapsulation fix here.
type InMemoryTransport struct {
	// Handler is called for every Send. Returns the response or
	// an error. Receives the canonical request shape (headers +
	// body) the production transport would have sent.
	Handler func(ctx context.Context, target string, body []byte, headers map[string]string) (*DispatchResponse, error)

	// mu guards audit. Acquired for every append in Send and for
	// every read in Snapshot/Len. The field is unexported so all
	// access goes through the mutex-guarded accessor methods —
	// there is no Audit-the-field anymore, only Snapshot()/Len()/At().
	mu sync.Mutex
	// audit captures every Send call for assertion. Protected by
	// mu. Unexported on purpose: see Snapshot/Len/At for the
	// read API. Devin Review round-10 ANALYSIS_0008.
	audit []InMemoryDispatch
}

// Snapshot returns a deep copy of the current audit slice under
// the mu lock. Use this from tests that want to inspect the audit
// log while there may still be active Send callers (or from tests
// that ran Send goroutines concurrently and want a race-free
// view). Within the snapshot each InMemoryDispatch shares the
// underlying Headers/Body byte buffers with the original entry —
// callers must not mutate them.
func (t *InMemoryTransport) Snapshot() []InMemoryDispatch {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]InMemoryDispatch, len(t.audit))
	copy(out, t.audit)
	return out
}

// Len returns the current audit length under the mu lock. Use
// from tests that just want a count without grabbing a snapshot.
func (t *InMemoryTransport) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.audit)
}

// At returns the i-th audit entry under the mu lock — a single-
// entry equivalent of Snapshot that avoids allocating a full
// slice copy when the test only needs one row (typically i == 0).
// Panics with a clear message if i is out of range; tests should
// call Len first if they're not certain. Returns a copy with the
// same shared-buffer caveat documented on Snapshot.
func (t *InMemoryTransport) At(i int) InMemoryDispatch {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i < 0 || i >= len(t.audit) {
		panic(fmt.Sprintf("runtime: InMemoryTransport.At(%d): out of range (audit len = %d)", i, len(t.audit)))
	}
	return t.audit[i]
}

// InMemoryDispatch is one entry in InMemoryTransport's audit log.
// Access via Snapshot, Len, or At — the underlying slice is
// unexported. Devin Review round-10 ANALYSIS_0008 on PR #127.
type InMemoryDispatch struct {
	Target  string
	Body    []byte
	Headers map[string]string
	Timeout time.Duration
}

// Send records the call in the audit log, then delegates to
// Handler. Safe for concurrent use — the audit-log append is
// guarded by t.mu (Devin Review round-5 on PR #127 caught the
// race when the round-4 TOCTOU regression test fanned out two
// concurrent Uninstall goroutines through one transport).
func (t *InMemoryTransport) Send(ctx context.Context, target string, body []byte, headers map[string]string, timeout time.Duration) (*DispatchResponse, error) {
	// Defensive copy of headers so the handler can't mutate the
	// caller's map.
	hdr := make(map[string]string, len(headers))
	for k, v := range headers {
		hdr[k] = v
	}
	entry := InMemoryDispatch{
		Target:  target,
		Body:    append([]byte(nil), body...),
		Headers: hdr,
		Timeout: timeout,
	}
	t.mu.Lock()
	t.audit = append(t.audit, entry)
	t.mu.Unlock()
	if t.Handler == nil {
		// Default: 200 OK with empty body. Lets tests that don't
		// care about response content omit the handler.
		return &DispatchResponse{Status: 200, Body: nil, Header: map[string]string{}, Latency: 0}, nil
	}
	return t.Handler(ctx, target, body, hdr)
}

// StaticResponseHandler returns an InMemoryTransport.Handler that
// returns the supplied status/body on every call. Convenience for
// the common "always return 200" or "always return 502" test.
func StaticResponseHandler(status int, body []byte) func(ctx context.Context, target string, body []byte, headers map[string]string) (*DispatchResponse, error) {
	return func(ctx context.Context, target string, _ []byte, _ map[string]string) (*DispatchResponse, error) {
		return &DispatchResponse{Status: status, Body: body, Header: map[string]string{}, Latency: 0}, nil
	}
}

// SequenceHandler returns an InMemoryTransport.Handler that cycles
// through the supplied responses in order. The last response in
// the sequence is repeated for any call beyond len(responses).
// Used by retry tests to simulate "first attempt fails, second
// succeeds" sequences.
//
// If both responses and errors are empty the handler returns
// (nil, nil) on every call — the dispatcher treats this as a
// transport-level failure with no response body. Devin Review
// ANALYSIS_0001 flagged a latent -1 index when responses was
// empty; the explicit guard below removes the foot-gun for any
// future caller that constructs a SequenceHandler from a
// dynamically-built slice.
//
// The closure is safe for concurrent calls. The internal cursor
// is an atomic.Int64 — Devin Review round-5 on PR #127 flagged
// the previous `idx := 0` capture as racy against any future test
// that fans a SequenceHandler across multiple goroutines.
// FetchAdd(1)-then-use makes "which entry does this call see"
// well-defined even under contention: two concurrent callers will
// receive responses at distinct indices in some interleaving.
// Order is not promised, but no caller can ever observe a torn
// idx read.
func SequenceHandler(responses []*DispatchResponse, errors []error) func(ctx context.Context, target string, body []byte, headers map[string]string) (*DispatchResponse, error) {
	var idx atomic.Int64
	return func(ctx context.Context, _ string, _ []byte, _ map[string]string) (*DispatchResponse, error) {
		cur := int(idx.Add(1) - 1)
		var (
			resp *DispatchResponse
			err  error
		)
		if len(responses) > 0 {
			i := cur
			if i >= len(responses) {
				i = len(responses) - 1
			}
			resp = responses[i]
		}
		if len(errors) > 0 {
			i := cur
			if i >= len(errors) {
				i = len(errors) - 1
			}
			err = errors[i]
		}
		return resp, err
	}
}
