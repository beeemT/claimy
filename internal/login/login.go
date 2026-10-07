// Package login implements Claimy's browser-based OIDC login and credential storage.
package login

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/beeemT/claimy/pkg/client"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	browserTimeout     = 5 * time.Minute
	defaultHTTPTimeout = 30 * time.Second
)

// browserLauncher prints the URL before trying the native browser, so the user
// can always complete the flow manually when a launcher is unavailable.
var browserLauncher = launchBrowser

// Login completes browser authorization and stores the refresh credential in the OS keychain.
func Login(ctx context.Context, server string, timeout time.Duration, stderr io.Writer) (*client.LoginIdentity, error) {
	if ctx == nil {
		return nil, errors.New("login context is required")
	}
	loginCtx, cancelLogin := context.WithTimeout(ctx, browserTimeout)
	defer cancelLogin()

	normalizedServer, err := normalizeServer(server)
	if err != nil {
		return nil, err
	}

	return loginWithServer(loginCtx, normalizedServer, timeout, stderr)
}

func loginWithServer(ctx context.Context, server string, timeout time.Duration, stderr io.Writer) (*client.LoginIdentity, error) {
	httpClient := newHTTPClient(timeout)
	config, err := fetchLoginConfig(ctx, server, timeout, httpClient)
	if err != nil {
		return nil, err
	}
	issuer, err := normalizeIssuer(config.Issuer)
	if err != nil {
		return nil, err
	}
	provider, metadata, err := discoverProvider(ctx, issuer, timeout, httpClient)
	if err != nil {
		return nil, errors.New("login provider discovery is invalid")
	}

	return loginWithProvider(ctx, server, timeout, stderr, httpClient, config, issuer, provider, metadata)
}

func loginWithProvider(
	ctx context.Context,
	server string,
	timeout time.Duration,
	stderr io.Writer,
	httpClient *http.Client,
	config *client.LoginConfig,
	issuer string,
	provider *oidc.Provider,
	metadata providerMetadata,
) (identity *client.LoginIdentity, loginErr error) {
	state, nonce, pkceVerifier, err := newLoginChallenges()
	if err != nil {
		return nil, err
	}
	listener, err := listenCallback(state, issuer)
	if err != nil {
		return nil, errors.New("start login callback failed")
	}
	listenerStopped := false
	defer func() {
		if listenerStopped {
			return
		}
		if err := listener.Shutdown(); err != nil {
			identity = nil
			loginErr = errors.Join(loginErr, err)
		}
	}()

	authConfig := oauth2.Config{
		ClientID:    config.ClientId,
		Endpoint:    oauth2.Endpoint{AuthURL: metadata.AuthorizationEndpoint, TokenURL: metadata.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams},
		RedirectURL: listener.redirectURL,
		Scopes:      config.Scopes,
	}
	challenge := base64.RawURLEncoding.EncodeToString(sha256Bytes([]byte(pkceVerifier)))
	authURL, err := authorizationURL(authConfig, state, nonce, challenge, config.AuthorizationParams)
	if err != nil {
		return nil, err
	}
	callback, err := awaitAuthorizationCallback(ctx, listener, authURL, stderr)
	if err != nil {
		return nil, err
	}
	if err := listener.Shutdown(); err != nil {
		listenerStopped = true

		return nil, err
	}
	listenerStopped = true

	token, err := exchangeAuthorizationCode(ctx, timeout, httpClient, authConfig, callback.code, pkceVerifier)
	if err != nil {
		return nil, err
	}
	identityClaims, err := verifyIdentityToken(ctx, provider, httpClient, timeout, issuer, config.ClientId, token, nonce)
	if err != nil {
		return nil, err
	}
	identity, err = fetchClaimyIdentity(ctx, timeout, httpClient, server, identityClaims)
	if err != nil {
		return nil, err
	}
	credential := storedCredential{
		RefreshToken: token.RefreshToken,
		Server:       server,
		Issuer:       issuer,
		ClientID:     config.ClientId,
		Subject:      identityClaims.subject,
		Email:        identityClaims.email,
	}
	if err := persistLogin(ctx, server, credential); err != nil {
		return nil, err
	}

	return identity, nil
}

func newLoginChallenges() (state, nonce, pkceVerifier string, err error) {
	state, err = randomURLToken(32)
	if err != nil {
		return "", "", "", err
	}
	nonce, err = randomURLToken(32)
	if err != nil {
		return "", "", "", err
	}
	pkceVerifier, err = randomURLToken(48)
	if err != nil {
		return "", "", "", err
	}

	return state, nonce, pkceVerifier, nil
}

