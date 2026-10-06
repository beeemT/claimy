//go:build integration && fixtures

package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/beeemT/claimy/internal/claims"
	"github.com/beeemT/claimy/test/support"
)

const integrationContextTimeout = 2 * time.Minute

func newTestRepository(t *testing.T) (*support.Fixture, *Repository, time.Time) {
	t.Helper()
	fixture := support.NewFixture(t)
	now := queryDBTime(t, fixture.SQLDB)
	repository := New(fixture.Client)
	fixedNow := now
	repository.operationTime = func(context.Context, *sql.Tx) (time.Time, error) {
		return fixedNow, nil
	}

	return fixture, repository, now
}

func queryDBTime(t *testing.T, db *sql.DB) time.Time {
	t.Helper()
	var value string
	if err := db.QueryRow(`SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f')`).Scan(&value); err != nil {
		t.Fatalf("read MySQL UTC time: %v", err)
	}
	parsed, err := parseDatabaseTime(value)
	if err != nil {
		t.Fatalf("parse MySQL UTC time: %v", err)
	}

	return parsed
}

func setOperationTime(repository *Repository, value time.Time) {
	repository.operationTime = func(context.Context, *sql.Tx) (time.Time, error) {
		return value.UTC().Truncate(time.Microsecond), nil
	}
}

func testActor(email, subject string, channel claims.Channel, projectID, jobID string) claims.Actor {
	actor := claims.Actor{Email: email, Issuer: "https://issuer.example.test", Subject: subject, Channel: channel}
	if channel == claims.GitLabCI {
		actor.GitLab = &claims.GitLabIdentity{Issuer: actor.Issuer, ProjectID: projectID, JobID: jobID, UserID: subject}
	}

	return actor
}

func testMeta(actor claims.Actor, payload string) claims.MutationMeta {
	principal := claims.Principal{Issuer: actor.Issuer, ID: actor.Subject}
	switch actor.Channel {
	case claims.GitLabCI:
		principal.Kind = "gitlab_job"
		principal.Issuer = actor.GitLab.Issuer
		key, err := json.Marshal([]string{actor.GitLab.ProjectID, actor.GitLab.JobID})
		if err != nil {
			panic(err)
		}
		digest := sha256.Sum256(key)
		principal.ID = hex.EncodeToString(digest[:])
	case claims.GoogleChat:
		principal.Kind = "google_chat_user"
	default:
		principal.Kind = "rest_user"
	}

	return claims.MutationMeta{Principal: principal, PayloadHash: sha256.Sum256([]byte(payload))}
}

func acquireRequest(actor claims.Actor, requestID, payload string, scope claims.Scope, environments []claims.Environment, expiresAt *time.Time) claims.AcquireRequest {
	return claims.AcquireRequest{
		Scope: scope, Environments: environments, ExpiresAt: expiresAt, RequestID: requestID,
		MutationMeta: testMeta(actor, payload),
	}
}

func mustAcquire(ctx context.Context, t *testing.T, repository *Repository, actor claims.Actor, request claims.AcquireRequest) claims.AcquireResult {
	t.Helper()
	result, err := repository.Acquire(ctx, actor, request)
	if err != nil {
		t.Fatalf("Acquire(%+v): %v", request.Scope, err)
	}

	return result
}

func mustQuery(ctx context.Context, t *testing.T, repository *Repository, actor claims.Actor, request claims.QueryRequest) claims.QueryResult {
	t.Helper()
	result, err := repository.Query(ctx, actor, request)
	if err != nil {
		t.Fatalf("Query(%+v): %v", request.Scope, err)
	}

	return result
}

func mustErrorCode(t *testing.T, err error, code claims.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", code)
	}
	var typed *claims.Error
	if !errors.As(err, &typed) {
		t.Fatalf("expected typed %q error, got %T: %v", code, err, err)
	}
	if typed.Code != code {
		t.Fatalf("expected error %q, got %q (%v)", code, typed.Code, err)
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows for %q: %v", query, err)
	}

	return count
}

func mustRelease(ctx context.Context, t *testing.T, repository *Repository, actor claims.Actor, claimID, requestID, payload string) claims.MutationResult {
	t.Helper()
	result, err := repository.Release(ctx, actor, claims.ReleaseRequest{
		ClaimID: claimID, RequestID: requestID, MutationMeta: testMeta(actor, payload),
	})
	if err != nil {
		t.Fatalf("Release(%s): %v", claimID, err)
	}

	return result
}

func mustChangeExpiry(ctx context.Context, t *testing.T, repository *Repository, actor claims.Actor, claimID, requestID, payload string, expiresAt time.Time, expectedRevision *uint32) claims.MutationResult {
	t.Helper()
	result, err := repository.ChangeExpiry(ctx, actor, claims.ExpiryRequest{
		ClaimID: claimID, ExpiresAt: expiresAt, ExpectedRevision: expectedRevision, RequestID: requestID,
		MutationMeta: testMeta(actor, payload),
	})
	if err != nil {
		t.Fatalf("ChangeExpiry(%s): %v", claimID, err)
	}

	return result
}

func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()

	return context.WithTimeout(context.Background(), integrationContextTimeout)
}

func acquireInGoroutines(ctx context.Context, repositories []*Repository, actors []claims.Actor, requests []claims.AcquireRequest) []acquireOutcome {
	outcomes := make([]acquireOutcome, len(repositories))
	started := make(chan struct{}, len(repositories))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range repositories {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			started <- struct{}{}
			<-start
			outcomes[index].Result, outcomes[index].Err = repositories[index].Acquire(ctx, actors[index], requests[index])
		}(index)
	}
	for range repositories {
		<-started
	}
	close(start)
	wait.Wait()

	return outcomes
}

type acquireOutcome struct {
	Result claims.AcquireResult
	Err    error
}

func fixedPointer(value time.Time) *time.Time {
	return &value
}

func ciActor(email, subject, job string) claims.Actor {
	return testActor(email, subject, claims.GitLabCI, "project-42", job)
}

func TestRepositoryAcquireScenarios(t *testing.T) {
	fixture, repository, wallNow := newTestRepository(t)
	ctx, cancel := testContext(t)
	defer cancel()
	state := acquisitionScenario{
		fixture:    fixture,
		repository: repository,
		ctx:        ctx,
		wallNow:    wallNow,
		expiresAt:  wallNow.Add(24 * time.Hour),
		owner:      testActor("owner@example.test", "owner-1", claims.REST, "", ""),
		other:      testActor("other@example.test", "other-1", claims.REST, "", ""),
	}
	runAcquireScenarios(t, state)
}

