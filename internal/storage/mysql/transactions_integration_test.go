//go:build integration && fixtures

package mysql

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beeemT/claimy/internal/claims"
	"github.com/beeemT/claimy/test/support"
	"github.com/gosoline-project/sqlc"
	"github.com/justtrackio/gosoline/pkg/cfg"
)

func TestRepositoryTransactionalFailureScenarios(t *testing.T) {
	fixture, repository, wallNow := newTestRepository(t)
	ctx, cancel := testContext(t)
	defer cancel()
	actor := testActor("transaction@example.test", "transaction-user", claims.REST, "", "")
	expiresAt := wallNow.Add(time.Hour)

	t.Run("S24 pre-commit SQL failure rolls back all rows", func(t *testing.T) {
		testPreCommitSQLFailure(ctx, t, fixture, repository, actor, expiresAt)
	})
	t.Run("S24 lost COMMIT acknowledgement is not retried and replay finds committed winner", func(t *testing.T) {
		testLostCommitAcknowledgement(ctx, t, fixture, repository, actor, wallNow, expiresAt)
	})
}

func testPreCommitSQLFailure(ctx context.Context, t *testing.T, fixture *support.Fixture, repository *Repository, actor claims.Actor, expiresAt time.Time) {
	if _, err := fixture.SQLDB.Exec(`CREATE TRIGGER claimy_test_fail_version BEFORE INSERT ON claim_versions FOR EACH ROW BEGIN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'forced claim history failure'; END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	defer func() {
		if _, err := fixture.SQLDB.Exec(`DROP TRIGGER claimy_test_fail_version`); err != nil {
			t.Errorf("drop failure trigger: %v", err)
		}
	}()
	request := acquireRequest(actor, "s24-rollback", "s24-rollback", claims.Scope{Group: "s24-rollback", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(expiresAt))
	_, err := repository.Acquire(ctx, actor, request)
	mustErrorCode(t, err, claims.StorageError)
	checks := []struct {
		name  string
		query string
		args  []any
	}{
		{"group", `SELECT COUNT(*) FROM app_groups WHERE canonical_name = ?`, []any{"s24-rollback"}},
		{"app", `SELECT COUNT(*) FROM apps a JOIN app_groups g ON g.id = a.group_id WHERE g.canonical_name = ?`, []any{"s24-rollback"}},
		{"claim", `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, []any{"s24-rollback"}},
		{"version", `SELECT COUNT(*) FROM claim_versions`, nil},
		{"environment", `SELECT COUNT(*) FROM claim_environments`, nil},
		{"result", `SELECT COUNT(*) FROM request_results WHERE request_id = ?`, []any{"s24-rollback"}},
	}
	for _, check := range checks {
		if countRows(t, fixture.SQLDB, check.query, check.args...) != 0 {
			t.Fatalf("pre-commit failure left %s state", check.name)
		}
	}
}

