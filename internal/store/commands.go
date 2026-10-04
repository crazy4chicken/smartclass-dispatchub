package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/id"
)

// Commands is the camera_commands audit and idempotency log: one row per
// control action, written whether the upstream command succeeded or not.
type Commands struct{ db dbtx }

// commandColumns is selected into a domain.Command; it must stay in sync with
// scanCommand.
const commandColumns = `id, session_id, room_code, device_id, action, camera_enum, actor_id,
	idempotency_key, request_id, command_id, outcome, http_status, detail, created_at`

// Insert writes one audit row and returns the stored command. An empty
// idempotency key is stored as NULL so it never collides with another empty
// key; a duplicate non-empty key reports ErrConflict.
func (c *Commands) Insert(ctx context.Context, cmd domain.Command) (domain.Command, error) {
	if cmd.ID == "" {
		cmd.ID = id.New()
	}
	if cmd.CreatedAt.IsZero() {
		cmd.CreatedAt = time.Now()
	}
	if cmd.IdempotencyKey != nil && *cmd.IdempotencyKey == "" {
		cmd.IdempotencyKey = nil
	}

	const query = `
		INSERT INTO camera_commands (id, session_id, room_code, device_id, action, camera_enum,
		                             actor_id, idempotency_key, request_id, command_id, outcome,
		                             http_status, detail, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING ` + commandColumns
	return scanCommand(c.db.QueryRow(ctx, query,
		cmd.ID, cmd.SessionID, cmd.RoomCode, cmd.DeviceID, cmd.Action, cmd.CameraEnum,
		cmd.ActorID, cmd.IdempotencyKey, cmd.RequestID, cmd.CommandID, cmd.Outcome,
		cmd.HTTPStatus, cmd.Detail, cmd.CreatedAt))
}

// ByIdempotencyKey returns the command stored under key. It returns
// ErrNotFound when the key was never used.
func (c *Commands) ByIdempotencyKey(ctx context.Context, key string) (domain.Command, error) {
	const query = `SELECT ` + commandColumns + ` FROM camera_commands WHERE idempotency_key = $1`
	return scanCommand(c.db.QueryRow(ctx, query, key))
}

// ListBySession returns a session's command log in chronological order.
func (c *Commands) ListBySession(ctx context.Context, sessionID string) ([]domain.Command, error) {
	const query = `
		SELECT ` + commandColumns + `
		FROM camera_commands
		WHERE session_id = $1
		ORDER BY created_at, id`
	rows, err := c.db.Query(ctx, query, sessionID)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	commands := make([]domain.Command, 0)
	for rows.Next() {
		cmd, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		commands = append(commands, cmd)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return commands, nil
}

// scanCommand reads one row selected with commandColumns.
func scanCommand(row pgx.Row) (domain.Command, error) {
	var (
		cmd         domain.Command
		sessionID   pgtype.Text
		idempotency pgtype.Text
		requestID   pgtype.Text
		commandID   pgtype.Text
		httpStatus  pgtype.Int4
		detail      pgtype.Text
	)
	err := row.Scan(
		&cmd.ID, &sessionID, &cmd.RoomCode, &cmd.DeviceID, &cmd.Action, &cmd.CameraEnum,
		&cmd.ActorID, &idempotency, &requestID, &commandID, &cmd.Outcome,
		&httpStatus, &detail, &cmd.CreatedAt,
	)
	if err != nil {
		return domain.Command{}, classify(err)
	}
	cmd.SessionID = textOrNil(sessionID)
	cmd.IdempotencyKey = textOrNil(idempotency)
	cmd.RequestID = textOrNil(requestID)
	cmd.CommandID = textOrNil(commandID)
	cmd.HTTPStatus = intOrNil(httpStatus)
	cmd.Detail = textOrNil(detail)
	return cmd, nil
}
