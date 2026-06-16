package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultJWKSRefreshInterval is how long a fetched JWKS document is
// considered fresh before a background refresh replaces it. Five
// minutes balances key-rotation responsiveness against load on the
// iam-core JWKS endpoint: a rotated key is picked up within this
// window without an explicit cache-miss fetch, and 5000 tenants
// sharing one issuer hit the endpoint at most ~12x/hour per API
// replica.
const DefaultJWKSRefreshInterval = 5 * time.Minute

// jwksMinRefreshInterval rate-limits the on-demand (cache-miss)
// refresh path so a burst of tokens carrying an unknown kid — e.g. an
// attacker spraying random kids, or a thundering herd right after a
// rotation — cannot turn into a fetch storm against iam-core. At most
// one cache-miss fetch per this interval; misses in between serve the
// existing cache (and therefore fail closed for genuinely unknown
// kids).
const jwksMinRefreshInterval = 10 * time.Second

// jwksValidAlgs is the allow-list of asymmetric JWS algorithms the
// validator will verify. HS* and "none" are deliberately excluded:
// accepting an HMAC algorithm here would open the classic algorithm-
// confusion attack where an attacker signs a forged token with the
// RSA/EC *public* key (which is, by definition, public) used as an
// HMAC secret. The validator therefore refuses to even look at a
// token whose header alg is not in this set.
var jwksValidAlgs = map[string]bool{
	"RS256": true, "RS384": true, "RS512": true,
	"ES256": true, "ES384": true, "ES512": true,
}

// JWKSValidatorConfig configures a JWKSValidator. Issuer is the only
// required field; the rest have safe defaults.
type JWKSValidatorConfig struct {
	// Issuer is the iam-core base URL (no trailing slash). It is
	// matched verbatim against the token's `iss` claim — a token
	// whose issuer does not equal this value is rejected even if its
	// signature verifies against a cached key, so one validator only
	// ever trusts one issuer.
	Issuer string
	// Audience, when non-empty, must appear in the token's `aud`
	// claim. Leaving it empty disables the audience check (useful in
	// tests); production deployments should set it to the API's
	// configured audience so tokens minted for a different client
	// cannot be replayed against Kapp.
	Audience string
	// JWKSURL overrides the derived `{Issuer}/.well-known/jwks.json`
	// endpoint. Primarily a test seam.
	JWKSURL string
	// RefreshInterval is the freshness window for the cached JWKS.
	// Zero selects DefaultJWKSRefreshInterval.
	RefreshInterval time.Duration
	// Leeway tolerates clock skew on the exp/nbf checks. Zero is
	// allowed (no skew tolerance).
	Leeway time.Duration
	// HTTPClient fetches the JWKS document. Zero selects a client
	// with a 10s timeout.
	HTTPClient *http.Client
	// Logger receives refresh-failure warnings. Zero selects
	// slog.Default().
	Logger *slog.Logger
	// now is a clock seam for tests. Zero selects time.Now.
	now func() time.Time
}

// JWKSValidator validates iam-core RS256/ES256 JWTs against public
// keys fetched from the issuer's JWKS endpoint. The key set is cached
// in memory and refreshed on a timer (and, rate-limited, on cache
// miss). Verify never blocks on the network when the signing key is
// already cached, which is the steady-state hot path.
//
// A JWKSValidator is safe for concurrent use.
type JWKSValidator struct {
	issuer          string
	audience        string
	jwksURL         string
	refreshInterval time.Duration
	leeway          time.Duration
	httpClient      *http.Client
	logger          *slog.Logger
	now             func() time.Time

	mu          sync.RWMutex
	keys        map[string]crypto.PublicKey // kid -> public key
	fetchedAt   time.Time
	refreshing  bool
	lastAttempt time.Time
}

// NewJWKSValidator constructs a validator. It does not perform any
// network I/O — keys are fetched lazily on first Verify (or eagerly
// if the caller invokes Refresh / Start). An empty Issuer is a
// configuration error.
func NewJWKSValidator(cfg JWKSValidatorConfig) (*JWKSValidator, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("auth: jwks validator requires an issuer")
	}
	issuer := strings.TrimRight(cfg.Issuer, "/")
	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		jwksURL = issuer + "/.well-known/jwks.json"
	}
	refresh := cfg.RefreshInterval
	if refresh <= 0 {
		refresh = DefaultJWKSRefreshInterval
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := cfg.now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &JWKSValidator{
		issuer:          issuer,
		audience:        cfg.Audience,
		jwksURL:         jwksURL,
		refreshInterval: refresh,
		leeway:          cfg.Leeway,
		httpClient:      httpClient,
		logger:          logger,
		now:             nowFn,
		keys:            map[string]crypto.PublicKey{},
	}, nil
}

