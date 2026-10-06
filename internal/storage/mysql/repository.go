package mysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/beeemT/claimy/internal/claims"
	driver "github.com/go-sql-driver/mysql"
	"github.com/gosoline-project/sqlc"
)

const (
	maxTransactionAttempts = 3
	retryDelay             = 10 * time.Millisecond
	datetimeLayout         = "2006-01-02 15:04:05.000000"
)

// Repository stores claim state, history and idempotent outcomes in MySQL.
type Repository struct {
	client        sqlc.Client
	operationTime func(context.Context, *sql.Tx) (time.Time, error)
}

type commitFailure struct {
	err error
}

func (e *commitFailure) Error() string {
	return fmt.Sprintf("commit transaction: %v", e.err)
}

func (e *commitFailure) Unwrap() error {
	return e.err
}

var _ claims.Store = (*Repository)(nil)

// New constructs a repository over the configured SQLC client.
func New(client sqlc.Client) *Repository {
	return &Repository{client: client, operationTime: readDatabaseTime}
}

type resultKey struct {
	kind      string
	issuer    string
	principal string
	requestID string
}

type acquirePlan struct {
	key          resultKey
	environments []claims.Environment
	source       claims.Source
	gitlab       *claims.GitLabIdentity
}

type savedResult struct {
	PayloadHash []byte         `db:"payload_sha256"`
	Outcome     string         `db:"outcome"`
	ClaimID     sql.NullString `db:"claim_id"`
	Response    []byte         `db:"response_json"`
}

type resultEnvelope struct {
	Result json.RawMessage `json:"result"`
	Audit  auditMetadata   `json:"_audit"`
}

type auditMetadata struct {
	ActorEmail   string         `json:"actorEmail"`
	ActorIssuer  string         `json:"actorIssuer"`
	ActorSubject string         `json:"actorSubject"`
	Channel      claims.Channel `json:"channel"`
}

func newEnvelope(actor claims.Actor, result any) ([]byte, error) {
	core, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	return json.Marshal(resultEnvelope{
		Result: core,
		Audit: auditMetadata{
			ActorEmail:   actor.Email,
			ActorIssuer:  actor.Issuer,
			ActorSubject: actor.Subject,
			Channel:      actor.Channel,
		},
	})
}

func decodeEnvelope(response []byte, out any) error {
	var envelope resultEnvelope
	if err := json.Unmarshal(response, &envelope); err != nil {
		return fmt.Errorf("decode stored request result: %w", err)
	}
	if len(envelope.Result) == 0 {
		return errors.New("stored request result has no result payload")
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("decode stored request result payload: %w", err)
	}

	return nil
}

func requestKey(actor claims.Actor, principal claims.Principal, requestID string) (resultKey, error) {
	if requestID == "" || principal.Kind == "" || principal.Issuer == "" || principal.ID == "" {
		return resultKey{}, claims.NewError(claims.Invalid, "mutation request identity is incomplete")
	}
	if actor.Email == "" || actor.Issuer == "" || actor.Subject == "" {
		return resultKey{}, claims.NewError(claims.Invalid, "actor identity is incomplete")
	}
	if _, _, err := sourceForActor(actor); err != nil {
		return resultKey{}, err
	}
	if err := validatePrincipalKind(principal.Kind); err != nil {
		return resultKey{}, err
	}

	return resultKey{kind: principal.Kind, issuer: principal.Issuer, principal: principal.ID, requestID: requestID}, nil
}

func validatePrincipalKind(kind string) error {
	switch kind {
	case "gitlab_job", "rest_user", "google_chat_user":
		return nil
	default:
		return claims.NewError(claims.Invalid, "mutation principal kind is invalid")
	}
}

func sourceForActor(actor claims.Actor) (claims.Source, *claims.GitLabIdentity, error) {
	switch actor.Channel {
	case claims.GitLabCI:
		if actor.GitLab == nil || actor.GitLab.Issuer == "" || actor.GitLab.ProjectID == "" || actor.GitLab.JobID == "" || actor.GitLab.UserID == "" {
			return "", nil, claims.NewError(claims.Invalid, "GitLab actor identity is incomplete")
		}

		return claims.CI, actor.GitLab, nil
	case claims.REST, claims.GoogleChat:
		if actor.GitLab != nil {
			return "", nil, claims.NewError(claims.Invalid, "manual actor cannot contain GitLab identity")
		}

		return claims.Manual, nil, nil
	default:
		return "", nil, claims.NewError(claims.Invalid, "actor channel is invalid")
	}
}