func awaitAuthorizationCallback(ctx context.Context, listener *callbackListener, authURL string, stderr io.Writer) (callbackResult, error) {
	if err := launchBrowserAndWait(ctx, authURL, stderr); err != nil {
		return callbackResult{}, err
	}
	callback, err := listener.Wait(ctx)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return callbackResult{}, errors.New("browser login was canceled")
		case errors.Is(err, context.DeadlineExceeded):
			return callbackResult{}, errors.New("browser login timed out")
		default:
			return callbackResult{}, errors.New("login callback failed")
		}
	}
	switch callback.failure {
	case "authorization":
		return callbackResult{}, errors.New("login authorization was denied")
	case callbackFailureInvalid:
		return callbackResult{}, errors.New("login callback is invalid")
	default:
		if callback.failure != "" {
			return callbackResult{}, errors.New("login callback is invalid")
		}
	}

	return callback, nil
}

func launchBrowserAndWait(ctx context.Context, authURL string, stderr io.Writer) error {
	browserCtx, cancelBrowser := context.WithCancel(ctx)
	launchDone := make(chan error, 1)
	go func() {
		launchDone <- browserLauncher(browserCtx, authURL, stderr)
	}()
	select {
	case err := <-launchDone:
		cancelBrowser()
		if err != nil {
			return err
		}
	case <-ctx.Done():
		cancelBrowser()
		<-launchDone

		return errors.New("browser login was canceled")
	}

	return nil
}

