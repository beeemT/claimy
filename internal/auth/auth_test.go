package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beeemT/claimy/internal/claims"
)

var (
	fixtureOnce            sync.Once
	fixturePrivate         *rsa.PrivateKey
	fixtureOtherPrivate    *rsa.PrivateKey
	fixtureRotationPrivate *rsa.PrivateKey
	fixtureErr             error
)

var testNow = time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)

func rsaFixtures(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	fixtureOnce.Do(func() {
		fixturePrivate, fixtureErr = rsa.GenerateKey(rand.Reader, 2048)
		if fixtureErr == nil {
			fixtureOtherPrivate, fixtureErr = rsa.GenerateKey(rand.Reader, 2048)
		}
		if fixtureErr == nil {
			fixtureRotationPrivate, fixtureErr = rsa.GenerateKey(rand.Reader, 2048)
		}
	})
	if fixtureErr != nil {
		t.Fatal("could not create signed RSA fixtures")
	}

	return fixturePrivate, fixtureOtherPrivate, fixtureRotationPrivate
}

type mapKeyProvider struct {
	mu    sync.Mutex
	keys  map[string]any
	err   error
	calls int
}

func (p *mapKeyProvider) Key(ctx context.Context, issuer, kid string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return nil, p.err
	}

	return p.keys[issuer+"\x00"+kid], nil
}

func (p *mapKeyProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls
}

type keyProviderFunc func(context.Context, string, string) (any, error)

func (provider keyProviderFunc) Key(ctx context.Context, issuer, kid string) (any, error) {
	return provider(ctx, issuer, kid)
}

type observedContext struct {
	context.Context
	doneObserved chan struct{}
	once         sync.Once
}

func (ctx *observedContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.doneObserved) })

	return ctx.Context.Done()
}

func testSettings() Settings {
	return Settings{
		TeamDomain: "example.com",
		REST: IssuerSettings{
			Issuer:   "https://identity.example.test",
			Audience: "https://claimy.example.test",
			JWKSURL:  "https://keys.example.test/rest.json",
		},
		GitLab: IssuerSettings{
			Issuer:   "https://gitlab.example.test",
			Audience: "https://claimy.example.test",
			JWKSURL:  "https://keys.example.test/gitlab.json",
		},
		Chat: IssuerSettings{
			Issuer:   googleIssuer,
			Audience: "https://claimy.example.test/chat",
			JWKSURL:  "https://www.googleapis.com/oauth2/v3/certs",
		},
	}
}

func testVerifier(t *testing.T) (*Verifier, *mapKeyProvider) {
	t.Helper()
	privateKey, _, _ := rsaFixtures(t)
	settings := testSettings()
	provider := &mapKeyProvider{keys: map[string]any{
		settings.REST.Issuer + "\x00" + "fixture-key":   &privateKey.PublicKey,
		settings.GitLab.Issuer + "\x00" + "fixture-key": &privateKey.PublicKey,
		settings.Chat.Issuer + "\x00" + "fixture-key":   &privateKey.PublicKey,
	}}
	verifier, err := NewWithKeys(settings, provider)
	if err != nil {
		t.Fatal("could not construct verifier")
	}
	verifier.now = func() time.Time { return testNow }

	return verifier, provider
}

func signedToken(t *testing.T, key *rsa.PrivateKey, kid string, header map[string]any, tokenClaims map[string]any) string {
	t.Helper()
	if header == nil {
		header = map[string]any{}
	}
	if _, ok := header["alg"]; !ok {
		header["alg"] = "RS256"
	}
	if _, ok := header["kid"]; !ok {
		header["kid"] = kid
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal("could not encode token header")
	}
	claimsJSON, err := json.Marshal(tokenClaims)
	if err != nil {
		t.Fatal("could not encode token claims")
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	input := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal("could not sign token fixture")
	}

	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func baseClaims(issuer, audience string) map[string]any {
	return map[string]any{
		"iss": issuer,
		"aud": audience,
		"exp": testNow.Add(time.Hour).Unix(),
		"nbf": testNow.Add(-time.Minute).Unix(),
		"iat": testNow.Unix(),
	}
}

func cloneClaims(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source)+1)
	for name, value := range source {
		cloned[name] = value
	}

	return cloned
}

func requireErrorCode(t *testing.T, err error, code claims.ErrorCode) {
	t.Helper()
	var typed *claims.Error
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("expected authentication error code %q", code)
	}
}

