//go:build integration && fixtures

package e2e

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beeemT/claimy/test/support"
)

const (
	e2eRESTIssuer   = "https://issuer.claimy.e2e.test"
	e2eRESTAudience = "claimy-image-smoke-rest"
	e2eChatIssuer   = "https://accounts.google.com"
	e2eChatAudience = "https://claimy.e2e.test/v1/chat/events"
	e2eGitLabIssuer = "https://gitlab.claimy.e2e.test"
	e2eTeamDomain   = "example.test"
	e2eChatIdentity = "claimy-e2e"
	e2eChatSpace    = "spaces/claimy-e2e"
	e2eKeyID        = "claimy-e2e-key"
)

func TestImageRuntimeIntegration(t *testing.T) {
	image := os.Getenv("CLAIMY_IMAGE")
	if image == "" {
		t.Skip("set CLAIMY_IMAGE to run the built-image integration test")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Fatalf("image integration requires Docker host networking support on Linux or macOS")
	}

	fixture := support.NewFixture(t)
	suffix := randomSuffix(t)
	tempDir := t.TempDir()
	jwks := startTestJWKS(t, tempDir)
	configPath := writeImageConfig(t, tempDir, fixture, jwks.url)
	container := startImage(t, image, configPath, jwks.caPath, suffix)
	t.Cleanup(func() { container.cleanup(t) })
	t.Cleanup(func() {
		if t.Failed() {
			logContainerFailureDiagnostics(t, container)
			logDependencyFailureDiagnostics(t, fixture, jwks)
		}
	})

	port := publishedPort(t, container.id)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 10 * time.Second}
	waitForHealth(t, client, baseURL, container.id)

	runRESTSmoke(t, client, baseURL, fixture, jwks, suffix)
	runChatSmoke(t, client, baseURL, fixture, jwks, suffix)
	assertGracefulShutdown(t, container)
}

func runRESTSmoke(t *testing.T, client *http.Client, baseURL string, fixture *support.Fixture, jwks *testJWKS, suffix string) {
	t.Helper()
	restSubject := "rest-user-" + suffix
	restEmail := "rest.owner@" + e2eTeamDomain
	acquireRequestID := "e2e-rest-acquire-" + suffix
	releaseRequestID := "e2e-rest-release-" + suffix
	restGroup := "image-rest-" + suffix
	restToken := signIDToken(t, jwks.jwtKey, e2eRESTIssuer, e2eRESTAudience, restSubject, restEmail, false)

	status, body := postJSON(t, client, baseURL, "/v1/claims/acquire", restToken, map[string]any{
		"group": restGroup, "environments": []string{"sandbox"}, "requestId": acquireRequestID,
	})
	requireStatus(t, status, http.StatusOK, "REST acquire", body)
	var acquired acquireResponse
	decodeJSON(t, body, &acquired, "REST acquire")
	if !acquired.Acquired || acquired.Claim == nil || acquired.Claim.ID == "" {
		t.Fatalf("REST acquire did not return an acquired claim")
	}
	restClaimID := acquired.Claim.ID
	if acquired.Claim.OwnerEmail != restEmail || acquired.Claim.Source != "manual" || !acquired.Claim.ActiveNow || acquired.Claim.Revision != 1 {
		t.Fatalf("REST acquire returned unexpected claim identity or state")
	}

	status, body = postJSON(t, client, baseURL, "/v1/claims/query", restToken, map[string]any{
		"group": restGroup, "environments": []string{"sandbox"},
	})
	requireStatus(t, status, http.StatusOK, "REST query", body)
	var queried queryResponse
	decodeJSON(t, body, &queried, "REST query")
	if !queried.Known || len(queried.Claims) != 1 {
		t.Fatalf("REST query did not return the acquired claim")
	}
	queriedClaim := queried.Claims[0]
	if queriedClaim.ID != restClaimID || queriedClaim.OwnerEmail != restEmail || queriedClaim.Source != "manual" || !queriedClaim.ActiveNow || queriedClaim.Scope.Group != restGroup {
		t.Fatalf("REST query returned unexpected owner or claim state")
	}
	if len(queriedClaim.Environments) != 1 || queriedClaim.Environments[0] != "sandbox" {
		t.Fatalf("REST query returned unexpected claim environments")
	}

	assertClaimRow(t, fixture, restClaimID, restEmail, "manual", false, 1)
	assertHistory(t, fixture, restClaimID, []historyRow{
		{Revision: 1, ActorEmail: restEmail, ActorIssuer: e2eRESTIssuer, ActorSubject: restSubject, Channel: "rest", Action: "acquired"},
	})
	assertRequestResult(t, fixture, "rest_user", e2eRESTIssuer, restSubject, acquireRequestID, "acquired", restClaimID)

	status, body = postJSON(t, client, baseURL, "/v1/claims/"+restClaimID+"/release", restToken, map[string]string{"requestId": releaseRequestID})
	requireStatus(t, status, http.StatusOK, "REST release", body)
	assertClaimRow(t, fixture, restClaimID, restEmail, "manual", true, 2)
	assertHistory(t, fixture, restClaimID, []historyRow{
		{Revision: 1, ActorEmail: restEmail, ActorIssuer: e2eRESTIssuer, ActorSubject: restSubject, Channel: "rest", Action: "acquired"},
		{Revision: 2, ActorEmail: restEmail, ActorIssuer: e2eRESTIssuer, ActorSubject: restSubject, Channel: "rest", Action: "released"},
	})
	assertRequestResult(t, fixture, "rest_user", e2eRESTIssuer, restSubject, releaseRequestID, "released", restClaimID)
}

