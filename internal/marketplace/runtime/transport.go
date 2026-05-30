package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
type InMemoryTransport struct {
	// Handler is called for every Send. Returns the response or
	// an error. Receives the canonical request shape (headers +
	// body) the production transport would have sent.
	Handler func(ctx context.Context, target string, body []byte, headers map[string]string) (*DispatchResponse, error)

	// Audit captures every Send call for assertion.
	Audit []InMemoryDispatch
}

// InMemoryDispatch is one entry in InMemoryTransport.Audit.
type InMemoryDispatch struct {
	Target  string
	Body    []byte
	Headers map[string]string
	Timeout time.Duration
}

// Send records the call in the audit log, then delegates to
// Handler.
func (t *InMemoryTransport) Send(ctx context.Context, target string, body []byte, headers map[string]string, timeout time.Duration) (*DispatchResponse, error) {
	// Defensive copy of headers so the handler can't mutate the
	// caller's map.
	hdr := make(map[string]string, len(headers))
	for k, v := range headers {
		hdr[k] = v
	}
	t.Audit = append(t.Audit, InMemoryDispatch{
		Target:  target,
		Body:    append([]byte(nil), body...),
		Headers: hdr,
		Timeout: timeout,
	})
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
func SequenceHandler(responses []*DispatchResponse, errors []error) func(ctx context.Context, target string, body []byte, headers map[string]string) (*DispatchResponse, error) {
	idx := 0
	return func(ctx context.Context, _ string, _ []byte, _ map[string]string) (*DispatchResponse, error) {
		var (
			resp *DispatchResponse
			err  error
		)
		if len(responses) > 0 {
			i := idx
			if i >= len(responses) {
				i = len(responses) - 1
			}
			resp = responses[i]
		}
		if len(errors) > 0 {
			i := idx
			if i >= len(errors) {
				i = len(errors) - 1
			}
			err = errors[i]
		}
		idx++
		return resp, err
	}
}