// Issuer returns the issuer this validator trusts. Used by the
// dual-issuer MultiVerifier to route tokens by their `iss` claim.
func (v *JWKSValidator) Issuer() string { return v.issuer }

// Start launches a background goroutine that refreshes the JWKS on
// the configured interval until ctx is cancelled. It performs one
// eager refresh before returning so a freshly-booted process has keys
// cached before the first request (the error is logged, not fatal:
// transient JWKS unavailability at boot must not wedge the process,
// and Verify will retry on demand). Calling Start is optional —
// Verify lazily fetches on cache miss regardless.
func (v *JWKSValidator) Start(ctx context.Context) {
	if err := v.Refresh(ctx); err != nil {
		v.logger.Warn("jwks initial refresh failed; will retry on demand",
			slog.String("issuer", v.issuer),
			slog.String("err", err.Error()),
		)
	}
	go func() {
		ticker := time.NewTicker(v.refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := v.Refresh(ctx); err != nil {
					v.logger.Warn("jwks background refresh failed; serving cached keys",
						slog.String("issuer", v.issuer),
						slog.String("err", err.Error()),
					)
				}
			}
		}
	}()
}

// Refresh fetches the JWKS document and atomically replaces the
// cached key set on success. On failure the previous cache is left
// untouched (serve-stale) and the error is returned for the caller to
// log. Concurrent Refresh calls are coalesced so a refresh storm
// collapses to a single in-flight fetch.
func (v *JWKSValidator) Refresh(ctx context.Context) error {
	v.mu.Lock()
	if v.refreshing {
		v.mu.Unlock()
		return nil // another goroutine is already fetching
	}
	v.refreshing = true
	v.lastAttempt = v.now()
	v.mu.Unlock()

	defer func() {
		v.mu.Lock()
		v.refreshing = false
		v.mu.Unlock()
	}()

	keys, err := v.fetchKeys(ctx)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = v.now()
	v.mu.Unlock()
	return nil
}

// Verify validates a compact-JWS iam-core token and maps its claims
// onto Kapp's Claims struct. It enforces, in order: a parseable
// header with an allow-listed asymmetric alg; a known signing key
// (kid); a valid signature; the issuer; the audience (when
// configured); and the exp/nbf time window. Only after every check
// passes are claims mapped — TenantID (and UserID) must resolve to a
// Kapp UUID or the token is rejected, because TenantID is the
// load-bearing RLS claim.
func (v *JWKSValidator) Verify(token string) (*Claims, error) {
	raw, err := v.verifySignatureAndDecode(token)
	if err != nil {
		return nil, err
	}
	return v.mapClaims(raw)
}

// verifySignatureAndDecode performs the cryptographic half of token
// validation shared by Verify (access tokens) and VerifyIDToken (OIDC
// id_tokens): it parses the header, enforces the asymmetric-alg
// allow-list, resolves the signing key by kid, verifies the
// signature, and returns the decoded — but not yet semantically
// validated — claim set. Issuer/audience/expiry checks are left to
// the caller because they differ between access tokens (aud = API
// audience) and id_tokens (aud = client_id, plus nonce binding).
func (v *JWKSValidator) verifySignatureAndDecode(token string) (map[string]json.RawMessage, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrTokenInvalid
	}
	header, err := decodeJWTHeader(parts[0])
	if err != nil {
		return nil, err
	}
	if !jwksValidAlgs[header.Alg] {
		// Fail closed on HS*/none/unknown: see jwksValidAlgs.
		return nil, fmt.Errorf("%w: unsupported alg %q", ErrTokenSignature, header.Alg)
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTokenSignature, err)
	}
	key, err := v.keyForKID(header.KID)
	if err != nil {
		return nil, err
	}
	if err := verifyASymSignature(header.Alg, key, signingInput, sig); err != nil {
		return nil, err
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}
	return raw, nil
}

