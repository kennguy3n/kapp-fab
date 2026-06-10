package bankfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultHTTPClient is the shared client used by live providers when a
// caller does not inject one. A bounded timeout keeps a hung provider
// from stalling a sync sweep across other connections.
var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

// httpDoer is the subset of *http.Client the providers use. Declaring it
// as an interface lets tests inject a transport that serves canned
// responses without a network round-trip.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// postJSON POSTs reqBody as JSON to url with the supplied headers and
// decodes a 2xx JSON response into out. Non-2xx responses return an
// error carrying the status and a bounded prefix of the body so the
// failure is diagnosable without leaking a huge payload into logs. The
// caller is responsible for never passing secrets into the error path —
// providers pass only the API response body, which is provider-authored.
func postJSON(ctx context.Context, client httpDoer, url string, headers map[string]string, reqBody, out any) error {
	if client == nil {
		client = defaultHTTPClient
	}
	var buf bytes.Buffer
	if reqBody != nil {
		if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
			return fmt.Errorf("bankfeed: encode request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return fmt.Errorf("bankfeed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("bankfeed: http call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bankfeed: provider returned %d: %s", resp.StatusCode, snippet(body))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("bankfeed: decode response: %w", err)
		}
	}
	return nil
}

// getJSON performs a GET with the supplied headers and decodes a 2xx
// JSON body into out.
func getJSON(ctx context.Context, client httpDoer, url string, headers map[string]string, out any) error {
	if client == nil {
		client = defaultHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("bankfeed: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("bankfeed: http call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bankfeed: provider returned %d: %s", resp.StatusCode, snippet(body))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("bankfeed: decode response: %w", err)
		}
	}
	return nil
}

// deleteResource issues a DELETE and treats any 2xx (including 204) as
// success. Used for provider-side revocation (e.g. GoCardless requisition
// deletion) where there is no response body to decode.
func deleteResource(ctx context.Context, client httpDoer, url string, headers map[string]string) error {
	if client == nil {
		client = defaultHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("bankfeed: build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("bankfeed: http call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bankfeed: provider returned %d: %s", resp.StatusCode, snippet(body))
	}
	return nil
}

// snippet bounds a response body for safe inclusion in an error message.
func snippet(b []byte) string {
	const maxLen = 512
	if len(b) > maxLen {
		return string(b[:maxLen]) + "…"
	}
	return string(b)
}