func runChatSmoke(t *testing.T, client *http.Client, baseURL string, fixture *support.Fixture, jwks *testJWKS, suffix string) {
	t.Helper()
	chatSubject := "users/chat-user-" + suffix
	chatEmail := "chat.owner@" + e2eTeamDomain
	chatToken := signIDToken(t, jwks.jwtKey, e2eChatIssuer, e2eChatAudience, "chat-service-"+suffix, "chat@system.gserviceaccount.com", true)
	chatGroup := "image-chat-" + suffix
	messageName := e2eChatSpace + "/messages/e2e-" + suffix
	chatUser := map[string]string{"name": chatSubject, "email": chatEmail, "type": "HUMAN"}
	chatEvent := map[string]any{
		"type": "MESSAGE", "user": chatUser,
		"space": map[string]string{"name": e2eChatSpace},
		"message": map[string]any{
			"name": messageName, "text": "/claim take sandbox " + chatGroup,
			"argumentText": "take sandbox " + chatGroup,
			"slashCommand": map[string]int{"commandId": 42}, "sender": chatUser,
		},
	}
	status, body := postJSON(t, client, baseURL, "/v1/chat/events", chatToken, chatEvent)
	requireStatus(t, status, http.StatusOK, "Google Chat command", body)
	var chatReply struct {
		Text string `json:"text"`
	}
	decodeJSON(t, body, &chatReply, "Google Chat command")
	if !strings.HasPrefix(chatReply.Text, "Claim acquired:") || !strings.Contains(chatReply.Text, "sandbox") || !strings.Contains(chatReply.Text, chatGroup) || !strings.Contains(chatReply.Text, chatEmail) {
		t.Fatalf("Google Chat command returned an unexpected rendered response")
	}

	chatClaimID := findClaimID(t, fixture, chatGroup)
	assertClaimRow(t, fixture, chatClaimID, chatEmail, "manual", false, 1)
	assertHistory(t, fixture, chatClaimID, []historyRow{
		{Revision: 1, ActorEmail: chatEmail, ActorIssuer: e2eChatIssuer, ActorSubject: chatSubject, Channel: "google_chat", Action: "acquired"},
	})
	requestID := chatRequestID(t, e2eChatIdentity, e2eChatSpace, messageName)
	assertRequestResult(t, fixture, "google_chat_user", e2eChatIssuer, chatSubject, requestID, "acquired", chatClaimID)
	if requests := jwks.requests.Load(); requests < 2 {
		t.Fatalf("signed REST and Chat identities did not both use the HTTPS JWKS fixture")
	}
}