func TestRESTManualIdentityCanonicalizesTeamEmailWithoutVerificationGate(t *testing.T) {
	verifier, _ := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	settings := verifier.restIssuer
	tokenClaims := baseClaims(settings.Issuer, settings.Audience)
	tokenClaims["sub"] = "google-user-42"
	tokenClaims["email"] = "  Alice+claimy@Example.COM  "
	tokenClaims["email_verified"] = false
	token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)

	actor, err := verifier.REST(context.Background(), token)
	if err != nil {
		t.Fatal("valid manual identity was rejected")
	}
	if actor.Email != "alice+claimy@example.com" || actor.Issuer != settings.Issuer || actor.Subject != "google-user-42" || actor.Channel != claims.REST || actor.GitLab != nil {
		t.Fatalf("manual identity did not produce the canonical REST actor: %#v", actor)
	}
}

func TestVerifierRejectsTokenThatExpiresDuringKeyLookup(t *testing.T) {
	for _, chat := range []bool{false, true} {
		name := "REST"
		if chat {
			name = "Chat"
		}
		t.Run(name, func(t *testing.T) {
			verifier, _ := testVerifier(t)
			privateKey, _, _ := rsaFixtures(t)
			settings := verifier.restIssuer
			if chat {
				settings = verifier.chatIssuer
			}
			tokenClaims := baseClaims(settings.Issuer, settings.Audience)
			tokenClaims["exp"] = testNow.Add(time.Second).Unix()
			if chat {
				tokenClaims["email"] = "chat@system.gserviceaccount.com"
				tokenClaims["email_verified"] = true
			} else {
				tokenClaims["sub"] = "manual-user"
				tokenClaims["email"] = "person@example.com"
			}
			token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)
			verifier.keys = keyProviderFunc(func(context.Context, string, string) (any, error) {
				verifier.now = func() time.Time { return testNow.Add(2 * time.Second) }

				return &privateKey.PublicKey, nil
			})

			var err error
			if chat {
				_, err = verifier.Chat(context.Background(), token, User{Type: "HUMAN", Name: "users/12345", Email: "person@example.com"})
			} else {
				_, err = verifier.REST(context.Background(), token)
			}
			requireErrorCode(t, err, claims.Unauthenticated)
		})
	}
}

func TestRESTGitLabUsesJobProjectIDAndLegacyProjectID(t *testing.T) {
	verifier, _ := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	settings := verifier.gitLabIssuer
	cases := []struct {
		name          string
		jobProjectID  bool
		wantProjectID string
	}{
		{name: "job project claim takes precedence", jobProjectID: true, wantProjectID: "job-project-9"},
		{name: "legacy project claim remains supported when absent", wantProjectID: "legacy-project-4"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			tokenClaims := baseClaims(settings.Issuer, settings.Audience)
			tokenClaims["user_id"] = "user-12"
			tokenClaims["user_email"] = " Alice@Example.com "
			tokenClaims["project_id"] = "legacy-project-4"
			tokenClaims["job_id"] = "job-18"
			if test.jobProjectID {
				tokenClaims["job_project_id"] = test.wantProjectID
			}
			token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)
			actor, err := verifier.REST(context.Background(), token)
			if err != nil {
				t.Fatal("valid GitLab identity was rejected")
			}
			if actor.Email != "alice@example.com" || actor.Subject != "user-12" || actor.Channel != claims.GitLabCI || actor.GitLab == nil || actor.GitLab.Issuer != settings.Issuer || actor.GitLab.ProjectID != test.wantProjectID || actor.GitLab.JobID != "job-18" || actor.GitLab.UserID != "user-12" {
				t.Fatalf("GitLab identity did not preserve its stable identities: %#v", actor)
			}
		})
	}
}

