// Package attachments wires inbound-email attachments into the
// platform's content-addressable file store (internal/files) and
// adds a virus-scanning hook.
//
// Design rationale:
//
//   - The file storage layer is delegated to `internal/files` so
//     attachments share the SHA-256 dedup + S3 / MinIO backend the
//     rest of the platform already uses. We do NOT introduce a new
//     storage abstraction; the helpdesk surface is a thin wrapper
//     that records the (message → file) link plus virus scan state.
//
//   - The virus scanner is an optional plug-in. When
//     KAPP_HELPDESK_VIRUS_SCAN_URL is unset, the scanner returns
//     "clean" for every payload (permit-all) and the boot log
//     emits a clear warning so the operator knows the security
//     gate is off. When the URL is set, the scanner POSTs the
//     payload and reads the verdict from the response.
//
//   - The HTTP scanner is shaped to the ClamAV REST API (POST
//     payload bytes, response JSON `{"verdict": "clean"|"infected",
//     "name": "..."}`) but is generic enough that any scanner
//     fronted by a REST endpoint can be swapped in. A 5xx from the
//     scanner is treated as scanner-unavailable, NOT
//     attachment-clean — the inbound email path fails closed so an
//     attacker can't trip the scanner offline and slip an infected
//     attachment through.
package attachments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Verdict is the scanner's per-payload decision.
type Verdict string

const (
	// VerdictClean — the scanner inspected the payload and
	// found no signatures.
	VerdictClean Verdict = "clean"

	// VerdictInfected — the scanner matched a virus signature.
	// The Detail field on the result carries the signature name
	// for the audit log.
	VerdictInfected Verdict = "infected"

	// VerdictSkipped — the scanner was not configured. The
	// attachment is admitted on the trust assumption that the
	// upstream MTA / IMAP server already screens, but the
	// "skipped" verdict is recorded so the audit log
	// distinguishes "we checked and it's clean" from "we never
	// checked".
	VerdictSkipped Verdict = "skipped"
)

// ScanResult bundles the verdict with optional diagnostic detail.
// The detail string carries the signature name on
// VerdictInfected so the agent UI can show "Quarantined: EICAR-
// Test-Signature" rather than just "infected".
type ScanResult struct {
	Verdict Verdict
	Detail  string
}

// Scanner is the virus-scanning interface. Implementations:
//
//   - permitAllScanner — no-op, returns VerdictSkipped.
//   - httpScanner — POSTs to a REST endpoint.
//
// Production callers wire one of these via NewScanner; tests inject
// fakes directly.
type Scanner interface {
	// Scan inspects the payload bytes and returns the verdict.
	// Errors are reserved for scanner-unavailable scenarios
	// (network, 5xx); a clean or infected verdict is always a
	// successful return.
	Scan(ctx context.Context, filename, contentType string, data []byte) (ScanResult, error)
}

// NewScanner wires a scanner from operator configuration. Empty
// URL → permitAll. Non-empty URL → http scanner with a 10-second
// per-request timeout (the helpdesk inbound path is synchronous;
// blocking it on a slow scanner is a DoS amplifier).
//
// The returned bool indicates whether a real scanner was wired —
// callers use this to surface a boot-time warning when scanning is
// off.
func NewScanner(url string, timeout time.Duration) (Scanner, bool) {
	url = strings.TrimSpace(url)
	if url == "" {
		return permitAllScanner{}, false
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &httpScanner{
		url:    url,
		client: &http.Client{Timeout: timeout},
	}, true
}

// permitAllScanner is the default when no URL is configured. Every
// scan returns VerdictSkipped — the attachment is admitted but the
// audit log records that no scan happened.
type permitAllScanner struct{}

// Scan implements Scanner by returning VerdictSkipped for every
// payload. Used when no scanner URL is configured.
func (permitAllScanner) Scan(_ context.Context, _, _ string, _ []byte) (ScanResult, error) {
	return ScanResult{Verdict: VerdictSkipped, Detail: "no scanner configured"}, nil
}

// httpScanner POSTs the payload to a REST endpoint and reads the
// verdict from the JSON response. The endpoint is expected to
// accept `application/octet-stream` request bodies and respond
// with `{"verdict": "clean"|"infected", "name": "<sig>"}`.
//
// The `X-Helpdesk-Filename` and `X-Helpdesk-Content-Type` headers
// are surfaced so the scanner can apply per-type rules (e.g. skip
// PNGs, deep-scan ZIPs). ClamAV REST + similar gateways read these
// headers; scanners that don't know about them ignore the headers
// and scan the body as-is.
type httpScanner struct {
	url    string
	client *http.Client
}

// Scan POSTs the payload to the scanner URL and decodes the
// verdict from the JSON response. Implements Scanner.
func (s *httpScanner) Scan(ctx context.Context, filename, contentType string, data []byte) (ScanResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(data))
	if err != nil {
		return ScanResult{}, fmt.Errorf("attachments: build scan request: %w", err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Helpdesk-Content-Type", contentType)
	if filename != "" {
		req.Header.Set("X-Helpdesk-Filename", filename)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return ScanResult{}, fmt.Errorf("attachments: scan request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		// Fail closed on scanner-unavailable — surfaces as
		// 5xx from the inbound-email path, relay retries.
		// An attacker who can DoS the scanner cannot use the
		// outage to admit infected payloads.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return ScanResult{}, fmt.Errorf("attachments: scanner status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		// 4xx from the scanner — payload was rejected by the
		// scanner itself (request shape error, oversize, etc).
		// Treat as a scanner failure so the inbound path
		// retries; do NOT silently admit.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return ScanResult{}, fmt.Errorf("attachments: scanner rejected request status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		Verdict string `json:"verdict"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&decoded); err != nil {
		return ScanResult{}, fmt.Errorf("attachments: decode scan response: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(decoded.Verdict)) {
	case "clean", "ok":
		return ScanResult{Verdict: VerdictClean, Detail: decoded.Name}, nil
	case "infected", "virus", "malicious":
		return ScanResult{Verdict: VerdictInfected, Detail: decoded.Name}, nil
	default:
		return ScanResult{}, fmt.Errorf("attachments: scanner returned unknown verdict %q", decoded.Verdict)
	}
}

// ErrInfected is returned by the Attacher when a scan verdict is
// VerdictInfected. The caller surfaces this as a permanent failure
// (no retry by the relay); the offending payload is recorded in
// email_attachments with verdict='infected' for the audit trail.
var ErrInfected = errors.New("attachments: payload infected")
