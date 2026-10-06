package mysql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/beeemT/claimy/internal/claims"
)

// Prune removes expired request results, old history versions, and terminal claims.
func (r *Repository) Prune(ctx context.Context) (claims.PruneResult, error) {
	var groupIDs []uint64
	if err := r.client.Select(ctx, &groupIDs, `SELECT id FROM app_groups ORDER BY id`); err != nil {
		return claims.PruneResult{}, storageError(err)
	}
	var total claims.PruneResult
	for index, groupID := range groupIDs {
		local, err := r.pruneGroup(ctx, groupID, index == 0)
		if err != nil {
			return total, operationError(err)
		}
		total.Results += local.Results
		total.Versions += local.Versions
		total.Claims += local.Claims
	}

	return total, nil
}

func (r *Repository) pruneGroup(ctx context.Context, groupID uint64, pruneOrphanResults bool) (claims.PruneResult, error) {
	var local claims.PruneResult
	err := r.withTransaction(ctx, false, func(tx *sql.Tx) error {
		opTime, err := r.lockExistingGroupAndTime(ctx, tx, groupID)
		if err != nil {
			return err
		}
		local.Results, err = pruneRequestResults(ctx, tx, groupID, opTime, pruneOrphanResults)
		if err != nil {
			return err
		}
		cutoff := opTime.Add(-claims.Retention)
		local.Versions, err = pruneExpiredVersions(ctx, tx, groupID, cutoff)
		if err != nil {
			return err
		}
		claimIDs, err := terminalClaims(ctx, tx, groupID, cutoff)
		if err != nil {
			return err
		}
		if len(claimIDs) == 0 {
			return nil
		}
		removed, err := deleteTerminalClaims(ctx, tx, claimIDs)
		if err != nil {
			return err
		}
		local.Versions += removed.Versions
		local.Claims = removed.Claims

		return nil
	})

	return local, err
}

func pruneRequestResults(ctx context.Context, tx *sql.Tx, groupID uint64, opTime time.Time, pruneOrphanResults bool) (int64, error) {
	var total int64
	if pruneOrphanResults {
		result, err := tx.ExecContext(ctx, `DELETE FROM request_results
			WHERE claim_id IS NULL AND retain_until <= ?`, datetimeArg(opTime))
		if err != nil {
			return 0, err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		total += removed
	}
	result, err := tx.ExecContext(ctx, `DELETE rr FROM request_results rr
		JOIN claims c ON c.id = rr.claim_id
		WHERE c.group_id = ? AND rr.retain_until <= ?`, groupID, datetimeArg(opTime))
	if err != nil {
		return 0, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	return total + removed, nil
}

func pruneExpiredVersions(ctx context.Context, tx *sql.Tx, groupID uint64, cutoff time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `DELETE v FROM claim_versions v
		JOIN claims c ON c.id = v.claim_id
		WHERE c.group_id = ? AND v.valid_to IS NOT NULL AND v.valid_to < ?`, groupID, datetimeArg(cutoff))
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

func deleteTerminalClaims(ctx context.Context, tx *sql.Tx, claimIDs []string) (claims.PruneResult, error) {
	placeholders, args := claimIDParameters(claimIDs)
	environments, err := tx.ExecContext(ctx, `DELETE FROM claim_environments WHERE claim_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return claims.PruneResult{}, err
	}
	if _, err = environments.RowsAffected(); err != nil {
		return claims.PruneResult{}, err
	}
	versions, err := tx.ExecContext(ctx, `DELETE FROM claim_versions WHERE claim_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return claims.PruneResult{}, err
	}
	removedVersions, err := versions.RowsAffected()
	if err != nil {
		return claims.PruneResult{}, err
	}
	claimRows, err := tx.ExecContext(ctx, `DELETE FROM claims WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return claims.PruneResult{}, err
	}
	removedClaims, err := claimRows.RowsAffected()
	if err != nil {
		return claims.PruneResult{}, err
	}

	return claims.PruneResult{Versions: removedVersions, Claims: removedClaims}, nil
}

func terminalClaims(ctx context.Context, tx *sql.Tx, groupID uint64, cutoff time.Time) (claimIDs []string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id FROM claims c
		WHERE c.group_id = ?
		AND LEAST(c.expires_at, COALESCE(c.released_at, c.expires_at)) < ?
		AND NOT EXISTS (SELECT 1 FROM request_results rr WHERE rr.claim_id = c.id)
		ORDER BY c.id FOR UPDATE`, groupID, datetimeArg(cutoff))
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	claimIDs = make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		claimIDs = append(claimIDs, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	return claimIDs, nil
}

func claimIDParameters(claimIDs []string) (string, []any) {
	placeholders := make([]string, len(claimIDs))
	args := make([]any, len(claimIDs))
	for i, id := range claimIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	return strings.Join(placeholders, ","), args
}
