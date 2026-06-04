package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// This file holds the small amount of HTTP/OAuth2 plumbing shared by
// the cloud-accounting adapters (QuickBooks, Xero, Sage). All three
// speak OAuth2 with a refresh-token grant and return JSON, so the
// token exchange and the decode-or-error helper live here rather than
// being copy-pasted into each adapter. The file-based Tally adapter
// does not use any of this.

// oauthAuthStyle selects how client credentials are presented to the
// token endpoint during a refresh-token grant.
type oauthAuthStyle int

const (
	// authStyleHeader sends client_id/client_secret as HTTP Basic
	// auth on the Authorization header (QuickBooks, Xero).
	authStyleHeader oauthAuthStyle = iota
	// authStyleBody sends client_id/client_secret as form fields in
	// the request body (Sage).
	authStyleBody
)

// oauth2Config carries everything the shared token exchange needs.
// Adapters embed the relevant fields in their own JSON config structs
// and translate into this shape at call time, so the on-the-wire
// config shape stays adapter-specific while the exchange logic is
// shared.
type oauth2Config struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	RefreshToken string
	AuthStyle    oauthAuthStyle
}

// oauth2Token is the decoded token-endpoint response. RefreshToken is
// echoed back when the provider rotates it (QuickBooks and Sage rotate
// on every refresh; callers should persist the new value).
type oauth2Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// refreshOAuth2Token performs a refresh-token grant against cfg.TokenURL
// and returns the new token. It is provider-agnostic: the only
// per-provider variation is whether the client credentials ride in the
// Authorization header or the form body, which cfg.AuthStyle selects.
func refreshOAuth2Token(ctx context.Context, client *http.Client, cfg oauth2Config) (oauth2Token, error) {
	if cfg.TokenURL == "" {
		return oauth2Token{}, fmt.Errorf("oauth2: token_url required to refresh access token")
	}
	if cfg.RefreshToken == "" {
		return oauth2Token{}, fmt.Errorf("oauth2: refresh_token required to refresh access token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cfg.RefreshToken)
	if cfg.AuthStyle == authStyleBody {
		form.Set("client_id", cfg.ClientID)
		form.Set("client_secret", cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauth2Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if cfg.AuthStyle == authStyleHeader {
		req.SetBasicAuth(cfg.ClientID, cfg.ClientSecret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return oauth2Token{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCappedBody(resp.Body)
	if err != nil {
		return oauth2Token{}, err
	}
	if resp.StatusCode >= 400 {
		return oauth2Token{}, fmt.Errorf("oauth2: token refresh failed: HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}
	var tok oauth2Token
	if err := json.Unmarshal(body, &tok); err != nil {
		return oauth2Token{}, fmt.Errorf("oauth2: decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return oauth2Token{}, fmt.Errorf("oauth2: token response had no access_token")
	}
	return tok, nil
}

// getJSON issues a GET against target with a bearer token plus any
// extra headers, and decodes a successful response into out. It is the
// single choke point for the cloud adapters' read calls so error
// shaping (status code + truncated body) stays consistent.
func getJSON(ctx context.Context, client *http.Client, target, bearer string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCappedBody(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: HTTP %d: %s", target, resp.StatusCode, truncateBody(body))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// maxResponseBytes caps how much of a response body the cloud adapters
// buffer in memory. It guards against a misconfigured or hostile
// endpoint (reachable when an operator overrides base_url/token_url)
// streaming an unbounded body and exhausting memory; the 60s client
// timeout only bounds time, not size.
const maxResponseBytes = 64 << 20 // 64 MiB

// readCappedBody reads at most maxResponseBytes from r, returning an
// error rather than buffering an oversized body. It reads one byte past
// the cap so an exactly-at-limit body is still accepted.
func readCappedBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("response body exceeds %d byte limit", maxResponseBytes)
	}
	return body, nil
}

// truncateBody bounds an error-path response body so a multi-megabyte
// HTML error page from a misconfigured gateway does not end up in a
// job's error blob verbatim.
func truncateBody(b []byte) string {
	const maxLen = 512
	s := strings.TrimSpace(string(b))
	if len(s) <= maxLen {
		return s
	}
	// Back off to a UTF-8 rune boundary so a multi-byte character is not
	// split mid-sequence, which would leave an invalid trailing byte.
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// mergeFieldMaps layers per-entity operator overrides on top of an
// adapter's built-in source→target field map. It mirrors the Frappe
// adapter's mergedConceptMap but is parameterised on the defaults so
// each cloud adapter can supply its own table without sharing global
// state. The result is always a fresh map, so callers may mutate it.
func mergeFieldMaps(defaults, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(defaults)+len(overrides))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// defaultHTTPTimeout matches the Frappe adapter's 60s ceiling. Cloud
// accounting APIs are slower than a LAN ERPNext, but 60s per page is
// still a generous bound that prevents a wedged TCP connection from
// hanging an import worker indefinitely.
const defaultHTTPTimeout = 60 * time.Second
