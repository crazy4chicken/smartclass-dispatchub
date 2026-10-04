package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/id"
)

// Photos is the photo-id resolution ledger: one row per capture request, from
// the upstream request id to the photo id webcam-server mints on upload.
type Photos struct{ db dbtx }

// photoColumns is selected into a domain.Photo; it must stay in sync with
// scanPhoto.
const photoColumns = `id, session_id, room_code, device_id, camera_enum, request_id, photo_id,
	source, actor_id, status, attempts, next_poll_at, taken_at, resolved_at, created_at`

// photoClaimLease is how long a claimed pending photo is hidden from other
// claimers. The resolver always overwrites it with a real next_poll_at; the
// lease only stops a crash between claim and outcome from hot-looping.
const photoClaimLease = 5 * time.Minute

// Create inserts one photo row. A missing id, status, taken_at or next_poll_at
// is filled in; the stored row is returned.
func (p *Photos) Create(ctx context.Context, photo domain.Photo) (domain.Photo, error) {
	if photo.ID == "" {
		photo.ID = id.New()
	}
	now := time.Now()
	if photo.TakenAt.IsZero() {
		photo.TakenAt = now
	}
	if photo.Status == "" {
		photo.Status = domain.PhotoPending
	}
	if photo.NextPollAt == nil && photo.Status == domain.PhotoPending {
		next := now
		photo.NextPollAt = &next
	}

	const query = `
		INSERT INTO session_photos (id, session_id, room_code, device_id, camera_enum, request_id,
		                            photo_id, source, actor_id, status, attempts, next_poll_at,
		                            taken_at, resolved_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
		RETURNING ` + photoColumns
	return scanPhoto(p.db.QueryRow(ctx, query,
		photo.ID, photo.SessionID, photo.RoomCode, photo.DeviceID, photo.CameraEnum,
		photo.RequestID, photo.PhotoID, photo.Source, photo.ActorID, photo.Status,
		photo.Attempts, photo.NextPollAt, photo.TakenAt, photo.ResolvedAt))
}

// Get returns one photo ledger row. It returns ErrNotFound when no such row
// exists.
func (p *Photos) Get(ctx context.Context, photoID string) (domain.Photo, error) {
	return scanPhoto(p.db.QueryRow(ctx, `SELECT `+photoColumns+` FROM session_photos WHERE id = $1`, photoID))
}

// ByRequestID returns the newest ledger row for an upstream request id. It
// returns ErrNotFound when no capture was recorded with that request id.
func (p *Photos) ByRequestID(ctx context.Context, requestID string) (domain.Photo, error) {
	const query = `
		SELECT ` + photoColumns + `
		FROM session_photos
		WHERE request_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT 1`
	return scanPhoto(p.db.QueryRow(ctx, query, requestID))
}

// ListBySession returns a session's photos, newest first.
func (p *Photos) ListBySession(ctx context.Context, sessionID string) ([]domain.Photo, error) {
	const query = `
		SELECT ` + photoColumns + `
		FROM session_photos
		WHERE session_id = $1
		ORDER BY taken_at DESC, id DESC`
	return p.query(ctx, query, sessionID)
}

// ClaimPending atomically claims up to limit pending photos whose next poll is
// due, in one statement with FOR UPDATE SKIP LOCKED. Claimed rows get a lease
// on next_poll_at; the caller must follow up with Resolve, Defer or
// MarkUnresolved.
func (p *Photos) ClaimPending(ctx context.Context, now time.Time, limit int) ([]domain.Photo, error) {
	const query = `
		UPDATE session_photos
		SET next_poll_at = $1 + make_interval(secs => $2)
		WHERE id IN (
			SELECT id FROM session_photos
			WHERE status = 'pending' AND (next_poll_at IS NULL OR next_poll_at <= $1)
			ORDER BY next_poll_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + photoColumns
	return p.query(ctx, query, now, photoClaimLease.Seconds(), claimBatchLimit(limit))
}

// Resolve stores the upstream photo id and closes the ledger row.
func (p *Photos) Resolve(ctx context.Context, photoID, upstreamPhotoID string, at time.Time) error {
	const query = `
		UPDATE session_photos
		SET photo_id = $2, status = 'resolved', resolved_at = $3, next_poll_at = NULL
		WHERE id = $1`
	tag, err := p.db.Exec(ctx, query, photoID, upstreamPhotoID, at)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// Defer schedules the next poll attempt and records the attempt count.
func (p *Photos) Defer(ctx context.Context, photoID string, next time.Time, attempts int) error {
	const query = `
		UPDATE session_photos
		SET next_poll_at = $2, attempts = $3, status = 'pending'
		WHERE id = $1`
	tag, err := p.db.Exec(ctx, query, photoID, next, attempts)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// MarkUnresolved gives up on a photo after the poll TTL has passed.
func (p *Photos) MarkUnresolved(ctx context.Context, photoID string) error {
	const query = `
		UPDATE session_photos
		SET status = 'unresolved', next_poll_at = NULL
		WHERE id = $1`
	tag, err := p.db.Exec(ctx, query, photoID)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// query runs a SELECT over photoColumns and scans every row.
func (p *Photos) query(ctx context.Context, sql string, args ...any) ([]domain.Photo, error) {
	rows, err := p.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	photos := make([]domain.Photo, 0)
	for rows.Next() {
		photo, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		photos = append(photos, photo)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return photos, nil
}

// scanPhoto reads one row selected with photoColumns.
func scanPhoto(row pgx.Row) (domain.Photo, error) {
	var (
		photo      domain.Photo
		sessionID  pgtype.Text
		photoID    pgtype.Text
		actorID    pgtype.Text
		nextPollAt pgtype.Timestamptz
		resolvedAt pgtype.Timestamptz
	)
	err := row.Scan(
		&photo.ID, &sessionID, &photo.RoomCode, &photo.DeviceID, &photo.CameraEnum,
		&photo.RequestID, &photoID, &photo.Source, &actorID, &photo.Status,
		&photo.Attempts, &nextPollAt, &photo.TakenAt, &resolvedAt, &photo.CreatedAt,
	)
	if err != nil {
		return domain.Photo{}, classify(err)
	}
	photo.SessionID = textOrNil(sessionID)
	photo.PhotoID = textOrNil(photoID)
	photo.ActorID = textOrNil(actorID)
	photo.NextPollAt = timeOrNil(nextPollAt)
	photo.ResolvedAt = timeOrNil(resolvedAt)
	return photo, nil
}
