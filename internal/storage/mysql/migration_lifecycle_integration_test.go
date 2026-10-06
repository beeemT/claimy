//go:build integration && fixtures

package mysql

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/beeemT/claimy/internal/claims"
	"github.com/pressly/goose/v3"
)

func TestMigrationLifecycle(t *testing.T) {
	fixture, repository, _ := newTestRepository(t)
	ctx, cancel := testContext(t)
	defer cancel()

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate migration test source")
	}
	migrationFS := os.DirFS(filepath.Join(filepath.Dir(sourceFile), "../../../build/migrations/claimy"))
	provider, err := goose.NewProvider(goose.DialectMySQL, fixture.SQLDB, migrationFS)
	if err != nil {
		t.Fatalf("create Goose provider: %v", err)
	}

	const claimSchema = `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE()
		AND table_name IN ('app_groups', 'apps', 'claims', 'claim_environments', 'claim_versions', 'request_results')`
	if tables := countRows(t, fixture.SQLDB, claimSchema); tables != 6 {
		t.Fatalf("fixture did not start with the complete claim schema: tables=%d", tables)
	}
	if _, err = provider.Down(ctx); err != nil {
		t.Fatalf("roll back claim migration: %v", err)
	}
	if tables := countRows(t, fixture.SQLDB, claimSchema); tables != 0 {
		t.Fatalf("claim schema remained after migration rollback: tables=%d", tables)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatalf("reapply claim migration: %v", err)
	}
	if tables := countRows(t, fixture.SQLDB, claimSchema); tables != 6 {
		t.Fatalf("claim schema was not restored: tables=%d", tables)
	}

	opTime := queryDBTime(t, fixture.SQLDB)
	setOperationTime(repository, opTime)
	actor := testActor("migration@example.test", "migration-user", claims.REST, "", "")
	request := acquireRequest(actor, "migration-restored-acquire", "migration-restored-acquire", claims.Scope{Group: "migration-restored", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(opTime.Add(time.Hour)))
	acquired := mustAcquire(ctx, t, repository, actor, request)
	if !acquired.Acquired || acquired.Claim == nil {
		t.Fatalf("consumer acquisition failed after migration restoration: %+v", acquired)
	}
}
