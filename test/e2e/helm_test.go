//go:build integration && fixtures

package e2e

import (
	"bytes"
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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const helmSmokeEnabled = "1"

// TestHelmChartIntegration exercises the packaged deployment path rather than
// just rendering its YAML. It deliberately runs only when CI (or an operator)
// opts in: a test invocation must never create a Kubernetes cluster implicitly.
func TestHelmChartIntegration(t *testing.T) {
	image := requireHelmSmokeImage(t)

	suffix := randomSuffix(t)
	cluster := "claimy-e2e-" + suffix
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	contextName := "kind-" + cluster
	release := "claimy-helm-" + suffix
	migrationJob := release + "-migration"
	password := fmt.Sprintf("claimy-${app.env}-${literal}@:$()/%s", suffix)
	createKindCluster(t, cluster, kubeconfig)
	t.Cleanup(func() {
		// Capture non-secret diagnostics while the private cluster still exists.
		if t.Failed() {
			captureHelmSmokeDiagnostics(t, kubeconfig, contextName, release, migrationJob, password)
		}
		// The kubeconfig is private to this test. Passing it explicitly to kind
		// keeps cleanup from touching a user's current context or kubeconfig.
		result := helmSmokeRunCleanup(t, 2*time.Minute, "kind", "delete", "cluster", "--name", cluster, "--kubeconfig", kubeconfig)
		if result.err != nil {
			t.Errorf("delete disposable kind cluster: %s", helmSmokeDiagnostic(result))
		}
	})
	hostIP := configureKindHostAlias(t, cluster)
	configureKindDNS(t, kubeconfig, contextName, hostIP)

	loadImage(t, cluster, image)
	applyMySQL(t, kubeconfig, contextName, password)

	tempDir := t.TempDir()
	jwks := startTestJWKS(t, tempDir)
	createJWKSSecret(t, kubeconfig, contextName, jwks.caPath)

	repository, imageTag, digest := helmSmokeImageRef(image)
	chart := filepath.Join(repositoryRoot(t), "build", "helm", "claimy")
	values := filepath.Join(tempDir, "values.yaml")
	valuesContent := helmSmokeValues(repository, imageTag, digest, jwks.url)
	if err := os.WriteFile(values, []byte(valuesContent), 0o600); err != nil {
		t.Fatalf("write Helm smoke values: %v", err)
	}

	// MySQL has a deliberately slow init container. This gives the pre-install
	// hook a visible running window in which the ConfigMap must not exist.
	installDone := make(chan helmSmokeResult, 1)
	installFinished := make(chan struct{})
	go func() {
		installDone <- helmSmokeRun(t, 6*time.Minute, "helm", "--kubeconfig", kubeconfig,
			"--kube-context", contextName, "install", release, chart,
			"--values", values, "--wait", "--timeout", "5m")
		close(installFinished)
	}()
	if !observePreInstallHookOrdering(t, kubeconfig, contextName, migrationJob, release, installFinished) {
		result := <-installDone
		t.Fatalf("Helm install never exposed a migration hook before the ConfigMap: %s", helmSmokeDiagnostic(result))
	}
	installResult := <-installDone
	if installResult.err != nil {
		t.Fatalf("Helm install failed (%v): %s", installResult.err, helmSmokeDiagnostic(installResult))
	}

	deployment := "deployment/" + release
	waitForKubectlCondition(t, kubeconfig, contextName, "available", deployment, 3*time.Minute)
	port, stopPortForward := startHelmPortForward(t, kubeconfig, contextName, "service/"+release, 8088)
	t.Cleanup(func() {
		if err := stopPortForward(); err != nil {
			t.Errorf("stop Helm service port-forward: %v", err)
		}
	})
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitForHelmHTTPStatus(t, baseURL+"/health", http.StatusOK, 2*time.Minute)
	waitForHelmHTTPStatus(t, baseURL+"/ready", http.StatusOK, 2*time.Minute)

	binary := buildNativeHelmCLI(t, tempDir)
	claimID, identity := runHelmNativeCISmoke(t, binary, baseURL, jwks.jwtKey, suffix)
	assertHelmCIPersistedIdentity(t, kubeconfig, contextName, password, claimID, identity)

	// Pause the owned MySQL server in place so the database files and the CI
	// claim survive the dependency outage. Liveness remains public and healthy
	// while the bounded DB ping makes readiness transition to 503.
	mysqlPod := mysqlSmokePod(t, kubeconfig, contextName)
	mysqlPID := pauseMySQL(t, kubeconfig, contextName, mysqlPod)
	mysqlPaused := true
	t.Cleanup(func() {
		if mysqlPaused {
			resumeMySQLCleanup(t, kubeconfig, contextName, mysqlPod, mysqlPID)
		}
	})
	waitForHelmHTTPStatus(t, baseURL+"/health", http.StatusOK, 30*time.Second)
	waitForHelmHTTPStatus(t, baseURL+"/ready", http.StatusServiceUnavailable, 90*time.Second)
	resumeMySQL(t, kubeconfig, contextName, mysqlPod, mysqlPID)
	mysqlPaused = false
	waitForHelmHTTPStatus(t, baseURL+"/ready", http.StatusOK, 2*time.Minute)

	if err := stopPortForward(); err != nil {
		t.Fatalf("stop pre-upgrade Helm service port-forward: %v", err)
	}

	upgradeValues := filepath.Join(tempDir, "upgrade-values.yaml")
	if err := os.WriteFile(upgradeValues, []byte(strings.Replace(valuesContent, "deadline: 25s", "deadline: 26s", 1)), 0o600); err != nil {
		t.Fatalf("write Helm upgrade values: %v", err)
	}
	upgrade := helmSmokeRun(t, 6*time.Minute, "helm", "--kubeconfig", kubeconfig,
		"--kube-context", contextName, "upgrade", release, chart,
		"--values", upgradeValues, "--wait", "--timeout", "5m")
	if upgrade.err != nil {
		t.Fatalf("Helm upgrade failed: %s", helmSmokeDiagnostic(upgrade))
	}
	waitForKubectlCondition(t, kubeconfig, contextName, "available", deployment, 3*time.Minute)
	upgradedPort, stopUpgradedPortForward := startHelmPortForward(t, kubeconfig, contextName, "service/"+release, 8088)
	t.Cleanup(func() {
		if err := stopUpgradedPortForward(); err != nil {
			t.Errorf("stop upgraded Helm service port-forward: %v", err)
		}
	})
	baseURL = "http://127.0.0.1:" + strconv.Itoa(upgradedPort)
	waitForHelmHTTPStatus(t, baseURL+"/health", http.StatusOK, 30*time.Second)
	waitForHelmHTTPStatus(t, baseURL+"/ready", http.StatusOK, 2*time.Minute)
	assertHelmCIPersistedIdentity(t, kubeconfig, contextName, password, claimID, identity)
}

func requireHelmSmokeImage(t *testing.T) string {
	t.Helper()
	if os.Getenv("CLAIMY_HELM_SMOKE") != helmSmokeEnabled {
		t.Skip("set CLAIMY_HELM_SMOKE=1 to run the disposable Helm integration test")
	}
	image := os.Getenv("CLAIMY_IMAGE")
	if image == "" {
		t.Fatalf("CLAIMY_IMAGE is required when CLAIMY_HELM_SMOKE=1")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Fatalf("the Helm integration test requires a Docker-backed local Kubernetes runtime, not %s", runtime.GOOS)
	}
	for _, tool := range []string{"docker", "helm", "kind", "kubectl", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required for the Helm integration test: %v", tool, err)
		}
	}
	if result := helmSmokeRun(t, 2*time.Minute, "docker", "info"); result.err != nil {
		t.Fatalf("Docker is not available for the Helm integration test: %s", helmSmokeDiagnostic(result))
	}

	return image
}