func exchangeAuthorizationCode(ctx context.Context, timeout time.Duration, httpClient *http.Client, config oauth2.Config, code, verifier string) (*oauth2.Token, error) {
	exchangeCtx, cancelExchange := requestContext(ctx, timeout, httpClient)
	defer cancelExchange()
	token, err := config.Exchange(exchangeCtx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil || token == nil || token.AccessToken == "" || token.RefreshToken == "" {
		return nil, errors.New("exchange login authorization failed")
	}

	return token, nil
}

func fetchClaimyIdentity(ctx context.Context, timeout time.Duration, httpClient *http.Client, server string, claims verifiedIdentity) (*client.LoginIdentity, error) {
	identityCtx, cancelIdentity := requestContext(ctx, timeout, httpClient)
	defer cancelIdentity()
	identityAPI, err := client.New(server, client.WithBearerToken(claims.idToken), client.WithHTTPClient(httpClient))
	if err != nil {
		return nil, errors.New("create Claimy identity client failed")
	}
	identity, err := identityAPI.Identity(identityCtx)
	if err != nil || identity == nil || canonicalEmail(string(identity.Email)) == "" {
		return nil, errors.New("claimy identity validation failed")
	}
	if canonicalEmail(string(identity.Email)) != claims.email {
		return nil, errors.New("login identity validation failed")
	}

	return identity, nil
}

// Token refreshes a saved login and returns its verified ID token.
func Token(ctx context.Context, server string, timeout time.Duration) (idToken string, tokenErr error) {
	if ctx == nil {
		return "", errors.New("login context is required")
	}
	normalizedServer, err := normalizeServer(server)
	if err != nil {
		return "", err
	}
	lock, err := lockForServer(normalizedServer)
	if err != nil {
		return "", errors.New("create login lock failed")
	}
	if err := acquireLock(ctx, lock); err != nil {
		return "", err
	}
	defer func() {
		if err := releaseLoginLock(lock); err != nil {
			idToken = ""
			tokenErr = errors.Join(tokenErr, err)
		}
	}()

	credential, err := loadCredential(normalizedServer)
	if err != nil {
		return "", err
	}

	return refreshLoginCredential(ctx, normalizedServer, timeout, credential)
}

func loadCredential(server string) (storedCredential, error) {
	raw, err := currentStore.Get(credentialKey(server))
	if errors.Is(err, errCredentialNotFound) {
		return storedCredential{}, errors.New("no saved login is available")
	}
	if err != nil {
		return storedCredential{}, errors.New("secure login credential unavailable")
	}
	var credential storedCredential
	if json.Unmarshal([]byte(raw), &credential) != nil || !credential.validFor(server) {
		return storedCredential{}, errors.New("stored login credential is invalid")
	}

	return credential, nil
}

func refreshLoginCredential(ctx context.Context, server string, timeout time.Duration, credential storedCredential) (string, error) {
	httpClient := newHTTPClient(timeout)
	config, err := fetchLoginConfig(ctx, server, timeout, httpClient)
	if err != nil {
		return "", err
	}
	issuer, err := normalizeIssuer(config.Issuer)
	if err != nil || issuer != credential.Issuer || config.ClientId != credential.ClientID {
		return "", errors.New("login provider configuration changed")
	}
	provider, metadata, err := discoverProvider(ctx, issuer, timeout, httpClient)
	if err != nil {
		return "", errors.New("login provider discovery is invalid")
	}
	token, err := refreshCredentialToken(ctx, timeout, httpClient, credential, metadata)
	if err != nil {
		return "", err
	}
	identity, err := verifyIdentityToken(ctx, provider, httpClient, timeout, issuer, credential.ClientID, token, "")
	if err != nil {
		return "", err
	}
	if identity.subject != credential.Subject || identity.email != credential.Email {
		return "", errors.New("login identity changed")
	}
	if err := persistRotatedRefreshToken(credential, server, token.RefreshToken); err != nil {
		return "", err
	}

	return identity.idToken, nil
}

func refreshCredentialToken(ctx context.Context, timeout time.Duration, httpClient *http.Client, credential storedCredential, metadata providerMetadata) (*oauth2.Token, error) {
	refreshConfig := oauth2.Config{
		ClientID: credential.ClientID,
		Endpoint: oauth2.Endpoint{TokenURL: metadata.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams},
	}
	refreshCtx, cancelRefresh := requestContext(ctx, timeout, httpClient)
	defer cancelRefresh()
	token, err := refreshConfig.TokenSource(refreshCtx, &oauth2.Token{RefreshToken: credential.RefreshToken}).Token()
	if err != nil || token == nil || token.AccessToken == "" {
		return nil, errors.New("refresh login credential failed")
	}

	return token, nil
}

func persistRotatedRefreshToken(credential storedCredential, server, refreshToken string) error {
	if refreshToken == "" || refreshToken == credential.RefreshToken {
		return nil
	}
	credential.RefreshToken = refreshToken
	encoded, err := json.Marshal(credential)
	if err != nil {
		return errors.New("save refreshed login credential failed")
	}
	if err := currentStore.Set(credentialKey(server), string(encoded)); err != nil {
		return errors.New("secure login storage unavailable")
	}

	return nil
}

// Logout removes the saved credential for server and clears its matching default URL.
func Logout(server string) (logoutErr error) {
	if strings.TrimSpace(server) == "" {
		defaultServer, err := DefaultURL()
		if err != nil {
			return err
		}
		if defaultServer == "" {
			return errors.New("no default Claimy server is configured")
		}
		server = defaultServer
	}
	normalizedServer, err := normalizeServer(server)
	if err != nil {
		return err
	}

	lock, err := lockForServer(normalizedServer)
	if err != nil {
		return errors.New("create login lock failed")
	}
	logoutCtx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
	defer cancel()
	if err := acquireLock(logoutCtx, lock); err != nil {
		return err
	}
	defer func() {
		logoutErr = errors.Join(logoutErr, releaseLoginLock(lock))
	}()

	if err := currentStore.Delete(credentialKey(normalizedServer)); err != nil && !errors.Is(err, errCredentialNotFound) {
		return errors.New("secure login storage unavailable")
	}
	if err := clearDefault(logoutCtx, normalizedServer); err != nil {
		return err
	}

	return nil
}

func fetchLoginConfig(parent context.Context, server string, timeout time.Duration, httpClient *http.Client) (*client.LoginConfig, error) {
	ctx, cancel := requestContext(parent, timeout, httpClient)
	defer cancel()
	api, err := client.New(server, client.WithHTTPClient(httpClient))
	if err != nil {
		return nil, errors.New("create Claimy client failed")
	}
	config, err := api.LoginConfig(ctx)
	if err != nil || config == nil {
		return nil, errors.New("retrieve login configuration failed")
	}
	if err := validateLoginConfig(config); err != nil {
		return nil, err
	}

	return config, nil
}

func requestContext(parent context.Context, timeout time.Duration, httpClient *http.Client) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	ctx = oidc.ClientContext(ctx, httpClient)
	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)

	return ctx, cancel
}

func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: responseLimitTransport{base: http.DefaultTransport, limit: maxLoginResponseBytes},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func sha256Bytes(value []byte) []byte {
	hash := sha256.Sum256(value)

	return hash[:]
}