type acquisitionScenario struct {
	fixture    *support.Fixture
	repository *Repository
	ctx        context.Context
	wallNow    time.Time
	expiresAt  time.Time
	owner      claims.Actor
	other      claims.Actor
}

func runAcquireScenarios(t *testing.T, state acquisitionScenario) {
	t.Run("S01 app sandbox is independent", func(t *testing.T) { testAcquireAppSandbox(t, state) })
	t.Run("S02 group claims cover future apps", func(t *testing.T) { testAcquireGroupClaim(t, state) })
	t.Run("S03 both environments are atomic", func(t *testing.T) { testAcquireBothEnvironments(t, state) })
	t.Run("S04 app and group overlap symmetrically", func(t *testing.T) { testAcquireOverlapSymmetry(t, state) })
	t.Run("S05 same-owner overlapping claims are independent", func(t *testing.T) { testAcquireSameOwnerOverlap(t, state) })
	t.Run("S06 same owner is allowed and another owner is not", func(t *testing.T) { testAcquireOwnerPermissions(t, state) })
	t.Run("S07 concurrent same-owner CI jobs both acquire", func(t *testing.T) { testAcquireConcurrentSameOwner(t, state) })
	t.Run("S08 concurrent different owners have one winner", func(t *testing.T) { testAcquireConcurrentDifferentOwners(t, state) })
	t.Run("S09 registration and app-group acquisition serialize", func(t *testing.T) { testAcquireRegistrationRace(t, state) })
	t.Run("S10 expiry equality uses half-open activity", func(t *testing.T) { testAcquireExpiryBoundary(t, state) })
	t.Run("S11 Berlin default expiry handles both DST changes for manual and CI", func(t *testing.T) { testAcquireDefaultExpiryDST(t, state) })
	t.Run("S12 invalid expiry rolls back catalog and idempotency writes", func(t *testing.T) { testAcquireInvalidExpiryRollback(t, state) })
	t.Run("S13 successful unknown target acquisition registers catalogs", func(t *testing.T) { testAcquireUnknownTarget(t, state) })
	t.Run("S14 busy unknown app registration commits without a claim", func(t *testing.T) { testAcquireBusyRegistration(t, state) })
	t.Run("S19 future queries are projections, not reservations", func(t *testing.T) { testAcquireFutureProjection(t, state) })
	t.Run("S20 unknown reads are side-effect free and inherit group claims", func(t *testing.T) { testAcquireUnknownReads(t, state) })
	t.Run("S21 acquisition replay preserves original claim and expiry", func(t *testing.T) { testAcquireReplay(t, state) })
	t.Run("S22 request payload mismatch is global and cross-group loser rolls back", func(t *testing.T) {
		testAcquirePayloadMismatch(t, state)
		testAcquireCrossGroupRace(t, state)
	})
	t.Run("S23 successful replay reports current inactive state", func(t *testing.T) { testAcquireInactiveReplay(t, state) })
}

func testAcquireAppSandbox(t *testing.T, state acquisitionScenario) {
	result := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s01-a", "s01-a", claims.Scope{Group: "s01-apps", App: "app-a"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if !result.Acquired {
		t.Fatal("app sandbox claim was not acquired")
	}
	for name, env := range map[string]claims.Environment{"sibling": claims.Sandbox, "production": claims.Prod} {
		query := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s01-apps", App: map[string]string{"sibling": "app-b", "production": "app-a"}[name]}, Environments: []claims.Environment{env}})
		if !query.Free {
			t.Fatalf("%s unexpectedly taken: %+v", name, query.Claims)
		}
	}
	query := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s01-apps", App: "app-a"}, Environments: []claims.Environment{claims.Sandbox}})
	if query.Free || len(query.Claims) != 1 {
		t.Fatalf("claimed app sandbox was not reported taken: %+v", query)
	}
}

func testAcquireGroupClaim(t *testing.T, state acquisitionScenario) {
	result := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s02-group", "s02-group", claims.Scope{Group: "s02-future"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if !result.Acquired {
		t.Fatal("group claim was not acquired")
	}
	before := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s02-future", App: "new-app"}, Environments: []claims.Environment{claims.Sandbox}})
	if before.Known || len(before.Claims) != 1 || !before.Claims[0].Inherited {
		t.Fatalf("unknown app did not inherit group claim: %+v", before)
	}
	busy := mustAcquire(state.ctx, t, state.repository, state.other, acquireRequest(state.other, "s02-new-app", "s02-new-app", claims.Scope{Group: "s02-future", App: "new-app"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if busy.Acquired || len(busy.Conflicts) != 1 {
		t.Fatalf("new app was not blocked by group claim: %+v", busy)
	}
	after := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s02-future", App: "new-app"}, Environments: []claims.Environment{claims.Sandbox}})
	if !after.Known || len(after.Claims) != 1 || !after.Claims[0].Inherited {
		t.Fatalf("busy acquisition did not persist app registration: %+v", after)
	}
}

func testAcquireBothEnvironments(t *testing.T, state acquisitionScenario) {
	mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s03-prod", "s03-prod", claims.Scope{Group: "s03-atomic", App: "api"}, []claims.Environment{claims.Prod}, fixedPointer(state.expiresAt)))
	before := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, "s03-atomic")
	busy := mustAcquire(state.ctx, t, state.repository, state.other, acquireRequest(state.other, "s03-both", "s03-both", claims.Scope{Group: "s03-atomic", App: "api"}, []claims.Environment{claims.Sandbox, claims.Prod}, fixedPointer(state.expiresAt)))
	if busy.Acquired || len(busy.Conflicts) != 1 || len(busy.Conflicts[0].Environments) != 1 || busy.Conflicts[0].Environments[0] != claims.Prod {
		t.Fatalf("both-environment request did not return the production blocker: %+v", busy)
	}
	after := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, "s03-atomic")
	if after != before {
		t.Fatalf("busy request inserted a partial claim: before=%d after=%d", before, after)
	}
	query := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s03-atomic", App: "api"}, Environments: []claims.Environment{claims.Sandbox}})
	if !query.Free {
		t.Fatalf("sandbox half of a busy request was committed: %+v", query)
	}
}