type acquireResponse struct {
	Acquired bool          `json:"acquired"`
	Claim    *runtimeClaim `json:"claim"`
}

type queryResponse struct {
	Known  bool           `json:"known"`
	Claims []runtimeClaim `json:"claims"`
}

type runtimeClaim struct {
	ID           string       `json:"id"`
	OwnerEmail   string       `json:"ownerEmail"`
	Source       string       `json:"source"`
	ActiveNow    bool         `json:"activeNow"`
	Revision     int          `json:"revision"`
	Environments []string     `json:"environments"`
	Scope        runtimeScope `json:"scope"`
}

type runtimeScope struct {
	Group string  `json:"group"`
	App   *string `json:"app,omitempty"`
}

type historyRow struct {
	Revision     int
	ActorEmail   string
	ActorIssuer  string
	ActorSubject string
	Channel      string
	Action       string
}

type claimRow struct {
	OwnerEmail string
	Source     string
	ReleasedAt sql.NullTime
	Revision   int
}

type testJWKS struct {
	url      string
	caPath   string
	jwtKey   *rsa.PrivateKey
	server   *http.Server
	requests atomic.Int64
}

type imageContainer struct {
	id      string
	stopped bool
}

func startTestJWKS(t *testing.T, tempDir string) *testJWKS {
	t.Helper()

	jwtKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test JWT key")
	}
	jwksDocument, err := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"kid": e2eKeyID,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(jwtKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(jwtKey.E)).Bytes()),
		}},
	})
	if err != nil {
		t.Fatalf("encode test JWKS")
	}

	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test CA key")
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "Claimy e2e test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create test CA certificate")
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatalf("parse test CA certificate")
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test JWKS TLS key")
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano() + 1),
		Subject:      pkix.Name{CommonName: "host.docker.internal"},
		DNSNames:     []string{"host.docker.internal"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, rootCert, &serverKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create test JWKS TLS certificate")
	}
	caPath := filepath.Join(tempDir, "jwks-test-ca.pem")
	if err = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), 0o644); err != nil {
		t.Fatalf("write test CA certificate")
	}

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen for HTTPS JWKS fixture")
	}
	certificate := tls.Certificate{Certificate: [][]byte{serverDER, rootDER}, PrivateKey: serverKey}
	mux := http.NewServeMux()
	fixture := &testJWKS{caPath: caPath, jwtKey: jwtKey}
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}
		fixture.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(jwksDocument); err != nil {
			t.Errorf("write JWKS response: %v", err)
		}
	})
	fixture.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	go func() {
		if err := fixture.server.Serve(tlsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve HTTPS JWKS fixture: %v", err)
		}
	}()
	fixture.url = fmt.Sprintf("https://host.docker.internal:%d/jwks", listener.Addr().(*net.TCPAddr).Port)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fixture.server.Shutdown(ctx); err != nil {
			t.Errorf("shut down HTTPS JWKS fixture: %v", err)
		}
	})

	return fixture
}

