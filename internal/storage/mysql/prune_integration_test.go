//go:build integration && fixtures

package mysql

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/beeemT/claimy/internal/claims"
)

func TestRepositoryPruneRetentionBoundaries(t *testing.T) {
	fixture, repository, wallNow := newTestRepository(t)
	ctx, cancel := testContext(t)
	defer cancel()
	actor := testActor("retention@example.test", "retention-user", claims.REST, "", "")
	pruneAt := wallNow.Add(time.Hour)
	cutoff := pruneAt.Add(-claims.Retention)
	state := preparePruneRetentionClaims(ctx, t, repository, actor, pruneAt, cutoff)

	setOperationTime(repository, pruneAt)
	pruned, err := repository.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	assertPruneCounts(t, pruned)
	assertPruneCleanup(t, fixture.SQLDB, state)
	assertPruneHistory(ctx, t, repository, actor, fixture.SQLDB, state, cutoff)

	tooOld := wallNow.Add(-claims.Retention - 24*time.Hour)
	_, err = repository.Query(ctx, actor, claims.QueryRequest{Scope: claims.Scope{Group: "retention-long"}, At: fixedPointer(tooOld)})
	mustErrorCode(t, err, claims.HistoryUnavailable)
}

type pruneRetentionClaims struct {
	long     *claims.Claim
	closed   *claims.Claim
	terminal *claims.Claim
	boundary *claims.Claim
}

