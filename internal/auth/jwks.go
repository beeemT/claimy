package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	jwksRequestTimeout     = 5 * time.Second
	jwksCacheDefaultTTL    = time.Minute
	jwksCacheMaxTTL        = 5 * time.Minute
	jwksRefreshCooldown    = time.Second
	jwksUnknownKidCooldown = 50 * time.Millisecond
	jwksMaxBytes           = 1 << 20
	jwksMaxKeys            = 64
	maxRSAExponent         = uint64(1<<31 - 1)
)

type httpKeyProvider struct {
	issuers map[string]string
	client  *http.Client

	mu             sync.Mutex
	cache          map[string]jwksCache
	flights        map[string]*jwksFlight
	lastAttempt    map[string]time.Time
	lastError      map[string]error
	lastUnknownKid map[string]time.Time
}

type jwksCache struct {
	keys      map[string]*rsa.PublicKey
	expiresAt time.Time
}

type jwksFlight struct {
	done      chan struct{}
	err       error
	abandoned bool
}

type keyLookupAction uint8

const (
	keyLookupUnknownIssuer keyLookupAction = iota
	keyLookupUnknownKid
	keyLookupFound
	keyLookupWait
	keyLookupRefresh
	keyLookupError
)

type keyLookup struct {
	action   keyLookupAction
	key      any
	endpoint string
	flight   *jwksFlight
	err      error
}

func newHTTPKeyProvider(settings Settings) (*httpKeyProvider, error) {
	issuers := make(map[string]string, 3)
	for _, issuer := range []IssuerSettings{settings.REST, settings.GitLab, settings.Chat} {
		if existing, ok := issuers[issuer.Issuer]; ok && existing != issuer.JWKSURL {
			return nil, errors.New("one issuer cannot use multiple JWKS URLs")
		}
		issuers[issuer.Issuer] = issuer.JWKSURL
	}
	client := &http.Client{
		Timeout: jwksRequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return newHTTPKeyProviderWithClient(issuers, client), nil
}

func newHTTPKeyProviderWithClient(issuers map[string]string, client *http.Client) *httpKeyProvider {
	if client == nil {
		client = &http.Client{Timeout: jwksRequestTimeout}
	}
	clone := *client
	clone.Timeout = jwksRequestTimeout
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	configured := make(map[string]string, len(issuers))
	for issuer, endpoint := range issuers {
		configured[issuer] = endpoint
	}

	return &httpKeyProvider{
		issuers:        configured,
		client:         &clone,
		cache:          make(map[string]jwksCache, len(configured)),
		flights:        make(map[string]*jwksFlight, len(configured)),
		lastAttempt:    make(map[string]time.Time, len(configured)),
		lastError:      make(map[string]error, len(configured)),
		lastUnknownKid: make(map[string]time.Time, len(configured)),
	}
}

// Key fetches only from the JWKS URL configured for issuer. A missing kid is
// represented by (nil, nil), while network and malformed-JWKS failures are
// returned as service errors. Unknown kids cause a bounded refresh so key
// rotations are picked up without accepting token-provided key locations.
func (p *httpKeyProvider) Key(ctx context.Context, issuer, kid string) (any, error) {
	if ctx == nil {
		return nil, errors.New("request context is required")
	}
	if !validStableID(kid) {
		return nil, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		lookup := p.prepareKeyLookup(issuer, kid, time.Now())
		switch lookup.action {
		case keyLookupUnknownIssuer, keyLookupUnknownKid:
			return nil, nil
		case keyLookupFound:
			return lookup.key, nil
		case keyLookupWait:
			if err := waitForJWKSFlight(ctx, lookup.flight); err != nil {
				return nil, err
			}
		case keyLookupRefresh:
			if err := p.refresh(ctx, issuer, kid, lookup.endpoint, lookup.flight); err != nil {
				return nil, err
			}
		case keyLookupError:
			return nil, lookup.err
		default:
			return nil, errors.New("invalid JWKS lookup state")
		}
	}
}

func (p *httpKeyProvider) prepareKeyLookup(issuer, kid string, now time.Time) keyLookup {
	p.mu.Lock()
	defer p.mu.Unlock()

	endpoint, trusted := p.issuers[issuer]
	if !trusted {
		return keyLookup{action: keyLookupUnknownIssuer}
	}
	cached, hasCache := p.cache[issuer]
	if hasCache && now.Before(cached.expiresAt) {
		if key := cached.keys[kid]; key != nil {
			return keyLookup{action: keyLookupFound, key: key}
		}
	}
	if flight := p.flights[issuer]; flight != nil {
		return keyLookup{action: keyLookupWait, flight: flight}
	}
	if previous, attempted := p.lastAttempt[issuer]; attempted && now.Sub(previous) < jwksRefreshCooldown {
		if err := p.lastError[issuer]; err != nil {
			return keyLookup{action: keyLookupError, err: err}
		}
	}
	if hasCache && now.Before(cached.expiresAt) {
		if previous, attempted := p.lastUnknownKid[issuer]; attempted && now.Sub(previous) < jwksUnknownKidCooldown {
			return keyLookup{action: keyLookupUnknownKid}
		}
	}

	flight := &jwksFlight{done: make(chan struct{})}
	p.flights[issuer] = flight
	p.lastAttempt[issuer] = now

	return keyLookup{action: keyLookupRefresh, endpoint: endpoint, flight: flight}
}

func waitForJWKSFlight(ctx context.Context, flight *jwksFlight) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return err
		}
		if flight.err != nil && !flight.abandoned {
			return flight.err
		}

		return nil
	}
}

