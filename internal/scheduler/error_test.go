package scheduler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/config"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// TestTranslateUpstreamMapping is the upstream → stable-detail table from plan
// §7: every branch keeps the upstream cause inspectable.
func TestTranslateUpstreamMapping(t *testing.T) {
	tests := []struct {
		name       string
		cause      error
		wantStatus int
		wantDetail string
	}{
		{
			name:       "device offline 409",
			cause:      &webcam.UpstreamError{Status: http.StatusConflict, Detail: `device "dev1" is offline`},
			wantStatus: http.StatusConflict,
			wantDetail: "device_offline",
		},
		{
			name:       "already streaming 409",
			cause:      &webcam.UpstreamError{Status: http.StatusConflict, Detail: `camera_enum 0 is already streaming on device "dev1"`},
			wantStatus: http.StatusConflict,
			wantDetail: "already_streaming",
		},
		{
			name:       "no active stream 404",
			cause:      &webcam.UpstreamError{Status: http.StatusNotFound, Detail: "no active stream for camera_enum 0"},
			wantStatus: http.StatusNotFound,
			wantDetail: "no_active_stream",
		},
		{
			name:       "unknown device 404",
			cause:      &webcam.UpstreamError{Status: http.StatusNotFound, Detail: `device "dev9" not found`},
			wantStatus: http.StatusConflict,
			wantDetail: "device_not_found",
		},
		{
			name:       "unregistered camera 400",
			cause:      &webcam.UpstreamError{Status: http.StatusBadRequest, Detail: `camera_enum 9 is not registered for device "dev1"`},
			wantStatus: http.StatusBadRequest,
			wantDetail: "invalid_request",
		},
		{
			name:       "bad gateway 502",
			cause:      &webcam.UpstreamError{Status: http.StatusBadGateway, Detail: "device connection is unavailable"},
			wantStatus: http.StatusServiceUnavailable,
			wantDetail: "upstream_unavailable",
		},
		{
			name:       "unauthorized 401",
			cause:      &webcam.UpstreamError{Status: http.StatusUnauthorized, Detail: "invalid_token"},
			wantStatus: http.StatusServiceUnavailable,
			wantDetail: "upstream_unavailable",
		},
		{
			name:       "transport failure",
			cause:      errors.New("connection refused"),
			wantStatus: http.StatusServiceUnavailable,
			wantDetail: "upstream_unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateUpstream(tt.cause)
			if got.Status != tt.wantStatus || got.Detail != tt.wantDetail {
				t.Fatalf("translateUpstream() = %d/%s, want %d/%s", got.Status, got.Detail, tt.wantStatus, tt.wantDetail)
			}
			if !errors.Is(got, tt.cause) {
				t.Fatalf("translateUpstream() lost the cause %v", tt.cause)
			}
			if got.Unwrap() == nil {
				t.Fatalf("translated error has no cause")
			}

			var upstream *webcam.UpstreamError
			if errors.As(tt.cause, &upstream) {
				if !webcam.IsStatus(got, upstream.Status) {
					t.Fatalf("webcam.IsStatus(translated, %d) = false", upstream.Status)
				}
				if webcam.DetailOf(got) != upstream.Detail {
					t.Fatalf("webcam.DetailOf(translated) = %q, want %q", webcam.DetailOf(got), upstream.Detail)
				}
				if upstreamStatus(got) != upstream.Status {
					t.Fatalf("upstreamStatus(translated) = %d, want %d", upstreamStatus(got), upstream.Status)
				}
			} else {
				if webcam.DetailOf(got) != "" || webcam.IsStatus(got, 0) {
					t.Fatalf("translated transport failure answered an upstream inspection")
				}
				if upstreamStatus(got) != 0 {
					t.Fatalf("upstreamStatus(transport failure) = %d, want 0", upstreamStatus(got))
				}
			}
		})
	}
}