func testAcquireOverlapSymmetry(t *testing.T, state acquisitionScenario) {
	for _, scenario := range []struct {
		name       string
		firstScope claims.Scope
		second     claims.Scope
	}{
		{name: "app-first", firstScope: claims.Scope{Group: "s04-app-first", App: "api"}, second: claims.Scope{Group: "s04-app-first"}},
		{name: "group-first", firstScope: claims.Scope{Group: "s04-group-first"}, second: claims.Scope{Group: "s04-group-first", App: "api"}},
	} {
		first := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s04-first-"+scenario.name, "s04-first-"+scenario.name, scenario.firstScope, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
		if !first.Acquired {
			t.Fatalf("first scope in %s was busy", scenario.name)
		}
		busy := mustAcquire(state.ctx, t, state.repository, state.other, acquireRequest(state.other, "s04-second-"+scenario.name, "s04-second-"+scenario.name, scenario.second, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
		if busy.Acquired || len(busy.Conflicts) != 1 {
			t.Fatalf("overlap was asymmetric in %s: %+v", scenario.name, busy)
		}
	}
}

func testAcquireSameOwnerOverlap(t *testing.T, state acquisitionScenario) {
	app := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s05-app", "s05-app", claims.Scope{Group: "s05-same-owner", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	group := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s05-group", "s05-group", claims.Scope{Group: "s05-same-owner"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if !app.Acquired || !group.Acquired || app.Claim.ID == group.Claim.ID {
		t.Fatalf("same-owner claims were not independently acquired: %+v %+v", app, group)
	}
	released := mustRelease(state.ctx, t, state.repository, state.other, group.Claim.ID, "s05-release", "s05-release")
	if !released.Changed || released.Claim.ID != group.Claim.ID {
		t.Fatalf("selected group claim was not released: %+v", released)
	}
	query := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s05-same-owner", App: "api"}, Environments: []claims.Environment{claims.Sandbox}})
	if query.Free || len(query.Claims) != 1 || query.Claims[0].ID != app.Claim.ID {
		t.Fatalf("releasing one ID affected the overlapping claim: %+v", query)
	}
}

func testAcquireOwnerPermissions(t *testing.T, state acquisitionScenario) {
	manual := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s06-manual", "s06-manual", claims.Scope{Group: "s06-permission"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if !manual.Acquired {
		t.Fatal("manual baseline claim was not acquired")
	}
	ownQuery := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s06-permission"}, Environments: []claims.Environment{claims.Sandbox}})
	if ownQuery.Free || !ownQuery.AllowedForCaller {
		t.Fatalf("same owner status/permission was conflated: %+v", ownQuery)
	}
	ci := ciActor(state.owner.Email, "ci-user", "job-s06")
	ciResult := mustAcquire(state.ctx, t, state.repository, ci, acquireRequest(ci, "s06-ci", "s06-ci", claims.Scope{Group: "s06-permission", App: "worker"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if !ciResult.Acquired || ciResult.Claim.Source != claims.CI {
		t.Fatalf("same-owner CI job was blocked: %+v", ciResult)
	}
	foreignQuery := mustQuery(state.ctx, t, state.repository, state.other, claims.QueryRequest{Scope: claims.Scope{Group: "s06-permission"}, Environments: []claims.Environment{claims.Sandbox}})
	if foreignQuery.Free || foreignQuery.AllowedForCaller {
		t.Fatalf("different owner was allowed through another owner's claim: %+v", foreignQuery)
	}
	busy := mustAcquire(state.ctx, t, state.repository, state.other, acquireRequest(state.other, "s06-other", "s06-other", claims.Scope{Group: "s06-permission"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if busy.Acquired || len(busy.Conflicts) == 0 {
		t.Fatalf("different owner did not receive blockers: %+v", busy)
	}
}

func testAcquireConcurrentSameOwner(t *testing.T, state acquisitionScenario) {
	firstClient := state.fixture.NewClient(t)
	secondClient := state.fixture.NewClient(t)
	firstRepo, secondRepo := New(firstClient), New(secondClient)
	setOperationTime(firstRepo, state.wallNow)
	setOperationTime(secondRepo, state.wallNow)
	actors := []claims.Actor{ciActor(state.owner.Email, "user-one", "job-one"), ciActor(state.owner.Email, "user-two", "job-two")}
	requests := []claims.AcquireRequest{
		acquireRequest(actors[0], "s07-one", "s07-one", claims.Scope{Group: "s07-jobs", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)),
		acquireRequest(actors[1], "s07-two", "s07-two", claims.Scope{Group: "s07-jobs", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)),
	}
	outcomes := acquireInGoroutines(state.ctx, []*Repository{firstRepo, secondRepo}, actors, requests)
	for i, outcome := range outcomes {
		if outcome.Err != nil || !outcome.Result.Acquired {
			t.Fatalf("same-owner job %d was serialized as a conflict: %+v", i, outcome)
		}
	}
	if outcomes[0].Result.Claim.ID == outcomes[1].Result.Claim.ID {
		t.Fatal("same-owner jobs did not receive independent claim IDs")
	}
}

func testAcquireConcurrentDifferentOwners(t *testing.T, state acquisitionScenario) {
	firstRepo := New(state.fixture.NewClient(t))
	secondRepo := New(state.fixture.NewClient(t))
	setOperationTime(firstRepo, state.wallNow)
	setOperationTime(secondRepo, state.wallNow)
	actors := []claims.Actor{state.owner, state.other}
	requests := []claims.AcquireRequest{
		acquireRequest(state.owner, "s08-owner", "s08-owner", claims.Scope{Group: "s08-race", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)),
		acquireRequest(state.other, "s08-other", "s08-other", claims.Scope{Group: "s08-race", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)),
	}
	outcomes := acquireInGoroutines(state.ctx, []*Repository{firstRepo, secondRepo}, actors, requests)
	winners := 0
	losers := 0
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			t.Fatalf("different-owner race returned a storage error: %v", outcome.Err)
		}
		if outcome.Result.Acquired {
			winners++
		} else if len(outcome.Result.Conflicts) > 0 {
			losers++
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("different-owner race results: winners=%d losers=%d (%+v)", winners, losers, outcomes)
	}
}

func testAcquireRegistrationRace(t *testing.T, state acquisitionScenario) {
	firstRepo := New(state.fixture.NewClient(t))
	secondRepo := New(state.fixture.NewClient(t))
	setOperationTime(firstRepo, state.wallNow)
	setOperationTime(secondRepo, state.wallNow)
	actors := []claims.Actor{state.owner, state.other}
	requests := []claims.AcquireRequest{
		acquireRequest(state.owner, "s09-app", "s09-app", claims.Scope{Group: "s09-register", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)),
		acquireRequest(state.other, "s09-group", "s09-group", claims.Scope{Group: "s09-register"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)),
	}
	outcomes := acquireInGoroutines(state.ctx, []*Repository{firstRepo, secondRepo}, actors, requests)
	winners := 0
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			t.Fatalf("registration race errored: %v", outcome.Err)
		}
		if outcome.Result.Acquired {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("app/group overlap did not yield one winner: %+v", outcomes)
	}
	if countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM app_groups WHERE canonical_name = ?`, "s09-register") != 1 {
		t.Fatal("racing registration created duplicate groups")
	}
	if countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM apps a JOIN app_groups g ON g.id = a.group_id WHERE g.canonical_name = ? AND a.canonical_name = ?`, "s09-register", "api") != 1 {
		t.Fatal("app registration was missed or duplicated in the race")
	}
	if countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, "s09-register") != 1 {
		t.Fatal("racing group/app operation committed more than one overlapping claim")
	}
}

func testAcquireExpiryBoundary(t *testing.T, state acquisitionScenario) {
	expiry := state.wallNow.Add(time.Hour)
	result := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s10-expiry", "s10-expiry", claims.Scope{Group: "s10-boundary", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(expiry)))
	before := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s10-boundary", App: "api"}, Environments: []claims.Environment{claims.Sandbox}, At: fixedPointer(expiry.Add(-time.Microsecond))})
	at := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s10-boundary", App: "api"}, Environments: []claims.Environment{claims.Sandbox}, At: fixedPointer(expiry)})
	after := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s10-boundary", App: "api"}, Environments: []claims.Environment{claims.Sandbox}, At: fixedPointer(expiry.Add(time.Microsecond))})
	if before.Free || len(before.Claims) != 1 || before.Claims[0].ID != result.Claim.ID || !at.Free || !after.Free {
		t.Fatalf("expiry boundary was not half-open: before=%+v at=%+v after=%+v", before, at, after)
	}
}

func testAcquireDefaultExpiryDST(t *testing.T, state acquisitionScenario) {
	cases := []struct {
		name     string
		opTime   time.Time
		expected time.Time
	}{
		{name: "spring", opTime: time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC), expected: time.Date(2026, 3, 29, 10, 0, 0, 0, time.UTC)},
		{name: "fall", opTime: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC), expected: time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)},
	}
	for _, scenario := range cases {
		setOperationTime(state.repository, scenario.opTime)
		manual := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s11-manual-"+scenario.name, "s11-manual-"+scenario.name, claims.Scope{Group: "s11-manual-" + scenario.name, App: "api"}, []claims.Environment{claims.Sandbox}, nil))
		ci := ciActor(state.owner.Email, "s11-ci-user", "s11-"+scenario.name)
		ciResult := mustAcquire(state.ctx, t, state.repository, ci, acquireRequest(ci, "s11-ci-"+scenario.name, "s11-ci-"+scenario.name, claims.Scope{Group: "s11-ci-" + scenario.name, App: "api"}, []claims.Environment{claims.Sandbox}, nil))
		if !manual.Acquired || !ciResult.Acquired || !manual.Claim.ExpiresAt.Equal(scenario.expected) || !ciResult.Claim.ExpiresAt.Equal(scenario.expected) {
			t.Fatalf("%s DST default mismatch: manual=%+v CI=%+v want=%s", scenario.name, manual, ciResult, scenario.expected)
		}
	}
	setOperationTime(state.repository, state.wallNow)
}

func testAcquireInvalidExpiryRollback(t *testing.T, state acquisitionScenario) {
	invalidAt := state.wallNow
	request := acquireRequest(state.owner, "s12-invalid", "s12-invalid", claims.Scope{Group: "s12-invalid", App: "api"}, []claims.Environment{claims.Sandbox}, &invalidAt)
	_, err := state.repository.Acquire(state.ctx, state.owner, request)
	mustErrorCode(t, err, claims.Invalid)
	checks := []struct {
		name  string
		query string
		args  []any
	}{
		{"group", `SELECT COUNT(*) FROM app_groups WHERE canonical_name = ?`, []any{"s12-invalid"}},
		{"app", `SELECT COUNT(*) FROM apps a JOIN app_groups g ON g.id = a.group_id WHERE g.canonical_name = ? AND a.canonical_name = ?`, []any{"s12-invalid", "api"}},
		{"claim", `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, []any{"s12-invalid"}},
		{"version", `SELECT COUNT(*) FROM claim_versions v JOIN claims c ON c.id = v.claim_id JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, []any{"s12-invalid"}},
		{"request result", `SELECT COUNT(*) FROM request_results WHERE request_id = ?`, []any{"s12-invalid"}},
	}
	for _, check := range checks {
		if countRows(t, state.fixture.SQLDB, check.query, check.args...) != 0 {
			t.Fatalf("invalid expiry left a row in %s", check.name)
		}
	}
}

func testAcquireUnknownTarget(t *testing.T, state acquisitionScenario) {
	result := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s13-register", "s13-register", claims.Scope{Group: "s13-new", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	query := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s13-new", App: "api"}, Environments: []claims.Environment{claims.Sandbox}})
	if !result.Acquired || !query.Known || len(query.Claims) != 1 || query.Claims[0].ID != result.Claim.ID {
		t.Fatalf("successful acquisition did not register its target: result=%+v query=%+v", result, query)
	}
}