func (r *Repository) lookupResult(ctx context.Context, key resultKey) (savedResult, bool, error) {
	var result savedResult
	err := r.client.Get(ctx, &result, `SELECT payload_sha256, outcome, claim_id, response_json
		FROM request_results
		WHERE principal_kind = ? AND principal_issuer = ? AND principal_id = ? AND request_id = ?`,
		key.kind, key.issuer, key.principal, key.requestID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return savedResult{}, false, nil
	}
	if err != nil {
		return savedResult{}, false, storageError(err)
	}

	return result, true, nil
}

func verifyReplay(result savedResult, payloadHash [32]byte) error {
	if len(result.PayloadHash) != len(payloadHash) || !equalBytes(result.PayloadHash, payloadHash[:]) {
		return claims.NewError(claims.ConflictError, "request ID was already used with a different payload")
	}

	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}

	return diff == 0
}

func insertResult(ctx context.Context, tx *sql.Tx, key resultKey, payloadHash [32]byte, outcome string, claimID any, response []byte, createdAt, retainUntil time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO request_results
		(principal_kind, principal_issuer, principal_id, request_id, payload_sha256, outcome, claim_id, response_json, created_at, retain_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.kind, key.issuer, key.principal, key.requestID, payloadHash[:], outcome, claimID, response,
		datetimeArg(createdAt), datetimeArg(retainUntil),
	)

	return err
}

func saveResult(ctx context.Context, tx *sql.Tx, actor claims.Actor, key resultKey, payloadHash [32]byte, outcome string, claimID any, result any, createdAt, retainUntil time.Time) error {
	response, err := newEnvelope(actor, result)
	if err != nil {
		return err
	}

	return insertResult(ctx, tx, key, payloadHash, outcome, claimID, response, createdAt, retainUntil)
}

// Acquire records a claim or a busy result for the request.
func (r *Repository) Acquire(ctx context.Context, actor claims.Actor, request claims.AcquireRequest) (claims.AcquireResult, error) {
	var empty claims.AcquireResult
	plan, err := prepareAcquirePlan(actor, request)
	if err != nil {
		return empty, err
	}
	replay, found, err := r.lookupAcquireReplay(ctx, plan.key, request.PayloadHash)
	if err != nil {
		return empty, err
	}
	if found {
		return r.refreshAcquireReplay(ctx, replay)
	}

	return r.acquireNewRequest(ctx, actor, request, plan)
}

func prepareAcquirePlan(actor claims.Actor, request claims.AcquireRequest) (acquirePlan, error) {
	key, err := requestKey(actor, request.Principal, request.RequestID)
	if err != nil {
		return acquirePlan{}, err
	}
	if request.Scope.Group == "" || len(request.Environments) == 0 || len(request.Environments) > 2 {
		return acquirePlan{}, claims.NewError(claims.Invalid, "acquisition target or environments are invalid")
	}
	environments, err := canonicalEnvironments(request.Environments)
	if err != nil {
		return acquirePlan{}, err
	}
	source, gitlab, err := sourceForActor(actor)
	if err != nil {
		return acquirePlan{}, err
	}

	return acquirePlan{key: key, environments: environments, source: source, gitlab: gitlab}, nil
}

func (r *Repository) refreshAcquireReplay(ctx context.Context, result claims.AcquireResult) (claims.AcquireResult, error) {
	if !result.Acquired || result.Claim == nil {
		return result, nil
	}
	if err := r.refreshAcquireActiveNow(ctx, result.Claim); err != nil {
		return claims.AcquireResult{}, err
	}

	return result, nil
}

func (r *Repository) acquireNewRequest(ctx context.Context, actor claims.Actor, request claims.AcquireRequest, plan acquirePlan) (claims.AcquireResult, error) {
	var empty claims.AcquireResult
	var result claims.AcquireResult
	replayed := false
	err := r.withTransaction(ctx, false, func(tx *sql.Tx) error {
		return r.acquireInTransaction(ctx, tx, actor, request, plan, &result, &replayed)
	})
	if err != nil {
		if isDuplicateKey(err) {
			return r.replayAcquireAfterRace(ctx, plan.key, request.PayloadHash)
		}

		return empty, operationError(err)
	}
	if replayed {
		return r.refreshAcquireReplay(ctx, result)
	}

	return result, nil
}

