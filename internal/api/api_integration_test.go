//go:build integration && fixtures

package api

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beeemT/claimy/internal/auth"
	"github.com/beeemT/claimy/internal/claims"
	claimmysql "github.com/beeemT/claimy/internal/storage/mysql"
	"github.com/beeemT/claimy/pkg/client"
	"github.com/beeemT/claimy/test/support"
	"github.com/gin-gonic/gin"
	"github.com/gosoline-project/httpserver"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/kernel"
	"github.com/justtrackio/gosoline/pkg/log"
)

const (
	apiIntegrationRESTIssuer     = "https://accounts.google.com"
	apiIntegrationRESTAudience   = "https://claimy.example.test/rest"
	apiIntegrationGitLabIssuer   = "https://gitlab.example.test"
	apiIntegrationGitLabAudience = "https://claimy.example.test/gitlab"
	apiIntegrationChatAudience   = "https://claimy.example.test/chat"
	apiIntegrationJWKSURL        = "https://keys.example.test/integration.json"
	apiIntegrationKeyID          = "claimy-api-integration"
)

type apiIntegrationKeyProvider struct {
	keys map[string]*rsa.PublicKey
}

func (p apiIntegrationKeyProvider) Key(ctx context.Context, issuer, keyID string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, ok := p.keys[issuer+"\x00"+keyID]
	if !ok {
		return nil, nil
	}

	return key, nil
}

type apiIntegrationServer struct {
	baseURL string
	client  *http.Client
	done    chan struct{}
}

type apiIntegrationRuntime struct {
	port     int
	kernel   kernel.Kernel
	exitCode *atomic.Int32
}

func newAPIIntegrationRuntime(t *testing.T, fixture *support.Fixture, identities auth.Authenticator) *apiIntegrationRuntime {
	t.Helper()

	mutableConfig, ok := fixture.Config.(cfg.GosoConf)
	if !ok {
		t.Fatal("integration fixture config does not support configuration options")
	}
	if err := mutableConfig.Option(cfg.WithConfigMap(map[string]any{
		"tracing":  map[string]any{"provider": "noop"},
		"sampling": map[string]any{"enabled": false},
		"metric":   map[string]any{"enabled": false},
	})); err != nil {
		t.Fatalf("configure integration HTTP server: %v", err)
	}

	rootLogger, ok := fixture.Logger.(log.GosoLogger)
	if !ok {
		t.Fatal("integration fixture logger does not support kernel startup")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve integration HTTP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved integration HTTP port: %v", err)
	}

	operations := claims.NewService(claimmysql.New(fixture.Client))
	routeFactory := func(ctx context.Context, config cfg.Config, logger log.Logger, router *httpserver.Router) error {
		return Register(ctx, config, logger, router, operations, identities, fixture.Client)
	}
	serverFactory := httpserver.NewServerWithSettings(fixture.Context, "api-integration", routeFactory, &httpserver.Settings{
		Port:        strconv.Itoa(port),
		Mode:        gin.TestMode,
		Compression: httpserver.CompressionSettings{Level: "none"},
		Timeout:     httpserver.TimeoutSettings{Shutdown: 5 * time.Second},
	}, httpserver.WithErrorMapper(ErrorMapper), httpserver.WithErrorHandler(ErrorHandler))

	exitCode := &atomic.Int32{}
	k, err := kernel.BuildKernel(fixture.Context, fixture.Config, rootLogger, []kernel.Option{
		kernel.WithModuleFactory("api-integration", serverFactory),
		kernel.WithExitHandler(func(code int) { exitCode.Store(int32(code)) }),
	})
	if err != nil {
		t.Fatalf("build registered API HTTP server: %v", err)
	}

	return &apiIntegrationRuntime{port: port, kernel: k, exitCode: exitCode}
}

func newAPIIntegrationServer(t *testing.T, fixture *support.Fixture, identities auth.Authenticator) *apiIntegrationServer {
	t.Helper()

	runtime := newAPIIntegrationRuntime(t, fixture, identities)
	server := &apiIntegrationServer{
		baseURL: fmt.Sprintf("http://localhost:%d", runtime.port),
		client:  &http.Client{Timeout: 5 * time.Second},
		done:    make(chan struct{}),
	}
	go func() {
		runtime.kernel.Run()
		close(server.done)
	}()
	t.Cleanup(func() {
		runtime.kernel.Stop("API integration test complete")
		select {
		case <-server.done:
		case <-time.After(10 * time.Second):
			t.Error("registered API HTTP server did not stop")
		}
		if code := runtime.exitCode.Load(); code != kernel.ExitCodeOk {
			t.Errorf("integration HTTP kernel exit code = %d, want %d", code, kernel.ExitCodeOk)
		}
	})

	waitForAPIIntegrationServer(t, server, runtime.kernel)

	return server
}