// TestControlErrorConstructors locks the status/detail pair of every control
// error constructor.
func TestControlErrorConstructors(t *testing.T) {
	tests := []struct {
		name       string
		err        *Error
		cause      error
		wantStatus int
		wantDetail string
	}{
		{"session not found", errSessionNotFound(store.ErrNotFound), store.ErrNotFound, http.StatusNotFound, "session_not_found"},
		{"session not planned", errSessionNotPlanned(store.ErrConflict), store.ErrConflict, http.StatusConflict, "session_not_planned"},
		{"no active stream", errNoActiveStream(store.ErrNotFound), store.ErrNotFound, http.StatusNotFound, "no_active_stream"},
		{"room not bound", errRoomNotBound(store.ErrNotFound), store.ErrNotFound, http.StatusNotFound, "room_not_bound"},
		{"device not found", errDeviceNotFound(store.ErrNotFound), store.ErrNotFound, http.StatusConflict, "device_not_found"},
		{"duplicate idempotency", errDuplicateIdempotency(store.ErrConflict), store.ErrConflict, http.StatusConflict, "duplicate_idempotency_key"},
		{"invalid input", errInvalidInput(store.ErrValidation), store.ErrValidation, http.StatusBadRequest, "invalid_request"},
		{"internal", errInternal(errors.New("boom")), nil, http.StatusInternalServerError, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Status != tt.wantStatus || tt.err.Detail != tt.wantDetail {
				t.Fatalf("error = %+v, want %d/%s", tt.err, tt.wantStatus, tt.wantDetail)
			}
			if tt.cause != nil && !errors.Is(tt.err, tt.cause) {
				t.Fatalf("errors.Is(err, %v) = false", tt.cause)
			}
			if !strings.Contains(tt.err.Error(), tt.wantDetail) {
				t.Fatalf("Error() = %q, want it to carry the detail", tt.err.Error())
			}
		})
	}
}

func TestErrorRenderingAndUnwrap(t *testing.T) {
	err := errSessionNotFound(store.ErrNotFound)
	want := fmt.Sprintf("scheduler: %d %s: %v", http.StatusNotFound, "session_not_found", store.ErrNotFound)
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(err.Error(), "scheduler: 404 session_not_found:") {
		t.Fatalf("Error() = %q, want the status and detail prefix", err.Error())
	}

	bare := errInternal(nil)
	if got, want := bare.Error(), "scheduler: 500 internal_error"; got != want {
		t.Fatalf("Error() with no cause = %q, want %q", got, want)
	}

	wrapped := fmt.Errorf("handler: %w", err)
	var control *Error
	if !errors.As(wrapped, &control) || control.Detail != "session_not_found" {
		t.Fatalf("errors.As through a caller wrap failed: %v", wrapped)
	}
	if !errors.Is(wrapped, store.ErrNotFound) {
		t.Fatalf("errors.Is through the caller wrap lost the store sentinel")
	}
}