// IDTokenClaims is the validated OIDC id_token profile the OAuth2
// authorization-code flow needs to identify the authenticated user.
// It is deliberately separate from Claims: an id_token proves *who*
// authenticated at iam-core, whereas Claims encodes the Kapp session
// (tenant, roles, session id) that the callback mints afterwards by
// resolving the user against Kapp's own membership tables.
type IDTokenClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	GivenName     string
	FamilyName    string
	Picture       string
	OrgID         string
	// Raw exposes every claim for callers that need a custom claim
	// (e.g. kapp_user_id / kapp_tenant_id) the struct does not name.
	Raw map[string]json.RawMessage
}

// VerifyIDToken validates an OIDC id_token returned from the token
// endpoint: signature (via JWKS), issuer, audience == clientID, the
// nonce binding (when expectedNonce is non-empty, it must equal the
// token's nonce — this is what defeats id_token replay/injection),
// and the exp/nbf window. It returns the profile on success.
//
// expectedNonce should be the nonce the client generated for this
// authorization request; pass "" only when the flow did not use a
// nonce (not recommended for interactive logins).
func (v *JWKSValidator) VerifyIDToken(idToken, clientID, expectedNonce string) (*IDTokenClaims, error) {
	raw, err := v.verifySignatureAndDecode(idToken)
	if err != nil {
		return nil, err
	}
	if iss := jsonString(raw["iss"]); iss != v.issuer {
		return nil, fmt.Errorf("%w: id_token issuer mismatch", ErrTokenInvalid)
	}
	if clientID != "" && !containsString(jsonStringOrSlice(raw["aud"]), clientID) {
		return nil, fmt.Errorf("%w: id_token audience mismatch", ErrTokenInvalid)
	}
	if expectedNonce != "" && jsonString(raw["nonce"]) != expectedNonce {
		return nil, fmt.Errorf("%w: id_token nonce mismatch", ErrTokenInvalid)
	}
	// Reuse Claims.Valid only for the time window; build a throwaway
	// with the parsed exp/nbf and a sentinel uid/tid so the required-
	// field checks pass (identity validity is asserted by the OIDC
	// checks above, not by Claims.Valid).
	exp := jsonInt64(raw["exp"])
	nbf := jsonInt64(raw["nbf"])
	if err := validTimeWindow(v.now(), v.leeway, exp, nbf); err != nil {
		return nil, err
	}
	return &IDTokenClaims{
		Subject:       jsonString(raw["sub"]),
		Email:         jsonString(raw["email"]),
		EmailVerified: jsonBool(raw["email_verified"]),
		Name:          jsonString(raw["name"]),
		GivenName:     jsonString(raw["given_name"]),
		FamilyName:    jsonString(raw["family_name"]),
		Picture:       jsonString(raw["picture"]),
		OrgID:         jsonString(raw["org_id"]),
		Raw:           raw,
	}, nil
}