type helmSmokeResult struct {
	stdout []byte
	stderr []byte
	err    error
}

func helmSmokeRun(t *testing.T, timeout time.Duration, command string, args ...string) helmSmokeResult {
	return helmSmokeRunWithContext(t.Context(), t, timeout, command, args...)
}

func helmSmokeRunCleanup(t *testing.T, timeout time.Duration, command string, args ...string) helmSmokeResult {
	return helmSmokeRunWithContext(context.Background(), t, timeout, command, args...)
}

func helmSmokeRunWithContext(parent context.Context, t *testing.T, timeout time.Duration, command string, args ...string) helmSmokeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return helmSmokeResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), err: err}
}

func helmSmokeDiagnostic(result helmSmokeResult) string {
	return helmSmokeDiagnosticWithSecrets(result)
}

func helmSmokeDiagnosticWithSecrets(result helmSmokeResult, secrets ...string) string {
	output := make([]byte, 0, len(result.stdout)+len(result.stderr)+1)
	output = append(output, result.stdout...)
	if len(result.stdout) > 0 && len(result.stderr) > 0 {
		output = append(output, '\n')
	}
	output = append(output, result.stderr...)
	message := redactDiagnosticText(output)
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "<redacted>")
		}
	}
	message = strings.TrimSpace(message)
	if len(message) > 4000 {
		message = message[:4000] + "..."
	}

	return message
}

