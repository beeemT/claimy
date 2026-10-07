package login

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/beeemT/claimy/pkg/client"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var errResponseTooLarge = errors.New("login HTTP response exceeded its size limit")

const (
	maxLoginResponseBytes = 2 << 20
	httpScheme            = "http"
	httpsScheme           = "https"
)

type responseLimitTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t responseLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.Body != nil {
		response.Body = &limitedResponseBody{ReadCloser: response.Body, remaining: t.limit}
	}

	return response, nil
}

type limitedResponseBody struct {
	io.ReadCloser
	remaining int64
}

func (body *limitedResponseBody) Read(buffer []byte) (int, error) {
	if body.remaining == 0 {
		var probe [1]byte
		count, err := body.ReadCloser.Read(probe[:])
		if count > 0 {
			return 0, errResponseTooLarge
		}

		return 0, err
	}
	if int64(len(buffer)) > body.remaining {
		buffer = buffer[:body.remaining]
	}
	count, err := body.ReadCloser.Read(buffer)
	body.remaining -= int64(count)

	return count, err
}

type providerMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

type verifiedIdentity struct {
	subject string
	email   string
	idToken string
}

func discoverProvider(parent context.Context, issuer string, timeout time.Duration, httpClient *http.Client) (*oidc.Provider, providerMetadata, error) {
	ctx, cancel := requestContext(parent, timeout, httpClient)
	defer cancel()

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, providerMetadata{}, err
	}
	var metadata providerMetadata
	if err := provider.Claims(&metadata); err != nil {
		return nil, providerMetadata{}, err
	}
	if metadata.Issuer != issuer || validateEndpointForIssuer(metadata.AuthorizationEndpoint, issuer) != nil || validateEndpointForIssuer(metadata.TokenEndpoint, issuer) != nil || validateEndpointForIssuer(metadata.JWKSURI, issuer) != nil {
		return nil, providerMetadata{}, errors.New("provider metadata is invalid")
	}
	if !contains(metadata.SigningAlgs, "RS256") {
		return nil, providerMetadata{}, errors.New("provider does not advertise RS256")
	}
	if len(metadata.CodeChallengeMethods) > 0 && !contains(metadata.CodeChallengeMethods, "S256") {
		return nil, providerMetadata{}, errors.New("provider does not advertise S256 PKCE")
	}

	return provider, metadata, nil
}

func verifyIdentityToken(parent context.Context, provider *oidc.Provider, httpClient *http.Client, timeout time.Duration, issuer, clientID string, token *oauth2.Token, nonce string) (verifiedIdentity, error) {
	if token == nil {
		return verifiedIdentity{}, errors.New("login provider returned no identity")
	}
	rawToken, ok := token.Extra("id_token").(string)
	if !ok || rawToken == "" {
		return verifiedIdentity{}, errors.New("login provider returned no identity")
	}

	ctx, cancel := requestContext(parent, timeout, httpClient)
	defer cancel()
	verifier := provider.VerifierContext(ctx, &oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: []string{"RS256"},
	})
	verified, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return verifiedIdentity{}, errors.New("login identity verification failed")
	}
	if verified.Issuer != issuer {
		return verifiedIdentity{}, errors.New("login identity issuer is invalid")
	}
	if len(verified.Audience) != 1 || verified.Audience[0] != clientID {
		return verifiedIdentity{}, errors.New("login identity audience is invalid")
	}
	if nonce != "" && subtle.ConstantTimeCompare([]byte(verified.Nonce), []byte(nonce)) != 1 {
		return verifiedIdentity{}, errors.New("login nonce validation failed")
	}

	var claims struct {
		Subject string `json:"sub"`
		Email   string `json:"email"`
	}
	if err := verified.Claims(&claims); err != nil || strings.TrimSpace(claims.Subject) == "" || canonicalEmail(claims.Email) == "" {
		return verifiedIdentity{}, errors.New("login identity is incomplete")
	}

	return verifiedIdentity{subject: claims.Subject, email: canonicalEmail(claims.Email), idToken: rawToken}, nil
}

func validateLoginConfig(config *client.LoginConfig) error {
	if config == nil || strings.TrimSpace(config.Issuer) == "" || strings.TrimSpace(config.ClientId) == "" ||
		config.Issuer != strings.TrimSpace(config.Issuer) || config.ClientId != strings.TrimSpace(config.ClientId) {
		return errors.New("login configuration is invalid")
	}
	if err := validateLoginScopes(config.Scopes); err != nil {
		return err
	}

	return validateAuthorizationParams(config.AuthorizationParams)
}

func validateLoginScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("login configuration is invalid")
	}
	seenScopes := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if strings.TrimSpace(scope) == "" || strings.TrimSpace(scope) != scope {
			return errors.New("login configuration is invalid")
		}
		if _, exists := seenScopes[scope]; exists {
			return errors.New("login configuration is invalid")
		}
		seenScopes[scope] = struct{}{}
	}
	if _, ok := seenScopes["openid"]; !ok {
		return errors.New("login configuration is invalid")
	}
	if _, ok := seenScopes["email"]; !ok {
		return errors.New("login configuration is invalid")
	}

	return nil
}

func validateAuthorizationParams(params *map[string]string) error {
	if params == nil {
		return nil
	}
	if len(*params) > 32 {
		return errors.New("login configuration contains too many authorization parameters")
	}
	seenParams := make(map[string]struct{}, len(*params))
	for key, value := range *params {
		canonical := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
		if strings.TrimSpace(key) == "" || key != strings.TrimSpace(key) || len(key) > 128 ||
			strings.TrimSpace(value) == "" || len(value) > 128 || hasControlCharacters(key) ||
			hasControlCharacters(value) || reservedAuthorizationParam(canonical) {
			return errors.New("login configuration contains a disallowed authorization parameter")
		}
		if _, exists := seenParams[canonical]; exists {
			return errors.New("login configuration contains duplicate authorization parameters")
		}
		seenParams[canonical] = struct{}{}
	}

	return nil
}

func hasControlCharacters(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) {
			return true
		}
	}

	return false
}

func authorizationURL(config oauth2.Config, state, nonce, challenge string, params *map[string]string) (string, error) {
	options := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}
	if params != nil {
		keys := make([]string, 0, len(*params))
		for key := range *params {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			options = append(options, oauth2.SetAuthURLParam(key, (*params)[key]))
		}
	}
	result := config.AuthCodeURL(state, options...)
	parsed, err := url.Parse(result)
	if err != nil || validateEndpoint(result) != nil {
		return "", errors.New("login authorization endpoint is invalid")
	}

	return parsed.String(), nil
}

func randomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", errors.New("generate login challenge failed")
	}

	return base64.RawURLEncoding.EncodeToString(value), nil
}

func normalizeIssuer(issuer string) (string, error) {
	if issuer == "" || strings.TrimSpace(issuer) != issuer || strings.Contains(issuer, "#") {
		return "", errors.New("login issuer is invalid")
	}
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", errors.New("login issuer is invalid")
	}
	if !secureOrLoopback(parsed) {
		return "", errors.New("login issuer is invalid")
	}

	return issuer, nil
}

func validateEndpointForIssuer(raw, issuer string) error {
	if err := validateEndpoint(raw); err != nil {
		return err
	}
	issuerURL, err := url.Parse(issuer)
	if err != nil {
		return errors.New("login issuer is invalid")
	}
	endpointURL, err := url.Parse(raw)
	if err != nil {
		return errors.New("login provider endpoint is invalid")
	}
	if strings.EqualFold(issuerURL.Scheme, httpsScheme) && !strings.EqualFold(endpointURL.Scheme, httpsScheme) {
		return errors.New("login provider endpoint is invalid")
	}

	return nil
}

func validateEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(raw, "#") {
		return errors.New("login provider endpoint is invalid")
	}
	if !secureOrLoopback(parsed) {
		return errors.New("login provider endpoint is invalid")
	}

	return nil
}

func secureOrLoopback(parsed *url.URL) bool {
	switch strings.ToLower(parsed.Scheme) {
	case httpsScheme:
		return parsed.Hostname() != ""
	case httpScheme:
		host := strings.ToLower(parsed.Hostname())

		return host == "127.0.0.1" || host == "localhost" || host == "::1"
	default:
		return false
	}
}

func canonicalEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}

	return false
}

func reservedAuthorizationParam(key string) bool {
	switch key {
	case "state", "nonce", "code", "code_challenge", "code_challenge_method", "redirect_uri",
		"client_id", "client_secret", "client_secret_basic", "client_secret_post", "client_assertion",
		"client_assertion_type", "response_type", "response_mode", "scope", "grant_type",
		"password", "username", "access_token", "refresh_token", "id_token", "token":
		return true
	default:
		return false
	}
}
