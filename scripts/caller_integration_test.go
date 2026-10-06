//go:build integration

package scripts

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const (
	callerToken      = "shell-test-secret"
	callerRequestID  = "project-42-job-7"
	callerClaimID    = "36d1e022-0e0f-4aaf-989f-cc365d0f74a2"
	callerReleaseID  = "project-42-job-7-release"
	callerAcquireURL = "/v1/claims/acquire"
)

type callerResponse struct {
	status int
	body   string
}

func TestAcquireCallerSubprocess(t *testing.T) {
	const acquiredID = "a531f883-d838-4115-aa68-470975d2d92d"

	tests := []struct {
		name       string
		response   callerResponse
		wantExit   int
		wantStdout string
	}{
		{
			name:       "acquired claim",
			response:   callerResponse{http.StatusOK, `{"acquired":true,"claim":{"id":"` + acquiredID + `","activeNow":true},"conflicts":[]}`},
			wantExit:   0,
			wantStdout: acquiredID + "\n",
		},
		{
			name:     "busy claim exits one",
			response: callerResponse{http.StatusOK, `{"acquired":false,"conflicts":[{"id":"busy"}]}`},
			wantExit: 1,
		},
		{
			name:     "inactive acquired claim exits one",
			response: callerResponse{http.StatusOK, `{"acquired":true,"claim":{"id":"` + acquiredID + `","activeNow":false},"conflicts":[]}`},
			wantExit: 1,
		},
		{
			name:     "unauthorized response exits two",
			response: callerResponse{http.StatusUnauthorized, `{"error":{"code":"unauthenticated"}}`},
			wantExit: 2,
		},
		{
			name:     "storage failure exits two",
			response: callerResponse{http.StatusInternalServerError, `{"error":{"code":"storage_error"}}`},
			wantExit: 2,
		},
		{
			name:     "missing active state is an invalid response shape",
			response: callerResponse{http.StatusOK, `{"acquired":true,"claim":{"id":"` + acquiredID + `"},"conflicts":[]}`},
			wantExit: 2,
		},
		{
			name:     "empty claim id is an invalid response shape",
			response: callerResponse{http.StatusOK, `{"acquired":true,"claim":{"id":"","activeNow":true},"conflicts":[]}`},
			wantExit: 2,
		},
		{
			name:     "busy response without conflicts is an invalid response shape",
			response: callerResponse{http.StatusOK, `{"acquired":false,"conflicts":[]}`},
			wantExit: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := startCallerServer(t, http.MethodPost, callerAcquireURL, test.response)
			environment := acquireEnvironment(t, server.URL+"/")
			code, stdout := runCaller(t, "ci-acquire.sh", environment)
			if code != test.wantExit {
				t.Fatalf("exit code = %d, want %d", code, test.wantExit)
			}
			if stdout != test.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout, test.wantStdout)
			}
		})
	}
}

func TestReleaseCallerSubprocess(t *testing.T) {
	tests := []struct {
		name     string
		response callerResponse
		wantExit int
	}{
		{
			name:     "released claim",
			response: callerResponse{http.StatusOK, `{"changed":true,"claim":{"id":"` + callerClaimID + `"}}`},
			wantExit: 0,
		},
		{
			name:     "unauthorized response exits two",
			response: callerResponse{http.StatusUnauthorized, `{"error":{"code":"unauthenticated"}}`},
			wantExit: 2,
		},
		{
			name:     "storage failure exits two",
			response: callerResponse{http.StatusInternalServerError, `{"error":{"code":"storage_error"}}`},
			wantExit: 2,
		},
		{
			name:     "missing changed flag is an invalid response shape",
			response: callerResponse{http.StatusOK, `{"claim":{"id":"` + callerClaimID + `"}}`},
			wantExit: 2,
		},
		{
			name:     "empty claim id is an invalid response shape",
			response: callerResponse{http.StatusOK, `{"changed":true,"claim":{"id":""}}`},
			wantExit: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := startCallerServer(t, http.MethodPost, "/v1/claims/"+callerClaimID+"/release", test.response)
			code, stdout := runCaller(t, "ci-release.sh", releaseEnvironment(t, server.URL+"/"))
			if code != test.wantExit {
				t.Fatalf("exit code = %d, want %d", code, test.wantExit)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty output", stdout)
			}
		})
	}
}

func TestCallerNetworkFailuresExitTwo(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	tests := []struct {
		name        string
		script      string
		environment func(*testing.T, string) []string
	}{
		{name: "acquire", script: "ci-acquire.sh", environment: acquireEnvironment},
		{name: "release", script: "ci-release.sh", environment: releaseEnvironment},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, stdout := runCaller(t, test.script, test.environment(t, url))
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 for a failed HTTP connection", code)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty output", stdout)
			}
		})
	}
}

func startCallerServer(t *testing.T, method, path string, response callerResponse) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method || r.URL.Path != path {
			http.NotFound(w, r)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.status)
		if _, err := w.Write([]byte(response.body)); err != nil {
			t.Errorf("write caller fixture response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	return server
}

func acquireEnvironment(t *testing.T, url string) []string {
	t.Helper()

	return callerEnvironment(t, map[string]string{
		"CLAIMY_URL":          url,
		"CLAIMY_ID_TOKEN":     callerToken,
		"CLAIMY_GROUP":        "payments",
		"CLAIMY_APP":          "dashboard",
		"CLAIMY_EXPIRES_AT":   "2030-03-04T05:06:07Z",
		"CLAIMY_REQUEST_ID":   callerRequestID,
		"CLAIMY_ENVIRONMENTS": `["sandbox","staging"]`,
	})
}

func releaseEnvironment(t *testing.T, url string) []string {
	t.Helper()

	return callerEnvironment(t, map[string]string{
		"CLAIMY_URL":                url,
		"CLAIMY_ID_TOKEN":           callerToken,
		"CLAIMY_CLAIM_ID":           callerClaimID,
		"CLAIMY_RELEASE_REQUEST_ID": callerReleaseID,
	})
}

func callerEnvironment(t *testing.T, variables map[string]string) []string {
	t.Helper()
	path := os.Getenv("PATH")
	if path == "" {
		t.Fatal("PATH is required to invoke bash, curl, jq, and mktemp")
	}
	tempDir := t.TempDir()
	environment := []string{"PATH=" + path, "HOME=" + tempDir, "TMPDIR=" + tempDir, "LC_ALL=C"}
	for key, value := range variables {
		environment = append(environment, key+"="+value)
	}

	return environment
}

func runCaller(t *testing.T, script string, environment []string) (int, string) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate caller integration test source")
	}
	cmd := exec.Command("bash", filepath.Join(filepath.Dir(source), script))
	cmd.Env = environment
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stdout.String()
	}
	t.Fatalf("could not execute %s: %v (stderr: %s)", script, err, stderr.String())

	return 0, ""
}