func testAcquireBusyRegistration(t *testing.T, state acquisitionScenario) {
	mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s14-group", "s14-group", claims.Scope{Group: "s14-busy"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	before := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, "s14-busy")
	busy := mustAcquire(state.ctx, t, state.repository, state.other, acquireRequest(state.other, "s14-app", "s14-app", claims.Scope{Group: "s14-busy", App: "new-worker"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if busy.Acquired || len(busy.Conflicts) != 1 {
		t.Fatalf("unknown app was not busy: %+v", busy)
	}
	after := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, "s14-busy")
	apps := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM apps a JOIN app_groups g ON g.id = a.group_id WHERE g.canonical_name = ? AND a.canonical_name = ?`, "s14-busy", "new-worker")
	if before != after || apps != 1 {
		t.Fatalf("busy operation did not commit only catalog state: claims=%d/%d apps=%d", before, after, apps)
	}
}

func testAcquireFutureProjection(t *testing.T, state acquisitionScenario) {
	future := state.wallNow.Add(2 * time.Hour)
	projection := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: claims.Scope{Group: "s19-projection", App: "api"}, Environments: []claims.Environment{claims.Sandbox}, At: fixedPointer(future)})
	if !projection.Projected || !projection.Free {
		t.Fatalf("free future projection was not marked as such: %+v", projection)
	}
	acquired := mustAcquire(state.ctx, t, state.repository, state.other, acquireRequest(state.other, "s19-now", "s19-now", claims.Scope{Group: "s19-projection", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if !acquired.Acquired {
		t.Fatalf("future projection reserved a claim: %+v", acquired)
	}
}

func testAcquireUnknownReads(t *testing.T, state acquisitionScenario) {
	mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s20-group", "s20-group", claims.Scope{Group: "s20-known"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	beforeGroups := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM app_groups`)
	beforeApps := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM apps`)
	appQuery := mustQuery(state.ctx, t, state.repository, state.other, claims.QueryRequest{Scope: claims.Scope{Group: "s20-known", App: "unregistered"}, Environments: []claims.Environment{claims.Sandbox}})
	unknownGroup := mustQuery(state.ctx, t, state.repository, state.other, claims.QueryRequest{Scope: claims.Scope{Group: "s20-missing"}, Environments: []claims.Environment{claims.Sandbox}})
	if appQuery.Known || len(appQuery.Claims) != 1 || !appQuery.Claims[0].Inherited || unknownGroup.Known || len(unknownGroup.Claims) != 0 {
		t.Fatalf("unknown-resource read contract failed: app=%+v group=%+v", appQuery, unknownGroup)
	}
	if countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM app_groups`) != beforeGroups || countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM apps`) != beforeApps {
		t.Fatal("unknown reads registered a catalog resource")
	}
}

func testAcquireReplay(t *testing.T, state acquisitionScenario) {
	actor := testActor("replay@example.test", "s21", claims.REST, "", "")
	request := acquireRequest(actor, "s21-request", "s21-payload", claims.Scope{Group: "s21-replay", App: "api"}, []claims.Environment{claims.Sandbox}, nil)
	first := mustAcquire(state.ctx, t, state.repository, actor, request)
	replay := mustAcquire(state.ctx, t, state.repository, actor, request)
	if !first.Acquired || !replay.Acquired || first.Claim.ID != replay.Claim.ID || !first.Claim.ExpiresAt.Equal(replay.Claim.ExpiresAt) {
		t.Fatalf("acquisition replay changed the stored result: first=%+v replay=%+v", first, replay)
	}
	if countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claims c JOIN app_groups g ON g.id = c.group_id WHERE g.canonical_name = ?`, "s21-replay") != 1 {
		t.Fatal("replay created a second claim")
	}
}