func (r *Repository) acquireInTransaction(ctx context.Context, tx *sql.Tx, actor claims.Actor, request claims.AcquireRequest, plan acquirePlan, result *claims.AcquireResult, replayed *bool) error {
	groupID, opTime, err := r.lockGroupAndTime(ctx, tx, request.Scope.Group)
	if err != nil {
		return err
	}
	existing, found, err := lookupResultTx(ctx, tx, plan.key)
	if err != nil {
		return err
	}
	if found {
		if err = verifyReplay(existing, request.PayloadHash); err != nil {
			return err
		}
		*replayed = true

		return decodeEnvelope(existing.Response, result)
	}
	appID, expiresAt, err := prepareAcquisition(ctx, tx, request, groupID, opTime)
	if err != nil {
		return err
	}
	conflicts, err := findConflicts(ctx, tx, groupID, appID, plan.environments, actor.Email, opTime)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		*result = claims.AcquireResult{Acquired: false, Conflicts: conflicts}

		return saveResult(ctx, tx, actor, plan.key, request.PayloadHash, "busy", nil, *result, opTime, resultRetention(opTime))
	}

	return persistAcquisition(ctx, tx, actor, request, plan, groupID, appID, expiresAt, opTime, result)
}

func prepareAcquisition(ctx context.Context, tx *sql.Tx, request claims.AcquireRequest, groupID uint64, opTime time.Time) (*uint64, time.Time, error) {
	var appID *uint64
	if request.Scope.App != "" {
		id, err := registerApp(ctx, tx, groupID, request.Scope.App, opTime)
		if err != nil {
			return nil, time.Time{}, err
		}
		appID = &id
	}
	expiresAt := time.Time{}
	var err error
	if request.ExpiresAt == nil {
		expiresAt, err = claims.DefaultExpiry(opTime)
		if err != nil {
			return nil, time.Time{}, storageError(err)
		}
	} else {
		expiresAt = request.ExpiresAt.UTC().Truncate(time.Microsecond)
	}
	if !expiresAt.After(opTime) {
		return nil, time.Time{}, claims.NewError(claims.Invalid, "expiry must be later than database time")
	}

	return appID, expiresAt, nil
}

func persistAcquisition(
	ctx context.Context,
	tx *sql.Tx,
	actor claims.Actor,
	request claims.AcquireRequest,
	plan acquirePlan,
	groupID uint64,
	appID *uint64,
	expiresAt, opTime time.Time,
	result *claims.AcquireResult,
) error {
	claimID, err := newClaimID()
	if err != nil {
		return err
	}
	var gitlabIssuer, gitlabProject, gitlabJob, gitlabUser any
	if plan.gitlab != nil {
		gitlabIssuer, gitlabProject, gitlabJob, gitlabUser = plan.gitlab.Issuer, plan.gitlab.ProjectID, plan.gitlab.JobID, plan.gitlab.UserID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO claims
		(id, group_id, app_id, owner_email, source, gitlab_issuer, gitlab_project_id, gitlab_job_id, gitlab_user_id, created_at, expires_at, released_at, revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 1)`,
		claimID, groupID, nullableUint64(appID), actor.Email, plan.source, gitlabIssuer, gitlabProject, gitlabJob, gitlabUser,
		datetimeArg(opTime), datetimeArg(expiresAt),
	)
	if err != nil {
		return err
	}
	if err = insertClaimEnvironments(ctx, tx, claimID, plan.environments); err != nil {
		return err
	}
	if err = insertVersion(ctx, tx, claimID, 1, opTime, nil, expiresAt, false, actor, "acquired"); err != nil {
		return err
	}
	claim := claims.Claim{
		ID:           claimID,
		Scope:        request.Scope,
		Environments: plan.environments,
		OwnerEmail:   actor.Email,
		Source:       plan.source,
		GitLab:       plan.gitlab,
		CreatedAt:    opTime,
		ExpiresAt:    expiresAt,
		Revision:     1,
		ActiveNow:    true,
	}
	*result = claims.AcquireResult{Acquired: true, Claim: &claim}

	return saveResult(ctx, tx, actor, plan.key, request.PayloadHash, "acquired", claimID, *result, opTime, resultRetention(expiresAt))
}

func insertClaimEnvironments(ctx context.Context, tx *sql.Tx, claimID string, environments []claims.Environment) error {
	for _, environment := range environments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO claim_environments (claim_id, environment) VALUES (?, ?)`, claimID, environment); err != nil {
			return err
		}
	}

	return nil
}