func createKindCluster(t *testing.T, cluster, kubeconfig string) {
	t.Helper()
	result := helmSmokeRun(t, 3*time.Minute, "kind", "create", "cluster", "--name", cluster,
		"--kubeconfig", kubeconfig, "--wait", "120s")
	if result.err != nil {
		// kind can leave a partially-created node after a timeout; use only
		// this test's private kubeconfig while attempting best-effort cleanup.
		_ = helmSmokeRun(t, time.Minute, "kind", "delete", "cluster", "--name", cluster, "--kubeconfig", kubeconfig)
		t.Fatalf("create private kind cluster: %s", helmSmokeDiagnostic(result))
	}
}

func configureKindHostAlias(t *testing.T, cluster string) string {
	t.Helper()
	node := cluster + "-control-plane"
	gateway := helmSmokeRun(t, time.Minute, "docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}", node)
	if gateway.err != nil {
		t.Fatalf("inspect private kind node network: %s", helmSmokeDiagnostic(gateway))
	}
	hostGateway := strings.TrimSpace(string(gateway.stdout))
	if hostGateway == "" {
		t.Fatalf("private kind node has no Docker network gateway")
	}
	existing := helmSmokeRun(t, time.Minute, "docker", "exec", node, "sh", "-c",
		`awk '$2 == "host.docker.internal" {print $1; exit}' /etc/hosts`)
	if existing.err != nil {
		t.Fatalf("inspect private kind host alias: %s", helmSmokeDiagnostic(existing))
	}
	hostIP := strings.TrimSpace(string(existing.stdout))
	if hostIP == "" {
		resolved := helmSmokeRun(t, time.Minute, "docker", "exec", node, "getent", "hosts", "host.docker.internal")
		if resolved.err == nil {
			for _, field := range strings.Fields(string(resolved.stdout)) {
				if parsed := net.ParseIP(field); parsed != nil && parsed.To4() != nil {
					hostIP = parsed.String()

					break
				}
			}
		}
	}
	if hostIP == "" {
		result := helmSmokeRun(t, time.Minute, "docker", "exec", node, "sh", "-c",
			"printf '%s host.docker.internal\\n' \"$1\" >> /etc/hosts", "helm-smoke", hostGateway)
		if result.err != nil {
			t.Fatalf("configure private kind host alias: %s", helmSmokeDiagnostic(result))
		}
		hostIP = hostGateway
	}

	return hostIP
}

func configureKindDNS(t *testing.T, kubeconfig, contextName, hostIP string) {
	t.Helper()
	corefile := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "get", "configmap", "coredns",
		"-n", "kube-system", "-o", "jsonpath={.data.Corefile}")
	if corefile.err != nil {
		t.Fatalf("read private kind CoreDNS configuration: %s", helmSmokeDiagnostic(corefile))
	}
	current := string(corefile.stdout)
	if strings.Contains(current, "host.docker.internal") {
		return
	}
	lines := strings.Split(current, "\n")
	inserted := false
	for index, line := range lines {
		if strings.TrimSpace(line) != "ready" {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		hosts := []string{
			indent + "hosts {",
			indent + "    " + hostIP + " host.docker.internal",
			indent + "    fallthrough",
			indent + "}",
		}
		updatedLines := append([]string{}, lines[:index+1]...)
		updatedLines = append(updatedLines, hosts...)
		updatedLines = append(updatedLines, lines[index+1:]...)
		lines = updatedLines
		inserted = true

		break
	}
	if !inserted {
		t.Fatalf("private kind CoreDNS configuration has no ready plugin marker")
	}
	updated := strings.Join(lines, "\n")
	patch, err := json.Marshal(map[string]map[string]string{"data": {"Corefile": updated}})
	if err != nil {
		t.Fatalf("encode private kind CoreDNS patch: %v", err)
	}
	result := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "patch", "configmap/coredns",
		"-n", "kube-system", "--type", "merge", "-p", string(patch))
	if result.err != nil {
		t.Fatalf("configure private kind CoreDNS host mapping: %s", helmSmokeDiagnostic(result))
	}
	restart := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "rollout", "restart",
		"deployment/coredns", "-n", "kube-system")
	if restart.err != nil {
		t.Fatalf("restart private kind CoreDNS: %s", helmSmokeDiagnostic(restart))
	}
	status := kubectlSmoke(t, kubeconfig, contextName, 3*time.Minute, nil, "rollout", "status",
		"deployment/coredns", "-n", "kube-system", "--timeout=2m")
	if status.err != nil {
		t.Fatalf("wait for private kind CoreDNS: %s", helmSmokeDiagnostic(status))
	}
}

