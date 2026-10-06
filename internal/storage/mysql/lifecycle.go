// Package mysql provides MySQL storage for claims.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/beeemT/claimy/internal/claims"
)

type claimRecord struct {
	GroupID    uint64
	Claim      claims.Claim
	ReleasedAt sql.NullString
}

func loadClaim(ctx context.Context, tx *sql.Tx, claimID string, forUpdate bool) (record claimRecord, err error) {
	query := `SELECT c.group_id, g.canonical_name, COALESCE(a.canonical_name, ''), c.owner_email, c.source,
		c.gitlab_issuer, c.gitlab_project_id, c.gitlab_job_id, c.gitlab_user_id,
		DATE_FORMAT(c.created_at, '%Y-%m-%d %H:%i:%s.%f'), DATE_FORMAT(c.expires_at, '%Y-%m-%d %H:%i:%s.%f'),
		CASE WHEN c.released_at IS NULL THEN NULL ELSE DATE_FORMAT(c.released_at, '%Y-%m-%d %H:%i:%s.%f') END,
		c.revision
		FROM claims c
		JOIN app_groups g ON g.id = c.group_id
		LEFT JOIN apps a ON a.id = c.app_id AND a.group_id = c.group_id
		WHERE c.id = ?`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var source string
	var gitlabIssuer, gitlabProject, gitlabJob, gitlabUser sql.NullString
	var createdAt, expiresAt string
	err = tx.QueryRowContext(ctx, query, claimID).Scan(
		&record.GroupID, &record.Claim.Scope.Group, &record.Claim.Scope.App, &record.Claim.OwnerEmail, &source,
		&gitlabIssuer, &gitlabProject, &gitlabJob, &gitlabUser, &createdAt, &expiresAt, &record.ReleasedAt, &record.Claim.Revision,
	)
	if err != nil {
		return claimRecord{}, err
	}
	record.Claim.ID = claimID
	record.Claim.Source = claims.Source(source)
	record.Claim.CreatedAt, err = parseDatabaseTime(createdAt)
	if err != nil {
		return claimRecord{}, err
	}
	record.Claim.ExpiresAt, err = parseDatabaseTime(expiresAt)
	if err != nil {
		return claimRecord{}, err
	}
	if record.ReleasedAt.Valid {
		releasedAt, parseErr := parseDatabaseTime(record.ReleasedAt.String)
		if parseErr != nil {
			return claimRecord{}, parseErr
		}
		record.Claim.ReleasedAt = &releasedAt
	}
	if gitlabIssuer.Valid {
		record.Claim.GitLab = &claims.GitLabIdentity{
			Issuer: gitlabIssuer.String, ProjectID: gitlabProject.String, JobID: gitlabJob.String, UserID: gitlabUser.String,
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT environment FROM claim_environments WHERE claim_id = ? ORDER BY FIELD(environment, 'sandbox', 'prod')`, claimID)
	if err != nil {
		return claimRecord{}, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	for rows.Next() {
		var environment string
		if err = rows.Scan(&environment); err != nil {
			return claimRecord{}, err
		}
		record.Claim.Environments = append(record.Claim.Environments, claims.Environment(environment))
	}
	if err = rows.Err(); err != nil {
		return claimRecord{}, err
	}
	if len(record.Claim.Environments) == 0 {
		return claimRecord{}, errors.New("claim has no environment rows")
	}

	return record, nil
}

func loadClaimGroup(ctx context.Context, client interface {
	Get(context.Context, any, string, ...any) error
}, claimID string,
) (uint64, error) {
	var groupID uint64
	err := client.Get(ctx, &groupID, `SELECT group_id FROM claims WHERE id = ?`, claimID)

	return groupID, err
}

func (r *Repository) lockExistingGroupAndTime(ctx context.Context, tx *sql.Tx, groupID uint64) (time.Time, error) {
	var lockedID uint64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM app_groups WHERE id = ? FOR UPDATE`, groupID).Scan(&lockedID); err != nil {
		return time.Time{}, err
	}

	return r.operationTime(ctx, tx)
}

// Release records a release for a claim.
func (r *Repository) Release(ctx context.Context, actor claims.Actor, request claims.ReleaseRequest) (claims.MutationResult, error) {
	var empty claims.MutationResult
	key, err := requestKey(actor, request.Principal, request.RequestID)
	if err != nil {
		return empty, err
	}
	if request.ClaimID == "" {
		return empty, claims.NewError(claims.Invalid, "claim ID is required")
	}
	if replay, found, replayErr := r.lookupMutationReplay(ctx, key, request.PayloadHash); replayErr != nil || found {
		return replay, replayErr
	}

	groupID, err := loadClaimGroup(ctx, r.client, request.ClaimID)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, claims.NewError(claims.NotFound, "claim not found")
	}
	if err != nil {
		return empty, storageError(err)
	}

	var result claims.MutationResult
	err = r.withTransaction(ctx, false, func(tx *sql.Tx) error {
		return r.releaseInTransaction(ctx, tx, actor, request, key, groupID, &result)
	})
	if err != nil {
		if isDuplicateKey(err) {
			return r.replayMutationAfterRace(ctx, key, request.PayloadHash)
		}

		return empty, operationError(err)
	}

	return result, nil
}