func TestRESTRejectsInvalidJWTSignaturesClaimsAndAlgorithms(t *testing.T) {
	verifier, _ := testVerifier(t)
	privateKey, wrongPrivateKey, _ := rsaFixtures(t)
	settings := verifier.restIssuer
	valid := baseClaims(settings.Issuer, settings.Audience)
	valid["sub"] = "manual-user"
	valid["email"] = "person@example.com"
	cases := []struct {
		name   string
		key    *rsa.PrivateKey
		header map[string]any
		mutate func(map[string]any)
	}{
		{name: "wrong signature", key: wrongPrivateKey},
		{name: "unsupported algorithm", key: privateKey, header: map[string]any{"alg": "HS256"}},
		{name: "unknown kid", key: privateKey, header: map[string]any{"kid": "unknown-key"}},
		{name: "wrong issuer", key: privateKey, mutate: func(tokenClaims map[string]any) { tokenClaims["iss"] = "https://attacker.example.test" }},
		{name: "wrong audience", key: privateKey, mutate: func(tokenClaims map[string]any) { tokenClaims["aud"] = "https://other.example.test" }},
		{name: "extra audience", key: privateKey, mutate: func(tokenClaims map[string]any) {
			tokenClaims["aud"] = []string{settings.Audience, "https://other.example.test"}
		}},
		{name: "expiry equality", key: privateKey, mutate: func(tokenClaims map[string]any) { tokenClaims["exp"] = testNow.Unix() }},
		{name: "future not before", key: privateKey, mutate: func(tokenClaims map[string]any) { tokenClaims["nbf"] = testNow.Add(time.Second).Unix() }},
		{name: "future issued at", key: privateKey, mutate: func(tokenClaims map[string]any) { tokenClaims["iat"] = testNow.Add(time.Second).Unix() }},
		{name: "missing expiry", key: privateKey, mutate: func(tokenClaims map[string]any) { delete(tokenClaims, "exp") }},
		{name: "missing stable subject", key: privateKey, mutate: func(tokenClaims map[string]any) { delete(tokenClaims, "sub") }},
		{name: "missing email", key: privateKey, mutate: func(tokenClaims map[string]any) { delete(tokenClaims, "email") }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			tokenClaims := cloneClaims(valid)
			if test.mutate != nil {
				test.mutate(tokenClaims)
			}
			token := signedToken(t, test.key, "fixture-key", test.header, tokenClaims)
			_, err := verifier.REST(context.Background(), token)
			requireErrorCode(t, err, claims.Unauthenticated)
		})
	}
}

func TestRESTRejectsIncompleteGitLabIdentity(t *testing.T) {
	verifier, _ := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	settings := verifier.gitLabIssuer
	valid := baseClaims(settings.Issuer, settings.Audience)
	valid["user_id"] = "user-12"
	valid["user_email"] = "person@example.com"
	valid["job_project_id"] = "job-project-9"
	valid["project_id"] = "legacy-project-4"
	valid["job_id"] = "job-18"
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing user id", mutate: func(tokenClaims map[string]any) { delete(tokenClaims, "user_id") }},
		{name: "missing user email", mutate: func(tokenClaims map[string]any) { delete(tokenClaims, "user_email") }},
		{name: "missing job id", mutate: func(tokenClaims map[string]any) { delete(tokenClaims, "job_id") }},
		{name: "missing both project identity claims", mutate: func(tokenClaims map[string]any) {
			delete(tokenClaims, "job_project_id")
			delete(tokenClaims, "project_id")
		}},
		{name: "invalid job project claim does not fall back", mutate: func(tokenClaims map[string]any) { tokenClaims["job_project_id"] = "" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			tokenClaims := cloneClaims(valid)
			test.mutate(tokenClaims)
			token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)
			_, err := verifier.REST(context.Background(), token)
			requireErrorCode(t, err, claims.Unauthenticated)
		})
	}
}

func TestRESTRequiresConfiguredTeamDomain(t *testing.T) {
	verifier, _ := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	settings := verifier.restIssuer
	tokenClaims := baseClaims(settings.Issuer, settings.Audience)
	tokenClaims["sub"] = "manual-user"
	tokenClaims["email"] = "person@outside.example"
	token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)
	_, err := verifier.REST(context.Background(), token)
	requireErrorCode(t, err, claims.Forbidden)
}

func TestChatBindsVerifiedGoogleServiceTokenToHumanUser(t *testing.T) {
	verifier, _ := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	settings := verifier.chatIssuer
	tokenClaims := baseClaims(settings.Issuer, settings.Audience)
	tokenClaims["email"] = "chat@system.gserviceaccount.com"
	tokenClaims["email_verified"] = true
	token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)

	actor, err := verifier.Chat(context.Background(), token, User{Type: "HUMAN", Name: "users/12345", Email: " Person@Example.com "})
	if err != nil {
		t.Fatal("valid Chat service token and human identity were rejected")
	}
	if actor.Email != "person@example.com" || actor.Issuer != googleIssuer || actor.Subject != "users/12345" || actor.Channel != claims.GoogleChat || actor.GitLab != nil {
		t.Fatalf("Chat actor did not use the human identity: %#v", actor)
	}
}

