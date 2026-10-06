package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/beeemT/claimy/internal/claims"
)

type queryClaim struct {
	id             string
	group          string
	app            string
	owner          string
	source         string
	gitlabIssuer   sql.NullString
	gitlabProject  sql.NullString
	gitlabJob      sql.NullString
	gitlabUser     sql.NullString
	createdAt      string
	selectedExpiry string
	currentExpiry  string
	currentRelease sql.NullString
	revision       uint32
}

// Query returns a snapshot of claims matching the request.
func (r *Repository) Query(ctx context.Context, actor claims.Actor, request claims.QueryRequest) (result claims.QueryResult, err error) {
	result.Claims = make([]claims.Claim, 0)
	if request.Scope.Group == "" && request.Scope.App != "" {
		return result, claims.NewError(claims.Invalid, "an app query requires a group")
	}
	environments := request.Environments
	if len(environments) == 0 {
		environments = []claims.Environment{claims.Sandbox, claims.Prod}
	}
	selectedEnvironments, err := canonicalEnvironments(environments)
	if err != nil {
		return result, err
	}

	readTx, tx, err := r.beginRead(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		if rollbackErr := rollbackRead(readTx); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, storageError(fmt.Errorf("rollback claim query snapshot: %w", rollbackErr)))
		}
	}()
	result, err = r.querySnapshot(ctx, tx, actor, request, selectedEnvironments)
	if err != nil {
		return result, err
	}
	if err = readTx.Commit(); err != nil {
		return result, storageError(fmt.Errorf("commit claim query snapshot: %w", err))
	}

	return result, nil
}

func (r *Repository) querySnapshot(ctx context.Context, tx *sql.Tx, actor claims.Actor, request claims.QueryRequest, environments []claims.Environment) (claims.QueryResult, error) {
	result := claims.QueryResult{Claims: make([]claims.Claim, 0)}
	databaseNow, err := databaseSnapshotTime(ctx, tx)
	if err != nil {
		return result, storageError(err)
	}
	selectedAt := databaseNow
	if request.At != nil {
		selectedAt = request.At.UTC().Truncate(time.Microsecond)
	}
	if selectedAt.Before(databaseNow.Add(-claims.Retention)) {
		return result, claims.NewError(claims.HistoryUnavailable, "requested history is older than retention")
	}
	result.At = selectedAt
	result.Projected = selectedAt.After(databaseNow)
	result.Known, err = queryTargetKnown(ctx, tx, request.Scope)
	if err != nil {
		return result, storageError(err)
	}
	if err = populateQueryClaims(ctx, tx, actor, request.Scope, environments, selectedAt, databaseNow, &result); err != nil {
		return result, storageError(err)
	}

	return result, nil
}

func queryTargetKnown(ctx context.Context, tx *sql.Tx, scope claims.Scope) (bool, error) {
	var known bool
	switch {
	case scope.Group == "":
		return true, nil
	case scope.App == "":
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM app_groups WHERE canonical_name = ?)`, scope.Group).Scan(&known)

		return known, err
	default:
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM apps a JOIN app_groups g ON g.id = a.group_id
			WHERE g.canonical_name = ? AND a.canonical_name = ?)`, scope.Group, scope.App).Scan(&known)

		return known, err
	}
}

func populateQueryClaims(ctx context.Context, tx *sql.Tx, actor claims.Actor, scope claims.Scope, environments []claims.Environment, selectedAt, databaseNow time.Time, result *claims.QueryResult) error {
	rows, err := readMatchingClaims(ctx, tx, scope, environments, selectedAt, databaseNow)
	if err != nil {
		return err
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.id
	}
	environmentMap, err := loadClaimEnvironments(ctx, tx, ids)
	if err != nil {
		return err
	}
	result.Free = true
	result.AllowedForCaller = true
	for _, row := range rows {
		claim, err := buildQueryClaim(row, environmentMap[row.id], scope, databaseNow)
		if err != nil {
			return err
		}
		result.Claims = append(result.Claims, claim)
		result.Free = false
		if row.owner != actor.Email {
			result.AllowedForCaller = false
		}
	}

	return nil
}

