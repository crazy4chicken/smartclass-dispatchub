// Package webcam is the typed client for smartclass-webcam-server. It mirrors
// the routes dispatchub consumes — the device commands (recording start and
// stop, camera switch, photo capture), the device, stream and photo resources
// and the /readyz probe — and decodes every upstream failure into
// *UpstreamError, the only error shape callers inspect.
package webcam

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Stream status values reported by webcam-server.
const (
	StreamActive    = "active"
	StreamCompleted = "completed"
	StreamFailed    = "failed"
)

// TokenSource supplies the Bearer credential for every outbound call. The
// production implementation is the iamauth client-credentials source; it
// caches the token and refreshes it before expiry.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func(ctx context.Context) (string, error)

// Token returns f(ctx).
func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// TokenRefresher is an optional TokenSource extension. A source that caches a
// credential implements it so the client can force a refresh after a 401 and
// retry once; when a source does not implement it, the client still re-requests
// the token but a caching source may hand back the same one.
type TokenRefresher interface {
	Refresh(ctx context.Context) (string, error)
}

// Stream is one recording stream of a device camera.
type Stream struct {
	ID         string     `json:"id"`
	DeviceID   string     `json:"device_id"`
	CameraEnum int        `json:"camera_enum"`
	Status     string     `json:"status"` // active | completed | failed
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
}

// Segment is one uploaded video chunk of a stream. DownloadURL is minted per
// response with a 15 minute TTL and must never be persisted.
type Segment struct {
	ID          string    `json:"id"`
	StreamID    string    `json:"stream_id"`
	SegmentSeq  int       `json:"segment_seq"`
	SizeBytes   int64     `json:"size_bytes"`
	DurationMS  *int      `json:"duration_ms,omitempty"`
	DownloadURL string    `json:"download_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// StreamDetail is a stream together with its segments.
type StreamDetail struct {
	Stream
	Segments []Segment `json:"segments"`
}

// Photo is one still image captured by a device camera. RequestID is absent
// for photos produced without a capture command; dispatchub only ever claims
// photos by the request id it generated.
type Photo struct {
	ID          string    `json:"id"`
	DeviceID    string    `json:"device_id"`
	CameraEnum  int       `json:"camera_enum"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	RequestID   *string   `json:"request_id,omitempty"`
	TakenAt     time.Time `json:"taken_at"`
	DownloadURL string    `json:"download_url,omitempty"`
}

// Camera is one camera reported by a device's live registration.
type Camera struct {
	CameraEnum int      `json:"camera_enum"`
	Resolution string   `json:"resolution"`
	FPS        int      `json:"fps"`
	Codecs     []string `json:"supported_codec"`
}

// Device is a registered device with its live registration state.
type Device struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Location string   `json:"location"`
	TeamID   *string  `json:"team_id,omitempty"`
	OwnerID  *string  `json:"owner_id,omitempty"`
	Online   bool     `json:"online"`
	Cameras  []Camera `json:"cameras"`
}

// CommandAck acknowledges a queued device command. RequestID is only set for
// the photo command.
type CommandAck struct {
	CommandID  string `json:"command_id"`
	CameraEnum int    `json:"camera_enum"`
	RequestID  string `json:"request_id,omitempty"`
}

// UpstreamError is a non-2xx response from webcam-server. Status is the HTTP
// status, Detail the RFC 9457 detail code (falling back to the HTTP status
// text when the body is not a problem document) and Body a bounded copy of the
// response body for logs.
type UpstreamError struct {
	Status int
	Detail string
	Body   string
}

// Error renders the upstream failure.
func (e *UpstreamError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("webcam: upstream status %d: %s", e.Status, e.Detail)
	}
	if e.Body != "" {
		return fmt.Sprintf("webcam: upstream status %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("webcam: upstream status %d", e.Status)
}

// IsStatus reports whether err carries a webcam-server response with the given
// HTTP status. It unwraps through caller-defined error types, so a scheduler
// error wrapping an *UpstreamError still answers.
func IsStatus(err error, status int) bool {
	var upstream *UpstreamError
	return errors.As(err, &upstream) && upstream.Status == status
}

// DetailOf returns the RFC 9457 detail of err, or "" when err carries no
// upstream problem.
func DetailOf(err error) string {
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Detail
	}
	return ""
}