func (r *Repository) releaseInTransaction(ctx context.Context, tx *sql.Tx, actor claims.Actor, request claims.ReleaseRequest, key resultKey, groupID uint64, result *claims.MutationResult) error {
	opTime, err := r.lockExistingGroupAndTime(ctx, tx, groupID)
	if err != nil {
		return err
	}
	if found, err := replayMutationTx(ctx, tx, key, request.PayloadHash, result); err != nil || found {
		return err
	}
	record, err := loadClaim(ctx, tx, request.ClaimID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return claims.NewError(claims.NotFound, "claim not found")
	}
	if err != nil {
		return err
	}

	return r.releaseClaim(ctx, tx, actor, request, key, record, opTime, result)
}

func (r *Repository) releaseClaim(ctx context.Context, tx *sql.Tx, actor claims.Actor, request claims.ReleaseRequest, key resultKey, record claimRecord, opTime time.Time, result *claims.MutationResult) error {
	active := !record.ReleasedAt.Valid && record.Claim.ExpiresAt.After(opTime)
	record.Claim.ActiveNow = active
	if !active {
		*result = claims.MutationResult{Changed: false, Claim: record.Claim}

		return saveResult(ctx, tx, actor, key, request.PayloadHash, "released", request.ClaimID, *result, opTime,
			resultRetention(terminalTime(record.Claim.ExpiresAt, record.Claim.ReleasedAt)))
	}
	if record.Claim.Revision == math.MaxUint32 {
		return storageError(errors.New("claim revision is exhausted"))
	}
	nextRevision := record.Claim.Revision + 1
	if err := closeOpenVersion(ctx, tx, request.ClaimID, opTime); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE claims SET released_at = ?, revision = ? WHERE id = ?`, datetimeArg(opTime), nextRevision, request.ClaimID); err != nil {
		return err
	}
	if err := insertVersion(ctx, tx, request.ClaimID, nextRevision, opTime, nil, record.Claim.ExpiresAt, true, actor, "released"); err != nil {
		return err
	}
	record.Claim.Revision = nextRevision
	record.Claim.ReleasedAt = &opTime
	record.Claim.ActiveNow = false
	*result = claims.MutationResult{Changed: true, Claim: record.Claim}
	retainUntil := resultRetention(terminalTime(record.Claim.ExpiresAt, record.Claim.ReleasedAt))
	if err := refreshClaimResultRetention(ctx, tx, request.ClaimID, retainUntil); err != nil {
		return err
	}

	return saveResult(ctx, tx, actor, key, request.PayloadHash, "released", request.ClaimID, *result, opTime, retainUntil)
}

// ChangeExpiry records an expiry change for a claim.
func (r *Repository) ChangeExpiry(ctx context.Context, actor claims.Actor, request claims.ExpiryRequest) (claims.MutationResult, error) {
	var empty claims.MutationResult
	key, err := requestKey(actor, request.Principal, request.RequestID)
	if err != nil {
		return empty, err
	}
	if request.ClaimID == "" {
		return empty, claims.NewError(claims.Invalid, "claim ID is required")
	}
	if request.ExpectedRevision == nil && actor.Channel != claims.GoogleChat {
		return empty, claims.NewError(claims.Invalid, "expected revision is required")
	}
	if replay, found, replayErr := r.lookupMutationReplay(ctx, key, request.PayloadHash); replayErr != nil || found {
		return replay, replayErr
	}

	groupID, err := loadClaimGroup(ctx, r.client, request.ClaimID)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, claims.NewError(claims.NotFound, "claim not found")
	}
	if err != nil {
		return empty, storageError(err)
	}

	expiresAt := request.ExpiresAt.UTC().Truncate(time.Microsecond)
	var result claims.MutationResult
	err = r.withTransaction(ctx, false, func(tx *sql.Tx) error {
		return r.changeExpiryInTransaction(ctx, tx, actor, request, key, groupID, expiresAt, &result)
	})
	if err != nil {
		if isDuplicateKey(err) {
			return r.replayMutationAfterRace(ctx, key, request.PayloadHash)
		}

		return empty, operationError(err)
	}

	return result, nil
}

func (r *Repository) changeExpiryInTransaction(ctx context.Context, tx *sql.Tx, actor claims.Actor, request claims.ExpiryRequest, key resultKey, groupID uint64, expiresAt time.Time, result *claims.MutationResult) error {
	opTime, err := r.lockExistingGroupAndTime(ctx, tx, groupID)
	if err != nil {
		return err
	}
	if found, err := replayMutationTx(ctx, tx, key, request.PayloadHash, result); err != nil || found {
		return err
	}
	record, err := loadClaim(ctx, tx, request.ClaimID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return claims.NewError(claims.NotFound, "claim not found")
	}
	if err != nil {
		return err
	}

	return r.changeClaimExpiry(ctx, tx, actor, request, key, record, opTime, expiresAt, result)
}

func (r *Repository) changeClaimExpiry(ctx context.Context, tx *sql.Tx, actor claims.Actor, request claims.ExpiryRequest, key resultKey, record claimRecord, opTime, expiresAt time.Time, result *claims.MutationResult) error {
	if record.ReleasedAt.Valid || !record.Claim.ExpiresAt.After(opTime) {
		return claims.NewError(claims.ConflictError, "inactive claim cannot be changed")
	}
	expectedRevision := record.Claim.Revision
	if request.ExpectedRevision != nil {
		expectedRevision = *request.ExpectedRevision
	}
	if expectedRevision != record.Claim.Revision {
		return claims.NewError(claims.ConflictError, "claim revision is stale")
	}
	if !expiresAt.After(opTime) {
		return claims.NewError(claims.Invalid, "expiry must be later than database time")
	}
	if expiresAt.Equal(record.Claim.ExpiresAt) {
		record.Claim.ActiveNow = true
		*result = claims.MutationResult{Changed: false, Claim: record.Claim}
		retainUntil := resultRetention(terminalTime(record.Claim.ExpiresAt, record.Claim.ReleasedAt))

		return saveResult(ctx, tx, actor, key, request.PayloadHash, "expiry_changed", request.ClaimID, *result, opTime, retainUntil)
	}

	return updateClaimExpiry(ctx, tx, actor, request, key, record, opTime, expiresAt, result)
}

func updateClaimExpiry(ctx context.Context, tx *sql.Tx, actor claims.Actor, request claims.ExpiryRequest, key resultKey, record claimRecord, opTime, expiresAt time.Time, result *claims.MutationResult) error {
	if record.Claim.Revision == math.MaxUint32 {
		return storageError(errors.New("claim revision is exhausted"))
	}
	nextRevision := record.Claim.Revision + 1
	if err := closeOpenVersion(ctx, tx, request.ClaimID, opTime); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE claims SET expires_at = ?, revision = ? WHERE id = ?`, datetimeArg(expiresAt), nextRevision, request.ClaimID); err != nil {
		return err
	}
	if err := insertVersion(ctx, tx, request.ClaimID, nextRevision, opTime, nil, expiresAt, false, actor, "expiry_changed"); err != nil {
		return err
	}
	record.Claim.ExpiresAt = expiresAt
	record.Claim.Revision = nextRevision
	record.Claim.ActiveNow = true
	*result = claims.MutationResult{Changed: true, Claim: record.Claim}
	retainUntil := resultRetention(expiresAt)
	if err := refreshClaimResultRetention(ctx, tx, request.ClaimID, retainUntil); err != nil {
		return err
	}

	return saveResult(ctx, tx, actor, key, request.PayloadHash, "expiry_changed", request.ClaimID, *result, opTime, retainUntil)
}

