package login

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *memoryStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	if !ok {
		return "", errCredentialNotFound
	}

	return value, nil
}

func (s *memoryStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value

	return nil
}

func (s *memoryStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)

	return nil
}

type oidcFixture struct {
	server          *httptest.Server
	key             *rsa.PrivateKey
	mu              sync.Mutex
	clientID        string
	metadataIssuer  string
	nonce           string
	challenge       string
	redirect        string
	codeAvailable   bool
	refreshToken    string
	refreshAttempts int
	refreshes       int
	identityReads   int
	mutateClaims    func(map[string]any)
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &oidcFixture{key: key, clientID: "claimy-cli"}
	mux := http.NewServeMux()
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	fixture.metadataIssuer = fixture.server.URL
	mux.HandleFunc("/v1/auth/config", fixture.loginConfig)
	mux.HandleFunc("/v1/auth/me", fixture.identity)
	mux.HandleFunc("/.well-known/openid-configuration", fixture.discovery)
	mux.HandleFunc("/jwks", fixture.jwks)
	mux.HandleFunc("/authorize", fixture.authorize)
	mux.HandleFunc("/token", fixture.token)

	return fixture
}

func (f *oidcFixture) loginConfig(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, map[string]any{"issuer": f.metadataIssuer, "clientId": f.clientID, "scopes": []string{"openid", "email"}})
}

func (f *oidcFixture) discovery(w http.ResponseWriter, _ *http.Request) {
	issuer := f.server.URL
	writeJSON(w, map[string]any{
		"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
		"jwks_uri": issuer + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported": []string{"S256"},
	})
}

func (f *oidcFixture) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"keys": []any{map[string]string{
		"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": "AQAB",
	}}})
}

func (f *oidcFixture) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	if query.Get("client_id") != f.clientID || query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || query.Get("nonce") == "" {
		http.Error(w, "invalid authorization", http.StatusBadRequest)

		return
	}
	redirect, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" {
		http.Error(w, "invalid callback", http.StatusBadRequest)

		return
	}
	f.nonce = query.Get("nonce")
	f.challenge = query.Get("code_challenge")
	f.redirect = redirect.String()
	f.codeAvailable = true
	values := redirect.Query()
	values.Set("state", query.Get("state"))
	values.Set("code", "single-use-code")
	redirect.RawQuery = values.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (f *oidcFixture) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)

		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Form.Get("client_id") != f.clientID || r.Form.Has("client_secret") {
		http.Error(w, "invalid public client", http.StatusBadRequest)

		return
	}
	nonce := ""
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !f.codeAvailable || r.Form.Get("code") != "single-use-code" || r.Form.Get("redirect_uri") != f.redirect || base64.RawURLEncoding.EncodeToString(digest[:]) != f.challenge {
			http.Error(w, "invalid code or PKCE", http.StatusBadRequest)

			return
		}
		f.codeAvailable = false
		nonce = f.nonce
	case "refresh_token":
		f.refreshAttempts++
		if r.Form.Get("refresh_token") != f.refreshToken || f.refreshToken == "" {
			http.Error(w, "invalid rotated credential", http.StatusBadRequest)

			return
		}
		f.refreshes++
	default:
		http.Error(w, "invalid grant", http.StatusBadRequest)

		return
	}
	f.refreshToken = fmt.Sprintf("refresh-%d", f.refreshes+1)
	claims := map[string]any{
		"iss": f.server.URL, "sub": "subject-1", "aud": f.clientID, "email": "alice@example.com",
		"exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "jti": f.refreshToken,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if f.mutateClaims != nil {
		f.mutateClaims(claims)
	}
	idToken, err := signJWT(f.key, claims)
	if err != nil {
		http.Error(w, "signing failed", http.StatusInternalServerError)

		return
	}
	writeJSON(w, map[string]any{
		"access_token": "opaque-access-token", "refresh_token": f.refreshToken, "id_token": idToken,
		"token_type": "Bearer", "expires_in": 300,
	})
}

func (f *oidcFixture) identity(w http.ResponseWriter, r *http.Request) {
	if !f.acceptsIDToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
		http.Error(w, "invalid identity token", http.StatusUnauthorized)

		return
	}
	f.mu.Lock()
	f.identityReads++
	f.mu.Unlock()
	writeJSON(w, map[string]string{"email": "alice@example.com"})
}

func (f *oidcFixture) acceptsIDToken(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Issuer   string `json:"iss"`
		Audience string `json:"aud"`
		Subject  string `json:"sub"`
		Email    string `json:"email"`
		Expires  int64  `json:"exp"`
	}
	if json.Unmarshal(body, &claims) != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	return claims.Issuer == f.server.URL && claims.Audience == f.clientID && claims.Subject == "subject-1" && claims.Email == "alice@example.com" && claims.Expires > time.Now().Unix()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}