func waitForAPIIntegrationServer(t *testing.T, server *apiIntegrationServer, k kernel.Kernel) {
	t.Helper()

	select {
	case <-k.Running():
	case <-server.done:
		t.Fatal("registered API HTTP server exited before becoming ready")
	case <-time.After(10 * time.Second):
		t.Fatal("registered API HTTP server did not start")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := server.client.Get(server.baseURL + "/health")
		if requestErr == nil {
			if err := response.Body.Close(); err != nil {
				t.Fatalf("close API health response: %v", err)
			}
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-server.done:
			t.Fatal("registered API HTTP server exited during startup")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("registered API HTTP server did not pass its health check")
}

func newAPIIntegrationVerifier(t *testing.T) (*auth.Verifier, *rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate API integration RSA key: %v", err)
	}
	wrongPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate wrong-signature RSA key: %v", err)
	}
	settings := auth.Settings{
		TeamDomain: "example.test",
		REST: auth.IssuerSettings{
			Issuer:   apiIntegrationRESTIssuer,
			Audience: apiIntegrationRESTAudience,
			JWKSURL:  apiIntegrationJWKSURL,
		},
		GitLab: auth.IssuerSettings{
			Issuer:   apiIntegrationGitLabIssuer,
			Audience: apiIntegrationGitLabAudience,
			JWKSURL:  "https://keys.example.test/gitlab.json",
		},
		Chat: auth.IssuerSettings{
			Issuer:   apiIntegrationRESTIssuer,
			Audience: apiIntegrationChatAudience,
			JWKSURL:  apiIntegrationJWKSURL,
		},
	}
	verifier, err := auth.NewWithKeys(settings, apiIntegrationKeyProvider{keys: map[string]*rsa.PublicKey{
		apiIntegrationRESTIssuer + "\x00" + apiIntegrationKeyID:   &privateKey.PublicKey,
		apiIntegrationGitLabIssuer + "\x00" + apiIntegrationKeyID: &privateKey.PublicKey,
	}})
	if err != nil {
		t.Fatalf("create API integration verifier: %v", err)
	}

	return verifier, privateKey, wrongPrivateKey
}

func apiIntegrationManualToken(t *testing.T, key *rsa.PrivateKey, email, subject string) string {
	t.Helper()

	return signAPIIntegrationToken(t, key, map[string]any{
		"iss":   apiIntegrationRESTIssuer,
		"aud":   apiIntegrationRESTAudience,
		"sub":   subject,
		"email": email,
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"nbf":   time.Now().Add(-time.Minute).Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
}

func apiIntegrationGitLabClaims(email string) map[string]any {
	now := time.Now()

	return map[string]any{
		"iss":            apiIntegrationGitLabIssuer,
		"aud":            apiIntegrationGitLabAudience,
		"iat":            now.Add(-time.Minute).Unix(),
		"nbf":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(time.Hour).Unix(),
		"user_id":        "gitlab-user-77",
		"user_email":     email,
		"job_project_id": "project-88",
		"job_id":         "job-99",
	}
}

func apiIntegrationGitLabToken(t *testing.T, key *rsa.PrivateKey, email string) string {
	t.Helper()

	return signAPIIntegrationToken(t, key, apiIntegrationGitLabClaims(email))
}

func signAPIIntegrationToken(t *testing.T, key *rsa.PrivateKey, tokenClaims map[string]any) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "kid": apiIntegrationKeyID, "typ": "JWT"})
	if err != nil {
		t.Fatalf("encode API integration JWT header: %v", err)
	}
	claimsJSON, err := json.Marshal(tokenClaims)
	if err != nil {
		t.Fatalf("encode API integration JWT claims: %v", err)
	}
	input := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign API integration JWT: %v", err)
	}

	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

type apiHTTPResponse struct {
	status int
	body   []byte
}

