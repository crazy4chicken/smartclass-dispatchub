package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/httpx"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// dateLayout is the wire format of the ?date= session filter.
const dateLayout = "2006-01-02"

// handleTermList answers GET /api/v1/terms.
func (s *server) handleTermList(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	terms, err := s.deps.Store.Terms.List(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err, "invalid_request")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newListResponse(truncate(terms, limit)))
}

// handlePeriodsGet answers GET /api/v1/terms/{term_code}/periods. A term
// without a 节次 table is not usable for imports, so it answers
// periods_not_configured rather than an empty list.
func (s *server) handlePeriodsGet(w http.ResponseWriter, r *http.Request) {
	termCode := chi.URLParam(r, "term_code")
	if _, err := s.deps.Store.Terms.Get(r.Context(), termCode); err != nil {
		s.writeStoreError(w, r, err, "term_not_found")
		return
	}
	periods, err := s.deps.Store.Terms.Periods(r.Context(), termCode)
	if err != nil {
		s.writeStoreError(w, r, err, "term_not_found")
		return
	}
	if len(periods) == 0 {
		httpx.WriteProblem(w, r, http.StatusNotFound, "periods_not_configured")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newListResponse(periods))
}

// handleRoomList answers GET /api/v1/rooms.
func (s *server) handleRoomList(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	rooms, err := s.deps.Store.Rooms.List(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err, "invalid_request")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newListResponse(truncate(rooms, limit)))
}