func (p *httpKeyProvider) refresh(ctx context.Context, issuer, kid, endpoint string, flight *jwksFlight) error {
	keys, ttl, fetchErr := p.fetch(ctx, endpoint)
	finished := time.Now()
	p.mu.Lock()
	flight.err = fetchErr
	flight.abandoned = fetchErr != nil && ctx.Err() != nil
	switch {
	case fetchErr == nil:
		p.cache[issuer] = jwksCache{
			keys:      keys,
			expiresAt: finished.Add(ttl),
		}
		if keys[kid] == nil {
			p.lastUnknownKid[issuer] = finished
		}
		delete(p.lastError, issuer)
	case ctx.Err() != nil:
		delete(p.lastAttempt, issuer)
		delete(p.lastError, issuer)
	default:
		p.lastError[issuer] = fetchErr
	}
	delete(p.flights, issuer)
	close(flight.done)
	p.mu.Unlock()

	return fetchErr
}

func (p *httpKeyProvider) fetch(ctx context.Context, endpoint string) (keys map[string]*rsa.PublicKey, ttl time.Duration, resultErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, errors.New("configured JWKS request could not be created")
	}
	request.Header.Set("Accept", "application/json, application/jwk-set+json")
	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}

		return nil, 0, errors.New("JWKS request failed")
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && resultErr == nil {
			resultErr = errors.New("JWKS response could not be closed")
		}
	}()
	if response.StatusCode != http.StatusOK {
		return nil, 0, errors.New("JWKS endpoint returned an unsuccessful status")
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if mediaType != "application/json" && mediaType != "application/jwk-set+json" {
		return nil, 0, errors.New("JWKS endpoint returned an unsupported content type")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, jwksMaxBytes+1))
	if err != nil {
		return nil, 0, errors.New("JWKS response could not be read")
	}
	if len(body) > jwksMaxBytes {
		return nil, 0, errors.New("JWKS response exceeded its size limit")
	}
	keys, err = parseJWKS(body)
	if err != nil {
		return nil, 0, err
	}

	return keys, cacheTTL(response.Header), nil
}

func cacheTTL(header http.Header) time.Duration {
	for _, directive := range strings.Split(header.Get("Cache-Control"), ",") {
		name, value, found := strings.Cut(strings.TrimSpace(directive), "=")
		if !strings.EqualFold(name, "max-age") || !found {
			continue
		}
		seconds, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(value), "\""), 10, 64)
		if err != nil || seconds <= 0 {
			return time.Second
		}
		if seconds >= int64(jwksCacheMaxTTL/time.Second) {
			return jwksCacheMaxTTL
		}

		return time.Duration(seconds) * time.Second
	}

	return jwksCacheDefaultTTL
}