func (s *apiIntegrationServer) request(t *testing.T, method, path, token string, body any) apiHTTPResponse {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s request body: %v", path, err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, s.baseURL+path, requestBody)
	if err != nil {
		t.Fatalf("create %s request: %v", path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := s.client.Do(request)
	if err != nil {
		t.Fatalf("send %s request: %v", path, err)
	}
	encoded, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		t.Fatalf("read %s response body: %v", path, readErr)
	}
	if closeErr != nil {
		t.Fatalf("close %s response body: %v", path, closeErr)
	}

	return apiHTTPResponse{status: response.StatusCode, body: encoded}
}

func decodeAPIResponse[T any](t *testing.T, response apiHTTPResponse) T {
	t.Helper()
	var decoded T
	if err := json.Unmarshal(response.body, &decoded); err != nil {
		t.Fatalf("decode HTTP %d response %q: %v", response.status, response.body, err)
	}

	return decoded
}

func assertAPIError(t *testing.T, response apiHTTPResponse, wantStatus int, wantCode client.ErrorDetailCode) {
	t.Helper()
	if response.status != wantStatus {
		t.Fatalf("HTTP status = %d, want %d (body %q)", response.status, wantStatus, response.body)
	}
	body := decodeAPIResponse[client.ErrorResponse](t, response)
	if body.Error.Status != int32(wantStatus) || body.Error.Code != wantCode {
		t.Fatalf("API error response = %#v, want status %d code %q", body.Error, wantStatus, wantCode)
	}
}

func insertCatalogGroup(t *testing.T, fixture *support.Fixture, name string) uint64 {
	t.Helper()
	result, err := fixture.SQLDB.ExecContext(t.Context(), "INSERT INTO app_groups (canonical_name, created_at) VALUES (?, UTC_TIMESTAMP(6))", name)
	if err != nil {
		t.Fatalf("insert catalog group %q: %v", name, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read catalog group %q ID: %v", name, err)
	}

	return uint64(id)
}

func insertCatalogApp(t *testing.T, fixture *support.Fixture, groupID uint64, name string) {
	t.Helper()
	if _, err := fixture.SQLDB.ExecContext(t.Context(), "INSERT INTO apps (group_id, canonical_name, created_at) VALUES (?, ?, UTC_TIMESTAMP(6))", groupID, name); err != nil {
		t.Fatalf("insert catalog app %q: %v", name, err)
	}
}

func apiTableCount(t *testing.T, fixture *support.Fixture, table string) int64 {
	t.Helper()
	var count int64
	if err := fixture.SQLDB.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}

	return count
}

type apiWriteCounts struct {
	groups       int64
	apps         int64
	claims       int64
	environments int64
	versions     int64
	results      int64
}

func apiCounts(t *testing.T, fixture *support.Fixture) apiWriteCounts {
	t.Helper()

	return apiWriteCounts{
		groups:       apiTableCount(t, fixture, "app_groups"),
		apps:         apiTableCount(t, fixture, "apps"),
		claims:       apiTableCount(t, fixture, "claims"),
		environments: apiTableCount(t, fixture, "claim_environments"),
		versions:     apiTableCount(t, fixture, "claim_versions"),
		results:      apiTableCount(t, fixture, "request_results"),
	}
}

func assertNoAPIWrites(t *testing.T, fixture *support.Fixture) {
	t.Helper()
	if counts := apiCounts(t, fixture); counts != (apiWriteCounts{}) {
		t.Fatalf("unauthorized request wrote API state: %#v", counts)
	}
}

func TestRegisteredCatalogHTTPRoutesAreReadOnlyAndGroupScoped(t *testing.T) {
	fixture := support.NewFixture(t)
	verifier, privateKey, _ := newAPIIntegrationVerifier(t)
	server := newAPIIntegrationServer(t, fixture, verifier)
	token := apiIntegrationManualToken(t, privateKey, "catalog-reader@example.test", "google-user-catalog")

	alphaID := insertCatalogGroup(t, fixture, "catalog-alpha")
	betaID := insertCatalogGroup(t, fixture, "catalog-beta")
	insertCatalogApp(t, fixture, alphaID, "api")
	insertCatalogApp(t, fixture, alphaID, "worker")
	insertCatalogApp(t, fixture, betaID, "worker")
	insertCatalogApp(t, fixture, betaID, "database")
	before := apiCounts(t, fixture)

	assertCatalogGroupReadRoutes(t, server, token, alphaID)
	assertCatalogGroupListRoutes(t, server, token)
	assertCatalogAppListRoutes(t, server, token, alphaID, betaID)
	assertCatalogWriteRoutes(t, server, token)

	if after := apiCounts(t, fixture); after != before {
		t.Fatalf("catalog read/write route changed database state: before %#v after %#v", before, after)
	}
}

func assertCatalogGroupReadRoutes(t *testing.T, server *apiIntegrationServer, token string, alphaID uint64) {
	t.Helper()
	assertAPIError(t, server.request(t, http.MethodGet, "/v1/catalog/groups/catalog-alpha", "", nil), http.StatusUnauthorized, client.ErrorDetailCodeUnauthenticated)

	groupResponse := server.request(t, http.MethodGet, "/v1/catalog/groups/catalog-alpha", token, nil)
	if groupResponse.status != http.StatusOK {
		t.Fatalf("read catalog group status = %d, want 200 (body %q)", groupResponse.status, groupResponse.body)
	}
	group := decodeAPIResponse[client.CatalogGroup](t, groupResponse)
	if group.Id != strconv.FormatUint(alphaID, 10) || group.CanonicalName != "catalog-alpha" {
		t.Fatalf("catalog group = %#v, want catalog-alpha ID %d", group, alphaID)
	}
	assertAPIError(t, server.request(t, http.MethodGet, "/v1/catalog/groups/catalog-missing", token, nil), http.StatusNotFound, client.ErrorDetailCodeNotFound)
}

func assertCatalogGroupListRoutes(t *testing.T, server *apiIntegrationServer, token string) {
	t.Helper()
	allGroupsResponse := server.request(t, http.MethodPost, "/v1/catalog/groups/query", token, map[string]any{})
	if allGroupsResponse.status != http.StatusOK {
		t.Fatalf("list catalog groups status = %d, want 200 (body %q)", allGroupsResponse.status, allGroupsResponse.body)
	}
	allGroups := decodeAPIResponse[client.CatalogGroupsResponse](t, allGroupsResponse)
	if allGroups.Total != 2 || len(allGroups.Results) != 2 {
		t.Fatalf("unfiltered catalog groups = %#v, want both seeded groups", allGroups)
	}
	groupNames := map[string]bool{}
	for _, result := range allGroups.Results {
		groupNames[result.CanonicalName] = true
	}
	if !groupNames["catalog-alpha"] || !groupNames["catalog-beta"] {
		t.Fatalf("unfiltered catalog group names = %#v", groupNames)
	}

	filteredGroupsResponse := server.request(t, http.MethodPost, "/v1/catalog/groups/query", token, map[string]any{
		"filter": map[string]any{"canonicalName": "catalog-beta"},
	})
	if filteredGroupsResponse.status != http.StatusOK {
		t.Fatalf("filtered catalog groups status = %d, want 200 (body %q)", filteredGroupsResponse.status, filteredGroupsResponse.body)
	}
	filteredGroups := decodeAPIResponse[client.CatalogGroupsResponse](t, filteredGroupsResponse)
	if filteredGroups.Total != 1 || len(filteredGroups.Results) != 1 || filteredGroups.Results[0].CanonicalName != "catalog-beta" {
		t.Fatalf("filtered catalog groups = %#v, want only catalog-beta", filteredGroups)
	}
}

func assertCatalogAppListRoutes(t *testing.T, server *apiIntegrationServer, token string, alphaID, betaID uint64) {
	t.Helper()
	filteredAppsResponse := server.request(t, http.MethodPost, "/v1/catalog/groups/catalog-alpha/apps/query", token, map[string]any{
		"filter": map[string]any{"canonicalName": "api"},
	})
	if filteredAppsResponse.status != http.StatusOK {
		t.Fatalf("filtered catalog apps status = %d, want 200 (body %q)", filteredAppsResponse.status, filteredAppsResponse.body)
	}
	filteredApps := decodeAPIResponse[client.CatalogAppsResponse](t, filteredAppsResponse)
	if filteredApps.Total != 1 || len(filteredApps.Results) != 1 || filteredApps.Results[0].CanonicalName != "api" || filteredApps.Results[0].GroupId != strconv.FormatUint(alphaID, 10) {
		t.Fatalf("filtered catalog apps = %#v, want only catalog-alpha/api", filteredApps)
	}

	betaAppsResponse := server.request(t, http.MethodPost, "/v1/catalog/groups/catalog-beta/apps/query", token, map[string]any{})
	if betaAppsResponse.status != http.StatusOK {
		t.Fatalf("list catalog-beta apps status = %d, want 200 (body %q)", betaAppsResponse.status, betaAppsResponse.body)
	}
	betaApps := decodeAPIResponse[client.CatalogAppsResponse](t, betaAppsResponse)
	if betaApps.Total != 2 || len(betaApps.Results) != 2 {
		t.Fatalf("catalog-beta apps = %#v, want only its two apps", betaApps)
	}
	for _, app := range betaApps.Results {
		if app.GroupId != strconv.FormatUint(betaID, 10) {
			t.Fatalf("catalog-beta app escaped group scope: %#v", app)
		}
	}
	assertAPIError(t, server.request(t, http.MethodPost, "/v1/catalog/groups/catalog-missing/apps/query", token, map[string]any{}), http.StatusNotFound, client.ErrorDetailCodeNotFound)
}

func assertCatalogWriteRoutes(t *testing.T, server *apiIntegrationServer, token string) {
	t.Helper()
	for _, request := range []struct {
		method string
		path   string
		body   any
	}{
		{method: http.MethodPost, path: "/v1/catalog/groups", body: map[string]any{"canonicalName": "catalog-created-through-api"}},
		{method: http.MethodPut, path: "/v1/catalog/groups/catalog-alpha", body: map[string]any{"canonicalName": "renamed"}},
		{method: http.MethodPatch, path: "/v1/catalog/groups/catalog-alpha", body: map[string]any{"canonicalName": "renamed"}},
		{method: http.MethodDelete, path: "/v1/catalog/groups/catalog-alpha"},
	} {
		response := server.request(t, request.method, request.path, token, request.body)
		if response.status != http.StatusNotFound {
			t.Fatalf("catalog write route %s %s status = %d, want 404 (body %q)", request.method, request.path, response.status, response.body)
		}
	}
}

func TestRegisteredClaimsHTTPAuthAndOwnerRules(t *testing.T) {
	fixture := support.NewFixture(t)
	verifier, privateKey, wrongPrivateKey := newAPIIntegrationVerifier(t)
	server := newAPIIntegrationServer(t, fixture, verifier)

	assertAPIAuthenticationFailures(t, fixture, server, privateKey, wrongPrivateKey)
	assertAPIOwnerAndMutationRules(t, fixture, server, privateKey)
}

func assertAPIAuthenticationFailures(t *testing.T, fixture *support.Fixture, server *apiIntegrationServer, privateKey, wrongPrivateKey *rsa.PrivateKey) {
	t.Helper()
	validGitLabClaims := apiIntegrationGitLabClaims("gitlab-owner@example.test")
	invalidSignature := signAPIIntegrationToken(t, wrongPrivateKey, validGitLabClaims)
	invalidIssuerClaims := apiIntegrationGitLabClaims("gitlab-owner@example.test")
	invalidIssuerClaims["iss"] = "https://untrusted.example.test"
	invalidAudienceClaims := apiIntegrationGitLabClaims("gitlab-owner@example.test")
	invalidAudienceClaims["aud"] = "https://other.example.test"
	expiredClaims := apiIntegrationGitLabClaims("gitlab-owner@example.test")
	expiredClaims["exp"] = time.Now().Add(-time.Hour).Unix()
	nonTeamToken := apiIntegrationGitLabToken(t, privateKey, "contractor@outside.example")

	for index, test := range []struct {
		name   string
		token  string
		status int
		code   client.ErrorDetailCode
	}{
		{name: "invalid GitLab signature", token: invalidSignature, status: http.StatusUnauthorized, code: client.ErrorDetailCodeUnauthenticated},
		{name: "untrusted GitLab issuer", token: signAPIIntegrationToken(t, privateKey, invalidIssuerClaims), status: http.StatusUnauthorized, code: client.ErrorDetailCodeUnauthenticated},
		{name: "wrong GitLab audience", token: signAPIIntegrationToken(t, privateKey, invalidAudienceClaims), status: http.StatusUnauthorized, code: client.ErrorDetailCodeUnauthenticated},
		{name: "expired GitLab token", token: signAPIIntegrationToken(t, privateKey, expiredClaims), status: http.StatusUnauthorized, code: client.ErrorDetailCodeUnauthenticated},
		{name: "non-team GitLab email", token: nonTeamToken, status: http.StatusForbidden, code: client.ErrorDetailCodeForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := server.request(t, http.MethodPost, "/v1/claims/acquire", test.token, map[string]any{
				"group":        "auth-denied-group",
				"app":          "denied-app",
				"environments": []string{"sandbox"},
				"requestId":    "auth-denied-" + strconv.Itoa(index),
			})
			assertAPIError(t, response, test.status, test.code)
			assertNoAPIWrites(t, fixture)
		})
	}
}