func (r *Repository) lookupAcquireReplay(ctx context.Context, key resultKey, payloadHash [32]byte) (claims.AcquireResult, bool, error) {
	var empty claims.AcquireResult
	existing, found, err := r.lookupResult(ctx, key)
	if err != nil || !found {
		return empty, found, err
	}
	if err = verifyReplay(existing, payloadHash); err != nil {
		return empty, true, err
	}
	var result claims.AcquireResult
	if err = decodeEnvelope(existing.Response, &result); err != nil {
		return empty, true, storageError(err)
	}

	return result, true, nil
}

func (r *Repository) replayAcquireAfterRace(ctx context.Context, key resultKey, payloadHash [32]byte) (claims.AcquireResult, error) {
	var empty claims.AcquireResult
	result, found, err := r.lookupAcquireReplay(ctx, key, payloadHash)
	if err != nil {
		return empty, err
	}
	if !found {
		return empty, storageError(errors.New("request-result unique-key conflict has no committed winner"))
	}
	if result.Acquired && result.Claim != nil {
		if err = r.refreshAcquireActiveNow(ctx, result.Claim); err != nil {
			return empty, err
		}
	}

	return result, nil
}

func (r *Repository) refreshAcquireActiveNow(ctx context.Context, claim *claims.Claim) (err error) {
	tx, raw, err := r.beginRead(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := rollbackRead(tx); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, storageError(fmt.Errorf("rollback acquisition replay read: %w", rollbackErr)))
		}
	}()
	at, err := databaseSnapshotTime(ctx, raw)
	if err != nil {
		return storageError(err)
	}
	var released sql.NullString
	var expires string
	err = raw.QueryRowContext(ctx, `SELECT DATE_FORMAT(expires_at, '%Y-%m-%d %H:%i:%s.%f'),
		CASE WHEN released_at IS NULL THEN NULL ELSE DATE_FORMAT(released_at, '%Y-%m-%d %H:%i:%s.%f') END
		FROM claims WHERE id = ?`, claim.ID).Scan(&expires, &released)
	if err != nil {
		return storageError(err)
	}
	expiresAt, err := parseDatabaseTime(expires)
	if err != nil {
		return storageError(err)
	}
	claim.ActiveNow = !released.Valid && expiresAt.After(at)
	if err = tx.Commit(); err != nil {
		return storageError(fmt.Errorf("commit acquisition replay read: %w", err))
	}

	return nil
}

func (r *Repository) beginRead(ctx context.Context) (sqlc.Tx, *sql.Tx, error) {
	options := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}

	return startTransaction(ctx, r.client, options)
}

func startTransaction(ctx context.Context, client sqlc.Client, options *sql.TxOptions) (sqlc.Tx, *sql.Tx, error) {
	tx, err := client.BeginTx(ctx, options)
	if err != nil {
		return nil, nil, storageError(err)
	}
	raw := tx.SQLTx()
	if raw == nil {
		cause := errors.New("SQL client did not provide a database/sql transaction")
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			cause = errors.Join(cause, fmt.Errorf("rollback transaction: %w", rollbackErr))
		}

		return nil, nil, storageError(cause)
	}

	return tx, raw, nil
}

func rollbackRead(tx sqlc.Tx) error {
	return tx.Rollback()
}

func (r *Repository) withTransaction(ctx context.Context, readOnly bool, fn func(*sql.Tx) error) error {
	options := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: readOnly}
	for attempt := 0; attempt < maxTransactionAttempts; attempt++ {
		tx, raw, err := startTransaction(ctx, r.client, options)
		if err != nil {
			return err
		}
		retryable, err := runTransactionAttempt(tx, raw, fn)
		if err == nil {
			return nil
		}
		if !retryable || attempt+1 == maxTransactionAttempts {
			return err
		}
		if err = waitForRetry(ctx, attempt); err != nil {
			return storageError(err)
		}
	}

	return storageError(errors.New("transaction retry limit reached"))
}

