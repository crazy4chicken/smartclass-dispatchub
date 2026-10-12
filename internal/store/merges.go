package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/id"
)

// Merges is the merge-job ledger: one row per completed recording, from the
// enqueue that follows MarkCompleted to the merged video smartclass-merge
// produced, or the error that ended the attempt.
type Merges struct{ db dbtx }

// mergeColumns is selected into a domain.SessionMerge; it must stay in sync
// with scanMerge.
const mergeColumns = `id, session_id, status, job_id, dest_bucket, dest_key, size_bytes,
	sha256, attempts, last_error, next_poll_at, created_at, updated_at`

// mergeClaimLease is how long a claimed merge row is hidden from other
// claimers. Every outcome (submitted, succeeded, failed, deferred) releases
// the lease and writes a real next_poll_at, so the lease costs a poll only
// when the claimer crashed between claiming the row and recording what came of
// it.
const mergeClaimLease = 2 * time.Minute

// Enqueue creates the pending merge job of a completed recording and returns
// the stored row. The unique index on session_id makes enqueueing idempotent
// per session: a session that already has a row reports ErrConflict, which the
// caller treats as "already enqueued". The new row is due immediately, so the
// next scheduler pass submits it.
func (m *Merges) Enqueue(ctx context.Context, sessionID string) (domain.SessionMerge, error) {
	const query = `
		INSERT INTO session_merges (id, session_id, status, next_poll_at)
		VALUES ($1, $2, 'pending', now())
		RETURNING ` + mergeColumns
	return scanMerge(m.db.QueryRow(ctx, query, id.New(), sessionID))
}

// GetBySession returns the merge job of one recording. It returns ErrNotFound
// when the session has no merge row.
func (m *Merges) GetBySession(ctx context.Context, sessionID string) (domain.SessionMerge, error) {
	const query = `SELECT ` + mergeColumns + ` FROM session_merges WHERE session_id = $1`
	return scanMerge(m.db.QueryRow(ctx, query, sessionID))
}

// ClaimDue atomically claims up to limit rows that still need work — pending
// (not submitted yet) or submitted (polling) — whose next poll is due, in one
// statement with FOR UPDATE SKIP LOCKED, and dates the claim. A row claimed
// within the lease window is skipped, so concurrent schedulers never work the
// same merge and a crash mid-claim cannot hot-loop. The caller must follow up
// with MarkSubmitted, MarkSucceeded, MarkFailed or Defer.
func (m *Merges) ClaimDue(ctx context.Context, now time.Time, limit int) ([]domain.SessionMerge, error) {
	const query = `
		UPDATE session_merges
		SET claimed_at = $1
		WHERE id IN (
			SELECT id FROM session_merges
			WHERE status IN ('pending','submitted')
			  AND next_poll_at <= $1::timestamptz
			  AND (claimed_at IS NULL
			       OR claimed_at < $1::timestamptz - make_interval(secs => $2::double precision))
			ORDER BY next_poll_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + mergeColumns
	return m.query(ctx, query, now, mergeClaimLease.Seconds(), claimBatchLimit(limit))
}

// MarkSubmitted records the smartclass-merge job id of a merge that is now
// running upstream and schedules its first poll. The submission error of the
// pending phase, if any, is cleared: it has been resolved.
func (m *Merges) MarkSubmitted(ctx context.Context, id, jobID string, nextPollAt time.Time) error {
	const query = `
		UPDATE session_merges
		SET status = 'submitted', job_id = $2, next_poll_at = $3, last_error = NULL,
		    claimed_at = NULL, updated_at = now()
		WHERE id = $1`
	tag, err := m.db.Exec(ctx, query, id, jobID, nextPollAt)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// MarkSucceeded closes the merge with the destination smartclass-merge
// reported: the object it wrote, that object's size and its digest. A
// succeeded row is outside the claim pool by state, so it is never polled
// again.
func (m *Merges) MarkSucceeded(ctx context.Context, id string, destBucket, destKey string, sizeBytes int64, sha256 string) error {
	const query = `
		UPDATE session_merges
		SET status = 'succeeded', dest_bucket = $2, dest_key = $3, size_bytes = $4, sha256 = $5,
		    last_error = NULL, claimed_at = NULL, updated_at = now()
		WHERE id = $1`
	tag, err := m.db.Exec(ctx, query, id, destBucket, destKey, sizeBytes, sha256)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// MarkFailed terminates the merge with the upstream detail kept in
// last_error: a permanent rejection, a job that failed upstream or one that
// outlived the poll TTL. A failed row is outside the claim pool by state, so
// it is never polled again.
func (m *Merges) MarkFailed(ctx context.Context, id, detail string) error {
	const query = `
		UPDATE session_merges
		SET status = 'failed', last_error = $2, claimed_at = NULL, updated_at = now()
		WHERE id = $1`
	tag, err := m.db.Exec(ctx, query, id, detailOrNil(detail))
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// Defer records one inconclusive or failed pass: the attempt counter grows, the
// detail is kept as last_error and the row becomes due again at nextPollAt. It
// leaves the state alone, so the same call retries a pending submission and
// re-polls a submitted job.
func (m *Merges) Defer(ctx context.Context, id, detail string, nextPollAt time.Time) error {
	const query = `
		UPDATE session_merges
		SET attempts = attempts + 1, last_error = $2, next_poll_at = $3, claimed_at = NULL, updated_at = now()
		WHERE id = $1`
	tag, err := m.db.Exec(ctx, query, id, detailOrNil(detail), nextPollAt)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// query runs a SELECT over mergeColumns and scans every row.
func (m *Merges) query(ctx context.Context, sql string, args ...any) ([]domain.SessionMerge, error) {
	rows, err := m.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	merges := make([]domain.SessionMerge, 0)
	for rows.Next() {
		merge, err := scanMerge(rows)
		if err != nil {
			return nil, err
		}
		merges = append(merges, merge)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return merges, nil
}

// scanMerge reads one row selected with mergeColumns.
func scanMerge(row pgx.Row) (domain.SessionMerge, error) {
	var (
		merge      domain.SessionMerge
		jobID      pgtype.Text
		destBucket pgtype.Text
		destKey    pgtype.Text
		sizeBytes  pgtype.Int8
		sha256     pgtype.Text
		lastError  pgtype.Text
		nextPollAt pgtype.Timestamptz
	)
	err := row.Scan(
		&merge.ID, &merge.SessionID, &merge.Status, &jobID, &destBucket, &destKey,
		&sizeBytes, &sha256, &merge.Attempts, &lastError, &nextPollAt,
		&merge.CreatedAt, &merge.UpdatedAt,
	)
	if err != nil {
		return domain.SessionMerge{}, classify(err)
	}
	merge.JobID = textOrNil(jobID)
	merge.DestBucket = textOrNil(destBucket)
	merge.DestKey = textOrNil(destKey)
	merge.SizeBytes = int64OrNil(sizeBytes)
	merge.SHA256 = textOrNil(sha256)
	merge.LastError = textOrNil(lastError)
	merge.NextPollAt = timeOrNil(nextPollAt)
	return merge, nil
}

// int64OrNil maps a scanned nullable BIGINT column to an *int64: NULL becomes
// nil.
func int64OrNil(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	number := value.Int64
	return &number
}

// detailOrNil maps an empty detail onto nil, so "no detail" is stored as NULL
// rather than as an empty string: a healthy defer does not record an error.
func detailOrNil(detail string) *string {
	if detail == "" {
		return nil
	}
	return &detail
}
