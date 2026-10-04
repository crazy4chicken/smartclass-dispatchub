package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/httpx"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/iamauth"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/scheduler"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
)

// cameraRequest is the body of POST /api/v1/rooms/{room_code}/camera/switch.
type cameraRequest struct {
	CameraEnum *int `json:"camera_enum"`
}

// commandAck is the 202 body of the camera switch command.
type commandAck struct {
	CommandID  string `json:"command_id"`
	CameraEnum int    `json:"camera_enum"`
}

// photoAccepted is the 202 body of the snapshot command.
type photoAccepted struct {
	SessionPhotoID string `json:"session_photo_id"`
	RequestID      string `json:"request_id"`
	Status         string `json:"status"`
}

// handleSessionStart answers POST /api/v1/sessions/{id}/recording/start: the
// manual (early or retry) start of a planned session.
func (s *server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	commands, ok := s.commands(w, r)
	if !ok {
		return
	}
	actor, _ := iamauth.ClaimsFromContext(r.Context())
	session, err := commands.StartSession(r.Context(), chi.URLParam(r, "id"), actor.Subject, idempotencyKey(r))
	if err != nil {
		s.writeCommandError(w, r, err, "session_not_found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, session)
}

// handleSessionStop answers POST /api/v1/sessions/{id}/recording/stop.
func (s *server) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	commands, ok := s.commands(w, r)
	if !ok {
		return
	}
	actor, _ := iamauth.ClaimsFromContext(r.Context())
	session, err := commands.StopSession(r.Context(), chi.URLParam(r, "id"), actor.Subject, idempotencyKey(r))
	if err != nil {
		s.writeCommandError(w, r, err, "session_not_found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, session)
}

// handleRoomStart answers POST /api/v1/rooms/{room_code}/recording/start by
// creating an ad-hoc manual session for the room.
func (s *server) handleRoomStart(w http.ResponseWriter, r *http.Request) {
	commands, ok := s.commands(w, r)
	if !ok {
		return
	}
	actor, _ := iamauth.ClaimsFromContext(r.Context())
	session, err := commands.StartAdHoc(r.Context(), chi.URLParam(r, "room_code"), actor.Subject, idempotencyKey(r))
	if err != nil {
		s.writeCommandError(w, r, err, "room_not_bound")
		return
	}
	w.Header().Set("Location", "/api/v1/sessions/"+session.ID)
	httpx.WriteJSON(w, http.StatusCreated, session)
}

// handleRoomStop answers POST /api/v1/rooms/{room_code}/recording/stop: the
// ad-hoc stop of the room's live session.
func (s *server) handleRoomStop(w http.ResponseWriter, r *http.Request) {
	commands, ok := s.commands(w, r)
	if !ok {
		return
	}
	actor, _ := iamauth.ClaimsFromContext(r.Context())
	session, err := commands.StopAdHoc(r.Context(), chi.URLParam(r, "room_code"), actor.Subject, idempotencyKey(r))
	if err != nil {
		s.writeCommandError(w, r, err, "room_not_bound")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, session)
}

// handleRoomSwitch answers POST /api/v1/rooms/{room_code}/camera/switch. The
// command is queued upstream, so the response is 202 with its command id.
func (s *server) handleRoomSwitch(w http.ResponseWriter, r *http.Request) {
	commands, ok := s.commands(w, r)
	if !ok {
		return
	}
	var request cameraRequest
	if err := httpx.DecodeJSON(w, r, &request); err != nil {
		s.writeInvalidRequest(w, r, "invalid JSON request body")
		return
	}
	if request.CameraEnum == nil {
		s.writeInvalidRequest(w, r, "camera_enum is required")
		return
	}
	actor, _ := iamauth.ClaimsFromContext(r.Context())
	ack, err := commands.SwitchCamera(r.Context(), chi.URLParam(r, "room_code"), *request.CameraEnum, actor.Subject, idempotencyKey(r))
	if err != nil {
		s.writeCommandError(w, r, err, "room_not_bound")
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, commandAck{CommandID: ack.CommandID, CameraEnum: ack.CameraEnum})
}

// handleRoomPhoto answers POST /api/v1/rooms/{room_code}/photo. The photo is
// legal while a stream is live and while the room is idle; the photo id itself
// is resolved asynchronously by the scheduler's poller, so the response is 202
// with the ledger id and the upstream request id.
func (s *server) handleRoomPhoto(w http.ResponseWriter, r *http.Request) {
	commands, ok := s.commands(w, r)
	if !ok {
		return
	}
	roomCode := chi.URLParam(r, "room_code")
	var sessionID *string
	if active, err := s.deps.Store.Sessions.ActiveByRoom(r.Context(), roomCode); err == nil {
		id := active.ID
		sessionID = &id
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("active session lookup failed", "room_code", roomCode, "error", err)
	}
	actor, _ := iamauth.ClaimsFromContext(r.Context())
	photo, err := commands.CapturePhoto(r.Context(), sessionID, roomCode, actor.Subject, idempotencyKey(r))
	if err != nil {
		s.writeCommandError(w, r, err, "room_not_bound")
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, photoAccepted{
		SessionPhotoID: photo.ID,
		RequestID:      photo.RequestID,
		Status:         photo.Status,
	})
}

// commands returns the scheduler or answers 503 when the router was built
// without one.
func (s *server) commands(w http.ResponseWriter, r *http.Request) (*scheduler.Scheduler, bool) {
	if s.deps.Commands == nil {
		s.writeUpstreamUnavailable(w, r, "scheduler is not configured")
		return nil, false
	}
	return s.deps.Commands, true
}