func buildQueryClaim(row queryClaim, environments []claims.Environment, scope claims.Scope, databaseNow time.Time) (claims.Claim, error) {
	createdAt, err := parseDatabaseTime(row.createdAt)
	if err != nil {
		return claims.Claim{}, err
	}
	selectedExpiry, err := parseDatabaseTime(row.selectedExpiry)
	if err != nil {
		return claims.Claim{}, err
	}
	currentExpiry, err := parseDatabaseTime(row.currentExpiry)
	if err != nil {
		return claims.Claim{}, err
	}
	claim := claims.Claim{
		ID:           row.id,
		Scope:        claims.Scope{Group: row.group, App: row.app},
		Environments: environments,
		OwnerEmail:   row.owner,
		Source:       claims.Source(row.source),
		CreatedAt:    createdAt,
		ExpiresAt:    selectedExpiry,
		Revision:     row.revision,
		ActiveNow:    !row.currentRelease.Valid && currentExpiry.After(databaseNow),
		Inherited:    scope.App != "" && row.app == "",
	}
	if row.gitlabIssuer.Valid {
		claim.GitLab = &claims.GitLabIdentity{
			Issuer:    row.gitlabIssuer.String,
			ProjectID: row.gitlabProject.String,
			JobID:     row.gitlabJob.String,
			UserID:    row.gitlabUser.String,
		}
	}

	return claim, nil
}

func readMatchingClaims(ctx context.Context, tx *sql.Tx, scope claims.Scope, environments []claims.Environment, at, databaseNow time.Time) (result []queryClaim, err error) {
	envPlaceholders, envArgs := environmentParameters(environments)
	historical := at.Before(databaseNow)
	selectedExpiry := "DATE_FORMAT(c.expires_at, '%Y-%m-%d %H:%i:%s.%f')"
	selectedRevision := "c.revision"
	joinVersion := ""
	if historical {
		selectedExpiry = "DATE_FORMAT(v.expires_at, '%Y-%m-%d %H:%i:%s.%f')"
		selectedRevision = "v.revision"
		joinVersion = "JOIN claim_versions v ON v.claim_id = c.id"
	}
	query := `SELECT DISTINCT c.id, g.canonical_name, COALESCE(a.canonical_name, ''), c.owner_email, c.source,
		c.gitlab_issuer, c.gitlab_project_id, c.gitlab_job_id, c.gitlab_user_id,
		DATE_FORMAT(c.created_at, '%Y-%m-%d %H:%i:%s.%f'), ` + selectedExpiry + `,
		DATE_FORMAT(c.expires_at, '%Y-%m-%d %H:%i:%s.%f'),
		CASE WHEN c.released_at IS NULL THEN NULL ELSE DATE_FORMAT(c.released_at, '%Y-%m-%d %H:%i:%s.%f') END,
		` + selectedRevision + `
		FROM claims c
		JOIN app_groups g ON g.id = c.group_id
		LEFT JOIN apps a ON a.id = c.app_id AND a.group_id = c.group_id
		JOIN claim_environments ce ON ce.claim_id = c.id
		` + joinVersion + `
		WHERE ce.environment IN (` + envPlaceholders + `)`
	args := append([]any(nil), envArgs...)
	if scope.Group != "" {
		query += ` AND g.canonical_name = ?`
		args = append(args, scope.Group)
	}
	if scope.App != "" {
		query += ` AND (c.app_id IS NULL OR a.canonical_name = ?)`
		args = append(args, scope.App)
	}
	if historical {
		query += ` AND v.valid_from <= ? AND (v.valid_to IS NULL OR ? < v.valid_to)
			AND v.revision = (SELECT MAX(v2.revision) FROM claim_versions v2
				WHERE v2.claim_id = c.id AND v2.valid_from <= ? AND (v2.valid_to IS NULL OR ? < v2.valid_to))
			AND v.released = 0 AND v.expires_at > ?`
		args = append(args, datetimeArg(at), datetimeArg(at), datetimeArg(at), datetimeArg(at), datetimeArg(at))
	} else {
		query += ` AND c.released_at IS NULL AND c.expires_at > ?`
		args = append(args, datetimeArg(at))
	}
	query += ` ORDER BY g.canonical_name, c.id`

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	result = make([]queryClaim, 0)
	for rows.Next() {
		var row queryClaim
		if err = rows.Scan(
			&row.id, &row.group, &row.app, &row.owner, &row.source,
			&row.gitlabIssuer, &row.gitlabProject, &row.gitlabJob, &row.gitlabUser,
			&row.createdAt, &row.selectedExpiry, &row.currentExpiry, &row.currentRelease, &row.revision,
		); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

func loadClaimEnvironments(ctx context.Context, tx *sql.Tx, claimIDs []string) (result map[string][]claims.Environment, err error) {
	result = make(map[string][]claims.Environment, len(claimIDs))
	if len(claimIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(claimIDs))
	args := make([]any, len(claimIDs))
	for i, id := range claimIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT claim_id, environment FROM claim_environments WHERE claim_id IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY claim_id, FIELD(environment, 'sandbox', 'prod')`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	for rows.Next() {
		var id, environment string
		if err = rows.Scan(&id, &environment); err != nil {
			return nil, err
		}
		result[id] = append(result[id], claims.Environment(environment))
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range claimIDs {
		if len(result[id]) == 0 {
			return nil, errors.New("claim has no environment rows")
		}
	}

	return result, nil
}