func TestChatRejectsUnverifiedServiceTokensAndNonHumanFallbacks(t *testing.T) {
	verifier, _ := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	settings := verifier.chatIssuer
	cases := []struct {
		name         string
		serviceEmail string
		verified     bool
		user         User
		wantCode     claims.ErrorCode
	}{
		{name: "wrong service identity", serviceEmail: "other@system.gserviceaccount.com", verified: true, user: User{Type: "HUMAN", Name: "users/12345", Email: "person@example.com"}, wantCode: claims.Unauthenticated},
		{name: "unverified service email", serviceEmail: "chat@system.gserviceaccount.com", verified: false, user: User{Type: "HUMAN", Name: "users/12345", Email: "person@example.com"}, wantCode: claims.Unauthenticated},
		{name: "service account is not the owner", serviceEmail: "chat@system.gserviceaccount.com", verified: true, user: User{Type: "SERVICE", Name: "users/12345", Email: "person@example.com"}, wantCode: claims.Unauthenticated},
		{name: "missing human stable id", serviceEmail: "chat@system.gserviceaccount.com", verified: true, user: User{Type: "HUMAN", Email: "person@example.com"}, wantCode: claims.Unauthenticated},
		{name: "missing human email", serviceEmail: "chat@system.gserviceaccount.com", verified: true, user: User{Type: "HUMAN", Name: "users/12345"}, wantCode: claims.Unauthenticated},
		{name: "out-of-team human", serviceEmail: "chat@system.gserviceaccount.com", verified: true, user: User{Type: "HUMAN", Name: "users/12345", Email: "person@outside.example"}, wantCode: claims.Forbidden},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			tokenClaims := baseClaims(settings.Issuer, settings.Audience)
			tokenClaims["email"] = test.serviceEmail
			tokenClaims["email_verified"] = test.verified
			token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)
			_, err := verifier.Chat(context.Background(), token, test.user)
			requireErrorCode(t, err, test.wantCode)
		})
	}
}

func TestChatRequiresConfiguredGoogleIssuerAndSingletonAudience(t *testing.T) {
	verifier, _ := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	settings := verifier.chatIssuer
	user := User{Type: "HUMAN", Name: "users/12345", Email: "person@example.com"}
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong issuer", mutate: func(tokenClaims map[string]any) { tokenClaims["iss"] = "https://attacker.example.test" }},
		{name: "wrong audience", mutate: func(tokenClaims map[string]any) { tokenClaims["aud"] = "https://other.example.test" }},
		{name: "extra audience", mutate: func(tokenClaims map[string]any) {
			tokenClaims["aud"] = []string{settings.Audience, "https://other.example.test"}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			tokenClaims := baseClaims(settings.Issuer, settings.Audience)
			tokenClaims["email"] = "chat@system.gserviceaccount.com"
			tokenClaims["email_verified"] = true
			test.mutate(tokenClaims)
			token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)
			_, err := verifier.Chat(context.Background(), token, user)
			requireErrorCode(t, err, claims.Unauthenticated)
		})
	}
}

func TestVerifierClassifiesKeyServiceFailureWithoutLeakingProviderDetails(t *testing.T) {
	verifier, provider := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	provider.err = errors.New("https://private.example.test/keys?token=secret")
	settings := verifier.restIssuer
	tokenClaims := baseClaims(settings.Issuer, settings.Audience)
	tokenClaims["sub"] = "manual-user"
	tokenClaims["email"] = "person@example.com"
	token := signedToken(t, privateKey, "fixture-key", nil, tokenClaims)
	_, err := verifier.REST(context.Background(), token)
	requireErrorCode(t, err, claims.StorageError)
	if err.Error() != "identity provider unavailable" || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private.example") {
		t.Fatal("provider details were exposed in the public error")
	}
}

func TestConfiguredJWKSRequiresHTTPSAndGoogleChatIssuer(t *testing.T) {
	settings := testSettings()
	settings.GitLab.JWKSURL = "http://keys.example.test/jwks"
	if _, err := NewWithKeys(settings, &mapKeyProvider{keys: map[string]any{}}); err == nil {
		t.Fatal("unencrypted JWKS endpoint was accepted")
	}
	settings = testSettings()
	settings.Chat.Issuer = "https://attacker.example.test"
	if _, err := NewWithKeys(settings, &mapKeyProvider{keys: map[string]any{}}); err == nil {
		t.Fatal("non-Google Chat issuer was accepted")
	}
}