func writeImageConfig(t *testing.T, tempDir string, fixture *support.Fixture, jwksURL string) string {
	t.Helper()
	getString := func(key string) string {
		value, err := fixture.Config.GetString(key)
		if err != nil {
			t.Fatalf("read fixture database setting %s", key)
		}

		return value
	}
	port, err := fixture.Config.GetInt("sqlc.default.uri.port")
	if err != nil {
		t.Fatalf("read fixture database port")
	}
	password := getString("sqlc.default.uri.password")
	content := fmt.Sprintf(`app:
  env: test
  project: claimy
  family: claimy
  name: claimy

dx:
  use_random_port: false

tracing:
  provider: noop

sampling:
  enabled: false

metric:
  enabled: false

httpserver:
  default:
    port: "8088"

resource_lifecycles:
  create:
    enabled: false

sqlc:
  default:
    driver: mysql
    uri:
      host: "host.docker.internal"
      port: %d
      user: %s
      password: %s
      database: %s
    parameters:
      loc: UTC
      time_zone: %s
    migrations:
      enabled: false
      path: build/migrations/claimy

claimy:
  mysql_version: "8.0.42"
  auth:
    team_domain: %s
    rest:
      issuer: %s
      audience: %s
      jwks_url: %s
    gitlab:
      issuer: %s
      audience: %s
      jwks_url: %s
    chat:
      issuer: %s
      audience: %s
      jwks_url: %s
  chat:
    app_identity: %s
    allowed_spaces:
      - %s
    deadline: 25s
`, port,
		yamlString(getString("sqlc.default.uri.user")),
		yamlString(password),
		yamlString(getString("sqlc.default.uri.database")),
		yamlString("'+00:00'"),
		yamlString(e2eTeamDomain),
		yamlString(e2eRESTIssuer),
		yamlString(e2eRESTAudience),
		yamlString(jwksURL),
		yamlString(e2eGitLabIssuer),
		yamlString("https://gitlab.claimy.e2e.test"),
		yamlString(jwksURL),
		yamlString(e2eChatIssuer),
		yamlString(e2eChatAudience),
		yamlString(jwksURL),
		yamlString(e2eChatIdentity),
		yamlString(e2eChatSpace),
	)
	path := filepath.Join(tempDir, "config.dist.yml")
	if err = os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write isolated image configuration")
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatalf("make isolated image configuration readable by the image user")
	}

	return path
}

func startImage(t *testing.T, image, configPath, caPath, suffix string) *imageContainer {
	t.Helper()
	name := "claimy-e2e-" + suffix
	args := []string{
		"run", "--detach", "--name", name,
		"--publish", "127.0.0.1::8088/tcp",
		"--mount", "type=bind,source=" + configPath + ",target=/app/config.dist.yml,readonly",
		"--mount", "type=bind,source=" + caPath + ",target=/app/jwks-test-ca.pem,readonly",
		"--env", "SSL_CERT_FILE=/app/jwks-test-ca.pem",
	}
	if runtime.GOOS == "linux" {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	args = append(args, image)
	id := strings.TrimSpace(dockerOutput(t, args...))
	if id == "" {
		t.Fatalf("Docker did not return the started Claimy container ID")
	}

	return &imageContainer{id: id}
}

func publishedPort(t *testing.T, containerID string) int {
	t.Helper()
	output := strings.TrimSpace(dockerOutput(t, "port", containerID, "8088/tcp"))
	lines := strings.Split(output, "\n")
	_, rawPort, err := net.SplitHostPort(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		t.Fatalf("Docker returned an invalid published HTTP port")
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("Docker returned an invalid published HTTP port")
	}

	return port
}

func waitForHealth(t *testing.T, client *http.Client, baseURL, containerID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/health")
		if err == nil {
			if err := response.Body.Close(); err != nil {
				t.Errorf("close health response: %v", err)
			}
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		running := strings.TrimSpace(dockerOutput(t, "inspect", "--format", "{{.State.Running}}", containerID))
		if running != "true" {
			t.Fatalf("Claimy image exited before GET /health became ready")
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Claimy image did not return healthy from GET /health before the startup deadline")
}

func postJSON(t *testing.T, client *http.Client, baseURL, path, token string, value any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode request for %s", path)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create request for %s", path)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request to %s failed", path)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close %s response: %v", path, err)
		}
	}()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read response from %s", path)
	}

	return response.StatusCode, responseBody
}

func requireStatus(t *testing.T, got, want int, operation string, responseBody []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("%s returned HTTP %d, want %d; response body: %s", operation, got, want, redactDiagnosticText(responseBody))
	}
}

var diagnosticSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[^\s"'\\]+`),
	regexp.MustCompile(`(?i)(["']?[[:alnum:]_.-]*(?:password|token|secret|authorization|private[_-]?key|dsn|uri)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,}]+)`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
	regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^:/\s]+:)[^@/\s]+@`),
}

func TestRedactDiagnosticText(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{name: "bearer", input: "Authorization: Bearer e30.eyJpayload.sig", secret: "e30.eyJpayload.sig"},
		{name: "json credential", input: `{"database_password":"db-secret"}`, secret: "db-secret"},
		{name: "dsn credential", input: "dsn=user:db-secret@tcp(db:3306)/claimy", secret: "db-secret"},
		{name: "jwt", input: "token eyJheader.eyJpayload.eyJsignature", secret: "eyJheader.eyJpayload.eyJsignature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDiagnosticText([]byte(tc.input))
			if strings.Contains(got, tc.secret) {
				t.Fatalf("diagnostic output leaked a secret: %s", got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("diagnostic output omitted the redaction marker: %s", got)
			}
		})
	}
}

func logDependencyFailureDiagnostics(t *testing.T, fixture *support.Fixture, jwks *testJWKS) {
	t.Helper()
	t.Logf("HTTPS JWKS requests received before failure: %d", jwks.requests.Load())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := fixture.SQLDB.PingContext(ctx); err != nil {
		t.Logf("disposable MySQL connectivity probe failed: %s", redactDiagnosticText([]byte(err.Error())))
	} else {
		t.Log("disposable MySQL connectivity probe succeeded")
	}
}

func redactDiagnosticText(value []byte) string {
	text := string(value)
	for _, pattern := range diagnosticSecrets {
		text = pattern.ReplaceAllString(text, "${1}[REDACTED]")
	}
	if len(text) > 2048 {
		text = text[:2048] + "…[truncated]"
	}

	return fmt.Sprintf("%q", text)
}

func logContainerFailureDiagnostics(t *testing.T, container *imageContainer) {
	t.Helper()
	state, err := dockerCommand("inspect", "--format",
		"status={{.State.Status}} exitCode={{.State.ExitCode}} error={{.State.Error}} oomKilled={{.State.OOMKilled}} startedAt={{.State.StartedAt}} finishedAt={{.State.FinishedAt}}",
		container.id,
	)
	if err != nil {
		t.Log("Claimy container exit diagnostics unavailable")
	} else {
		t.Logf("Claimy container exit diagnostics: %s", strings.TrimSpace(string(state)))
	}

	logs, err := dockerCommand("logs", "--tail", "200", container.id)
	if err != nil {
		t.Log("Claimy application error logs unavailable")

		return
	}
	logged := 0
	for _, line := range strings.Split(string(logs), "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "error") && !strings.Contains(lower, "fatal") &&
			!strings.Contains(lower, "panic") && !strings.Contains(lower, "failed") {
			continue
		}
		t.Logf("Claimy application error: %s", redactDiagnosticText([]byte(line)))
		logged++
		if logged == 50 {
			t.Log("Claimy application error logs truncated after 50 matching lines")

			return
		}
	}
	if logged == 0 {
		t.Log("Claimy container emitted no allowlisted application error log lines")
	}
}

func decodeJSON(t *testing.T, body []byte, target any, operation string) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("%s returned an invalid JSON response", operation)
	}
}

func assertClaimRow(t *testing.T, fixture *support.Fixture, id, owner, source string, released bool, revision int) {
	t.Helper()
	var row claimRow
	err := fixture.SQLDB.QueryRowContext(fixture.Context,
		"SELECT owner_email, source, released_at, revision FROM claims WHERE id = ?", id,
	).Scan(&row.OwnerEmail, &row.Source, &row.ReleasedAt, &row.Revision)
	if err != nil {
		t.Fatalf("read persisted claim row")
	}
	if row.OwnerEmail != owner || row.Source != source || row.ReleasedAt.Valid != released || row.Revision != revision {
		t.Fatalf("persisted claim row has unexpected owner, release state, or revision")
	}
}

func assertHistory(t *testing.T, fixture *support.Fixture, claimID string, want []historyRow) {
	t.Helper()
	rows, err := fixture.SQLDB.QueryContext(fixture.Context,
		"SELECT revision, actor_email, actor_issuer, actor_subject, channel, action FROM claim_versions WHERE claim_id = ? ORDER BY revision",
		claimID,
	)
	if err != nil {
		t.Fatalf("read persisted claim history")
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close persisted claim history rows: %v", err)
		}
	}()
	var got []historyRow
	for rows.Next() {
		var row historyRow
		if err = rows.Scan(&row.Revision, &row.ActorEmail, &row.ActorIssuer, &row.ActorSubject, &row.Channel, &row.Action); err != nil {
			t.Fatalf("scan persisted claim history")
		}
		got = append(got, row)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("iterate persisted claim history")
	}
	if len(got) != len(want) {
		t.Fatalf("persisted claim history has %d revisions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("persisted claim history revision %d has unexpected actor or action", i+1)
		}
	}
}

func assertRequestResult(t *testing.T, fixture *support.Fixture, principalKind, issuer, principalID, requestID, outcome, claimID string) {
	t.Helper()
	var gotOutcome string
	var gotClaimID sql.NullString
	err := fixture.SQLDB.QueryRowContext(fixture.Context,
		"SELECT outcome, claim_id FROM request_results WHERE principal_kind = ? AND principal_issuer = ? AND principal_id = ? AND request_id = ?",
		principalKind, issuer, principalID, requestID,
	).Scan(&gotOutcome, &gotClaimID)
	if err != nil {
		t.Fatalf("read persisted mutation result")
	}
	if gotOutcome != outcome || !gotClaimID.Valid || gotClaimID.String != claimID {
		t.Fatalf("persisted mutation result has an unexpected outcome or claim")
	}
}

func findClaimID(t *testing.T, fixture *support.Fixture, group string) string {
	t.Helper()
	var id string
	err := fixture.SQLDB.QueryRowContext(fixture.Context,
		"SELECT c.id FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?",
		group,
	).Scan(&id)
	if err != nil {
		t.Fatalf("find persisted Chat claim")
	}

	return id
}

func chatRequestID(t *testing.T, appIdentity, spaceName, messageName string) string {
	t.Helper()
	key, err := json.Marshal([3]string{appIdentity, spaceName, messageName})
	if err != nil {
		t.Fatalf("encode Chat idempotency key")
	}

	digest := sha256.Sum256(key)

	return hex.EncodeToString(digest[:])
}

func assertGracefulShutdown(t *testing.T, container *imageContainer) {
	t.Helper()
	if _, err := dockerCommand("stop", "-t", "10", container.id); err != nil {
		t.Fatalf("Docker could not stop the Claimy image gracefully")
	}
	container.stopped = true
	code := strings.TrimSpace(dockerOutput(t, "inspect", "--format", "{{.State.ExitCode}}", container.id))
	exitCode, err := strconv.Atoi(code)
	if err != nil || exitCode != 0 {
		t.Fatalf("Claimy image did not exit cleanly after SIGTERM")
	}
}

func (c *imageContainer) cleanup(t *testing.T) {
	t.Helper()
	if c.id == "" {
		return
	}
	if !c.stopped {
		if _, err := dockerCommand("stop", "-t", "10", c.id); err != nil {
			t.Errorf("Docker could not stop the Claimy image during cleanup: %v", err)
		}
	}
	if _, err := dockerCommand("rm", "-f", c.id); err != nil {
		t.Errorf("Docker could not remove the Claimy image during cleanup: %v", err)
	}
}

func dockerOutput(t *testing.T, args ...string) string {
	t.Helper()
	output, err := dockerCommand(args...)
	if err != nil {
		t.Fatalf("Docker %s command failed", args[0])
	}

	return string(output)
}

func dockerCommand(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...)
	var output strings.Builder
	command.Stdout = &output
	command.Stderr = io.Discard
	err := command.Run()

	return []byte(output.String()), err
}

func signIDToken(t *testing.T, key *rsa.PrivateKey, issuer, audience, subject, email string, emailVerified bool) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": e2eKeyID, "typ": "JWT"})
	if err != nil {
		t.Fatalf("encode JWT header")
	}
	claims := map[string]any{
		"iss":   issuer,
		"aud":   audience,
		"sub":   subject,
		"email": email,
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(5 * time.Minute).Unix(),
	}
	if emailVerified {
		claims["email_verified"] = true
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encode JWT claims")
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign test JWT")
	}

	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate isolated test identity")
	}

	return hex.EncodeToString(raw[:])
}

func yamlString(value string) string {
	return strconv.Quote(value)
}