func assertAPIOwnerAndMutationRules(t *testing.T, fixture *support.Fixture, server *apiIntegrationServer, privateKey *rsa.PrivateKey) {
	t.Helper()
	manualClaim, aliceToken := acquireAPIManualOwnerClaim(t, fixture, server, privateKey)
	ciClaim := acquireAPIGitLabOwnerClaim(t, server, manualClaim, privateKey)
	manualManagedClaim := acquireAPIManagedManualClaim(t, server, aliceToken)
	carolToken := apiIntegrationManualToken(t, privateKey, "carol@example.test", "google-user-carol")
	managedClaims := []apiManagedClaim{
		{name: "manual", claim: manualManagedClaim, expectedOwner: "alice@example.test"},
		{name: "CI", claim: ciClaim, expectedOwner: "bob@example.test"},
	}

	assertAPINonOwnerExpiryChanges(t, server, carolToken, managedClaims)
	assertAPINonOwnerReleases(t, server, carolToken, managedClaims)
	assertAPINonOwnerAudit(t, fixture, managedClaims)
}

func acquireAPIManualOwnerClaim(t *testing.T, fixture *support.Fixture, server *apiIntegrationServer, privateKey *rsa.PrivateKey) (claims.Claim, string) {
	t.Helper()
	aliceToken := apiIntegrationManualToken(t, privateKey, "alice@example.test", "google-user-alice")
	forgedOwnerResponse := server.request(t, http.MethodPost, "/v1/claims/acquire", aliceToken, map[string]any{
		"group":        "forged-owner-group",
		"environments": []string{"sandbox"},
		"requestId":    "forged-owner-request",
		"ownerEmail":   "victim@example.test",
	})
	assertAPIError(t, forgedOwnerResponse, http.StatusBadRequest, client.ErrorDetailCodeInvalidRequest)
	assertNoAPIWrites(t, fixture)

	manualClaim := acquireAPIClaim(t, server, aliceToken, map[string]any{
		"group":        "owner-check",
		"app":          "service",
		"environments": []string{"sandbox"},
		"requestId":    "owner-check-alice",
	})
	if manualClaim.OwnerEmail != "alice@example.test" || manualClaim.Source != claims.Manual {
		t.Fatalf("REST claim owner/source = %q/%q, want alice/manual", manualClaim.OwnerEmail, manualClaim.Source)
	}

	return manualClaim, aliceToken
}

