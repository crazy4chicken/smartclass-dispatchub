package store

import (
	"context"
	"fmt"
	"time"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Rooms is the room-to-device binding sub-store. A zero value is not usable;
// Open assembles it with the shared pool.
type Rooms struct{ db dbtx }

// scanRoom reads one rooms row.
func scanRoom(row pgx.Row) (domain.Room, error) {
	var (
		room  domain.Room
		team  pgtype.Text
		owner pgtype.Text
	)
	if err := row.Scan(&room.RoomCode, &room.Name, &room.DeviceID, &room.CameraEnum,
		&team, &owner, &room.Enabled, &room.CreatedAt, &room.UpdatedAt); err != nil {
		return domain.Room{}, err
	}
	room.TeamID = textOrNil(team)
	room.OwnerID = textOrNil(owner)
	return room, nil
}

// List returns every room binding ordered by room_code.
func (r *Rooms) List(ctx context.Context) ([]domain.Room, error) {
	const query = `
SELECT room_code, name, device_id, camera_enum, team_id, owner_id, enabled, created_at, updated_at
FROM rooms
ORDER BY room_code`
	return r.listRooms(ctx, query, "list rooms")
}

// ListEnabled returns only enabled room bindings ordered by room_code.
func (r *Rooms) ListEnabled(ctx context.Context) ([]domain.Room, error) {
	const query = `
SELECT room_code, name, device_id, camera_enum, team_id, owner_id, enabled, created_at, updated_at
FROM rooms
WHERE enabled
ORDER BY room_code`
	return r.listRooms(ctx, query, "list enabled rooms")
}

// Get returns one room binding by code, or ErrNotFound.
func (r *Rooms) Get(ctx context.Context, roomCode string) (domain.Room, error) {
	const query = `
SELECT room_code, name, device_id, camera_enum, team_id, owner_id, enabled, created_at, updated_at
FROM rooms
WHERE room_code = $1`
	room, err := scanRoom(r.db.QueryRow(ctx, query, roomCode))
	if err != nil {
		return domain.Room{}, fmt.Errorf("get room %s: %w", roomCode, classify(err))
	}
	return room, nil
}

// GetByDevice returns the binding for a (device_id, camera_enum) pair, or
// ErrNotFound.
func (r *Rooms) GetByDevice(ctx context.Context, deviceID string, cameraEnum int) (domain.Room, error) {
	const query = `
SELECT room_code, name, device_id, camera_enum, team_id, owner_id, enabled, created_at, updated_at
FROM rooms
WHERE device_id = $1 AND camera_enum = $2`
	room, err := scanRoom(r.db.QueryRow(ctx, query, deviceID, cameraEnum))
	if err != nil {
		return domain.Room{}, fmt.Errorf("get room for device %s camera %d: %w", deviceID, cameraEnum, classify(err))
	}
	return room, nil
}

// Upsert inserts or updates a binding and returns the stored row. Rebinding a
// device/camera pair already owned by another room is an ErrConflict.
func (r *Rooms) Upsert(ctx context.Context, room domain.Room) (domain.Room, error) {
	const query = `
INSERT INTO rooms (room_code, name, device_id, camera_enum, team_id, owner_id, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (room_code) DO UPDATE SET
    name = EXCLUDED.name,
    device_id = EXCLUDED.device_id,
    camera_enum = EXCLUDED.camera_enum,
    team_id = EXCLUDED.team_id,
    owner_id = EXCLUDED.owner_id,
    enabled = EXCLUDED.enabled,
    updated_at = now()
RETURNING room_code, name, device_id, camera_enum, team_id, owner_id, enabled, created_at, updated_at`
	stored, err := scanRoom(r.db.QueryRow(ctx, query,
		room.RoomCode, room.Name, room.DeviceID, room.CameraEnum, room.TeamID, room.OwnerID, room.Enabled))
	if err != nil {
		return domain.Room{}, fmt.Errorf("upsert room %s: %w", room.RoomCode, classify(err))
	}
	return stored, nil
}

// Delete removes a binding. A missing room is ErrNotFound; a room still
// referenced by entries or sessions is ErrConflict.
func (r *Rooms) Delete(ctx context.Context, roomCode string) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM rooms WHERE room_code = $1`, roomCode)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("delete room %s: room is still referenced: %w", roomCode, ErrConflict)
		}
		return fmt.Errorf("delete room %s: %w", roomCode, classify(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete room %s: %w", roomCode, ErrNotFound)
	}
	return nil
}

// CountReferences reports how many schedule entries and sessions still
// reference the room. A room with history cannot be deleted without breaking
// those rows (the foreign keys are plain REFERENCES), so the API asks the
// operator to disable the binding instead.
func (r *Rooms) CountReferences(ctx context.Context, roomCode string) (int, error) {
	const query = `
SELECT (SELECT count(*) FROM schedule_entries WHERE room_code = $1)
     + (SELECT count(*) FROM sessions WHERE room_code = $1)`
	var refs int
	if err := r.db.QueryRow(ctx, query, roomCode).Scan(&refs); err != nil {
		return 0, fmt.Errorf("count references for room %s: %w", roomCode, classify(err))
	}
	return refs, nil
}

// HasSessionsAfter reports whether a room has any non-terminal session
// (planned, starting, recording or stopping) that ends after the given
// instant. It is the guard for unbinding a room.
func (r *Rooms) HasSessionsAfter(ctx context.Context, roomCode string, after time.Time) (bool, error) {
	const query = `
SELECT EXISTS (
    SELECT 1 FROM sessions
    WHERE room_code = $1
      AND ends_at > $2
      AND status IN ('planned', 'starting', 'recording', 'stopping')
)`
	var pending bool
	if err := r.db.QueryRow(ctx, query, roomCode, after).Scan(&pending); err != nil {
		return false, fmt.Errorf("check sessions for room %s: %w", roomCode, classify(err))
	}
	return pending, nil
}

// listRooms runs a room select and drains it.
func (r *Rooms) listRooms(ctx context.Context, query, operation string) ([]domain.Room, error) {
	rows, err := r.db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, classify(err))
	}
	defer rows.Close()
	rooms := make([]domain.Room, 0, 16)
	for rows.Next() {
		room, err := scanRoom(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", operation, classify(err))
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", operation, classify(err))
	}
	return rooms, nil
}