func testAcquirePayloadMismatch(t *testing.T, state acquisitionScenario) {
	actor := testActor("s22@example.test", "s22", claims.REST, "", "")
	first := mustAcquire(state.ctx, t, state.repository, actor, acquireRequest(actor, "s22-local", "s22-original", claims.Scope{Group: "s22-local"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	if !first.Acquired {
		t.Fatal("baseline request did not acquire")
	}
	_, err := state.repository.Acquire(state.ctx, actor, acquireRequest(actor, "s22-local", "s22-changed", claims.Scope{Group: "s22-local"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	mustErrorCode(t, err, claims.ConflictError)
	_, err = state.repository.Acquire(state.ctx, actor, acquireRequest(actor, "s22-local", "s22-other-group", claims.Scope{Group: "s22-mismatch"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)))
	mustErrorCode(t, err, claims.ConflictError)
	if countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM app_groups WHERE canonical_name = ?`, "s22-mismatch") != 0 {
		t.Fatal("changed-payload cross-group request committed provisional registration")
	}
}

func testAcquireCrossGroupRace(t *testing.T, state acquisitionScenario) {
	racer := testActor("cross-race@example.test", "s22-cross", claims.REST, "", "")
	firstRepo := New(state.fixture.NewClient(t))
	secondRepo := New(state.fixture.NewClient(t))
	setOperationTime(firstRepo, state.wallNow)
	setOperationTime(secondRepo, state.wallNow)
	// Hold each transaction after its replay lookup so both group locks race on the global request key.
	if _, err := state.fixture.SQLDB.Exec(`CREATE TRIGGER claimy_test_delay_request_result BEFORE INSERT ON request_results FOR EACH ROW SET @claimy_delay = SLEEP(5)`); err != nil {
		t.Fatalf("create request-result race barrier: %v", err)
	}
	defer func() {
		if _, err := state.fixture.SQLDB.Exec(`DROP TRIGGER claimy_test_delay_request_result`); err != nil {
			t.Errorf("drop request-result race barrier: %v", err)
		}
	}()
	actors := []claims.Actor{racer, racer}
	requests := []claims.AcquireRequest{
		acquireRequest(racer, "s22-global", "s22-group-one", claims.Scope{Group: "s22-race-one"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)),
		acquireRequest(racer, "s22-global", "s22-group-two", claims.Scope{Group: "s22-race-two"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt)),
	}
	outcomes := acquireInGoroutines(state.ctx, []*Repository{firstRepo, secondRepo}, actors, requests)
	winner := -1
	losers := 0
	for i, outcome := range outcomes {
		switch {
		case outcome.Result.Acquired:
			winner = i
		case outcome.Err != nil:
			mustErrorCode(t, outcome.Err, claims.ConflictError)
			losers++
		default:
			t.Fatalf("cross-group loser unexpectedly returned a business busy result: %+v", outcome)
		}
	}
	if winner < 0 || losers != 1 {
		t.Fatalf("global request race did not have one winner and one mismatch loser: %+v", outcomes)
	}
	winnerGroup := requests[winner].Scope.Group
	loserGroup := requests[1-winner].Scope.Group
	if countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM app_groups WHERE canonical_name IN (?, ?)`, requests[0].Scope.Group, requests[1].Scope.Group) != 1 ||
		countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM app_groups WHERE canonical_name = ?`, loserGroup) != 0 ||
		countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM request_results WHERE principal_id = ? AND request_id = ?`, racer.Subject, "s22-global") != 1 {
		t.Fatalf("cross-group unique-key loser did not roll back: winner=%s loser=%s", winnerGroup, loserGroup)
	}
}

func testAcquireInactiveReplay(t *testing.T, state acquisitionScenario) {
	ci := ciActor("ci@example.test", "s23-user", "s23-job")
	request := acquireRequest(ci, "s23-acquire", "s23-acquire", claims.Scope{Group: "s23-replay", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.expiresAt))
	first := mustAcquire(state.ctx, t, state.repository, ci, request)
	if !first.Acquired {
		t.Fatal("CI claim was not acquired")
	}
	mustRelease(state.ctx, t, state.repository, state.other, first.Claim.ID, "s23-release", "s23-release")
	replay := mustAcquire(state.ctx, t, state.repository, ci, request)
	if !replay.Acquired || replay.Claim.ID != first.Claim.ID || replay.Claim.ActiveNow {
		t.Fatalf("acquisition replay did not reflect current inactive state: %+v", replay)
	}
}

func TestRepositoryLifecycleScenarios(t *testing.T) {
	fixture, repository, wallNow := newTestRepository(t)
	ctx, cancel := testContext(t)
	defer cancel()
	state := lifecycleScenario{
		fixture:        fixture,
		repository:     repository,
		ctx:            ctx,
		wallNow:        wallNow,
		operationStart: wallNow.Add(-time.Hour),
		owner:          testActor("lifecycle-owner@example.test", "lifecycle-owner", claims.REST, "", ""),
		manager:        testActor("manager@example.test", "manager", claims.GoogleChat, "", ""),
		restManager:    testActor("rest-manager@example.test", "rest-manager", claims.REST, "", ""),
		ci:             ciActor("ci-owner@example.test", "ci-user", "lifecycle-job"),
	}
	setOperationTime(repository, state.operationStart)

	t.Run("S15 any member manages manual and CI claims with immutable ownership", func(t *testing.T) {
		testLifecycleOwnership(t, state)
	})
	t.Run("S16 expiry edits preserve half-open as-of history", func(t *testing.T) {
		testLifecycleExpiryHistory(t, state)
	})
	t.Run("S17 expired claim cannot be revived", func(t *testing.T) {
		testExpiredClaimCannotBeRevived(t, state)
	})
	t.Run("inactive release after terminal retention remains replayable until prune", func(t *testing.T) {
		testInactiveReleaseRetention(t, state)
	})
	t.Run("far-future expiry saturates replay retention at MySQL maximum", func(t *testing.T) {
		testFarFutureRetention(t, state)
	})
	t.Run("same-microsecond transitions are ordered by revision", func(t *testing.T) {
		testSameMicrosecondTransitions(t, state)
	})
	t.Run("no-op and inactive actions keep audit without changing history", func(t *testing.T) {
		testNoOpAndInactiveAudit(t, state)
	})
}

type lifecycleScenario struct {
	fixture        *support.Fixture
	repository     *Repository
	ctx            context.Context
	wallNow        time.Time
	operationStart time.Time
	owner          claims.Actor
	manager        claims.Actor
	restManager    claims.Actor
	ci             claims.Actor
}

func testLifecycleOwnership(t *testing.T, state lifecycleScenario) {
	firstExpiry := state.wallNow.Add(time.Hour)
	manual := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s15-manual", "s15-manual", claims.Scope{Group: "s15-manual", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(firstExpiry)))
	if !manual.Acquired {
		t.Fatal("manual claim was not acquired")
	}
	nextExpiry := state.wallNow.Add(3 * time.Hour)
	changed := mustChangeExpiry(state.ctx, t, state.repository, state.manager, manual.Claim.ID, "s15-manual-expiry", "s15-manual-expiry", nextExpiry, ptrRevision(1))
	if !changed.Changed || changed.Claim.OwnerEmail != state.owner.Email || changed.Claim.Source != claims.Manual || changed.Claim.Scope != manual.Claim.Scope {
		t.Fatalf("manager changed immutable claim fields: %+v", changed)
	}
	released := mustRelease(state.ctx, t, state.repository, state.manager, manual.Claim.ID, "s15-manual-release", "s15-manual-release")
	if !released.Changed || released.Claim.OwnerEmail != state.owner.Email || released.Claim.Source != claims.Manual || released.Claim.Scope != manual.Claim.Scope {
		t.Fatalf("manager could not release exactly the manual claim: %+v", released)
	}

	ciResult := mustAcquire(state.ctx, t, state.repository, state.ci, acquireRequest(state.ci, "s15-ci", "s15-ci", claims.Scope{Group: "s15-ci", App: "worker"}, []claims.Environment{claims.Sandbox}, fixedPointer(firstExpiry)))
	if !ciResult.Acquired || ciResult.Claim.Source != claims.CI {
		t.Fatalf("CI claim was not acquired: %+v", ciResult)
	}
	chatExpiry := state.wallNow.Add(4 * time.Hour)
	chatChanged := mustChangeExpiry(state.ctx, t, state.repository, state.manager, ciResult.Claim.ID, "s15-ci-expiry", "s15-ci-expiry", chatExpiry, ptrRevision(1))
	if !chatChanged.Changed || chatChanged.Claim.OwnerEmail != state.ci.Email || chatChanged.Claim.Source != claims.CI || chatChanged.Claim.GitLab == nil || chatChanged.Claim.GitLab.JobID != state.ci.GitLab.JobID {
		t.Fatalf("Chat manager changed immutable CI identity: %+v", chatChanged)
	}
	ciReleased := mustRelease(state.ctx, t, state.repository, state.restManager, ciResult.Claim.ID, "s15-ci-release", "s15-ci-release")
	if !ciReleased.Changed || ciReleased.Claim.OwnerEmail != state.ci.Email || ciReleased.Claim.Source != claims.CI {
		t.Fatalf("REST member could not release the CI claim without changing ownership: %+v", ciReleased)
	}
	assertLifecycleActors(t, state, manual.Claim.ID, ciResult.Claim.ID)
}

func assertLifecycleActors(t *testing.T, state lifecycleScenario, manualID, ciID string) {
	var ciVersions []struct {
		Revision   uint32 `db:"revision"`
		ActorEmail string `db:"actor_email"`
		Channel    string `db:"channel"`
		Action     string `db:"action"`
	}
	if err := state.fixture.Client.Select(state.ctx, &ciVersions, `SELECT revision, actor_email, channel, action FROM claim_versions WHERE claim_id = ? ORDER BY revision`, ciID); err != nil {
		t.Fatalf("read CI claim audit history: %v", err)
	}
	if len(ciVersions) != 3 || ciVersions[1].ActorEmail != state.manager.Email || ciVersions[1].Channel != string(state.manager.Channel) || ciVersions[2].ActorEmail != state.restManager.Email || ciVersions[2].Channel != string(state.restManager.Channel) || ciVersions[2].Action != "released" {
		t.Fatalf("CI history did not retain REST and Chat actors: %+v", ciVersions)
	}

	var versions []struct {
		Revision     uint32 `db:"revision"`
		ActorEmail   string `db:"actor_email"`
		ActorIssuer  string `db:"actor_issuer"`
		ActorSubject string `db:"actor_subject"`
		Channel      string `db:"channel"`
		Action       string `db:"action"`
	}
	if err := state.fixture.Client.Select(state.ctx, &versions, `SELECT revision, actor_email, actor_issuer, actor_subject, channel, action FROM claim_versions WHERE claim_id = ? ORDER BY revision`, manualID); err != nil {
		t.Fatalf("read manual claim audit history: %v", err)
	}
	if len(versions) != 3 || versions[0].Action != "acquired" || versions[1].ActorEmail != state.manager.Email || versions[1].Channel != string(state.manager.Channel) || versions[2].Action != "released" || versions[2].ActorSubject != state.manager.Subject {
		t.Fatalf("manual history did not retain each authorized actor: %+v", versions)
	}
}

func testLifecycleExpiryHistory(t *testing.T, state lifecycleScenario) {
	setOperationTime(state.repository, state.operationStart)
	oldExpiry := state.wallNow.Add(5 * time.Hour)
	acquired := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s16-acquire", "s16-acquire", claims.Scope{Group: "s16-history", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(oldExpiry)))
	changeAt := state.operationStart.Add(10 * time.Minute)
	setOperationTime(state.repository, changeAt)
	newExpiry := state.wallNow.Add(8 * time.Hour)
	updated := mustChangeExpiry(state.ctx, t, state.repository, state.manager, acquired.Claim.ID, "s16-change", "s16-change", newExpiry, ptrRevision(1))
	if !updated.Changed || updated.Claim.Revision != 2 {
		t.Fatalf("expiry edit did not append revision 2: %+v", updated)
	}
	before := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: acquired.Claim.Scope, Environments: acquired.Claim.Environments, At: fixedPointer(changeAt.Add(-time.Microsecond))})
	after := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: acquired.Claim.Scope, Environments: acquired.Claim.Environments, At: fixedPointer(changeAt)})
	if len(before.Claims) != 1 || before.Claims[0].Revision != 1 || !before.Claims[0].ExpiresAt.Equal(oldExpiry) || len(after.Claims) != 1 || after.Claims[0].Revision != 2 || !after.Claims[0].ExpiresAt.Equal(newExpiry) {
		t.Fatalf("expiry history was rewritten instead of versioned: before=%+v after=%+v", before, after)
	}
	setOperationTime(state.repository, state.wallNow)
}