func acquireAPIGitLabOwnerClaim(t *testing.T, server *apiIntegrationServer, manualClaim claims.Claim, privateKey *rsa.PrivateKey) claims.Claim {
	t.Helper()
	bobGitLabToken := apiIntegrationGitLabToken(t, privateKey, "bob@example.test")
	busyResponse := server.request(t, http.MethodPost, "/v1/claims/acquire", bobGitLabToken, map[string]any{
		"group":        "owner-check",
		"app":          "service",
		"environments": []string{"sandbox"},
		"requestId":    "owner-check-bob",
	})
	if busyResponse.status != http.StatusOK {
		t.Fatalf("different-account overlapping acquisition status = %d, want 200 (body %q)", busyResponse.status, busyResponse.body)
	}
	busy := decodeAPIResponse[claims.AcquireResult](t, busyResponse)
	if busy.Acquired || busy.Claim != nil || len(busy.Conflicts) != 1 || busy.Conflicts[0].OwnerEmail != manualClaim.OwnerEmail {
		t.Fatalf("different-account overlap = %#v, want busy conflict owned by Alice", busy)
	}

	ciClaim := acquireAPIClaim(t, server, bobGitLabToken, map[string]any{
		"group":        "managed-ci",
		"app":          "worker",
		"environments": []string{"prod"},
		"requestId":    "managed-ci-bob",
	})
	if ciClaim.OwnerEmail != "bob@example.test" || ciClaim.Source != claims.CI || ciClaim.GitLab == nil || ciClaim.GitLab.ProjectID != "project-88" {
		t.Fatalf("GitLab claim = %#v, want valid CI owner and identity without a compliance-role claim", ciClaim)
	}

	return ciClaim
}