func preparePruneRetentionClaims(ctx context.Context, t *testing.T, repository *Repository, actor claims.Actor, pruneAt, cutoff time.Time) pruneRetentionClaims {
	longExpiry := pruneAt.Add(10 * 24 * time.Hour)
	setOperationTime(repository, cutoff.Add(-24*time.Hour))
	long := mustAcquire(ctx, t, repository, actor, acquireRequest(actor, "retention-long", "retention-long", claims.Scope{Group: "retention-long", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(longExpiry)))
	setOperationTime(repository, cutoff.Add(24*time.Hour))
	longUpdated := mustChangeExpiry(ctx, t, repository, actor, long.Claim.ID, "retention-long-change", "retention-long-change", pruneAt.Add(20*24*time.Hour), ptrRevision(1))
	if !longUpdated.Changed {
		t.Fatal("long-running claim history setup did not create a later revision")
	}

	setOperationTime(repository, cutoff.Add(-48*time.Hour))
	busyExpiry := pruneAt.Add(40 * 24 * time.Hour)
	mustAcquire(ctx, t, repository, actor, acquireRequest(actor, "retention-busy-baseline", "retention-busy-baseline", claims.Scope{Group: "retention-busy"}, []claims.Environment{claims.Sandbox}, fixedPointer(busyExpiry)))
	busyActor := testActor("other-retention@example.test", "other-retention-user", claims.REST, "", "")
	busy := mustAcquire(ctx, t, repository, busyActor, acquireRequest(busyActor, "retention-busy", "retention-busy", claims.Scope{Group: "retention-busy"}, []claims.Environment{claims.Sandbox}, fixedPointer(busyExpiry)))
	if busy.Acquired || len(busy.Conflicts) != 1 {
		t.Fatalf("expired-result setup did not save a busy outcome: %+v", busy)
	}

	setOperationTime(repository, cutoff.Add(-48*time.Hour))
	closed := mustAcquire(ctx, t, repository, actor, acquireRequest(actor, "retention-closed", "retention-closed", claims.Scope{Group: "retention-closed", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(pruneAt.Add(30*24*time.Hour))))
	setOperationTime(repository, cutoff.Add(-24*time.Hour))
	closedUpdated := mustChangeExpiry(ctx, t, repository, actor, closed.Claim.ID, "retention-closed-change", "retention-closed-change", pruneAt.Add(31*24*time.Hour), ptrRevision(1))
	if !closedUpdated.Changed {
		t.Fatal("closed-history setup did not create a later revision")
	}

	setOperationTime(repository, cutoff.Add(-48*time.Hour))
	terminalExpiry := cutoff.Add(-24 * time.Hour)
	terminal := mustAcquire(ctx, t, repository, actor, acquireRequest(actor, "retention-terminal", "retention-terminal", claims.Scope{Group: "retention-terminal", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(terminalExpiry)))
	if !terminal.Acquired {
		t.Fatal("terminal claim setup did not acquire")
	}

	setOperationTime(repository, cutoff.Add(-24*time.Hour))
	boundary := mustAcquire(ctx, t, repository, actor, acquireRequest(actor, "retention-boundary", "retention-boundary", claims.Scope{Group: "retention-boundary"}, []claims.Environment{claims.Sandbox}, fixedPointer(cutoff)))
	if !boundary.Acquired {
		t.Fatal("exact-cutoff claim setup did not acquire")
	}

	return pruneRetentionClaims{long: long.Claim, closed: closed.Claim, terminal: terminal.Claim, boundary: boundary.Claim}
}

func assertPruneCounts(t *testing.T, pruned claims.PruneResult) {
	t.Helper()
	if pruned.Claims != 1 || pruned.Versions != 2 || pruned.Results != 3 {
		t.Fatalf("prune did not apply strict cleanup boundaries: %+v", pruned)
	}
}

func assertPruneCleanup(t *testing.T, db *sql.DB, state pruneRetentionClaims) {
	t.Helper()
	if countRows(t, db, `SELECT COUNT(*) FROM claims WHERE id = ?`, state.terminal.ID) != 0 {
		t.Fatal("terminal claim older than cutoff remained after its results expired")
	}
	if countRows(t, db, `SELECT COUNT(*) FROM claim_versions WHERE claim_id = ?`, state.terminal.ID) != 0 ||
		countRows(t, db, `SELECT COUNT(*) FROM claim_environments WHERE claim_id = ?`, state.terminal.ID) != 0 {
		t.Fatal("terminal claim children were not removed before the claim")
	}
	if countRows(t, db, `SELECT COUNT(*) FROM request_results WHERE request_id = ?`, "retention-terminal") != 0 {
		t.Fatal("expired terminal acquisition result was retained")
	}
	if countRows(t, db, `SELECT COUNT(*) FROM request_results WHERE request_id = ?`, "retention-busy") != 0 {
		t.Fatal("expired busy idempotency result was retained")
	}
	if countRows(t, db, `SELECT COUNT(*) FROM claims WHERE id = ?`, state.boundary.ID) != 1 ||
		countRows(t, db, `SELECT COUNT(*) FROM request_results WHERE request_id = ?`, "retention-boundary") != 0 {
		t.Fatal("claim at the strict terminal cutoff was deleted or its exact-expiry result was retained")
	}
	if countRows(t, db, `SELECT COUNT(*) FROM request_results WHERE claim_id = ?`, state.long.ID) != 2 ||
		countRows(t, db, `SELECT COUNT(*) FROM claims WHERE id = ?`, state.long.ID) != 1 {
		t.Fatal("active claim or its acquisition/change idempotency results were pruned")
	}
	if countRows(t, db, `SELECT COUNT(*) FROM claim_versions WHERE claim_id = ?`, state.long.ID) != 2 {
		t.Fatal("history version spanning the cutoff was pruned")
	}
	if countRows(t, db, `SELECT COUNT(*) FROM claim_versions WHERE claim_id = ?`, state.closed.ID) != 1 {
		t.Fatal("closed version older than cutoff was retained or open version was pruned")
	}
}

func assertPruneHistory(ctx context.Context, t *testing.T, repository *Repository, actor claims.Actor, db *sql.DB, state pruneRetentionClaims, cutoff time.Time) {
	t.Helper()
	atCutoffLong := mustQuery(ctx, t, repository, actor, claims.QueryRequest{Scope: state.long.Scope, Environments: state.long.Environments, At: fixedPointer(cutoff)})
	atCutoffClosed := mustQuery(ctx, t, repository, actor, claims.QueryRequest{Scope: state.closed.Scope, Environments: state.closed.Environments, At: fixedPointer(cutoff)})
	if len(atCutoffLong.Claims) != 1 || atCutoffLong.Claims[0].Revision != 1 || len(atCutoffClosed.Claims) != 1 || atCutoffClosed.Claims[0].Revision != 2 {
		t.Fatalf("pruning changed retained as-of history: long=%+v closed=%+v", atCutoffLong, atCutoffClosed)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM app_groups WHERE canonical_name IN (?, ?, ?, ?, ?)`, "retention-long", "retention-closed", "retention-terminal", "retention-boundary", "retention-busy") != 5 {
		t.Fatal("pruning removed durable resource catalog entries")
	}
}