func testExpiredClaimCannotBeRevived(t *testing.T, state lifecycleScenario) {
	acquireAt := state.wallNow.Add(-3 * time.Hour)
	expiry := state.wallNow.Add(-2 * time.Hour)
	setOperationTime(state.repository, acquireAt)
	claim := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s17-expired", "s17-expired", claims.Scope{Group: "s17-revive", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(expiry)))
	setOperationTime(state.repository, state.wallNow.Add(-time.Hour))
	_, err := state.repository.ChangeExpiry(state.ctx, state.owner, claims.ExpiryRequest{ClaimID: claim.Claim.ID, ExpiresAt: state.wallNow.Add(time.Hour), ExpectedRevision: ptrRevision(1), RequestID: "s17-change", MutationMeta: testMeta(state.owner, "s17-change")})
	mustErrorCode(t, err, claims.ConflictError)
	newClaim := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "s17-new", "s17-new", claims.Scope{Group: "s17-revive", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(state.wallNow.Add(time.Hour))))
	if !newClaim.Acquired || newClaim.Claim.ID == claim.Claim.ID {
		t.Fatalf("expired claim was revived or reused instead of a new claim: old=%+v new=%+v", claim, newClaim)
	}
	setOperationTime(state.repository, state.wallNow)
}

func testInactiveReleaseRetention(t *testing.T, state lifecycleScenario) {
	terminal := state.wallNow.Add(-claims.Retention - 2*time.Hour)
	setOperationTime(state.repository, terminal.Add(-time.Hour))
	old := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "old-inactive-release", "old-inactive-release", claims.Scope{Group: "old-inactive-release"}, []claims.Environment{claims.Sandbox}, fixedPointer(terminal)))
	setOperationTime(state.repository, state.wallNow)
	released := mustRelease(state.ctx, t, state.repository, state.manager, old.Claim.ID, "old-inactive-release-release", "old-inactive-release-release")
	if released.Changed || released.Claim.ActiveNow {
		t.Fatalf("old inactive release unexpectedly changed claim: %+v", released)
	}
	var versionCount int
	if err := state.fixture.Client.Get(state.ctx, &versionCount, `SELECT COUNT(*) FROM claim_versions WHERE claim_id = ?`, old.Claim.ID); err != nil {
		t.Fatalf("count old inactive history: %v", err)
	}
	if versionCount != 1 {
		t.Fatalf("inactive release appended history: %d versions", versionCount)
	}
	setOperationTime(state.repository, terminal.Add(claims.Retention).Add(time.Microsecond))
	if _, err := state.repository.Prune(state.ctx); err != nil {
		t.Fatalf("prune old inactive release: %v", err)
	}
	if countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claims WHERE id = ?`, old.Claim.ID) != 0 ||
		countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM request_results WHERE claim_id = ?`, old.Claim.ID) != 0 {
		t.Fatal("terminal claim or its expired replay results remained after terminal retention")
	}
}