func (r *Repository) lookupMutationReplay(ctx context.Context, key resultKey, payloadHash [32]byte) (claims.MutationResult, bool, error) {
	var empty claims.MutationResult
	existing, found, err := r.lookupResult(ctx, key)
	if err != nil || !found {
		return empty, found, err
	}
	if err = verifyReplay(existing, payloadHash); err != nil {
		return empty, true, err
	}
	var result claims.MutationResult
	if err = decodeEnvelope(existing.Response, &result); err != nil {
		return empty, true, storageError(err)
	}

	return result, true, nil
}

func replayMutationTx(ctx context.Context, tx *sql.Tx, key resultKey, payloadHash [32]byte, result *claims.MutationResult) (bool, error) {
	existing, found, err := lookupResultTx(ctx, tx, key)
	if err != nil || !found {
		return found, err
	}
	if err = verifyReplay(existing, payloadHash); err != nil {
		return true, err
	}

	return true, decodeEnvelope(existing.Response, result)
}

func (r *Repository) replayMutationAfterRace(ctx context.Context, key resultKey, payloadHash [32]byte) (claims.MutationResult, error) {
	var empty claims.MutationResult
	existing, found, err := r.lookupResult(ctx, key)
	if err != nil {
		return empty, err
	}
	if !found {
		return empty, storageError(errors.New("request-result unique-key conflict has no committed winner"))
	}
	if err = verifyReplay(existing, payloadHash); err != nil {
		return empty, err
	}
	var result claims.MutationResult
	if err = decodeEnvelope(existing.Response, &result); err != nil {
		return empty, storageError(err)
	}

	return result, nil
}

func closeOpenVersion(ctx context.Context, tx *sql.Tx, claimID string, at time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE claim_versions SET valid_to = ? WHERE claim_id = ? AND valid_to IS NULL`, datetimeArg(at), claimID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("claim %s has no unique open history version", claimID)
	}

	return nil
}

func refreshClaimResultRetention(ctx context.Context, tx *sql.Tx, claimID string, retainUntil time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE request_results SET retain_until = ? WHERE claim_id = ?`, datetimeArg(retainUntil), claimID)

	return err
}

func terminalTime(expiresAt time.Time, releasedAt *time.Time) time.Time {
	if releasedAt != nil && releasedAt.Before(expiresAt) {
		return *releasedAt
	}

	return expiresAt
}