type jwk struct {
	KeyType    string   `json:"kty"`
	KeyID      string   `json:"kid"`
	Use        string   `json:"use"`
	Algorithm  string   `json:"alg"`
	Operations []string `json:"key_ops"`
	Modulus    string   `json:"n"`
	Exponent   string   `json:"e"`
}

func parseJWKS(body []byte) (map[string]*rsa.PublicKey, error) {
	encodedKeys, err := encodedJWKSKeys(body)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(encodedKeys))
	for _, encodedKey := range encodedKeys {
		keyID, publicKey, usable, err := parseJWK(encodedKey)
		if err != nil {
			return nil, err
		}
		if !usable {
			continue
		}
		if _, duplicate := keys[keyID]; duplicate {
			return nil, errors.New("JWKS response contains duplicate key IDs")
		}
		keys[keyID] = publicKey
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS response contains no usable RSA signing keys")
	}

	return keys, nil
}

func encodedJWKSKeys(body []byte) ([]json.RawMessage, error) {
	root, err := decodeJSONObject(body)
	if err != nil {
		return nil, errors.New("JWKS response is malformed")
	}
	rawKeys, ok := root["keys"]
	if !ok {
		return nil, errors.New("JWKS response has no keys")
	}
	var encodedKeys []json.RawMessage
	if err := json.Unmarshal(rawKeys, &encodedKeys); err != nil {
		return nil, errors.New("JWKS response has an invalid key list")
	}
	if len(encodedKeys) == 0 || len(encodedKeys) > jwksMaxKeys {
		return nil, errors.New("JWKS response has an invalid key count")
	}

	return encodedKeys, nil
}

func parseJWK(encodedKey json.RawMessage) (string, *rsa.PublicKey, bool, error) {
	if _, err := decodeJSONObject(encodedKey); err != nil {
		return "", nil, false, errors.New("JWKS response contains a malformed key")
	}
	var key jwk
	if err := json.Unmarshal(encodedKey, &key); err != nil {
		return "", nil, false, errors.New("JWKS response contains a malformed key")
	}
	if !isRSASigningKey(key) || !validStableID(key.KeyID) {
		return "", nil, false, nil
	}
	publicKey, err := rsaPublicKey(key.Modulus, key.Exponent)
	if err != nil {
		return "", nil, false, errors.New("JWKS response contains an invalid RSA key")
	}

	return key.KeyID, publicKey, true, nil
}

func isRSASigningKey(key jwk) bool {
	return key.KeyType == "RSA" &&
		(key.Use == "" || key.Use == "sig") &&
		(key.Algorithm == "" || key.Algorithm == "RS256") &&
		canVerify(key.Operations)
}

func canVerify(operations []string) bool {
	if len(operations) == 0 {
		return true
	}
	for _, operation := range operations {
		if operation == "verify" {
			return true
		}
	}

	return false
}

func rsaPublicKey(encodedModulus, encodedExponent string) (*rsa.PublicKey, error) {
	modulusBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedModulus)
	if err != nil || len(modulusBytes) == 0 || modulusBytes[0] == 0 || len(modulusBytes) > 1024 {
		return nil, errors.New("invalid modulus")
	}
	exponentBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedExponent)
	if err != nil || len(exponentBytes) == 0 || exponentBytes[0] == 0 || len(exponentBytes) > 4 {
		return nil, errors.New("invalid exponent")
	}
	exponent := uint64(0)
	for _, b := range exponentBytes {
		exponent = exponent<<8 | uint64(b)
	}
	if exponent > uint64(^uint(0)>>1) || exponent > maxRSAExponent {
		return nil, errors.New("invalid exponent")
	}
	publicExponent := int(exponent)
	if publicExponent < 3 || publicExponent&1 == 0 {
		return nil, errors.New("invalid exponent")
	}
	publicKey := &rsa.PublicKey{N: new(big.Int).SetBytes(modulusBytes), E: publicExponent}
	if !validRSAPublicKey(publicKey) {
		return nil, errors.New("unsupported RSA key size")
	}

	return publicKey, nil
}