func testFarFutureRetention(t *testing.T, state lifecycleScenario) {
	setOperationTime(state.repository, state.wallNow)
	farExpiry := time.Date(9999, 12, 31, 12, 0, 0, 0, time.UTC)
	request := acquireRequest(state.owner, "far-future", "far-future", claims.Scope{Group: "far-future"}, []claims.Environment{claims.Sandbox}, fixedPointer(farExpiry))
	acquired := mustAcquire(state.ctx, t, state.repository, state.owner, request)
	replayed := mustAcquire(state.ctx, t, state.repository, state.owner, request)
	if replayed.Claim == nil || replayed.Claim.ID != acquired.Claim.ID {
		t.Fatalf("far-future acquisition replay created a different claim: first=%+v replay=%+v", acquired, replayed)
	}
	var retained string
	if err := state.fixture.Client.Get(state.ctx, &retained, `SELECT DATE_FORMAT(retain_until, '%Y-%m-%d %H:%i:%s.%f') FROM request_results WHERE request_id = ?`, "far-future"); err != nil {
		t.Fatalf("read far-future retention: %v", err)
	}
	assertMaxRetention(t, retained, "far-future retention was not saturated")
	editedExpiry := time.Date(9999, 12, 31, 18, 0, 0, 0, time.UTC)
	edited := mustChangeExpiry(state.ctx, t, state.repository, state.manager, acquired.Claim.ID, "far-future-edit", "far-future-edit", editedExpiry, ptrRevision(1))
	if !edited.Changed || !edited.Claim.ExpiresAt.Equal(editedExpiry) {
		t.Fatalf("far-future expiry edit failed: %+v", edited)
	}
	if err := state.fixture.Client.Get(state.ctx, &retained, `SELECT DATE_FORMAT(retain_until, '%Y-%m-%d %H:%i:%s.%f') FROM request_results WHERE request_id = ?`, "far-future-edit"); err != nil {
		t.Fatalf("read far-future edit retention: %v", err)
	}
	assertMaxRetention(t, retained, "far-future edit retention was not saturated")
}

