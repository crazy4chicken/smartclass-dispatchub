package scheduler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// Error is a control-path failure. The HTTP layer reports Status with Detail
// as the RFC 9457 detail code (plan §7) and may inspect the cause through
// errors.As/errors.Is: Err is always the underlying failure — store.ErrNotFound,
// store.ErrConflict, the *webcam.UpstreamError and so on.
type Error struct {
	Status int    // HTTP status the API layer should return
	Detail string // stable detail code from plan §7
	Err    error  // wrapped cause
}

// Error renders the failure with its status, detail code and cause.
func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("scheduler: %d %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("scheduler: %d %s: %v", e.Status, e.Detail, e.Err)
}

// Unwrap exposes the underlying cause so errors.Is and errors.As keep working
// through the control error.
func (e *Error) Unwrap() error { return e.Err }

// Control error constructors. Every control-path failure is one of these.
func errSessionNotFound(cause error) *Error {
	return &Error{Status: http.StatusNotFound, Detail: "session_not_found", Err: cause}
}

func errSessionNotPlanned(cause error) *Error {
	return &Error{Status: http.StatusConflict, Detail: "session_not_planned", Err: cause}
}

func errNoActiveStream(cause error) *Error {
	return &Error{Status: http.StatusNotFound, Detail: "no_active_stream", Err: cause}
}

func errRoomNotBound(cause error) *Error {
	return &Error{Status: http.StatusNotFound, Detail: "room_not_bound", Err: cause}
}

func errDeviceNotFound(cause error) *Error {
	return &Error{Status: http.StatusConflict, Detail: "device_not_found", Err: cause}
}

func errDuplicateIdempotency(cause error) *Error {
	return &Error{Status: http.StatusConflict, Detail: "duplicate_idempotency_key", Err: cause}
}

func errInvalidInput(cause error) *Error {
	return &Error{Status: http.StatusBadRequest, Detail: "invalid_request", Err: cause}
}

// errInternal is reserved for programmer errors and broken invariants.
func errInternal(cause error) *Error {
	return &Error{Status: http.StatusInternalServerError, Detail: "internal_error", Err: cause}
}

// translateUpstream converts any webcam client failure into the stable
// dispatchub error contract, keeping the upstream error as the cause so
// webcam.IsStatus and webcam.DetailOf still answer on the result.
func translateUpstream(err error) *Error {
	switch {
	case webcam.IsStatus(err, http.StatusConflict):
		if isAlreadyStreaming(err) {
			return &Error{Status: http.StatusConflict, Detail: "already_streaming", Err: err}
		}
		return &Error{Status: http.StatusConflict, Detail: "device_offline", Err: err}
	case webcam.IsStatus(err, http.StatusNotFound):
		if isNoActiveStream(err) {
			return &Error{Status: http.StatusNotFound, Detail: "no_active_stream", Err: err}
		}
		return errDeviceNotFound(err)
	case webcam.IsStatus(err, http.StatusBadRequest):
		return errInvalidInput(err)
	default:
		// Transport errors, 401/403, 5xx and anything unexpected all mean the
		// upstream could not be used.
		return &Error{Status: http.StatusServiceUnavailable, Detail: "upstream_unavailable", Err: err}
	}
}

// isAlreadyStreaming reports the 409 detail webcam-server answers when the
// camera already has an active stream.
func isAlreadyStreaming(err error) bool {
	return strings.Contains(webcam.DetailOf(err), "already streaming")
}

// isNoActiveStream reports the 404 detail webcam-server answers when a stop
// finds nothing to stop.
func isNoActiveStream(err error) bool {
	return strings.Contains(webcam.DetailOf(err), "no active stream")
}

// upstreamDetail is the stable detail code used for logs, audits and
// sessions.last_error.
func upstreamDetail(err error) string {
	return translateUpstream(err).Detail
}

// upstreamStatus reports the HTTP status behind an upstream failure, or 0 when
// no response was received.
func upstreamStatus(err error) int {
	var upstream *webcam.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status
	}
	return 0
}

// outcomeFor maps an HTTP status onto the camera_commands outcome vocabulary.
func outcomeFor(status int) string {
	if status >= http.StatusInternalServerError {
		return domain.OutcomeFailed
	}
	return domain.OutcomeRejected
}

// replayGuard rejects an idempotency key that was first used for a different
// action or a different room/session; replaying it would answer with the wrong
// outcome.
func replayGuard(cmd domain.Command, action, roomCode string, sessionID *string) error {
	if cmd.Action != action || cmd.RoomCode != roomCode {
		return errDuplicateIdempotency(store.ErrConflict)
	}
	if sessionID != nil && (cmd.SessionID == nil || *cmd.SessionID != *sessionID) {
		return errDuplicateIdempotency(store.ErrConflict)
	}
	return nil
}