func loadImage(t *testing.T, cluster, image string) {
	t.Helper()
	result := helmSmokeRun(t, 3*time.Minute, "kind", "load", "docker-image", image, "--name", cluster)
	if result.err != nil {
		t.Fatalf("load Claimy image into private kind cluster: %s", helmSmokeDiagnostic(result))
	}
}

func kubectlSmoke(t *testing.T, kubeconfig, contextName string, timeout time.Duration, input []byte, args ...string) helmSmokeResult {
	return kubectlSmokeWithContext(t.Context(), t, kubeconfig, contextName, timeout, input, args...)
}

func kubectlSmokeCleanup(t *testing.T, kubeconfig, contextName string, timeout time.Duration, input []byte, args ...string) helmSmokeResult {
	return kubectlSmokeWithContext(context.Background(), t, kubeconfig, contextName, timeout, input, args...)
}

func kubectlSmokeWithContext(parent context.Context, t *testing.T, kubeconfig, contextName string, timeout time.Duration, input []byte, args ...string) helmSmokeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	commandArgs := append([]string{"--kubeconfig", kubeconfig, "--context", contextName}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", commandArgs...)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return helmSmokeResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), err: err}
}

func captureHelmSmokeDiagnostics(t *testing.T, kubeconfig, contextName, release, migrationJob, password string) {
	t.Helper()
	diagnostics := []struct {
		name string
		args []string
	}{
		{
			name: "all pods",
			args: []string{"get", "pods", "-A", "-o", "wide"},
		},
		{
			name: "Claimy resources",
			args: []string{
				"get", "jobs,deployments,pods", "-n", "default",
				"-l", "app.kubernetes.io/instance=" + release, "-o", "wide",
			},
		},
		{
			name: "migration job description",
			args: []string{"describe", "job/" + migrationJob, "-n", "default"},
		},
		{
			name: "Claimy deployment description",
			args: []string{"describe", "deployment/" + release, "-n", "default"},
		},
		{
			name: "migration logs",
			args: []string{
				"logs", "job/" + migrationJob, "-n", "default",
				"--all-containers=true", "--tail=200",
			},
		},
		{
			name: "Claimy logs",
			args: []string{
				"logs", "deployment/" + release, "-n", "default",
				"--all-containers=true", "--tail=200",
			},
		},
		{
			name: "cluster events",
			args: []string{"get", "events", "-A", "--sort-by=.lastTimestamp"},
		},
	}
	for _, diagnostic := range diagnostics {
		result := kubectlSmokeCleanup(t, kubeconfig, contextName, 30*time.Second, nil, diagnostic.args...)
		output := helmSmokeDiagnosticWithSecrets(result, password)
		if output == "" {
			output = "<no output>"
		}
		t.Logf("Helm smoke diagnostic (%s):\n%s", diagnostic.name, output)
	}
}