func assertMaxRetention(t *testing.T, actual, message string) {
	t.Helper()
	if actual != maxMySQLDateTime {
		t.Fatalf("%s: %s", message, actual)
	}
}

func testSameMicrosecondTransitions(t *testing.T, state lifecycleScenario) {
	transitionAt := state.wallNow.Add(-30 * time.Minute)
	setOperationTime(state.repository, transitionAt)
	firstExpiry := state.wallNow.Add(2 * time.Hour)
	acquired := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "micro-acquire", "micro-acquire", claims.Scope{Group: "micro-history", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(firstExpiry)))
	secondExpiry := state.wallNow.Add(3 * time.Hour)
	updated := mustChangeExpiry(state.ctx, t, state.repository, state.manager, acquired.Claim.ID, "micro-change", "micro-change", secondExpiry, ptrRevision(1))
	released := mustRelease(state.ctx, t, state.repository, state.manager, acquired.Claim.ID, "micro-release", "micro-release")
	if !updated.Changed || !released.Changed {
		t.Fatalf("same-microsecond transitions were not applied: change=%+v release=%+v", updated, released)
	}
	var versions []struct {
		Revision  uint32         `db:"revision"`
		ValidFrom string         `db:"valid_from"`
		ValidTo   sql.NullString `db:"valid_to"`
		Released  bool           `db:"released"`
	}
	if err := state.fixture.Client.Select(state.ctx, &versions, `SELECT revision, DATE_FORMAT(valid_from, '%Y-%m-%d %H:%i:%s.%f') AS valid_from,
		CASE WHEN valid_to IS NULL THEN NULL ELSE DATE_FORMAT(valid_to, '%Y-%m-%d %H:%i:%s.%f') END AS valid_to,
		released FROM claim_versions WHERE claim_id = ? ORDER BY revision`, acquired.Claim.ID); err != nil {
		t.Fatalf("read same-microsecond versions: %v", err)
	}
	if len(versions) != 3 || !versions[0].ValidTo.Valid || !versions[1].ValidTo.Valid || versions[0].ValidFrom != versions[0].ValidTo.String || versions[1].ValidFrom != versions[1].ValidTo.String || !versions[2].Released {
		t.Fatalf("same-microsecond history intervals were not ordered by revision: %+v", versions)
	}
	atBoundary := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: acquired.Claim.Scope, Environments: acquired.Claim.Environments, At: fixedPointer(transitionAt)})
	beforeCreation := mustQuery(state.ctx, t, state.repository, state.owner, claims.QueryRequest{Scope: acquired.Claim.Scope, Environments: acquired.Claim.Environments, At: fixedPointer(transitionAt.Add(-time.Microsecond))})
	if len(atBoundary.Claims) != 0 || len(beforeCreation.Claims) != 0 {
		t.Fatalf("zero-length versions were selected at a shared timestamp: at=%+v before=%+v", atBoundary, beforeCreation)
	}
	setOperationTime(state.repository, state.wallNow)
}

func testNoOpAndInactiveAudit(t *testing.T, state lifecycleScenario) {
	setOperationTime(state.repository, state.wallNow)
	claimExpiry := state.wallNow.Add(6 * time.Hour)
	acquired := mustAcquire(state.ctx, t, state.repository, state.owner, acquireRequest(state.owner, "audit-acquire", "audit-acquire", claims.Scope{Group: "audit-noop", App: "api"}, []claims.Environment{claims.Sandbox}, fixedPointer(claimExpiry)))
	noop := mustChangeExpiry(state.ctx, t, state.repository, state.manager, acquired.Claim.ID, "audit-noop-edit", "audit-noop-edit", claimExpiry, ptrRevision(1))
	if noop.Changed || noop.Claim.Revision != 1 {
		t.Fatalf("same-value edit appended a revision: %+v", noop)
	}
	beforeRelease := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claim_versions WHERE claim_id = ?`, acquired.Claim.ID)
	mustRelease(state.ctx, t, state.repository, state.manager, acquired.Claim.ID, "audit-release", "audit-release")
	inactive := mustRelease(state.ctx, t, state.repository, state.owner, acquired.Claim.ID, "audit-inactive-release", "audit-inactive-release")
	if inactive.Changed {
		t.Fatalf("already inactive release changed claim history: %+v", inactive)
	}
	afterRelease := countRows(t, state.fixture.SQLDB, `SELECT COUNT(*) FROM claim_versions WHERE claim_id = ?`, acquired.Claim.ID)
	if beforeRelease != 1 || afterRelease != 2 {
		t.Fatalf("no-op/inactive operations changed history count unexpectedly: before=%d after=%d", beforeRelease, afterRelease)
	}
	assertSavedActor(t, state.fixture.SQLDB, state.manager, "audit-noop-edit", true)
	assertSavedActor(t, state.fixture.SQLDB, state.owner, "audit-inactive-release", false)
	setOperationTime(state.repository, state.wallNow)
}

func assertSavedActor(t *testing.T, db *sql.DB, actor claims.Actor, requestID string, checkIssuer bool) {
	t.Helper()
	var response []byte
	if err := db.QueryRow(`SELECT response_json FROM request_results WHERE principal_id = ? AND request_id = ?`, actor.Subject, requestID).Scan(&response); err != nil {
		t.Fatalf("read request audit metadata: %v", err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(response, &saved); err != nil {
		t.Fatalf("decode request result: %v", err)
	}
	var audit auditMetadata
	if err := json.Unmarshal(saved["_audit"], &audit); err != nil {
		t.Fatalf("decode request actor metadata: %v", err)
	}
	if audit.ActorEmail != actor.Email || (checkIssuer && audit.ActorIssuer != actor.Issuer) || audit.ActorSubject != actor.Subject || audit.Channel != actor.Channel {
		t.Fatalf("request did not retain acting user and channel: %+v", audit)
	}
}

func ptrRevision(value uint32) *uint32 { return &value }