func TestTokenCannotSelectRemoteKeyLocation(t *testing.T) {
	verifier, provider := testVerifier(t)
	privateKey, _, _ := rsaFixtures(t)
	settings := verifier.restIssuer
	tokenClaims := baseClaims(settings.Issuer, settings.Audience)
	tokenClaims["sub"] = "manual-user"
	tokenClaims["email"] = "person@example.com"
	token := signedToken(t, privateKey, "fixture-key", map[string]any{"jku": "https://attacker.example.test/jwks"}, tokenClaims)
	_, err := verifier.REST(context.Background(), token)
	requireErrorCode(t, err, claims.Unauthenticated)
	if provider.callCount() != 0 {
		t.Fatal("token-supplied key location reached the key provider")
	}
}

func TestHTTPSJWKSCacheRotationAndUnknownKeyRefresh(t *testing.T) {
	oldKey, _, rotationKey := rsaFixtures(t)
	issuer := "https://identity.example.test"
	var mu sync.RWMutex
	body := jwksJSON(t, "old-key", &oldKey.PublicKey)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		mu.RLock()
		defer mu.RUnlock()
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "public, max-age=300")
		if _, err := response.Write(body); err != nil {
			t.Errorf("write JWKS fixture: %v", err)
		}
	}))
	defer server.Close()
	provider := newHTTPKeyProviderWithClient(map[string]string{issuer: server.URL}, server.Client())
	ctx := context.Background()

	key, err := provider.Key(ctx, issuer, "old-key")
	if err != nil || key == nil || requests.Load() != 1 {
		t.Fatal("configured HTTPS JWKS key was not fetched")
	}
	if _, err := provider.Key(ctx, issuer, "old-key"); err != nil || requests.Load() != 1 {
		t.Fatal("valid cached JWKS key triggered another HTTP request")
	}
	mu.Lock()
	body = jwksJSON(t, "new-key", &rotationKey.PublicKey)
	mu.Unlock()
	key, err = provider.Key(ctx, issuer, "new-key")
	if err != nil || key == nil || requests.Load() != 2 {
		t.Fatal("unknown kid did not refresh the configured JWKS for rotation")
	}
	if key, err = provider.Key(ctx, issuer, "missing-key"); err != nil || key != nil || requests.Load() != 3 {
		t.Fatal("unknown kid did not fail closed after a bounded refresh")
	}
	if key, err = provider.Key(ctx, issuer, "missing-key"); err != nil || key != nil || requests.Load() != 3 {
		t.Fatal("repeated unknown kid bypassed the refresh cooldown")
	}
	if key, err = provider.Key(ctx, "https://untrusted.example.test", "new-key"); err != nil || key != nil || requests.Load() != 3 {
		t.Fatal("untrusted issuer caused a JWKS request")
	}
}

func TestHTTPSJWKSRequestHonorsCancellation(t *testing.T) {
	issuer := "https://identity.example.test"
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
	}))
	defer server.Close()
	provider := newHTTPKeyProviderWithClient(map[string]string{issuer: server.URL}, server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := provider.Key(ctx, issuer, "fixture-key")
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled JWKS request did not preserve request cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled JWKS request did not stop")
	}
}

func TestHTTPSJWKSWaiterRetriesAbandonedDeadlineFlight(t *testing.T) {
	_, _, rotationKey := rsaFixtures(t)
	issuer := "https://identity.example.test"
	body := jwksJSON(t, "rotated-key", &rotationKey.PublicKey)
	firstRequestStarted := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			close(firstRequestStarted)
			<-request.Context().Done()

			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "public, max-age=300")
		if _, err := response.Write(body); err != nil {
			t.Errorf("write JWKS fixture: %v", err)
		}
	}))
	defer server.Close()
	provider := newHTTPKeyProviderWithClient(map[string]string{issuer: server.URL}, server.Client())

	initiatingContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	initiatorResult := make(chan error, 1)
	go func() {
		_, err := provider.Key(initiatingContext, issuer, "rotated-key")
		initiatorResult <- err
	}()
	select {
	case <-firstRequestStarted:
	case <-time.After(time.Second):
		t.Fatal("initiating JWKS request did not start")
	}

	waiterContext := &observedContext{Context: context.Background(), doneObserved: make(chan struct{})}
	waiterResult := make(chan struct {
		key any
		err error
	}, 1)
	go func() {
		key, err := provider.Key(waiterContext, issuer, "rotated-key")
		waiterResult <- struct {
			key any
			err error
		}{key: key, err: err}
	}()
	select {
	case <-waiterContext.doneObserved:
	case <-time.After(time.Second):
		t.Fatal("live caller did not join the in-flight JWKS lookup")
	}
	if requests.Load() != 1 {
		t.Fatal("second fetch began before the initiating request abandoned its flight")
	}
	select {
	case err := <-initiatorResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("initiating fetch did not end with its caller deadline")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initiating request did not stop at its deadline")
	}

	select {
	case result := <-waiterResult:
		if result.err != nil || result.key == nil || requests.Load() != 2 {
			t.Fatal("live waiter did not retry and retrieve the configured JWKS key")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("live waiter did not complete after the canceled flight")
	}
}