func TestOutcomeFor(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{0, domain.OutcomeRejected},
		{http.StatusOK, domain.OutcomeRejected},
		{http.StatusBadRequest, domain.OutcomeRejected},
		{http.StatusConflict, domain.OutcomeRejected},
		{http.StatusUnprocessableEntity, domain.OutcomeRejected},
		{499, domain.OutcomeRejected},
		{http.StatusInternalServerError, domain.OutcomeFailed},
		{http.StatusBadGateway, domain.OutcomeFailed},
		{http.StatusServiceUnavailable, domain.OutcomeFailed},
	}
	for _, tt := range tests {
		if got := outcomeFor(tt.status); got != tt.want {
			t.Fatalf("outcomeFor(%d) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestUpstreamStatus(t *testing.T) {
	upstream := &webcam.UpstreamError{Status: http.StatusBadGateway, Detail: "device connection is unavailable"}
	if got := upstreamStatus(upstream); got != http.StatusBadGateway {
		t.Fatalf("upstreamStatus(direct) = %d, want 502", got)
	}
	if got := upstreamStatus(translateUpstream(upstream)); got != http.StatusBadGateway {
		t.Fatalf("upstreamStatus(translated) = %d, want 502", got)
	}
	if got := upstreamStatus(errors.New("connection refused")); got != 0 {
		t.Fatalf("upstreamStatus(transport) = %d, want 0", got)
	}
}

func TestUpstreamDetailMatching(t *testing.T) {
	streaming := &webcam.UpstreamError{Status: http.StatusConflict, Detail: `camera_enum 0 is already streaming on device "dev1"`}
	offline := &webcam.UpstreamError{Status: http.StatusConflict, Detail: `device "dev1" is offline`}
	noStream := &webcam.UpstreamError{Status: http.StatusNotFound, Detail: "no active stream for camera_enum 1"}
	notFound := &webcam.UpstreamError{Status: http.StatusNotFound, Detail: `device "dev9" not found`}

	if !isAlreadyStreaming(streaming) || isAlreadyStreaming(offline) {
		t.Fatalf("isAlreadyStreaming did not match the upstream wording precisely")
	}
	if !isNoActiveStream(noStream) || isNoActiveStream(notFound) {
		t.Fatalf("isNoActiveStream did not match the upstream wording precisely")
	}
	if got := upstreamDetail(streaming); got != "already_streaming" {
		t.Fatalf("upstreamDetail() = %q, want already_streaming", got)
	}
	if got := upstreamDetail(errors.New("connection refused")); got != "upstream_unavailable" {
		t.Fatalf("upstreamDetail(transport) = %q, want upstream_unavailable", got)
	}
}

func TestReplayGuard(t *testing.T) {
	stored := domain.Command{
		Action:    domain.ActionStart,
		RoomCode:  "A301",
		SessionID: new("session-1"),
	}

	if err := replayGuard(stored, domain.ActionStart, "A301", new("session-1")); err != nil {
		t.Fatalf("replayGuard(same command) = %v, want nil", err)
	}
	// A session-less request may replay a stored key (the guard only compares
	// session ids when the request carries one).
	if err := replayGuard(stored, domain.ActionStart, "A301", nil); err != nil {
		t.Fatalf("replayGuard(no request session) = %v, want nil", err)
	}

	conflicts := []struct {
		name      string
		action    string
		roomCode  string
		sessionID *string
	}{
		{"different action", domain.ActionStop, "A301", new("session-1")},
		{"different room", domain.ActionStart, "B102", new("session-1")},
		{"different session", domain.ActionStart, "A301", new("session-2")},
		{"session request against a session-less record", domain.ActionStart, "A301", new("session-1")},
	}
	for _, tt := range conflicts {
		t.Run(tt.name, func(t *testing.T) {
			record := stored
			if tt.name == "session request against a session-less record" {
				record.SessionID = nil
			}
			err := replayGuard(record, tt.action, tt.roomCode, tt.sessionID)
			if err == nil {
				t.Fatalf("replayGuard() = nil, want a duplicate_idempotency_key conflict")
			}
			var control *Error
			if !errors.As(err, &control) || control.Status != http.StatusConflict || control.Detail != "duplicate_idempotency_key" {
				t.Fatalf("replayGuard() = %v, want 409 duplicate_idempotency_key", err)
			}
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("replayGuard() lost the store conflict sentinel")
			}
		})
	}

	sessionless := domain.Command{Action: domain.ActionPhoto, RoomCode: "A301"}
	if err := replayGuard(sessionless, domain.ActionPhoto, "A301", nil); err != nil {
		t.Fatalf("replayGuard(session-less replay) = %v, want nil", err)
	}
}

// TestStartBackoffBounds pins the exponential backoff measured in scheduler
// ticks and its 16-tick ceiling.
func TestStartBackoffBounds(t *testing.T) {
	scheduler := New(Deps{Config: config.Config{SchedTick: 10 * time.Second}})
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{-3, 10 * time.Second},
		{0, 10 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 160 * time.Second},
		{6, 160 * time.Second},
		{100, 160 * time.Second},
	}
	for _, tt := range tests {
		if got := scheduler.startBackoff(tt.failures); got != tt.want {
			t.Fatalf("startBackoff(%d) = %s, want %s", tt.failures, got, tt.want)
		}
	}

	// A zero-value Config must not produce a zero-tick scheduler.
	bare := New(Deps{})
	if got := bare.startBackoff(1); got != 30*time.Second {
		t.Fatalf("startBackoff on a zero-value config = %s, want the 30s tick default", got)
	}
	if got := bare.startBackoff(5); got != 16*30*time.Second {
		t.Fatalf("startBackoff ceiling on a zero-value config = %s, want 16 ticks", got)
	}
}