func testLostCommitAcknowledgement(ctx context.Context, t *testing.T, fixture *support.Fixture, repository *Repository, actor claims.Actor, wallNow, expiresAt time.Time) {
	proxyClient, dropped := newCommitProxyClient(t, fixture)
	t.Cleanup(func() {
		if err := proxyClient.Close(); err != nil {
			t.Errorf("close proxy-routed SQL client: %v", err)
		}
	})
	proxyRepository := New(proxyClient)
	ambiguousContext, ambiguousCancel := context.WithTimeout(ctx, 20*time.Second)
	defer ambiguousCancel()
	setOperationTime(proxyRepository, wallNow)
	request := acquireRequest(actor, "s24-ambiguous", "s24-ambiguous", claims.Scope{Group: "s24-ambiguous", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(expiresAt))
	_, err := proxyRepository.Acquire(ambiguousContext, actor, request)
	mustErrorCode(t, err, claims.StorageError)
	select {
	case <-dropped:
	case <-time.After(5 * time.Second):
		t.Fatal("packet proxy did not observe and drop the COMMIT acknowledgement")
	}
	assertAmbiguousCommitRows(t, fixture)
	replayed := mustAcquire(ctx, t, repository, actor, request)
	if !replayed.Acquired || replayed.Claim == nil || replayed.Claim.ActiveNow != true {
		t.Fatalf("committed ambiguous result was not replayed: %+v", replayed)
	}
	if countRows(t, fixture.SQLDB, `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, "s24-ambiguous") != 1 {
		t.Fatal("ambiguous COMMIT was retried as a second acquisition")
	}
}

func newCommitProxyClient(t *testing.T, fixture *support.Fixture) (sqlc.Client, <-chan struct{}) {
	host, err := fixture.Config.GetString("sqlc.default.uri.host")
	if err != nil {
		t.Fatalf("read MySQL host: %v", err)
	}
	port, err := fixture.Config.GetInt("sqlc.default.uri.port")
	if err != nil {
		t.Fatalf("read MySQL port: %v", err)
	}
	proxyAddress, dropped := startCommitAckDropProxy(t, net.JoinHostPort(host, strconv.Itoa(port)))
	proxyHost, proxyPortText, err := net.SplitHostPort(proxyAddress)
	if err != nil {
		t.Fatalf("split proxy address: %v", err)
	}
	proxyPort, err := strconv.Atoi(proxyPortText)
	if err != nil {
		t.Fatalf("parse proxy port %q: %v", proxyPortText, err)
	}
	configuration, ok := fixture.Config.(cfg.GosoConf)
	if !ok {
		t.Fatal("fixture config does not support the public mutable Gosoline config interface")
	}
	if err = configuration.Option(cfg.WithConfigSetting("sqlc.default.uri.host", proxyHost), cfg.WithConfigSetting("sqlc.default.uri.port", proxyPort)); err != nil {
		t.Fatalf("point independent SQL client at packet proxy: %v", err)
	}
	proxyClient, err := sqlc.NewClient(fixture.Context, fixture.Config, fixture.Logger, "default")
	if err != nil {
		t.Fatalf("create proxy-routed SQL client: %v", err)
	}

	return proxyClient, dropped
}

func assertAmbiguousCommitRows(t *testing.T, fixture *support.Fixture) {
	committedRows := []struct {
		name  string
		query string
	}{
		{"group", `SELECT COUNT(*) FROM app_groups WHERE canonical_name = 's24-ambiguous'`},
		{"app", `SELECT COUNT(*) FROM apps a JOIN app_groups g ON g.id = a.group_id WHERE g.canonical_name = 's24-ambiguous' AND a.canonical_name = 'api'`},
		{"claim", `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = 's24-ambiguous'`},
		{"environment", `SELECT COUNT(*) FROM claim_environments ce JOIN claims c ON c.id = ce.claim_id JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = 's24-ambiguous'`},
		{"history", `SELECT COUNT(*) FROM claim_versions v JOIN claims c ON c.id = v.claim_id JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = 's24-ambiguous'`},
		{"request result", `SELECT COUNT(*) FROM request_results WHERE request_id = 's24-ambiguous'`},
	}
	for _, row := range committedRows {
		if countRows(t, fixture.SQLDB, row.query) != 1 {
			t.Fatalf("COMMIT acknowledgement loss left committed claim without exactly one %s row", row.name)
		}
	}
}

func TestRepositoryMySQLConstraints(t *testing.T) {
	fixture, repository, wallNow := newTestRepository(t)
	ctx, cancel := testContext(t)
	defer cancel()
	actor := testActor("constraints@example.test", "constraint-user", claims.REST, "", "")
	expiresAt := wallNow.Add(time.Hour)
	acquired := mustAcquire(ctx, t, repository, actor, acquireRequest(actor, "constraint-baseline", "constraint-baseline", claims.Scope{Group: "constraint-group", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(expiresAt)))

	var appID, groupID uint64
	if err := fixture.SQLDB.QueryRow(`SELECT g.id, a.id FROM app_groups g JOIN apps a ON a.group_id = g.id WHERE g.canonical_name = ? AND a.canonical_name = ?`, "constraint-group", "api").Scan(&groupID, &appID); err != nil {
		t.Fatalf("read constraint fixture IDs: %v", err)
	}

	t.Run("CI identity is present exactly for CI claims", func(t *testing.T) {
		manualWithCI := `INSERT INTO claims (id, group_id, app_id, owner_email, source, gitlab_issuer, gitlab_project_id, gitlab_job_id, gitlab_user_id, created_at, expires_at, released_at, revision)
			VALUES ('10000000-0000-4000-8000-000000000001', ?, ?, 'invalid@example.test', 'manual', 'https://issuer.example.test', '1', 'job', 'user', ?, ?, NULL, 1)`
		if _, err := fixture.SQLDB.Exec(manualWithCI, groupID, appID, datetimeArg(wallNow), datetimeArg(expiresAt)); err == nil {
			t.Fatal("manual claim with CI identity passed the schema check")
		}
		ciWithoutIdentity := `INSERT INTO claims (id, group_id, app_id, owner_email, source, created_at, expires_at, released_at, revision)
			VALUES ('10000000-0000-4000-8000-000000000002', ?, ?, 'invalid@example.test', 'ci', ?, ?, NULL, 1)`
		if _, err := fixture.SQLDB.Exec(ciWithoutIdentity, groupID, appID, datetimeArg(wallNow), datetimeArg(expiresAt)); err == nil {
			t.Fatal("CI claim without identity passed the schema check")
		}
	})

	t.Run("only one open history version exists", func(t *testing.T) {
		_, err := fixture.SQLDB.Exec(`INSERT INTO claim_versions (claim_id, revision, valid_from, valid_to, expires_at, released, actor_email, actor_issuer, actor_subject, channel, action)
			VALUES (?, 2, ?, NULL, ?, FALSE, 'constraints@example.test', 'https://issuer.example.test', 'constraint-user', 'rest', 'expiry_changed')`, acquired.Claim.ID, datetimeArg(wallNow.Add(time.Microsecond)), datetimeArg(expiresAt))
		if err == nil {
			t.Fatal("claim accepted two open history versions")
		}
	})

	t.Run("foreign-key relationships restrict destructive deletes", func(t *testing.T) {
		if _, err := fixture.SQLDB.Exec(`DELETE FROM apps WHERE id = ?`, appID); err == nil {
			t.Fatal("app referenced by a claim was deleted")
		}
		if _, err := fixture.SQLDB.Exec(`DELETE FROM claims WHERE id = ?`, acquired.Claim.ID); err == nil {
			t.Fatal("claim referenced by environment/history/idempotency rows was deleted")
		}
		if _, err := fixture.SQLDB.Exec(`DELETE FROM app_groups WHERE id = ?`, groupID); err == nil {
			t.Fatal("group referenced by an app and claim was deleted")
		}
	})
}

func startCommitAckDropProxy(t *testing.T, upstreamAddress string) (string, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for MySQL proxy: %v", err)
	}
	dropped := make(chan struct{})
	var dropOnce sync.Once
	var connections sync.WaitGroup
	var acceptLoop sync.WaitGroup
	acceptLoop.Add(1)
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close MySQL proxy listener: %v", err)
		}
		acceptLoop.Wait()
		connections.Wait()
	})
	go func() {
		defer acceptLoop.Done()
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				proxyMySQLConnection(t, client, upstreamAddress, dropped, &dropOnce)
			}()
		}
	}()

	return listener.Addr().String(), dropped
}

func proxyMySQLConnection(t *testing.T, client net.Conn, upstreamAddress string, dropped chan struct{}, dropOnce *sync.Once) {
	upstream, err := net.DialTimeout("tcp", upstreamAddress, 5*time.Second)
	if err != nil {
		closeProxyConnection(t, "client", client)

		return
	}
	var pendingCommit atomic.Bool
	var closeOnce sync.Once
	closed := make(chan struct{})
	closeBoth := func() {
		closeOnce.Do(func() {
			closeProxyConnection(t, "client", client)
			closeProxyConnection(t, "upstream", upstream)
			close(closed)
		})
	}
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		pumpMySQLClientRequests(client, upstream, &pendingCommit, closeBoth)
	}()
	go func() {
		defer pumps.Done()
		pumpMySQLUpstreamResponses(client, upstream, dropped, dropOnce, &pendingCommit, closeBoth)
	}()
	<-closed
	pumps.Wait()
}

func pumpMySQLClientRequests(client, upstream net.Conn, pendingCommit *atomic.Bool, closeBoth func()) {
	for {
		packet, payload, readErr := readMySQLPacket(client)
		if readErr != nil {
			closeBoth()

			return
		}
		if isCommitPacket(payload) {
			pendingCommit.Store(true)
		}
		if err := writeAll(upstream, packet); err != nil {
			closeBoth()

			return
		}
	}
}

func pumpMySQLUpstreamResponses(client, upstream net.Conn, dropped chan struct{}, dropOnce *sync.Once, pendingCommit *atomic.Bool, closeBoth func()) {
	for {
		packet, _, readErr := readMySQLPacket(upstream)
		if readErr != nil {
			closeBoth()

			return
		}
		if pendingCommit.CompareAndSwap(true, false) {
			dropOnce.Do(func() { close(dropped) })
			closeBoth()

			return
		}
		if err := writeAll(client, packet); err != nil {
			closeBoth()

			return
		}
	}
}

func closeProxyConnection(t *testing.T, name string, connection net.Conn) {
	if err := connection.Close(); err != nil {
		t.Errorf("close MySQL proxy %s connection: %v", name, err)
	}
}

func readMySQLPacket(reader io.Reader) ([]byte, []byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, nil, err
	}
	payloadLength := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	packet := make([]byte, len(header)+payloadLength)
	copy(packet, header[:])
	if _, err := io.ReadFull(reader, packet[len(header):]); err != nil {
		return nil, nil, err
	}

	return packet, packet[len(header):], nil
}

func isCommitPacket(payload []byte) bool {
	if len(payload) < 2 || payload[0] != 0x03 {
		return false
	}

	return strings.EqualFold(strings.TrimSpace(string(payload[1:])), "COMMIT")
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		data = data[written:]
	}

	return nil
}