func acquireAPIManagedManualClaim(t *testing.T, server *apiIntegrationServer, aliceToken string) claims.Claim {
	t.Helper()

	return acquireAPIClaim(t, server, aliceToken, map[string]any{
		"group":        "managed-manual",
		"app":          "api",
		"environments": []string{"sandbox"},
		"requestId":    "managed-manual-alice",
	})
}

type apiManagedClaim struct {
	name          string
	claim         claims.Claim
	expectedOwner string
}

func assertAPINonOwnerExpiryChanges(t *testing.T, server *apiIntegrationServer, carolToken string, managedClaims []apiManagedClaim) {
	t.Helper()
	for _, test := range managedClaims {
		t.Run("non-owner changes "+test.name+" expiry", func(t *testing.T) {
			newExpiry := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
			response := server.request(t, http.MethodPatch, "/v1/claims/"+test.claim.ID, carolToken, map[string]any{
				"expiresAt":        newExpiry.Format(time.RFC3339),
				"expectedRevision": 1,
				"requestId":        "carol-expiry-" + test.name,
			})
			if response.status != http.StatusOK {
				t.Fatalf("non-owner expiry status = %d, want 200 (body %q)", response.status, response.body)
			}
			mutation := decodeAPIResponse[claims.MutationResult](t, response)
			if !mutation.Changed || mutation.Claim.Revision != 2 || mutation.Claim.OwnerEmail != test.claim.OwnerEmail || !mutation.Claim.ExpiresAt.Equal(newExpiry) {
				t.Fatalf("non-owner expiry result = %#v, want changed revision 2 preserving owner and requested expiry", mutation)
			}
		})
	}
}