// keyForKID returns the cached public key for kid, triggering a
// rate-limited on-demand refresh when the kid is unknown or the cache
// is stale. An empty kid matches the sole cached key when exactly one
// is present (some issuers omit kid when they publish a single key);
// otherwise an empty kid is ambiguous and rejected.
func (v *JWKSValidator) keyForKID(kid string) (crypto.PublicKey, error) {
	if key, ok := v.lookupKey(kid); ok {
		return key, nil
	}
	// Cache miss: the key may be newly rotated in. Attempt a single
	// rate-limited refresh, then look again.
	v.maybeRefreshOnMiss()
	if key, ok := v.lookupKey(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: unknown signing key", ErrTokenSignature)
}

func (v *JWKSValidator) lookupKey(kid string) (crypto.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if kid != "" {
		key, ok := v.keys[kid]
		return key, ok
	}
	if len(v.keys) == 1 {
		for _, key := range v.keys {
			return key, true
		}
	}
	return nil, false
}

// maybeRefreshOnMiss performs a synchronous refresh when the cache is
// older than jwksMinRefreshInterval, swallowing the error (the caller
// re-checks the cache and fails closed if the key is still absent).
// The rate-limit prevents unknown-kid sprays from hammering iam-core.
func (v *JWKSValidator) maybeRefreshOnMiss() {
	v.mu.RLock()
	recent := v.now().Sub(v.lastAttempt) < jwksMinRefreshInterval
	v.mu.RUnlock()
	if recent {
		return
	}
	if err := v.Refresh(context.Background()); err != nil {
		v.logger.Warn("jwks on-demand refresh failed; failing closed for unknown kid",
			slog.String("issuer", v.issuer),
			slog.String("err", err.Error()),
		)
	}
}

func (v *JWKSValidator) mapClaims(raw map[string]json.RawMessage) (*Claims, error) {
	if iss := jsonString(raw["iss"]); iss != v.issuer {
		return nil, fmt.Errorf("%w: issuer mismatch", ErrTokenInvalid)
	}
	aud := jsonStringOrSlice(raw["aud"])
	if v.audience != "" && !containsString(aud, v.audience) {
		return nil, fmt.Errorf("%w: audience mismatch", ErrTokenInvalid)
	}
	c := &Claims{
		Issuer:    v.issuer,
		Email:     jsonString(raw["email"]),
		OrgID:     jsonString(raw["org_id"]),
		JWTID:     jsonString(raw["jti"]),
		ExpiresAt: jsonInt64(raw["exp"]),
		IssuedAt:  jsonInt64(raw["iat"]),
		NotBefore: jsonInt64(raw["nbf"]),
	}
	if len(aud) > 0 {
		if v.audience != "" {
			c.Audience = v.audience
		} else {
			c.Audience = aud[0]
		}
	}
	// Tenancy: kapp_tenant_id is the custom claim iam-core stamps on
	// every Kapp-provisioned tenant's tokens (see internal/tenant/
	// iam_sync.go). It is the load-bearing RLS claim, so a missing or
	// unparseable value is fatal — we never fall back to a zero tenant.
	tid, err := uuid.Parse(jsonString(raw["kapp_tenant_id"]))
	if err != nil {
		return nil, fmt.Errorf("%w: missing or invalid kapp_tenant_id", ErrTokenInvalid)
	}
	c.TenantID = tid
	// Identity: prefer the explicit kapp_user_id custom claim; fall
	// back to `sub` when it is itself a Kapp UUID (deployments that
	// register the Kapp user id as the iam-core subject). A token we
	// cannot tie to a Kapp user is rejected — UserID is required by
	// Claims.Valid and is stamped into audit/`created_by`.
	if uid, err := uuid.Parse(jsonString(raw["kapp_user_id"])); err == nil {
		c.UserID = uid
	} else if uid, err := uuid.Parse(jsonString(raw["sub"])); err == nil {
		c.UserID = uid
	} else {
		return nil, fmt.Errorf("%w: missing or invalid kapp_user_id", ErrTokenInvalid)
	}
	c.Roles = v.namespacedStrings(raw, "roles")
	c.Permissions = v.namespacedStrings(raw, "permissions")
	if err := c.Valid(v.now(), v.leeway); err != nil {
		return nil, err
	}
	return c, nil
}

// namespacedStrings reads an authz claim that iam-core emits under
// the namespaced key `{issuer}/{name}` (e.g. "https://auth.example.com/roles"),
// falling back to a plain `{name}` claim. Both string-array and
// space-delimited-string encodings are accepted.
func (v *JWKSValidator) namespacedStrings(raw map[string]json.RawMessage, name string) []string {
	if val, ok := raw[v.issuer+"/"+name]; ok {
		if s := jsonStringSlice(val); len(s) > 0 {
			return s
		}
	}
	return jsonStringSlice(raw[name])
}

// --- JWK parsing -----------------------------------------------------

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"` // RSA modulus
	E   string `json:"e"` // RSA exponent
	Crv string `json:"crv"`
	X   string `json:"x"` // EC x
	Y   string `json:"y"` // EC y
}

func (v *JWKSValidator) fetchKeys(ctx context.Context) (map[string]crypto.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("auth: build jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: fetch jwks: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("auth: decode jwks: %w", err)
	}
	out := make(map[string]crypto.PublicKey, len(doc.Keys))
	for i := range doc.Keys {
		k := &doc.Keys[i]
		if k.Use != "" && k.Use != "sig" {
			continue // encryption keys are not signing keys
		}
		pub, err := k.publicKey()
		if err != nil {
			// Skip individual malformed keys rather than failing the
			// whole refresh — one bad key must not blind us to the
			// good ones alongside it.
			v.logger.Warn("jwks: skipping unparseable key",
				slog.String("kid", k.Kid),
				slog.String("err", err.Error()),
			)
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("auth: jwks contained no usable signing keys")
	}
	return out, nil
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		nBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
		if err != nil {
			return nil, fmt.Errorf("rsa modulus: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
		if err != nil {
			return nil, fmt.Errorf("rsa exponent: %w", err)
		}
		if len(nBytes) == 0 || len(eBytes) == 0 {
			return nil, errors.New("rsa key missing modulus or exponent")
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		if e < 2 {
			return nil, errors.New("rsa exponent too small")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported ec curve %q", k.Crv)
		}
		xBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.X, "="))
		if err != nil {
			return nil, fmt.Errorf("ec x: %w", err)
		}
		yBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.Y, "="))
		if err != nil {
			return nil, fmt.Errorf("ec y: %w", err)
		}
		x := new(big.Int).SetBytes(xBytes)
		y := new(big.Int).SetBytes(yBytes)
		if !curve.IsOnCurve(x, y) {
			return nil, errors.New("ec point not on curve")
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("unsupported kty %q", k.Kty)
	}
}

