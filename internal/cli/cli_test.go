package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testClaimID   = "123e4567-e89b-12d3-a456-426614174000"
	activeClaim   = `{"acquired":true,"claim":{"id":"123e4567-e89b-12d3-a456-426614174000","scope":{"group":"backend"},"environments":["sandbox","prod"],"ownerEmail":"owner@example.com","source":"manual","createdAt":"2026-10-06T00:00:00Z","expiresAt":"2026-10-07T00:00:00Z","revision":1,"activeNow":true,"inherited":false},"conflicts":[]}`
	inactiveClaim = `{"acquired":true,"claim":{"id":"123e4567-e89b-12d3-a456-426614174000","scope":{"group":"backend"},"environments":["sandbox","prod"],"ownerEmail":"owner@example.com","source":"manual","createdAt":"2026-10-06T00:00:00Z","expiresAt":"2026-10-07T00:00:00Z","revision":1,"activeNow":false,"inherited":false}}`
	busyClaim     = `{"acquired":false,"conflicts":[{"id":"123e4567-e89b-12d3-a456-426614174000","scope":{"group":"backend"},"environments":["sandbox"],"ownerEmail":"owner@example.com","source":"manual","expiresAt":"2026-10-07T00:00:00Z"}]}`
)

func TestRunAcquireIDOutputRequiresActiveAcquisition(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		wantStatus int
		wantOutput string
	}{
		{name: "active claim", response: activeClaim, wantStatus: 0, wantOutput: testClaimID + "\n"},
		{name: "inactive replay", response: inactiveClaim, wantStatus: 1},
		{name: "busy claim", response: busyClaim, wantStatus: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertAcquireIDOutput(t, test.response, test.wantStatus, test.wantOutput)
		})
	}
}

func assertAcquireIDOutput(t *testing.T, responseBody string, wantStatus int, wantOutput string) {
	t.Helper()
	server := newAcquireIDOutputServer(t, responseBody)
	defer server.Close()

	t.Setenv(claimyTokenKey, "environment-token-sentinel")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token-sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	status := Run(context.Background(), []string{
		"--url", server.URL,
		"--token-file", tokenFile,
		"acquire",
		"--group", "backend",
		"--environments", "both",
		"--request-id", "cli-acquire-test",
		"--output", "id",
	}, &stdout, &stderr)
	if status != wantStatus {
		t.Fatalf("Run() status = %d, want %d; stderr: %s", status, wantStatus, stderr.String())
	}
	if stdout.String() != wantOutput {
		t.Fatalf("Run() stdout = %q, want %q", stdout.String(), wantOutput)
	}
}

func newAcquireIDOutputServer(t *testing.T, responseBody string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/claims/acquire" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer file-token-sentinel" {
			t.Errorf("token file did not override the environment token")
		}
		response.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(response, responseBody); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
}

func TestRunExplicitEmptyTokenFileDoesNotFallback(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(claimyTokenKey, "environment-token-sentinel")

	var stdout, stderr strings.Builder
	status := Run(context.Background(), []string{
		"--url", server.URL,
		"--token-file", "",
		"query",
		"--group", "backend",
	}, &stdout, &stderr)
	if status != 2 {
		t.Fatalf("Run() status = %d, want 2; stderr: %s", status, stderr.String())
	}
	if requests.Load() != 0 {
		t.Fatal("an explicitly empty token-file flag fell back to the environment")
	}
	if strings.Contains(stderr.String(), "environment-token-sentinel") {
		t.Fatalf("token-file error leaked the environment token: %s", stderr.String())
	}
}

func TestRunBusyAcquirePrintsJSONByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(response, busyClaim); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()
	t.Setenv(claimyTokenKey, "test-token")

	var stdout, stderr strings.Builder
	status := Run(context.Background(), []string{
		"--url", server.URL,
		"acquire",
		"--group", "backend",
		"--environments", "sandbox",
		"--request-id", "cli-busy-test",
	}, &stdout, &stderr)
	if status != 1 {
		t.Fatalf("Run() status = %d, want 1; stderr: %s", status, stderr.String())
	}

	var response struct {
		Acquired  bool            `json:"acquired"`
		Conflicts json.RawMessage `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &response); err != nil {
		t.Fatalf("busy acquire did not produce JSON: %v", err)
	}
	if response.Acquired || len(response.Conflicts) == 0 || string(response.Conflicts) == "null" {
		t.Fatalf("busy acquire output lost the conflict outcome: %s", stdout.String())
	}
}

func TestRunQueryDefaultsToBothEnvironments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/claims/query" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		var body struct {
			Group        string   `json:"group"`
			Environments []string `json:"environments"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode query request: %v", err)
		}
		if body.Group != "backend" || len(body.Environments) != 2 || body.Environments[0] != "sandbox" || body.Environments[1] != "prod" {
			t.Errorf("query request did not select both environments: %+v", body)
		}

		response.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(response, `{"known":true,"at":"2026-10-06T00:00:00Z","projected":false,"free":true,"allowedForCaller":true,"claims":[]}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()
	t.Setenv(claimyTokenKey, "test-token")

	var stdout, stderr strings.Builder
	status := Run(context.Background(), []string{
		"--url", server.URL,
		"query",
		"--group", "backend",
	}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("Run() status = %d, want 0; stderr: %s", status, stderr.String())
	}

	var response struct {
		Known bool `json:"known"`
		Free  bool `json:"free"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &response); err != nil {
		t.Fatalf("query did not produce JSON: %v", err)
	}
	if !response.Known || !response.Free {
		t.Fatalf("query output changed the response state: %+v", response)
	}
}

func TestRunExpiryAcceptsMaximumRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPatch || request.URL.Path != "/v1/claims/"+testClaimID {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}

		var body struct {
			ExpectedRevision uint64 `json:"expectedRevision"`
			RequestID        string `json:"requestId"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode expiry request: %v", err)
		}
		if body.ExpectedRevision != 4294967295 || body.RequestID != "cli-expiry-test" {
			t.Errorf("expiry request lost its revision or idempotency key: %+v", body)
		}

		response.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprint(response, `{"changed":true,"claim":{"id":"123e4567-e89b-12d3-a456-426614174000","scope":{"group":"backend"},"environments":["sandbox"],"ownerEmail":"owner@example.com","source":"manual","createdAt":"2026-10-06T00:00:00Z","expiresAt":"2026-10-08T00:00:00Z","revision":4294967295,"activeNow":true,"inherited":false}}`)
		if err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()
	t.Setenv(claimyTokenKey, "test-token")

	var stdout, stderr strings.Builder
	status := Run(context.Background(), []string{
		"--url", server.URL,
		"expiry",
		"--id", testClaimID,
		"--expires-at", "2026-10-08T00:00:00Z",
		"--revision", "4294967295",
		"--request-id", "cli-expiry-test",
	}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("Run() status = %d, want 0; stderr: %s", status, stderr.String())
	}

	var result struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil {
		t.Fatalf("expiry did not produce JSON: %v", err)
	}
	if !result.Changed {
		t.Fatalf("expiry output did not report a changed claim: %s", stdout.String())
	}
}

func TestRunInputBoundariesFailBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "malformed mutation UUID",
			args: []string{"release", "--id", "not-a-uuid", "--request-id", "cli-test"},
		},
		{
			name: "revision above uint32",
			args: []string{"expiry", "--id", testClaimID, "--expires-at", "2026-10-07T00:00:00Z", "--revision", "4294967296", "--request-id", "cli-test"},
		},
		{
			name: "revision zero",
			args: []string{"expiry", "--id", testClaimID, "--expires-at", "2026-10-07T00:00:00Z", "--revision", "0", "--request-id", "cli-test"},
		},
		{
			name: "expiry without offset",
			args: []string{"expiry", "--id", testClaimID, "--expires-at", "2026-10-07T00:00:00", "--revision", "1", "--request-id", "cli-test"},
		},
		{
			name: "page limit above maximum",
			args: []string{"catalog", "groups", "--limit", "101"},
		},
		{
			name: "negative page offset",
			args: []string{"catalog", "apps", "--group", "backend", "--offset", "-1"},
		},
		{
			name: "page offset above int32",
			args: []string{"catalog", "groups", "--offset", "2147483648"},
		},
		{
			name: "query time without offset",
			args: []string{"query", "--group", "backend", "--at", "2026-10-07T00:00:00"},
		},
		{
			name: "empty explicit optional app",
			args: []string{"query", "--group", "backend", "--app", ""},
		},
		{
			name: "nonpositive timeout",
			args: []string{"--timeout", "0", "query", "--group", "backend"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(claimyTokenKey, "")
			args := append([]string{"--url", server.URL}, test.args...)
			var stdout, stderr strings.Builder
			if status := Run(context.Background(), args, &stdout, &stderr); status != 2 {
				t.Fatalf("Run() status = %d, want 2; stderr: %s", status, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("invalid input wrote to stdout: %q", stdout.String())
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid input reached the API %d times", got)
	}
}

func TestRunHelpDoesNotRequireAuthenticationOrNetwork(t *testing.T) {
	t.Setenv(claimyTokenKey, "help-token-sentinel")
	for _, args := range [][]string{
		{"--help"},
		{"acquire", "--help"},
		{"catalog", "--help"},
		{"catalog", "group", "--help"},
	} {
		var stdout, stderr strings.Builder
		if status := Run(context.Background(), args, &stdout, &stderr); status != 0 {
			t.Fatalf("Run(%q) status = %d, want 0; stderr: %s", args, status, stderr.String())
		}
		if stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("Run(%q) did not render help to stderr", args)
		}
		if strings.Contains(stderr.String(), "help-token-sentinel") {
			t.Fatalf("help output leaked the token: %s", stderr.String())
		}
	}
}

func TestRunRedactsBearerTokenFromAPIError(t *testing.T) {
	const token = "secret-token-sentinel"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusUnauthorized)
		if _, err := fmt.Fprintf(response, `{"error":{"code":"unauthenticated","message":%q,"status":401}}`, token); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()
	t.Setenv(claimyTokenKey, token)

	var stdout, stderr strings.Builder
	status := Run(context.Background(), []string{
		"--url", server.URL,
		"query",
		"--group", "backend",
	}, &stdout, &stderr)
	if status != 2 {
		t.Fatalf("Run() status = %d, want 2; stderr: %s", status, stderr.String())
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatalf("API error leaked the bearer token: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "HTTP 401") {
		t.Fatalf("API error diagnostic omitted the HTTP status: %s", stderr.String())
	}
}