func assertAPINonOwnerReleases(t *testing.T, server *apiIntegrationServer, carolToken string, managedClaims []apiManagedClaim) {
	t.Helper()
	for _, test := range managedClaims {
		t.Run("non-owner releases "+test.name, func(t *testing.T) {
			response := server.request(t, http.MethodPost, "/v1/claims/"+test.claim.ID+"/release", carolToken, map[string]any{
				"requestId": "carol-release-" + test.name,
			})
			if response.status != http.StatusOK {
				t.Fatalf("non-owner release status = %d, want 200 (body %q)", response.status, response.body)
			}
			mutation := decodeAPIResponse[claims.MutationResult](t, response)
			if !mutation.Changed || mutation.Claim.OwnerEmail != test.claim.OwnerEmail || mutation.Claim.ReleasedAt == nil {
				t.Fatalf("non-owner release result = %#v, want released claim with original owner", mutation)
			}
		})
	}
}

func assertAPINonOwnerAudit(t *testing.T, fixture *support.Fixture, managedClaims []apiManagedClaim) {
	t.Helper()
	for _, test := range managedClaims {
		var stored struct {
			OwnerEmail string       `db:"owner_email"`
			Revision   uint32       `db:"revision"`
			ReleasedAt sql.NullTime `db:"released_at"`
		}
		if err := fixture.Client.Get(t.Context(), &stored, "SELECT owner_email, revision, released_at FROM claims WHERE id = ?", test.claim.ID); err != nil {
			t.Fatalf("read managed claim %s: %v", test.claim.ID, err)
		}
		if stored.OwnerEmail != test.expectedOwner || stored.Revision != 3 || !stored.ReleasedAt.Valid {
			t.Fatalf("managed claim state = %#v, want owner %q, revision 3, released", stored, test.expectedOwner)
		}

		var carolActions int
		if err := fixture.Client.Get(t.Context(), &carolActions, "SELECT COUNT(*) FROM claim_versions WHERE claim_id = ? AND actor_email = ? AND actor_subject = ? AND channel = ?", test.claim.ID, "carol@example.test", "google-user-carol", claims.REST); err != nil {
			t.Fatalf("read non-owner claim audit rows: %v", err)
		}
		if carolActions != 2 {
			t.Fatalf("Carol audit rows for claim %s = %d, want expiry change and release", test.claim.ID, carolActions)
		}
	}
}