// --- signature verification -----------------------------------------

func verifyASymSignature(alg string, key crypto.PublicKey, signingInput string, sig []byte) error {
	hashed, hashFn := hashForAlg(alg, signingInput)
	switch alg[:2] {
	case "RS":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: key type mismatch for %s", ErrTokenSignature, alg)
		}
		if err := rsa.VerifyPKCS1v15(pub, hashFn, hashed, sig); err != nil {
			return ErrTokenSignature
		}
		return nil
	case "ES":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: key type mismatch for %s", ErrTokenSignature, alg)
		}
		// JWS ECDSA signatures are the raw R||S concatenation, each
		// padded to the curve's byte size — NOT ASN.1 DER. Split in
		// half and verify.
		keySize := (pub.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*keySize {
			return fmt.Errorf("%w: bad ecdsa signature length", ErrTokenSignature)
		}
		r := new(big.Int).SetBytes(sig[:keySize])
		s := new(big.Int).SetBytes(sig[keySize:])
		if !ecdsa.Verify(pub, hashed, r, s) {
			return ErrTokenSignature
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported alg %q", ErrTokenSignature, alg)
	}
}

func hashForAlg(alg, signingInput string) ([]byte, crypto.Hash) {
	switch alg[2:] {
	case "256":
		sum := sha256.Sum256([]byte(signingInput))
		return sum[:], crypto.SHA256
	case "384":
		sum := sha512.Sum384([]byte(signingInput))
		return sum[:], crypto.SHA384
	case "512":
		sum := sha512.Sum512([]byte(signingInput))
		return sum[:], crypto.SHA512
	default:
		sum := sha256.Sum256([]byte(signingInput))
		return sum[:], crypto.SHA256
	}
}

// --- claim coercion helpers ------------------------------------------

type jwtHeader struct {
	Alg string `json:"alg"`
	KID string `json:"kid"`
	Typ string `json:"typ"`
}

func decodeJWTHeader(seg string) (jwtHeader, error) {
	var h jwtHeader
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return h, fmt.Errorf("%w: header decode: %w", ErrTokenInvalid, err)
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, fmt.Errorf("%w: header parse: %w", ErrTokenInvalid, err)
	}
	return h, nil
}

func jsonString(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return s
	}
	return ""
}

func jsonInt64(r json.RawMessage) int64 {
	if len(r) == 0 {
		return 0
	}
	var n json.Number
	if err := json.Unmarshal(r, &n); err == nil {
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	}
	return 0
}

func jsonBool(r json.RawMessage) bool {
	if len(r) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(r, &b); err == nil {
		return b
	}
	// Some issuers encode booleans as the strings "true"/"false".
	if s := jsonString(r); s != "" {
		return strings.EqualFold(s, "true")
	}
	return false
}

// jsonStringOrSlice coerces a claim that may be encoded either as a
// single string or an array of strings into a slice.
func jsonStringOrSlice(r json.RawMessage) []string {
	if len(r) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var ss []string
	if err := json.Unmarshal(r, &ss); err == nil {
		return ss
	}
	return nil
}

// jsonStringSlice coerces a claim into a string slice, additionally
// splitting a single space-delimited string (the OAuth2 `scope`
// encoding) into its elements.
func jsonStringSlice(r json.RawMessage) []string {
	if len(r) == 0 {
		return nil
	}
	var ss []string
	if err := json.Unmarshal(r, &ss); err == nil {
		return ss
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		if s = strings.TrimSpace(s); s == "" {
			return nil
		}
		return strings.Fields(s)
	}
	return nil
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