func applyMySQL(t *testing.T, kubeconfig, contextName, password string) {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: claimy-db
type: Opaque
stringData:
  password: %s
  root-password: %s
---
apiVersion: v1
kind: Service
metadata:
  name: claimy-mysql
spec:
  type: ClusterIP
  ports:
    - name: mysql
      port: 3306
      targetPort: mysql
  selector:
    app.kubernetes.io/name: claimy-mysql
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: claimy-mysql
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: claimy-mysql
  template:
    metadata:
      labels:
        app.kubernetes.io/name: claimy-mysql
    spec:
      shareProcessNamespace: true
      terminationGracePeriodSeconds: 10
      initContainers:
        - name: delay-start
          image: busybox:1.36
          command: ["sh", "-c", "sleep 15"]
      containers:
        - name: mysql
          image: mysql:8.0.42
          ports:
            - name: mysql
              containerPort: 3306
          env:
            - name: MYSQL_DATABASE
              value: claimy
            - name: MYSQL_USER
              value: claimy
            - name: MYSQL_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: claimy-db
                  key: password
            - name: MYSQL_ROOT_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: claimy-db
                  key: root-password
          readinessProbe:
            exec:
              command: ["sh", "-c", "mysqladmin ping -h 127.0.0.1 -uroot -p\"$MYSQL_ROOT_PASSWORD\""]
            periodSeconds: 2
            timeoutSeconds: 2
            failureThreshold: 30
`, yamlString(password), yamlString(password))
	result := kubectlSmoke(t, kubeconfig, contextName, 2*time.Minute, []byte(manifest), "apply", "-f", "-")
	if result.err != nil {
		t.Fatalf("apply disposable MySQL manifest: %s", helmSmokeDiagnostic(result))
	}
}

func createJWKSSecret(t *testing.T, kubeconfig, contextName, caPath string) {
	t.Helper()
	result := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "create", "secret", "generic", "claimy-jwks-ca",
		"--from-file=ca.pem="+caPath)
	if result.err != nil {
		t.Fatalf("create disposable JWKS CA Secret: %s", helmSmokeDiagnostic(result))
	}
}

func observePreInstallHookOrdering(t *testing.T, kubeconfig, contextName, migrationJob, release string, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		job := kubectlSmoke(t, kubeconfig, contextName, 10*time.Second, nil, "get", "job", migrationJob)
		if job.err == nil {
			configMap := kubectlSmoke(t, kubeconfig, contextName, 10*time.Second, nil, "get", "configmap", release)
			if configMap.err != nil {
				return true
			}
		}
		select {
		case <-done:
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

func waitForKubectlCondition(t *testing.T, kubeconfig, contextName, condition, resource string, timeout time.Duration) {
	t.Helper()
	result := kubectlSmoke(t, kubeconfig, contextName, timeout, nil, "wait", "--for=condition="+condition, resource,
		"--timeout="+timeout.String())
	if result.err != nil {
		t.Fatalf("wait for %s on %s: %s", condition, resource, helmSmokeDiagnostic(result))
	}
}

func mysqlSmokePod(t *testing.T, kubeconfig, contextName string) string {
	t.Helper()
	result := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "get", "pods",
		"-l", "app.kubernetes.io/name=claimy-mysql", "-o", "jsonpath={.items[0].metadata.name}")
	pod := strings.TrimSpace(string(result.stdout))
	if result.err != nil || pod == "" {
		t.Fatalf("find disposable MySQL pod: %s", helmSmokeDiagnostic(result))
	}

	return pod
}

func pauseMySQL(t *testing.T, kubeconfig, contextName, pod string) string {
	t.Helper()
	const findMySQLPID = `for comm in /proc/[0-9]*/comm; do
	pid=${comm%/comm}
	pid=${pid##*/}
	[ "$pid" -gt 1 ] 2>/dev/null || continue
	name=$(cat "$comm" 2>/dev/null) || continue
	[ "$name" = "mysqld" ] || continue
	printf '%s\n' "$pid"
	exit 0
done
exit 1`
	result := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "exec", pod, "--", "sh", "-c", findMySQLPID)
	if result.err != nil {
		t.Fatalf("find MySQL server process: %s", helmSmokeDiagnostic(result))
	}
	pid := strings.TrimSpace(string(result.stdout))
	pidNumber, err := strconv.Atoi(pid)
	if err != nil || pidNumber <= 1 {
		t.Fatalf("discovered invalid MySQL server PID %q", pid)
	}
	stop := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "exec", pod, "--", "sh", "-c",
		"kill -STOP \"$1\"", "--", pid)
	if stop.err != nil {
		t.Fatalf("pause MySQL server PID %s: %s", pid, helmSmokeDiagnostic(stop))
	}
	assertMySQLProcessState(t, kubeconfig, contextName, pod, pid, "T")

	return pid
}

func resumeMySQL(t *testing.T, kubeconfig, contextName, pod, pid string) {
	t.Helper()
	resumeMySQLWithKubectl(t, kubectlSmoke, kubeconfig, contextName, pod, pid, "resume disposable MySQL server")
}

func resumeMySQLCleanup(t *testing.T, kubeconfig, contextName, pod, pid string) {
	t.Helper()
	resumeMySQLWithKubectl(t, kubectlSmokeCleanup, kubeconfig, contextName, pod, pid, "resume disposable MySQL server during cleanup")
}

func resumeMySQLWithKubectl(
	t *testing.T,
	run func(*testing.T, string, string, time.Duration, []byte, ...string) helmSmokeResult,
	kubeconfig, contextName, pod, pid, operation string,
) {
	t.Helper()
	result := run(t, kubeconfig, contextName, time.Minute, nil, "exec", pod, "--", "sh", "-c",
		"kill -CONT \"$1\"", "--", pid)
	if result.err != nil {
		t.Errorf("%s: %s", operation, helmSmokeDiagnostic(result))

		return
	}
	assertMySQLProcessStateWithKubectl(t, run, kubeconfig, contextName, pod, pid, "not-stopped")
}

func assertMySQLProcessState(t *testing.T, kubeconfig, contextName, pod, pid, want string) {
	assertMySQLProcessStateWithKubectl(t, kubectlSmoke, kubeconfig, contextName, pod, pid, want)
}

func assertMySQLProcessStateWithKubectl(
	t *testing.T,
	run func(*testing.T, string, string, time.Duration, []byte, ...string) helmSmokeResult,
	kubeconfig, contextName, pod, pid, want string,
) {
	t.Helper()
	script := `state=$(awk '$1 == "State:" {print $2; exit}' "/proc/$1/status")
if [ "$2" = "not-stopped" ]; then
	[ "$state" != "T" ] && [ "$state" != "t" ]
else
	[ "$state" = "$2" ]
fi`
	result := run(t, kubeconfig, contextName, time.Minute, nil, "exec", pod, "--", "sh", "-c",
		script, "--", pid, want)
	if result.err != nil {
		t.Fatalf("MySQL server PID %s state check (%s): %s", pid, want, helmSmokeDiagnostic(result))
	}
}

func startHelmPortForward(t *testing.T, kubeconfig, contextName, service string, targetPort int) (int, func() error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port for Helm smoke: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release local port for Helm smoke: %v", err)
	}
	args := []string{
		"--kubeconfig", kubeconfig, "--context", contextName, "port-forward", "--address", "127.0.0.1", service,
		fmt.Sprintf("%d:%d", port, targetPort),
	}
	cmd := exec.Command("kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start Helm service port-forward: %v", err)
	}
	var stopOnce sync.Once
	var stopErr error
	stop := func() error {
		stopOnce.Do(func() {
			stopErr = stopHelmPortForward(cmd)
			if stopErr != nil && strings.TrimSpace(stderr.String()) != "" {
				stopErr = fmt.Errorf("%w; stderr: %s", stopErr, strings.TrimSpace(stderr.String()))
			}
		})

		return stopErr
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		probe, probeErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if probeErr == nil {
			if err := probe.Close(); err != nil {
				stopErr := stop()
				t.Fatalf("close Helm service port-forward probe: %v (stop port-forward: %v)", err, stopErr)
			}

			return port, stop
		}
		time.Sleep(250 * time.Millisecond)
	}
	stopErr = stop()
	t.Fatalf("Helm service port-forward did not become ready: %s (stop port-forward: %v)",
		strings.TrimSpace(stderr.String()), stopErr)

	return 0, nil
}

func stopHelmPortForward(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("port-forward process was not started")
	}
	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait()
	if killErr != nil {
		return fmt.Errorf("kill port-forward: %w; process exit: %v", killErr, waitErr)
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != -1 {
		return fmt.Errorf("port-forward did not terminate from expected signal: %w", waitErr)
	}

	return nil
}

func waitForHelmHTTPStatus(t *testing.T, url string, want int, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1024))
			closeErr := response.Body.Close()
			switch {
			case readErr != nil:
				last = readErr.Error()
			case closeErr != nil:
				last = closeErr.Error()
			case response.StatusCode == want:
				return
			default:
				last = fmt.Sprintf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
			}
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s did not return HTTP %d: %s", url, want, last)
}

func buildNativeHelmCLI(t *testing.T, tempDir string) string {
	t.Helper()
	binary := filepath.Join(tempDir, "claimy")
	root := repositoryRoot(t)
	result := helmSmokeRunInDir(t, 3*time.Minute, root, "go", "build", "-trimpath", "-o", binary, "./cmd/claimy")
	if result.err != nil {
		t.Fatalf("build native Claimy CLI for Helm smoke: %s", helmSmokeDiagnostic(result))
	}

	return binary
}

func helmSmokeRunInDir(t *testing.T, timeout time.Duration, dir, command string, args ...string) helmSmokeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return helmSmokeResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), err: err}
}

func runHelmNativeCISmoke(t *testing.T, binary, baseURL string, key *rsa.PrivateKey, suffix string) (string, helmSmokeIdentity) {
	t.Helper()
	identity := helmSmokeIdentity{
		issuer:    e2eGitLabIssuer,
		projectID: "project-" + suffix,
		jobID:     "job-" + suffix,
		userID:    "user-" + suffix,
	}
	token := signHelmGitLabToken(t, key, identity)
	tokenPath := writeCLIToken(t, t.TempDir(), "gitlab-id-token", token)
	run := func(args ...string) cliCommandResult {
		commandArgs := append([]string{"--url", baseURL, "--token-file", tokenPath}, args...)

		return runCLICommand(t, nil, binary, commandArgs...)
	}
	group := "helm-smoke-" + suffix
	requestID := "acquire-" + suffix
	acquired := run("acquire", "--group", group, "--environments", "sandbox", "--request-id", requestID)
	requireCLIExit(t, acquired, 0, "Helm native GitLab CI acquire")
	requireNoCLIStderr(t, acquired, "Helm native GitLab CI acquire")
	var response struct {
		Acquired bool `json:"acquired"`
		Claim    struct {
			ID string `json:"id"`
		} `json:"claim"`
	}
	decodeJSON(t, acquired.stdout, &response, "Helm native GitLab CI acquire response")
	if !response.Acquired || response.Claim.ID == "" {
		t.Fatalf("Helm native GitLab CI acquire did not return an active claim")
	}

	catalogResult := run("catalog", "group", "--group", group)
	requireCLIExit(t, catalogResult, 0, "Helm native GitLab CI catalog group")
	requireNoCLIStderr(t, catalogResult, "Helm native GitLab CI catalog group")
	var catalogGroup struct {
		CanonicalName string `json:"canonicalName"`
	}
	decodeJSON(t, catalogResult.stdout, &catalogGroup, "Helm native GitLab CI catalog group response")
	if catalogGroup.CanonicalName != group {
		t.Fatalf("Helm native GitLab CI catalog lookup returned canonical name %q, want %q",
			catalogGroup.CanonicalName, group)
	}

	queryResult := run("query", "--group", group, "--environments", "sandbox")
	requireCLIExit(t, queryResult, 0, "Helm native GitLab CI query")
	requireNoCLIStderr(t, queryResult, "Helm native GitLab CI query")
	var queryResponse struct {
		Claims []struct {
			ID string `json:"id"`
		} `json:"claims"`
	}
	decodeJSON(t, queryResult.stdout, &queryResponse, "Helm native GitLab CI query response")
	foundClaim := false
	for _, claim := range queryResponse.Claims {
		if claim.ID == response.Claim.ID {
			foundClaim = true

			break
		}
	}
	if !foundClaim {
		t.Fatalf("Helm native GitLab CI query did not return the acquired claim")
	}
	released := run("release", "--id", response.Claim.ID, "--request-id", "release-"+suffix)
	requireCLIExit(t, released, 0, "Helm native GitLab CI release")
	requireNoCLIStderr(t, released, "Helm native GitLab CI release")

	return response.Claim.ID, identity
}

type helmSmokeIdentity struct {
	issuer, projectID, jobID, userID string
}

func assertHelmCIPersistedIdentity(t *testing.T, kubeconfig, contextName, password, claimID string, identity helmSmokeIdentity) {
	t.Helper()
	pod := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "get", "pods", "-l", "app.kubernetes.io/name=claimy-mysql",
		"-o", "jsonpath={.items[0].metadata.name}")
	if pod.err != nil || strings.TrimSpace(string(pod.stdout)) == "" {
		t.Fatalf("find MySQL pod for persisted Claimy identity: %s", helmSmokeDiagnostic(pod))
	}
	query := "SELECT source,gitlab_issuer,gitlab_project_id,gitlab_job_id,gitlab_user_id FROM claims WHERE id='" + claimID + "'"
	result := kubectlSmoke(t, kubeconfig, contextName, time.Minute, nil, "exec", strings.TrimSpace(string(pod.stdout)), "--",
		"env", "MYSQL_PWD="+password, "mysql", "-uclaimy", "-Dclaimy", "-Nse", query)
	if result.err != nil {
		t.Fatalf("read persisted Helm GitLab CI identity: %s", helmSmokeDiagnostic(result))
	}
	fields := strings.Split(strings.TrimSpace(string(result.stdout)), "\t")
	if len(fields) != 5 || fields[0] != "ci" || fields[1] != identity.issuer || fields[2] != identity.projectID || fields[3] != identity.jobID || fields[4] != identity.userID {
		t.Fatalf("persisted Helm claim has unexpected GitLab identity: %q", strings.TrimSpace(string(result.stdout)))
	}
}

func helmSmokeImageRef(image string) (repository, tag, digest string) {
	if at := strings.LastIndexByte(image, '@'); at >= 0 {
		return image[:at], "synthetic", image[at+1:]
	}
	lastSlash := strings.LastIndexByte(image, '/')
	lastColon := strings.LastIndexByte(image, ':')
	if lastColon > lastSlash {
		return image[:lastColon], image[lastColon+1:], ""
	}

	return image, "latest", ""
}

func helmSmokeValues(repository, imageTag, digest, jwksURL string) string {
	return fmt.Sprintf(`nameOverride: claimy
image:
  repository: %s
  tag: %s
  digest: %s
  pullPolicy: IfNotPresent
database:
  host: claimy-mysql
  port: 3306
  name: claimy
  user: claimy
  existingSecret: claimy-db
  passwordKey: password
  parameters:
    loc: UTC
    time_zone: "'+00:00'"
config:
  app:
    env: test
    project: claimy
    family: claimy
    name: claimy
  tracing:
    provider: noop
  sampling:
    enabled: false
  metric:
    enabled: false
  httpserver:
    default:
      port: "8088"
  sqlc:
    default:
      driver: mysql
      uri:
        host: claimy-mysql
        port: 3306
        user: claimy
        database: claimy
      parameters:
        loc: UTC
        time_zone: "'+00:00'"
      migrations:
        enabled: false
        path: build/migrations/claimy
  claimy:
    auth:
      team_domain: example.test
      rest:
        issuer: https://issuer.claimy.e2e.test
        audience: claimy-helm-rest
        jwks_url: %s
      gitlab:
        issuer: %s
        audience: claimy-helm-gitlab
        jwks_url: %s
      chat:
        issuer: %s
        audience: claimy-helm-chat
        jwks_url: %s
      cli:
        enabled: false
        client_id: claimy-helm-rest
        scopes:
          - openid
          - email
        authorization_params: {}
    chat:
      app_identity: claimy-helm-smoke
      allowed_spaces:
        - spaces/claimy-helm-smoke
      deadline: 25s
service:
  port: 8088
migrations:
  enabled: true
  backoffLimit: 6
  activeDeadlineSeconds: 600
  resources: {}
extraEnv:
  - name: SSL_CERT_FILE
    value: /etc/claimy/jwks/ca.pem
extraVolumes:
  - name: jwks-ca
    secret:
      secretName: claimy-jwks-ca
extraVolumeMounts:
  - name: jwks-ca
    mountPath: /etc/claimy/jwks
    readOnly: true
`, yamlString(repository), yamlString(imageTag), yamlString(digest), yamlString(jwksURL), yamlString(e2eGitLabIssuer), yamlString(jwksURL), yamlString(e2eChatIssuer), yamlString(jwksURL))
}

func signHelmGitLabToken(t *testing.T, key *rsa.PrivateKey, identity helmSmokeIdentity) string {
	t.Helper()
	claims := map[string]any{
		"iss":            identity.issuer,
		"aud":            "claimy-helm-gitlab",
		"user_id":        identity.userID,
		"user_email":     "helm-smoke@example.test",
		"job_project_id": identity.projectID,
		"job_id":         identity.jobID,
		"iat":            time.Now().Unix(),
		"exp":            time.Now().Add(5 * time.Minute).Unix(),
	}
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": e2eKeyID, "typ": "JWT"})
	if err != nil {
		t.Fatalf("encode Helm smoke JWT header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encode Helm smoke JWT claims: %v", err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign Helm smoke JWT: %v", err)
	}

	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