func acquireAPIClaim(t *testing.T, server *apiIntegrationServer, token string, body map[string]any) claims.Claim {
	t.Helper()
	response := server.request(t, http.MethodPost, "/v1/claims/acquire", token, body)
	if response.status != http.StatusOK {
		t.Fatalf("acquire claim status = %d, want 200 (body %q)", response.status, response.body)
	}
	result := decodeAPIResponse[claims.AcquireResult](t, response)
	if !result.Acquired || result.Claim == nil {
		t.Fatalf("acquire claim result = %#v, want acquired", result)
	}

	return *result.Claim
}

func TestRESTQueryRequiresGroupAndExpiryRevisionRange(t *testing.T) {
	fixture := support.NewFixture(t)
	verifier, key, _ := newAPIIntegrationVerifier(t)
	server := newAPIIntegrationServer(t, fixture, verifier)
	token := apiIntegrationManualToken(t, key, "boundary@example.test", "boundary-user")
	assertAPIError(t, server.request(t, http.MethodPost, "/v1/claims/query", token, map[string]any{}), http.StatusBadRequest, client.ErrorDetailCodeInvalidRequest)
	assertAPIError(t, server.request(t, http.MethodPost, "/v1/claims/query", token, map[string]any{"group": nil}), http.StatusBadRequest, client.ErrorDetailCodeInvalidRequest)
	assertAPIError(t, server.request(t, http.MethodPost, "/v1/claims/query", token, map[string]any{"group": "g", "app": nil}), http.StatusBadRequest, client.ErrorDetailCodeInvalidRequest)
	assertAPIError(t, server.request(t, http.MethodPost, "/v1/claims/acquire", token, map[string]any{"group": "g", "app": "", "environments": []string{"sandbox"}, "requestId": "blank-app"}), http.StatusBadRequest, client.ErrorDetailCodeInvalidRequest)
	for _, revision := range []int64{0, -1, 4294967296} {
		assertAPIError(t, server.request(t, http.MethodPatch, "/v1/claims/not-a-uuid", token, map[string]any{"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "expectedRevision": revision, "requestId": fmt.Sprintf("bad-revision-%d", revision)}), http.StatusBadRequest, client.ErrorDetailCodeInvalidRequest)
	}
}

func TestCIScriptsAcquireAndReleaseRegisteredRoute(t *testing.T) {
	fixture := support.NewFixture(t)
	verifier, key, _ := newAPIIntegrationVerifier(t)
	server := newAPIIntegrationServer(t, fixture, verifier)
	token := apiIntegrationGitLabToken(t, key, "shell@example.test")
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	env := append(os.Environ(), "CLAIMY_URL="+server.baseURL, "CLAIMY_ID_TOKEN="+token, "CLAIMY_GROUP=shell-group", "CLAIMY_APP=shell-app", "CLAIMY_REQUEST_ID=shell-acquire-1", "CLAIMY_ENVIRONMENTS=[\"sandbox\"]")
	acquire := exec.Command("bash", filepath.Join(root, "scripts", "ci-acquire.sh"))
	acquire.Env = env
	out, err := acquire.Output()
	if err != nil {
		t.Fatalf("ci acquire: %v", err)
	}
	claimID := strings.TrimSpace(string(out))
	if claimID == "" {
		t.Fatal("ci acquire returned empty claim ID")
	}
	releaseEnv := make([]string, len(env), len(env)+2)
	copy(releaseEnv, env)
	releaseEnv = append(releaseEnv, "CLAIMY_CLAIM_ID="+claimID, "CLAIMY_RELEASE_REQUEST_ID=shell-release-1")
	release := exec.Command("bash", filepath.Join(root, "scripts", "ci-release.sh"))
	release.Env = releaseEnv
	if out, err := release.CombinedOutput(); err != nil {
		t.Fatalf("ci release: %v (%s)", err, out)
	}
	var released sql.NullTime
	if err := fixture.Client.Get(t.Context(), &released, "SELECT released_at FROM claims WHERE id = ?", claimID); err != nil {
		t.Fatalf("read shell claim: %v", err)
	}
	if !released.Valid {
		t.Fatal("shell release did not persist released_at")
	}
}