func runTransactionAttempt(tx sqlc.Tx, raw *sql.Tx, fn func(*sql.Tx) error) (bool, error) {
	err := fn(raw)
	if err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return false, storageError(fmt.Errorf("rollback transaction after %v: %w", err, rollbackErr))
		}

		return retryableTransactionError(err), err
	}
	if err = tx.Commit(); err != nil {
		// A failed COMMIT acknowledgement is ambiguous and must never be retried or treated as a request-key race.
		return false, storageError(&commitFailure{err: err})
	}

	return false, nil
}

func retryableTransactionError(err error) bool {
	var commitErr *commitFailure
	if errors.As(err, &commitErr) {
		return false
	}
	var mysqlErr *driver.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}

	return mysqlErr.Number == 1205 || mysqlErr.Number == 1213
}

func isDuplicateKey(err error) bool {
	var commitErr *commitFailure
	if errors.As(err, &commitErr) {
		return false
	}
	var mysqlErr *driver.MySQLError

	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

func waitForRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(retryDelay * time.Duration(attempt+1))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func operationError(err error) error {
	var typed *claims.Error
	if errors.As(err, &typed) {
		return err
	}

	return storageError(err)
}

func storageError(err error) error {
	return &claims.Error{Code: claims.StorageError, Message: "claim storage operation failed", Cause: err}
}

func (r *Repository) lockGroupAndTime(ctx context.Context, tx *sql.Tx, group string) (uint64, time.Time, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_groups (canonical_name, created_at)
		VALUES (?, UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`, group); err != nil {
		return 0, time.Time{}, err
	}
	var groupID uint64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM app_groups WHERE canonical_name = ? FOR UPDATE`, group).Scan(&groupID); err != nil {
		return 0, time.Time{}, err
	}
	opTime, err := r.operationTime(ctx, tx)
	if err != nil {
		return 0, time.Time{}, err
	}

	return groupID, opTime, nil
}

func readDatabaseTime(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var value string
	err := tx.QueryRowContext(ctx, `SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f')`).Scan(&value)
	if err != nil {
		return time.Time{}, err
	}

	return parseDatabaseTime(value)
}

// Reading the catalog alongside UTC time establishes the read view used by the query transaction.
func databaseSnapshotTime(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var value string
	var catalogRead int
	err := tx.QueryRowContext(ctx, `SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f'),
		EXISTS(SELECT 1 FROM app_groups LIMIT 1)`).Scan(&value, &catalogRead)
	if err != nil {
		return time.Time{}, err
	}

	return parseDatabaseTime(value)
}

func registerApp(ctx context.Context, tx *sql.Tx, groupID uint64, name string, createdAt time.Time) (uint64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO apps (group_id, canonical_name, created_at)
		VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`, groupID, name, datetimeArg(createdAt)); err != nil {
		return 0, err
	}
	var id uint64
	err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE group_id = ? AND canonical_name = ?`, groupID, name).Scan(&id)

	return id, err
}

func findConflicts(ctx context.Context, tx *sql.Tx, groupID uint64, appID *uint64, environments []claims.Environment, owner string, at time.Time) (result []claims.Conflict, err error) {
	envPlaceholders, envArgs := environmentParameters(environments)
	query := `SELECT c.id, g.canonical_name, COALESCE(a.canonical_name, ''), c.owner_email, c.source,
		DATE_FORMAT(c.expires_at, '%Y-%m-%d %H:%i:%s.%f'), ce.environment
		FROM claims c
		JOIN app_groups g ON g.id = c.group_id
		LEFT JOIN apps a ON a.id = c.app_id AND a.group_id = c.group_id
		JOIN claim_environments ce ON ce.claim_id = c.id
		WHERE c.group_id = ? AND c.released_at IS NULL AND c.expires_at > ?
		AND ce.environment IN (` + envPlaceholders + `) AND c.owner_email <> ?`
	args := []any{groupID, datetimeArg(at)}
	args = append(args, envArgs...)
	args = append(args, owner)
	if appID != nil {
		query += ` AND (c.app_id IS NULL OR c.app_id = ?)`
		args = append(args, *appID)
	}
	query += ` ORDER BY c.id, FIELD(ce.environment, 'sandbox', 'prod') FOR UPDATE`

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	byID := make(map[string]*claims.Conflict)
	for rows.Next() {
		var id, group, app, email, source, expiry, environment string
		if err = rows.Scan(&id, &group, &app, &email, &source, &expiry, &environment); err != nil {
			return nil, err
		}
		conflict := byID[id]
		if conflict == nil {
			expiresAt, parseErr := parseDatabaseTime(expiry)
			if parseErr != nil {
				return nil, parseErr
			}
			conflict = &claims.Conflict{ID: id, Scope: claims.Scope{Group: group, App: app}, OwnerEmail: email, Source: claims.Source(source), ExpiresAt: expiresAt}
			byID[id] = conflict
		}
		conflict.Environments = append(conflict.Environments, claims.Environment(environment))
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	result = make([]claims.Conflict, 0, len(byID))
	for _, conflict := range byID {
		result = append(result, *conflict)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })

	return result, nil
}

