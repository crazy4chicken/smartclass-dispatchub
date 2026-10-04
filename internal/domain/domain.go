// Package domain holds the dispatchub data model. The JSON tags are the HTTP
// contract, so they must not change without a versioned API break.
package domain

import "time"

// Session status values (sessions.status).
const (
	SessionPlanned   = "planned"
	SessionStarting  = "starting"
	SessionRecording = "recording"
	SessionStopping  = "stopping"
	SessionCompleted = "completed"
	SessionFailed    = "failed"
	SessionCanceled  = "canceled"
	SessionMissed    = "missed"
)

// Session origins (sessions.origin).
const (
	OriginSchedule = "schedule"
	OriginManual   = "manual"
)

// Import batch status values (imports.status).
const (
	ImportDryRun    = "dry_run"
	ImportCommitted = "committed"
	ImportFailed    = "failed"
)

// Photo resolution states (session_photos.status).
const (
	PhotoPending    = "pending"
	PhotoResolved   = "resolved"
	PhotoUnresolved = "unresolved"
)

// Photo sources (session_photos.source). The column vocabulary differs from
// the session origins: "schedule" is stored as "scheduled".
const (
	PhotoSourceScheduled = "scheduled"
	PhotoSourceManual    = "manual"
)

// Command outcomes (camera_commands.outcome).
const (
	OutcomeAccepted = "accepted"
	OutcomeRejected = "rejected"
	OutcomeFailed   = "failed"
)

// Command actions (camera_commands.action).
const (
	ActionStart  = "start"
	ActionStop   = "stop"
	ActionSwitch = "switch_camera"
	ActionPhoto  = "photo"
)

// Term is a teaching term: weeks run from the Monday of teaching week 1.
type Term struct {
	TermCode    string    `json:"term_code"`
	Name        string    `json:"name"`
	Week1Monday time.Time `json:"week1_monday"` // date only, UTC midnight
	Weeks       int       `json:"weeks"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Period is one 节次 of a term's daily timetable.
type Period struct {
	PeriodNo  int    `json:"period_no"`
	StartTime string `json:"start_time"` // "HH:MM"
	EndTime   string `json:"end_time"`   // "HH:MM"
}

// Room binds a room_code to a webcam-server device and camera.
type Room struct {
	RoomCode   string    `json:"room_code"`
	Name       string    `json:"name"`
	DeviceID   string    `json:"device_id"`
	CameraEnum int       `json:"camera_enum"`
	TeamID     *string   `json:"team_id,omitempty"`
	OwnerID    *string   `json:"owner_id,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ImportError is one rejected CSV row or cell.
type ImportError struct {
	Row     int    `json:"row"`
	Column  string `json:"column,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Import is one CSV upload batch.
type Import struct {
	ID         string        `json:"id"`
	TermCode   string        `json:"term_code"`
	Mode       string        `json:"mode"` // replace | append
	Filename   string        `json:"filename"`
	SHA256     string        `json:"sha256"`
	RowCount   int           `json:"row_count"`
	OKCount    int           `json:"ok_count"`
	ErrorCount int           `json:"error_count"`
	Errors     []ImportError `json:"errors"`
	Status     string        `json:"status"`
	ImportedBy string        `json:"imported_by"`
	CreatedAt  time.Time     `json:"created_at"`
}

// Entry is one normalized CSV row (a course meeting in a room).
type Entry struct {
	ID              string    `json:"id"`
	ImportID        string    `json:"import_id"`
	TermCode        string    `json:"term_code"`
	CourseCode      string    `json:"course_code"`
	CourseName      string    `json:"course_name"`
	TeacherUsername string    `json:"teacher_username"`
	TeacherUserID   *string   `json:"teacher_user_id,omitempty"`
	RoomCode        string    `json:"room_code"`
	Weekday         int       `json:"weekday"`
	PeriodStart     int       `json:"period_start"`
	PeriodEnd       int       `json:"period_end"`
	Weeks           string    `json:"weeks"`
	CreatedAt       time.Time `json:"created_at"`
}

// Session is one recording occurrence, scheduled from an entry or ad hoc.
type Session struct {
	ID         string     `json:"id"`
	EntryID    *string    `json:"entry_id,omitempty"`
	RoomCode   string     `json:"room_code"`
	DeviceID   string     `json:"device_id"`
	CameraEnum int        `json:"camera_enum"`
	StartsAt   time.Time  `json:"starts_at"`
	EndsAt     time.Time  `json:"ends_at"`
	Status     string     `json:"status"`
	Origin     string     `json:"origin"`
	StreamID   *string    `json:"stream_id,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	StoppedAt  *time.Time `json:"stopped_at,omitempty"`
	RetryCount int        `json:"retry_count"`
	LastError  *string    `json:"last_error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Photo tracks one snapshot from POST …/photo to its resolved photo id.
type Photo struct {
	ID         string     `json:"id"`
	SessionID  *string    `json:"session_id,omitempty"`
	RoomCode   string     `json:"room_code"`
	DeviceID   string     `json:"device_id"`
	CameraEnum int        `json:"camera_enum"`
	RequestID  string     `json:"request_id"`
	PhotoID    *string    `json:"photo_id,omitempty"`
	Source     string     `json:"source"`
	ActorID    *string    `json:"actor_id,omitempty"`
	Status     string     `json:"status"`
	Attempts   int        `json:"attempts"`
	NextPollAt *time.Time `json:"next_poll_at,omitempty"`
	TakenAt    time.Time  `json:"taken_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Command is the audit and idempotency record of one control command sent to
// webcam-server.
type Command struct {
	ID             string    `json:"id"`
	SessionID      *string   `json:"session_id,omitempty"`
	RoomCode       string    `json:"room_code"`
	DeviceID       string    `json:"device_id"`
	Action         string    `json:"action"`
	CameraEnum     int       `json:"camera_enum"`
	ActorID        string    `json:"actor_id"`
	IdempotencyKey *string   `json:"idempotency_key,omitempty"`
	RequestID      *string   `json:"request_id,omitempty"`
	CommandID      *string   `json:"command_id,omitempty"`
	Outcome        string    `json:"outcome"`
	HTTPStatus     *int      `json:"http_status,omitempty"`
	Detail         *string   `json:"detail,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}