// roomLiveSession is the active recording of a room, when one exists.
type roomLiveSession struct {
	ID        string     `json:"id"`
	StreamID  *string    `json:"stream_id,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// roomLiveResponse is the body of GET /api/v1/rooms/{room_code}/live.
type roomLiveResponse struct {
	Online        bool             `json:"online"`
	Cameras       []webcam.Camera  `json:"cameras"`
	ActiveSession *roomLiveSession `json:"active_session,omitempty"`
}

// handleRoomLive answers GET /api/v1/rooms/{room_code}/live with the device's
// live state proxied from webcam-server plus dispatchub's active session.
func (s *server) handleRoomLive(w http.ResponseWriter, r *http.Request) {
	room, err := s.deps.Store.Rooms.Get(r.Context(), chi.URLParam(r, "room_code"))
	if err != nil {
		s.writeStoreError(w, r, err, "room_not_bound")
		return
	}
	client, ok := s.webcamClient(w, r)
	if !ok {
		return
	}
	device, err := client.Device(r.Context(), room.DeviceID)
	if err != nil {
		if webcam.IsStatus(err, http.StatusNotFound) {
			httpx.WriteProblem(w, r, http.StatusNotFound, "device_not_found")
			return
		}
		s.writeUpstreamUnavailable(w, r, "webcam-server device lookup failed")
		s.log.Warn("live room state failed", "room_code", room.RoomCode, "error", err)
		return
	}
	response := roomLiveResponse{Online: device.Online, Cameras: device.Cameras}
	if response.Cameras == nil {
		response.Cameras = []webcam.Camera{}
	}
	if active, err := s.deps.Store.Sessions.ActiveByRoom(r.Context(), room.RoomCode); err == nil {
		response.ActiveSession = &roomLiveSession{ID: active.ID, StreamID: active.StreamID, StartedAt: active.StartedAt}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("active session lookup failed", "room_code", room.RoomCode, "error", err)
	}
	httpx.WriteJSON(w, http.StatusOK, response)
}

// handleImportList answers GET /api/v1/timetable/imports.
func (s *server) handleImportList(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	termCode := strings.TrimSpace(r.URL.Query().Get("term_code"))
	imports, err := s.deps.Store.Imports.List(r.Context(), termCode, limit)
	if err != nil {
		s.writeStoreError(w, r, err, "invalid_request")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newListResponse(imports))
}

// handleImportGet answers GET /api/v1/timetable/imports/{id}.
func (s *server) handleImportGet(w http.ResponseWriter, r *http.Request) {
	imp, err := s.deps.Store.Imports.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeStoreError(w, r, err, "invalid_request")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, imp)
}

// handleSessionList answers GET /api/v1/sessions with the documented filters.
func (s *server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := store.SessionFilter{
		TermCode:   strings.TrimSpace(query.Get("term_code")),
		RoomCode:   strings.TrimSpace(query.Get("room_code")),
		Status:     strings.TrimSpace(query.Get("status")),
		Date:       strings.TrimSpace(query.Get("date")),
		CourseCode: strings.TrimSpace(query.Get("course_code")),
		Limit:      limit,
	}
	if filter.Status != "" && !knownSessionStatus(filter.Status) {
		s.writeInvalidRequest(w, r, "status must be one of planned, starting, recording, stopping, completed, failed, canceled, missed")
		return
	}
	if filter.Date != "" {
		if _, err := time.Parse(dateLayout, filter.Date); err != nil {
			s.writeInvalidRequest(w, r, "date must be formatted as YYYY-MM-DD")
			return
		}
	}
	sessions, err := s.deps.Store.Sessions.List(r.Context(), filter)
	if err != nil {
		s.writeStoreError(w, r, err, "invalid_request")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newListResponse(sessions))
}

// sessionDetail is the body of GET /api/v1/sessions/{id}: the session, its
// photo ledger and the live upstream stream when one is stored.
type sessionDetail struct {
	domain.Session
	Photos   []domain.Photo `json:"photos"`
	Upstream *webcam.Stream `json:"upstream,omitempty"`
}

// handleSessionGet answers GET /api/v1/sessions/{id}. The upstream lookup is
// best-effort: a webcam-server outage degrades the response to the stored
// session instead of failing it.
func (s *server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	session, err := s.deps.Store.Sessions.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeStoreError(w, r, err, "session_not_found")
		return
	}
	photos, err := s.deps.Store.Photos.ListBySession(r.Context(), session.ID)
	if err != nil {
		s.writeStoreError(w, r, err, "session_not_found")
		return
	}
	detail := sessionDetail{Session: session, Photos: photos}
	if detail.Photos == nil {
		detail.Photos = []domain.Photo{}
	}
	if session.StreamID != nil && *session.StreamID != "" && s.deps.Webcam != nil {
		stream, err := s.deps.Webcam.StreamDetail(r.Context(), *session.StreamID)
		if err != nil {
			s.log.Warn("live session state failed", "session_id", session.ID, "stream_id", *session.StreamID, "error", err)
		} else {
			detail.Upstream = &stream.Stream
		}
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

// artifactSegment is one recorded chunk with a freshly minted download URL.
type artifactSegment struct {
	SegmentSeq  int    `json:"segment_seq"`
	SizeBytes   int64  `json:"size_bytes"`
	DurationMS  *int   `json:"duration_ms,omitempty"`
	DownloadURL string `json:"download_url"`
}

// artifactPhoto is one resolved snapshot with a freshly minted download URL.
type artifactPhoto struct {
	PhotoID     string    `json:"photo_id"`
	TakenAt     time.Time `json:"taken_at"`
	DownloadURL string    `json:"download_url"`
}

// artifactsResponse is the body of GET /api/v1/sessions/{id}/artifacts.
// download_url never leaves webcam-server's 15-minute TTL, so it is fetched on
// every call and never persisted.
type artifactsResponse struct {
	StreamID *string           `json:"stream_id,omitempty"`
	Status   string            `json:"status"`
	Segments []artifactSegment `json:"segments"`
	Photos   []artifactPhoto   `json:"photos"`
}

// handleSessionArtifacts answers GET /api/v1/sessions/{id}/artifacts by
// re-deriving the segment list from webcam-server and resolving every ledger
// photo to a live download URL.
func (s *server) handleSessionArtifacts(w http.ResponseWriter, r *http.Request) {
	session, err := s.deps.Store.Sessions.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeStoreError(w, r, err, "session_not_found")
		return
	}
	response := artifactsResponse{
		StreamID: session.StreamID,
		Status:   session.Status,
		Segments: []artifactSegment{},
		Photos:   []artifactPhoto{},
	}
	client := s.deps.Webcam
	if session.StreamID != nil && *session.StreamID != "" && client != nil {
		stream, err := client.StreamDetail(r.Context(), *session.StreamID)
		switch {
		case err == nil:
			response.Status = stream.Status
			for _, segment := range stream.Segments {
				response.Segments = append(response.Segments, artifactSegment{
					SegmentSeq:  segment.SegmentSeq,
					SizeBytes:   segment.SizeBytes,
					DurationMS:  segment.DurationMS,
					DownloadURL: segment.DownloadURL,
				})
			}
		case webcam.IsStatus(err, http.StatusNotFound):
			// The stored stream id is unknown upstream; report the session
			// status with no segments instead of failing the whole call.
			s.log.Warn("artifacts: stream is unknown upstream", "session_id", session.ID, "stream_id", *session.StreamID)
		default:
			s.writeUpstreamUnavailable(w, r, "webcam-server stream lookup failed")
			s.log.Warn("artifacts: stream lookup failed", "session_id", session.ID, "error", err)
			return
		}
	}
	photos, err := s.deps.Store.Photos.ListBySession(r.Context(), session.ID)
	if err != nil {
		s.writeStoreError(w, r, err, "session_not_found")
		return
	}
	if client != nil {
		for _, ledger := range photos {
			if ledger.PhotoID == nil || *ledger.PhotoID == "" {
				continue
			}
			photo, err := client.Photo(r.Context(), *ledger.PhotoID)
			if err != nil {
				s.log.Warn("artifacts: photo lookup failed", "session_id", session.ID, "photo_id", *ledger.PhotoID, "error", err)
				continue
			}
			response.Photos = append(response.Photos, artifactPhoto{
				PhotoID:     photo.ID,
				TakenAt:     photo.TakenAt,
				DownloadURL: photo.DownloadURL,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, response)
}

// webcamClient returns the webcam-server client or answers 503.
func (s *server) webcamClient(w http.ResponseWriter, r *http.Request) (*webcam.Client, bool) {
	if s.deps.Webcam == nil {
		s.writeUpstreamUnavailable(w, r, "webcam-server client is not configured")
		return nil, false
	}
	return s.deps.Webcam, true
}

// truncate caps a slice to limit entries.
func truncate[T any](items []T, limit int) []T {
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

// knownSessionStatus reports whether status is one of the documented session
// states.
func knownSessionStatus(status string) bool {
	switch status {
	case domain.SessionPlanned, domain.SessionStarting, domain.SessionRecording,
		domain.SessionStopping, domain.SessionCompleted, domain.SessionFailed,
		domain.SessionCanceled, domain.SessionMissed:
		return true
	default:
		return false
	}
}
