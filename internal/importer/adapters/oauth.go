package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/singleflight"
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

// oauthTokenCache memoizes access tokens minted via the refresh-token
// grant so a single import run performs at most one grant per
// connection.
//
// The pipeline drives an import as Discover then Export on the *same*
// adapter instance, passing the same immutable job config to both.
// Without memoization each phase independently runs a refresh-token
// grant; because QuickBooks and Sage rotate the refresh token on every
// exchange, Discover's grant invalidates the refresh token still sitting
// in the config, so Export's grant is rejected and the refresh-token-only
// flow never completes. Reusing Discover's freshly-minted (and still
// valid) access token for Export sidesteps the second grant entirely.
//
// Entries are keyed by a hash of the connection's secret material
// (token_url + client_id + refresh_token), so a cached access token is
// never handed to a different connection/tenant. Access tokens are
// short-lived; entries carry the provider-reported expiry and expired
// entries are dropped on the next access, bounding the cache to roughly
// the set of actively-importing connections.
type oauthTokenCache struct {
	mu      sync.Mutex
	entries map[string]oauthCacheEntry
	// group collapses concurrent grants for the same connection into a
	// single refresh-token exchange (see resolve).
	group singleflight.Group
}

// oauthResolveResult is the shared payload singleflight hands to every
// caller that joined one grant.
type oauthResolveResult struct {
	token   string
	rotated bool
}

// oauthCacheEntry is one memoized access token and the instant after
// which it must no longer be reused.
type oauthCacheEntry struct {
	accessToken string
	expiresAt   time.Time
}

// oauthTokenExpirySkew is shaved off the provider-reported lifetime so a
// token is never reused right up to its expiry instant.
const oauthTokenExpirySkew = 30 * time.Second

// oauthBridgeTTL is the fallback lifetime used when the token endpoint
// does not advertise expires_in. It only needs to outlast the gap
// between Discover and Export within one run; every real provider here
// reports a lifetime well above this.
const oauthBridgeTTL = 60 * time.Second

// resolve returns a bearer token for cfg, reusing a still-valid access
// token minted earlier in the same run and otherwise performing a
// refresh-token grant. rotated reports whether the underlying grant
// returned a new refresh token, so the caller can surface a "persist the
// new refresh_token" note; a cache hit never rotates.
//
// Concurrent calls for the same connection are collapsed via singleflight
// so only one grant runs: a naive check-then-refresh has a window where two
// goroutines both miss the cache and both exchange the refresh token, and
// because QuickBooks/Sage rotate it on every exchange the second grant
// fails with invalid_grant. The leader performs the grant and populates the
// cache; every joiner shares that result.
func (c *oauthTokenCache) resolve(ctx context.Context, client *http.Client, cfg oauth2Config) (token string, rotated bool, err error) {
	key := oauthCacheKey(cfg)
	if tok, ok := c.lookup(key, time.Now()); ok {
		return tok, false, nil
	}

	v, err, _ := c.group.Do(key, func() (any, error) {
		// A concurrent leader may have populated the cache while we
		// queued behind it; reuse its token instead of granting again.
		if tok, ok := c.lookup(key, time.Now()); ok {
			return oauthResolveResult{token: tok}, nil
		}
		tok, err := refreshOAuth2Token(ctx, client, cfg)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		c.store(key, tok.AccessToken, now.Add(oauthCacheTTL(tok)), now)
		return oauthResolveResult{
			token:   tok.AccessToken,
			rotated: tok.RefreshToken != "" && tok.RefreshToken != cfg.RefreshToken,
		}, nil
	})
	if err != nil {
		return "", false, err
	}
	res, ok := v.(oauthResolveResult)
	if !ok {
		return "", false, fmt.Errorf("oauth2: unexpected token cache result type %T", v)
	}
	return res.token, res.rotated, nil
}

// lookup returns a cached access token for key when one is present and not
// yet expired, evicting it otherwise.
func (c *oauthTokenCache) lookup(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.entries, key)
		return "", false
	}
	return entry.accessToken, true
}

// store records an access token under key with the given expiry, first
// dropping any other expired entries so a long-lived process importing many
// distinct connections does not accumulate them.
func (c *oauthTokenCache) store(key, accessToken string, expiresAt, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]oauthCacheEntry)
	}
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = oauthCacheEntry{accessToken: accessToken, expiresAt: expiresAt}
}

// oauthCacheTTL converts a token's reported lifetime into a safe cache
// duration. A skew is shaved off so a token is never served right up to its
// expiry; for short-lived tokens the skew is capped at half the lifetime so
// the cached duration stays positive while still leaving a margin. When no
// lifetime is advertised, a short fallback bridges the Discover->Export gap.
func oauthCacheTTL(tok oauth2Token) time.Duration {
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		return oauthBridgeTTL
	}
	skew := oauthTokenExpirySkew
	if skew > ttl/2 {
		skew = ttl / 2
	}
	return ttl - skew
}

// oauthCacheKey derives a non-reversible cache key from the connection's
// secret material so refresh tokens are not retained verbatim as map
// keys.
func oauthCacheKey(cfg oauth2Config) string {
	sum := sha256.Sum256([]byte(cfg.TokenURL + "\x00" + cfg.ClientID + "\x00" + cfg.RefreshToken))
	return hex.EncodeToString(sum[:])
}

// validateOAuthCreds fails fast on incomplete credentials so an operator
// gets a clear config error instead of an opaque HTTP 400 from the token
// endpoint. A standalone access token is sufficient; otherwise a
// refresh-token grant needs the refresh token plus client credentials.
// provider prefixes the error (e.g. "quickbooks").
func validateOAuthCreds(provider, accessToken, refreshToken, clientID, clientSecret string) error {
	if accessToken != "" {
		return nil
	}
	if refreshToken == "" {
		return fmt.Errorf("%s: access_token or refresh_token required", provider)
	}
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("%s: client_id and client_secret are required to use a refresh_token", provider)
	}
	return nil
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
	// A 304 Not Modified (Xero's incremental-sync "nothing changed"
	// response to If-Modified-Since) carries no body, as does any other
	// empty success. Leaving out untouched lets the caller observe zero
	// rows instead of failing on an "unexpected end of JSON input" decode.
	if out == nil || resp.StatusCode == http.StatusNotModified || len(body) == 0 {
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

// stringID coerces a source record's identifier to a string. The cloud
// providers here all return string ids today, but json.Unmarshal into
// map[string]any decodes a JSON number as float64, so a bare
// `v.(string)` would silently yield "" for a numeric id and leave the
// row with an empty SourceID — breaking reconciliation/dedup. Coercing
// numbers keeps the id stable regardless of how the provider encodes it.
func stringID(v any) string {
	switch id := v.(type) {
	case string:
		return id
	case json.Number:
		return id.String()
	case float64:
		return strconv.FormatFloat(id, 'f', -1, 64)
	default:
		return ""
	}
}

// defaultHTTPTimeout matches the Frappe adapter's 60s ceiling. Cloud
// accounting APIs are slower than a LAN ERPNext, but 60s per page is
// still a generous bound that prevents a wedged TCP connection from
// hanging an import worker indefinitely.
const defaultHTTPTimeout = 60 * time.Second