func insertVersion(ctx context.Context, tx *sql.Tx, claimID string, revision uint32, validFrom time.Time, validTo *time.Time, expiresAt time.Time, released bool, actor claims.Actor, action string) error {
	var validToArg any
	if validTo != nil {
		validToArg = datetimeArg(*validTo)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO claim_versions
		(claim_id, revision, valid_from, valid_to, expires_at, released, actor_email, actor_issuer, actor_subject, channel, action)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		claimID, revision, datetimeArg(validFrom), validToArg, datetimeArg(expiresAt), released,
		actor.Email, actor.Issuer, actor.Subject, actor.Channel, action,
	)

	return err
}

func lookupResultTx(ctx context.Context, tx *sql.Tx, key resultKey) (savedResult, bool, error) {
	var result savedResult
	err := tx.QueryRowContext(ctx, `SELECT payload_sha256, outcome, claim_id, response_json
		FROM request_results
		WHERE principal_kind = ? AND principal_issuer = ? AND principal_id = ? AND request_id = ?`,
		key.kind, key.issuer, key.principal, key.requestID,
	).Scan(&result.PayloadHash, &result.Outcome, &result.ClaimID, &result.Response)
	if errors.Is(err, sql.ErrNoRows) {
		return savedResult{}, false, nil
	}

	return result, err == nil, err
}

func environmentParameters(environments []claims.Environment) (string, []any) {
	placeholders := make([]string, len(environments))
	args := make([]any, len(environments))
	for i, environment := range environments {
		placeholders[i] = "?"
		args[i] = environment
	}

	return strings.Join(placeholders, ","), args
}

func canonicalEnvironments(environments []claims.Environment) ([]claims.Environment, error) {
	result := append([]claims.Environment(nil), environments...)
	for _, environment := range result {
		if environment != claims.Sandbox && environment != claims.Prod {
			return nil, claims.NewError(claims.Invalid, "unsupported claim environment")
		}
	}
	sort.Slice(result, func(i, j int) bool { return environmentOrder(result[i]) < environmentOrder(result[j]) })
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, claims.NewError(claims.Invalid, "duplicate claim environment")
		}
	}

	return result, nil
}

func environmentOrder(environment claims.Environment) int {
	if environment == claims.Sandbox {
		return 0
	}

	return 1
}

func nullableUint64(value *uint64) any {
	if value == nil {
		return nil
	}

	return *value
}

func datetimeArg(value time.Time) string {
	return value.UTC().Truncate(time.Microsecond).Format(datetimeLayout)
}

const maxMySQLDateTime = "9999-12-31 23:59:59.999999"

func resultRetention(terminal time.Time) time.Time {
	terminal = terminal.UTC().Truncate(time.Microsecond)
	maximum, err := time.ParseInLocation(datetimeLayout, maxMySQLDateTime, time.UTC)
	if err != nil || terminal.After(maximum.Add(-claims.Retention)) {
		return maximum
	}

	return terminal.Add(claims.Retention)
}

func parseDatabaseTime(value string) (time.Time, error) {
	parsed, err := time.ParseInLocation(datetimeLayout, value, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse UTC database datetime %q: %w", value, err)
	}

	return parsed, nil
}

func newClaimID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", storageError(err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])

	return string(encoded[:]), nil
}
