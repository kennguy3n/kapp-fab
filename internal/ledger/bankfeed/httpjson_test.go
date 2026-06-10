package bankfeed

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc lets a test serve canned HTTP responses without a
// network round-trip. It satisfies httpDoer.
type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// jsonResponse builds an *http.Response with the given status and body.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestPostJSONDecodesSuccess(t *testing.T) {
	var captured *http.Request
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = req
		return jsonResponse(200, `{"value":"ok"}`), nil
	})
	var out struct {
		Value string `json:"value"`
	}
	err := postJSON(context.Background(), doer, "https://x/y",
		map[string]string{"X-Test": "1"}, map[string]any{"a": 1}, &out)
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if out.Value != "ok" {
		t.Fatalf("value = %q; want ok", out.Value)
	}
	if captured.Header.Get("Content-Type") != "application/json" {
		t.Errorf("missing content-type header")
	}
	if captured.Header.Get("X-Test") != "1" {
		t.Errorf("custom header not forwarded")
	}
	// Body must be valid JSON of the request payload.
	b, _ := io.ReadAll(captured.Body)
	if !bytes.Contains(b, []byte(`"a":1`)) {
		t.Errorf("request body = %s; want it to encode payload", b)
	}
}

func TestPostJSONNon2xxReturnsError(t *testing.T) {
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(403, `{"error":"forbidden"}`), nil
	})
	err := postJSON(context.Background(), doer, "https://x", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v; want a 403 error", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v; want it to include the body snippet", err)
	}
}

func TestGetJSONDecodesSuccess(t *testing.T) {
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Errorf("method = %s; want GET", req.Method)
		}
		return jsonResponse(200, `{"n":7}`), nil
	})
	var out struct {
		N int `json:"n"`
	}
	if err := getJSON(context.Background(), doer, "https://x", nil, &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if out.N != 7 {
		t.Fatalf("n = %d; want 7", out.N)
	}
}

func TestDeleteResourceAccepts204(t *testing.T) {
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodDelete {
			t.Errorf("method = %s; want DELETE", req.Method)
		}
		return jsonResponse(204, ``), nil
	})
	if err := deleteResource(context.Background(), doer, "https://x", nil); err != nil {
		t.Fatalf("deleteResource: %v", err)
	}
}

func TestDeleteResourceNon2xx(t *testing.T) {
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(500, `boom`), nil
	})
	if err := deleteResource(context.Background(), doer, "https://x", nil); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestSnippetBounds(t *testing.T) {
	long := strings.Repeat("a", 1000)
	got := snippet([]byte(long))
	if len([]rune(got)) > 520 { // 512 + ellipsis
		t.Fatalf("snippet too long: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long snippet should be truncated with ellipsis")
	}
	if s := snippet([]byte("short")); s != "short" {
		t.Errorf("short snippet = %q; want unchanged", s)
	}
}

func TestSnippetRedactsCredentials(t *testing.T) {
	body := `{"error":"bad","access_token":"access-sandbox-abc123","secret":"sssh","client_id":"cid-9","note":"keep me"}`
	got := snippet([]byte(body))
	for _, leaked := range []string{"access-sandbox-abc123", "sssh", "cid-9"} {
		if strings.Contains(got, leaked) {
			t.Errorf("snippet leaked secret %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("snippet should mark redactions: %s", got)
	}
	// Non-sensitive fields are preserved for diagnosability.
	if !strings.Contains(got, "keep me") {
		t.Errorf("snippet dropped non-sensitive field: %s", got)
	}
}