func TestHTTPSJWKSWaiterDoesNotRetryProviderFailure(t *testing.T) {
	issuer := "https://identity.example.test"
	firstRequestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var requests atomic.Int32
	var firstOnce sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		firstOnce.Do(func() { close(firstRequestStarted) })
		<-releaseResponse
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	provider := newHTTPKeyProviderWithClient(map[string]string{issuer: server.URL}, server.Client())

	initiatorResult := make(chan error, 1)
	go func() {
		_, err := provider.Key(context.Background(), issuer, "fixture-key")
		initiatorResult <- err
	}()
	select {
	case <-firstRequestStarted:
	case <-time.After(time.Second):
		t.Fatal("initiating JWKS request did not start")
	}

	waiterContext := &observedContext{Context: context.Background(), doneObserved: make(chan struct{})}
	waiterResult := make(chan error, 1)
	go func() {
		_, err := provider.Key(waiterContext, issuer, "fixture-key")
		waiterResult <- err
	}()
	select {
	case <-waiterContext.doneObserved:
	case <-time.After(time.Second):
		t.Fatal("live caller did not join the in-flight JWKS lookup")
	}
	if requests.Load() != 1 {
		t.Fatal("provider-failure test did not share the initiating flight")
	}
	close(releaseResponse)

	select {
	case err := <-initiatorResult:
		if err == nil {
			t.Fatal("upstream failure was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("initiating provider failure did not return")
	}
	select {
	case err := <-waiterResult:
		if err == nil || requests.Load() != 1 {
			t.Fatal("live waiter retried an upstream provider failure")
		}
	case <-time.After(time.Second):
		t.Fatal("live waiter did not receive the shared provider failure")
	}
}

func TestHTTPSJWKSDoesNotFollowRedirects(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	provider := newHTTPKeyProviderWithClient(map[string]string{"https://identity.example.test": redirect.URL}, redirect.Client())
	if _, err := provider.Key(context.Background(), "https://identity.example.test", "fixture-key"); err == nil {
		t.Fatal("redirect response was accepted as a JWKS document")
	}
	if targetRequests.Load() != 0 {
		t.Fatal("JWKS client followed an endpoint redirect")
	}
}

func jwksJSON(t *testing.T, kid string, key *rsa.PublicKey) []byte {
	t.Helper()
	exponent := big.NewInt(int64(key.E)).Bytes()
	document := map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"kid": kid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(exponent),
		}},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal("could not encode JWKS fixture")
	}

	return encoded
}

func TestJWKSCacheAgeIsBounded(t *testing.T) {
	if cacheTTL(http.Header{"Cache-Control": []string{"max-age=86400"}}) != jwksCacheMaxTTL {
		t.Fatal("JWKS cache lifetime exceeded its configured maximum")
	}
	if cacheTTL(http.Header{"Cache-Control": []string{"max-age=0"}}) != time.Second {
		t.Fatal("zero cache lifetime did not use the bounded refresh floor")
	}
	if cacheTTL(http.Header{}) != jwksCacheDefaultTTL {
		t.Fatal("JWKS endpoint without cache policy did not use the default TTL")
	}
	if cacheTTL(http.Header{"Cache-Control": []string{"max-age=" + strconv.FormatInt(int64(jwksCacheMaxTTL/time.Second), 10)}}) != jwksCacheMaxTTL {
		t.Fatal("JWKS max-age boundary was not retained")
	}
}
