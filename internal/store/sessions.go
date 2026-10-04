package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/id"
)

// Sessions persists recording sessions: the schedule materialized into rows,
// the manual sessions operators create and every state transition the
// scheduler drives.
type Sessions struct{ db dbtx }

// sessionColumns is selected into a domain.Session; it must stay in sync with
// scanSession.
const sessionColumns = `id, entry_id, room_code, device_id, camera_enum, starts_at, ends_at,
	status, origin, stream_id, started_at, stopped_at, retry_count, last_error, created_at, updated_at`

// Session list bounds; a filter limit of zero means the default.
const (
	defaultSessionListLimit = 100
	maxSessionListLimit     = 1000
)

// SessionFilter narrows Sessions.List. Zero-value fields do not filter. Date is
// a YYYY-MM-DD calendar day interpreted in UTC: it matches sessions whose
// starts_at falls in [00:00, next 00:00) UTC, independent of the configured
// display timezone. CourseCode only matches scheduled sessions — manual
// sessions have no entry and are excluded by that filter.
type SessionFilter struct {
	TermCode   string
	RoomCode   string
	Status     string
	Date       string
	CourseCode string
	Limit      int
}

// Get returns one session. It returns ErrNotFound when no such session exists.
func (s *Sessions) Get(ctx context.Context, sessionID string) (domain.Session, error) {
	return scanSession(s.db.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = $1`, sessionID))
}

// List returns sessions matching f, newest starts_at first.
func (s *Sessions) List(ctx context.Context, f SessionFilter) ([]domain.Session, error) {
	// Date boundaries are computed here so the database session timezone plays
	// no part: the day string always denotes a UTC calendar day.
	var dayStart, dayEnd *time.Time
	if f.Date != "" {
		start, err := time.Parse("2006-01-02", f.Date)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid date %q, want YYYY-MM-DD", ErrValidation, f.Date)
		}
		start = start.UTC()
		end := start.AddDate(0, 0, 1)
		dayStart, dayEnd = &start, &end
	}

	const query = `
		SELECT s.id, s.entry_id, s.room_code, s.device_id, s.camera_enum, s.starts_at, s.ends_at,
		       s.status, s.origin, s.stream_id, s.started_at, s.stopped_at, s.retry_count,
		       s.last_error, s.created_at, s.updated_at
		FROM sessions s
		LEFT JOIN schedule_entries e ON e.id = s.entry_id
		WHERE ($1 = '' OR e.term_code = $1)
		  AND ($2 = '' OR s.room_code = $2)
		  AND ($3 = '' OR s.status = $3)
		  AND ($4::timestamptz IS NULL OR (s.starts_at >= $4 AND s.starts_at < $5))
		  AND ($6 = '' OR e.course_code = $6)
		ORDER BY s.starts_at DESC, s.id DESC
		LIMIT $7`
	return s.query(ctx, query, f.TermCode, f.RoomCode, f.Status, dayStart, dayEnd, f.CourseCode, sessionListLimit(f.Limit))
}

// ActiveByRoom returns the room's live session, newest first. Live means
// starting, recording or stopping; a room without one returns ErrNotFound.
func (s *Sessions) ActiveByRoom(ctx context.Context, roomCode string) (domain.Session, error) {
	const query = `
		SELECT ` + sessionColumns + `
		FROM sessions
		WHERE room_code = $1 AND status IN ('starting','recording','stopping')
		ORDER BY starts_at DESC, id DESC
		LIMIT 1`
	return scanSession(s.db.QueryRow(ctx, query, roomCode))
}

// OverlappingLive reports whether a live or planned session already occupies
// the same (device_id, camera_enum) at any instant of [startsAt, endsAt).
// excludeSessionID skips one row and excludeTerm skips sessions scheduled from
// that term's entries, so a replace import does not collide with the very
// sessions its commit supersedes; manual sessions (entry_id IS NULL) always
// count, and an empty exclusion is "exclude nothing". It is the import guard:
// a second recording of one camera at the same time is a double-booking, not a
// schedule.
func (s *Sessions) OverlappingLive(ctx context.Context, deviceID string, cameraEnum int, startsAt, endsAt time.Time, excludeSessionID, excludeTerm string) (bool, error) {
	const query = `
SELECT EXISTS (
    SELECT 1 FROM sessions s
    LEFT JOIN schedule_entries e ON e.id = s.entry_id
    WHERE s.device_id = $1
      AND s.camera_enum = $2
      AND s.status IN ('planned','starting','recording','stopping')
      AND s.starts_at < $4
      AND s.ends_at > $3
      AND ($5 = '' OR s.id <> $5)
      AND ($6 = '' OR e.term_code IS DISTINCT FROM $6)
)`
	var overlap bool
	if err := s.db.QueryRow(ctx, query, deviceID, cameraEnum, startsAt, endsAt, excludeSessionID, excludeTerm).Scan(&overlap); err != nil {
		return false, fmt.Errorf("check session overlap for device %s camera %d: %w", deviceID, cameraEnum, classify(err))
	}
	return overlap, nil
}

// CreateManual inserts an ad-hoc session. A missing id, status or origin is
// filled in (planned / manual) and the stored row is returned.
func (s *Sessions) CreateManual(ctx context.Context, sess domain.Session) (domain.Session, error) {
	if sess.ID == "" {
		sess.ID = id.New()
	}
	if sess.Status == "" {
		sess.Status = domain.SessionPlanned
	}
	sess.Origin = domain.OriginManual

	const query = `
		INSERT INTO sessions (id, entry_id, room_code, device_id, camera_enum, starts_at, ends_at,
		                      status, origin, retry_count, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0, now(), now())
		RETURNING ` + sessionColumns
	return scanSession(s.db.QueryRow(ctx, query,
		sess.ID, sess.EntryID, sess.RoomCode, sess.DeviceID, sess.CameraEnum,
		sess.StartsAt, sess.EndsAt, sess.Status, sess.Origin))
}

// ClaimDue atomically moves up to limit planned sessions that reached
// starts_at - prestart (and have not ended yet) to starting, in one statement
// with FOR UPDATE SKIP LOCKED so concurrent schedulers never fight over a row.
func (s *Sessions) ClaimDue(ctx context.Context, now time.Time, prestart time.Duration, limit int) ([]domain.Session, error) {
	const query = `
		UPDATE sessions SET status = 'starting', updated_at = now()
		WHERE id IN (
			SELECT id FROM sessions
			WHERE status = 'planned' AND starts_at <= $2 AND ends_at > $1
			ORDER BY starts_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + sessionColumns
	return s.query(ctx, query, now, now.Add(prestart), claimBatchLimit(limit))
}

// ClaimExpired atomically moves up to limit recordings whose ends_at + grace
// has passed to stopping, in one statement with FOR UPDATE SKIP LOCKED.
func (s *Sessions) ClaimExpired(ctx context.Context, now time.Time, grace time.Duration, limit int) ([]domain.Session, error) {
	const query = `
		UPDATE sessions SET status = 'stopping', updated_at = now()
		WHERE id IN (
			SELECT id FROM sessions
			WHERE status = 'recording' AND ends_at <= $2
			ORDER BY ends_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + sessionColumns
	return s.query(ctx, query, now, now.Add(-grace), claimBatchLimit(limit))
}

// MarkRecording stores the upstream stream id and moves the session to
// recording. It applies from every state a manual start admits (starting,
// planned, failed, missed); any other state is an ErrConflict so a late
// scheduler result cannot overwrite a manual action.
func (s *Sessions) MarkRecording(ctx context.Context, sessionID, streamID string, startedAt time.Time) error {
	const query = `
		UPDATE sessions
		SET status = 'recording', stream_id = $2, started_at = $3, last_error = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('starting','planned','failed','missed')`
	tag, err := s.db.Exec(ctx, query, sessionID, streamID, startedAt)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// MarkCompleted finishes the session with the upstream ended_at. It is
// idempotent: an already completed session is left untouched and does not
// report a conflict.
func (s *Sessions) MarkCompleted(ctx context.Context, sessionID string, stoppedAt time.Time) error {
	const query = `
		UPDATE sessions
		SET status = 'completed', stopped_at = $2, updated_at = now()
		WHERE id = $1 AND status IN ('starting','recording','stopping','completed')`
	tag, err := s.db.Exec(ctx, query, sessionID, stoppedAt)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// MarkStopped returns a stopping session to recording after a failed stop, so
// the scheduler and the watchdog keep tracking the live stream.
func (s *Sessions) MarkStopped(ctx context.Context, sessionID string) error {
	const query = `
		UPDATE sessions
		SET status = 'recording', updated_at = now()
		WHERE id = $1 AND status = 'stopping'`
	tag, err := s.db.Exec(ctx, query, sessionID)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// MarkFailed terminates the session with the upstream detail kept in
// last_error. Repeating the transition is allowed.
func (s *Sessions) MarkFailed(ctx context.Context, sessionID, detail string) error {
	const query = `
		UPDATE sessions
		SET status = 'failed', last_error = $2, updated_at = now()
		WHERE id = $1 AND status IN ('planned','starting','recording','stopping','failed')`
	tag, err := s.db.Exec(ctx, query, sessionID, detail)
	if err != nil {
		return classify(err)
	}
	return requireRowAffected(tag)
}

// BumpRetry records one failed start attempt and returns the new retry count.
func (s *Sessions) BumpRetry(ctx context.Context, sessionID, detail string) (int, error) {
	const query = `
		UPDATE sessions
		SET retry_count = retry_count + 1, last_error = $2, updated_at = now()
		WHERE id = $1
		RETURNING retry_count`
	var count int
	if err := s.db.QueryRow(ctx, query, sessionID, detail).Scan(&count); err != nil {
		return 0, classify(err)
	}
	return count, nil
}

// SetStreamID persists the upstream stream handle without touching the
// session state. It returns ErrNotFound for an unknown session.
func (s *Sessions) SetStreamID(ctx context.Context, sessionID, streamID string) error {
	const query = `UPDATE sessions SET stream_id = $2, updated_at = now() WHERE id = $1`
	tag, err := s.db.Exec(ctx, query, sessionID, streamID)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() == 0 {
		return withSentinel(ErrNotFound, errors.New("session row missing"))
	}
	return nil
}

// MarkMissed marks every planned session that starts before the given instant
// as missed and reports how many rows changed.
func (s *Sessions) MarkMissed(ctx context.Context, before time.Time) (int64, error) {
	const query = `
		UPDATE sessions
		SET status = 'missed', updated_at = now()
		WHERE status = 'planned' AND starts_at <= $1`
	tag, err := s.db.Exec(ctx, query, before)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

// CancelFuture cancels the room's planned sessions that have not started yet
// and reports how many rows changed.
func (s *Sessions) CancelFuture(ctx context.Context, roomCode string) (int64, error) {
	const query = `
		UPDATE sessions
		SET status = 'canceled', updated_at = now()
		WHERE room_code = $1 AND status = 'planned' AND starts_at > now()`
	tag, err := s.db.Exec(ctx, query, roomCode)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

// ListRecording returns the sessions the scheduler still drives, oldest update
// first: starting (including retries whose backoff has elapsed), recording and
// stopping. A session that reached a terminal state is never returned.
func (s *Sessions) ListRecording(ctx context.Context, limit int) ([]domain.Session, error) {
	const query = `
		SELECT ` + sessionColumns + `
		FROM sessions
		WHERE status IN ('starting','recording','stopping')
		ORDER BY updated_at, starts_at, id
		LIMIT $1`
	return s.query(ctx, query, claimBatchLimit(limit))
}

// query runs a SELECT over sessionColumns and scans every row.
func (s *Sessions) query(ctx context.Context, sql string, args ...any) ([]domain.Session, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	sessions := make([]domain.Session, 0)
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return sessions, nil
}

// scanSession reads one row selected with sessionColumns.
func scanSession(row pgx.Row) (domain.Session, error) {
	var (
		sess      domain.Session
		entryID   pgtype.Text
		streamID  pgtype.Text
		startedAt pgtype.Timestamptz
		stoppedAt pgtype.Timestamptz
		lastError pgtype.Text
	)
	err := row.Scan(
		&sess.ID, &entryID, &sess.RoomCode, &sess.DeviceID, &sess.CameraEnum,
		&sess.StartsAt, &sess.EndsAt, &sess.Status, &sess.Origin, &streamID,
		&startedAt, &stoppedAt, &sess.RetryCount, &lastError, &sess.CreatedAt, &sess.UpdatedAt,
	)
	if err != nil {
		return domain.Session{}, classify(err)
	}
	sess.EntryID = textOrNil(entryID)
	sess.StreamID = textOrNil(streamID)
	sess.StartedAt = timeOrNil(startedAt)
	sess.StoppedAt = timeOrNil(stoppedAt)
	sess.LastError = textOrNil(lastError)
	return sess, nil
}

// claimBatchLimit clamps a claim batch to a sane positive size; a zero or
// negative limit means the caller wants the default of one.
func claimBatchLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	return limit
}

// requireRowAffected turns an UPDATE that matched no row into ErrConflict, so
// callers can tell a state-machine rejection from a missing row.
func requireRowAffected(tag pgconn.CommandTag) error {
	if tag.RowsAffected() == 0 {
		return withSentinel(ErrConflict, errors.New("no row matched the state guard"))
	}
	return nil
}

// sessionListLimit applies the default and maximum page sizes.
func sessionListLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultSessionListLimit
	case limit > maxSessionListLimit:
		return maxSessionListLimit
	default:
		return limit
	}
}