func signJWT(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": "test", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}

	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func isolateLogin(t *testing.T) *memoryStore {
	t.Helper()
	previousStore, previousLauncher, previousConfigDir := currentStore, browserLauncher, userConfigDir
	t.Cleanup(func() {
		currentStore, browserLauncher, userConfigDir = previousStore, previousLauncher, previousConfigDir
	})
	store := &memoryStore{values: make(map[string]string)}
	currentStore = store
	directory := t.TempDir()
	userConfigDir = func() (string, error) { return directory, nil }
	browserLauncher = func(ctx context.Context, raw string, _ io.Writer) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			t.Error("create authorization request")

			return errors.New("create authorization request failed")
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				t.Error("authorization request failed")
			}

			return errors.New("authorization request failed")
		}
		if err := response.Body.Close(); err != nil {
			t.Error("close authorization response")

			return errors.New("close authorization response failed")
		}

		return nil
	}

	return store
}

func TestLoginRefreshRotationAndLogout(t *testing.T) {
	store := isolateLogin(t)
	fixture := newOIDCFixture(t)
	identity, err := Login(t.Context(), fixture.server.URL, time.Second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if string(identity.Email) != "alice@example.com" {
		t.Fatalf("unexpected identity: %v", identity.Email)
	}
	if fixture.identityReads != 1 {
		t.Fatal("login was not accepted by the protected Claimy endpoint")
	}
	for range 2 {
		token, err := Token(t.Context(), fixture.server.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !fixture.acceptsIDToken(token) {
			t.Fatal("returned credential is not an accepted signed ID token")
		}
		encoded, err := store.Get(credentialKey(fixture.server.URL))
		if err != nil {
			t.Fatal(err)
		}
		var saved storedCredential
		if err := json.Unmarshal([]byte(encoded), &saved); err != nil {
			t.Fatal(err)
		}
		if saved.RefreshToken != fixture.refreshToken {
			t.Fatal("rotated refresh credential was not persisted")
		}
	}
	if fixture.refreshes != 2 {
		t.Fatal("provider did not accept both rotated refresh credentials")
	}
	if err := Logout(fixture.server.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(credentialKey(fixture.server.URL)); err != errCredentialNotFound {
		t.Fatal("logout retained refresh credential")
	}
	server, err := DefaultURL()
	if err != nil || server != "" {
		t.Fatal("logout retained default server")
	}
}

func TestLoginRejectsInvalidIdentityBeforePersistence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong nonce", mutate: func(c map[string]any) { c["nonce"] = "wrong" }},
		{name: "expired", mutate: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		{name: "multiple audiences", mutate: func(c map[string]any) { c["aud"] = []string{"claimy-cli", "other"} }},
		{name: "different issuer", mutate: func(c map[string]any) { c["iss"] = "https://other.example.test" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := isolateLogin(t)
			fixture := newOIDCFixture(t)
			fixture.mutateClaims = tc.mutate
			if _, err := Login(t.Context(), fixture.server.URL, time.Second, io.Discard); err == nil {
				t.Fatal("invalid identity was accepted")
			}
			if _, err := store.Get(credentialKey(fixture.server.URL)); err != errCredentialNotFound {
				t.Fatal("invalid identity was persisted")
			}
			server, err := DefaultURL()
			if err != nil || server != "" {
				t.Fatal("invalid identity changed default server")
			}
			if fixture.identityReads != 0 {
				t.Fatal("invalid identity reached protected Claimy endpoint")
			}
		})
	}
}

func TestRefreshRejectsChangedProviderBeforeSendingCredential(t *testing.T) {
	for _, change := range []string{"issuer", "client ID"} {
		t.Run(change, func(t *testing.T) {
			assertRefreshRejectsChangedProvider(t, change)
		})
	}
}

func assertRefreshRejectsChangedProvider(t *testing.T, change string) {
	t.Helper()
	store := isolateLogin(t)
	fixture := newOIDCFixture(t)
	if _, err := Login(t.Context(), fixture.server.URL, time.Second, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(credentialKey(fixture.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	if change == "issuer" {
		fixture.metadataIssuer = "https://other.example.test"
	} else {
		fixture.clientID = "other-public-client"
	}
	fixture.mu.Unlock()
	if _, err := Token(t.Context(), fixture.server.URL, time.Second); err == nil {
		t.Fatal("changed provider binding was accepted")
	}
	if fixture.refreshAttempts != 0 {
		t.Fatal("saved credential was sent after provider binding changed")
	}
	after, err := store.Get(credentialKey(fixture.server.URL))
	if err != nil || after != before {
		t.Fatal("failed refresh changed saved credential")
	}
}
