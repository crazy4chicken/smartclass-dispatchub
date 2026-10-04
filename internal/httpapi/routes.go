package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/httpx"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/iamauth"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/scheduler"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// Every-scoped permission keys of the routes. Require expands each into the
// any → team → own ladder for the resolved resource.
const (
	permissionReadAny    = "dispatch:read:any"
	permissionManageAny  = "dispatch:manage:any"
	permissionControlAny = "dispatch:control:any"
)

// guard describes the authorization of one route: the every-scoped permission
// key and, for routes with a resource identity, the resolver that loads it.
// An empty permission marks a public route.
type guard struct {
	permission string
	resolve    iamauth.ScopeResolver
}

// registerRoutes is the service's only route table: the runtime router and the
// OpenAPI generator both replay it, so adding a route here is the single step
// that exposes it (and cmd/genspec immediately demands its documentation).
func registerRoutes(s *server, r chi.Router) {
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.Route("/api/v1", func(api chi.Router) {
		api.Use(s.deps.Auth.Middleware())

		s.register(api, http.MethodGet, "/terms", s.handleTermList, guard{permission: permissionReadAny})
		s.register(api, http.MethodPost, "/terms", s.handleTermCreate, guard{permission: permissionManageAny})
		s.register(api, http.MethodGet, "/terms/{term_code}/periods", s.handlePeriodsGet, guard{permission: permissionReadAny})
		s.register(api, http.MethodPut, "/terms/{term_code}/periods", s.handlePeriodsPut, guard{permission: permissionManageAny})

		s.register(api, http.MethodGet, "/rooms", s.handleRoomList, guard{permission: permissionReadAny})
		s.register(api, http.MethodPut, "/rooms/{room_code}", s.handleRoomUpsert, guard{permission: permissionManageAny, resolve: s.roomBindingScope})
		s.register(api, http.MethodDelete, "/rooms/{room_code}", s.handleRoomDelete, guard{permission: permissionManageAny, resolve: s.roomScope})

		s.register(api, http.MethodPost, "/timetable/imports", s.handleImportCreate, guard{permission: permissionManageAny})
		s.register(api, http.MethodGet, "/timetable/imports", s.handleImportList, guard{permission: permissionReadAny})
		s.register(api, http.MethodGet, "/timetable/imports/{id}", s.handleImportGet, guard{permission: permissionReadAny})

		s.register(api, http.MethodGet, "/sessions", s.handleSessionList, guard{permission: permissionReadAny})
		s.register(api, http.MethodGet, "/sessions/{id}", s.handleSessionGet, guard{permission: permissionReadAny, resolve: s.sessionScope})
		s.register(api, http.MethodGet, "/sessions/{id}/artifacts", s.handleSessionArtifacts, guard{permission: permissionReadAny, resolve: s.sessionScope})
		s.register(api, http.MethodPost, "/sessions/{id}/recording/start", s.handleSessionStart, guard{permission: permissionControlAny, resolve: s.sessionScope})
		s.register(api, http.MethodPost, "/sessions/{id}/recording/stop", s.handleSessionStop, guard{permission: permissionControlAny, resolve: s.sessionScope})

		s.register(api, http.MethodPost, "/rooms/{room_code}/recording/start", s.handleRoomStart, guard{permission: permissionControlAny, resolve: s.roomScope})
		s.register(api, http.MethodPost, "/rooms/{room_code}/recording/stop", s.handleRoomStop, guard{permission: permissionControlAny, resolve: s.roomScope})
		s.register(api, http.MethodPost, "/rooms/{room_code}/camera/switch", s.handleRoomSwitch, guard{permission: permissionControlAny, resolve: s.roomScope})
		s.register(api, http.MethodPost, "/rooms/{room_code}/photo", s.handleRoomPhoto, guard{permission: permissionControlAny, resolve: s.roomScope})
		s.register(api, http.MethodGet, "/rooms/{room_code}/live", s.handleRoomLive, guard{permission: permissionReadAny, resolve: s.roomScope})
	})
}

// register mounts one route together with its permission guard.
func (s *server) register(r chi.Router, method, pattern string, handler http.HandlerFunc, g guard) {
	if g.permission == "" {
		r.Method(method, pattern, handler)
		return
	}
	r.With(s.deps.Auth.Require(g.permission, g.resolve)).Method(method, pattern, handler)
}

// roomScope resolves the room named by the {room_code} path parameter.
func (s *server) roomScope(r *http.Request) (iamauth.Scope, error) {
	room, err := s.deps.Store.Rooms.Get(r.Context(), chi.URLParam(r, "room_code"))
	if err != nil {
		return iamauth.Scope{}, resolveResourceError(err, "room_not_bound")
	}
	return scopeOfRoom(room), nil
}

// roomBindingScope resolves PUT /rooms/{room_code}, where a missing row is not
// an error: the caller may be creating the binding. A missing room resolves to
// an empty scope, so only the any-scoped key can grant the create; an existing
// room keeps its ownership snapshot, so the team and own rungs still apply to a
// re-bind.
func (s *server) roomBindingScope(r *http.Request) (iamauth.Scope, error) {
	room, err := s.deps.Store.Rooms.Get(r.Context(), chi.URLParam(r, "room_code"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return iamauth.Scope{}, nil
		}
		return iamauth.Scope{}, &iamauth.ResourceError{Status: http.StatusInternalServerError, Detail: "internal_error", Err: err}
	}
	return scopeOfRoom(room), nil
}

// sessionScope resolves the room of the session named by the {id} path
// parameter: sessions inherit their permission identity from their room.
func (s *server) sessionScope(r *http.Request) (iamauth.Scope, error) {
	session, err := s.deps.Store.Sessions.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		return iamauth.Scope{}, resolveResourceError(err, "session_not_found")
	}
	room, err := s.deps.Store.Rooms.Get(r.Context(), session.RoomCode)
	if err != nil {
		return iamauth.Scope{}, resolveResourceError(err, "room_not_bound")
	}
	return scopeOfRoom(room), nil
}

// scopeOfRoom maps a room's snapshotted ownership onto the permission ladder.
func scopeOfRoom(room domain.Room) iamauth.Scope {
	scope := iamauth.Scope{}
	if room.TeamID != nil {
		scope.TeamID = *room.TeamID
	}
	if room.OwnerID != nil {
		scope.OwnerID = *room.OwnerID
	}
	return scope
}

// resolveResourceError maps a failed resource lookup onto the status and
// stable detail the authorization guard answers with.
func resolveResourceError(err error, notFoundDetail string) error {
	if errors.Is(err, store.ErrNotFound) {
		return &iamauth.ResourceError{Status: http.StatusNotFound, Detail: notFoundDetail, Err: err}
	}
	return &iamauth.ResourceError{Status: http.StatusInternalServerError, Detail: "internal_error", Err: err}
}

// writeCommandError maps a scheduler or webcam error onto the stable API error
// codes. The order matters: the scheduler's typed detail is authoritative,
// then the store sentinels, then the upstream status.
func (s *server) writeCommandError(w http.ResponseWriter, r *http.Request, err error, notFoundDetail string) {
	if err == nil {
		s.writeInternalError(w, r, errors.New("command failed without an error"))
		return
	}
	var schedErr *scheduler.Error
	if errors.As(err, &schedErr) {
		status := schedErr.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		detail := schedErr.Detail
		if detail == "" {
			detail = "internal_error"
		}
		if status >= http.StatusInternalServerError {
			s.log.Error("recording command failed", "method", r.Method, "path", r.URL.Path, "detail", detail, "error", err)
		}
		httpx.WriteProblem(w, r, status, detail)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteProblem(w, r, http.StatusNotFound, notFoundDetail)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		detail := "invalid_request"
		if idempotencyKey(r) != "" {
			detail = "duplicate_idempotency_key"
		}
		httpx.WriteProblem(w, r, http.StatusConflict, detail)
		return
	}
	if status, detail := upstreamErrorDetail(err); status != 0 {
		if status >= http.StatusInternalServerError {
			s.log.Error("upstream command failed", "method", r.Method, "path", r.URL.Path, "detail", detail, "error", err)
		}
		httpx.WriteProblem(w, r, status, detail)
		return
	}
	s.writeInternalError(w, r, err)
}

// upstreamErrorDetail maps a webcam-server failure onto the stable codes: the
// RFC 9457 detail disambiguates the two documented 409s and 502 means the
// upstream device connection is gone.
func upstreamErrorDetail(err error) (int, string) {
	detail := strings.ToLower(webcam.DetailOf(err))
	switch {
	case webcam.IsStatus(err, http.StatusConflict) && strings.Contains(detail, "offline"):
		return http.StatusConflict, "device_offline"
	case webcam.IsStatus(err, http.StatusConflict) && strings.Contains(detail, "already streaming"):
		return http.StatusConflict, "already_streaming"
	case webcam.IsStatus(err, http.StatusNotFound) && strings.Contains(detail, "no active stream"):
		return http.StatusNotFound, "no_active_stream"
	case webcam.IsStatus(err, http.StatusBadRequest):
		return http.StatusBadRequest, "invalid_request"
	case webcam.IsStatus(err, http.StatusBadGateway):
		return http.StatusServiceUnavailable, "upstream_unavailable"
	}
	var transport *url.Error
	if errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusServiceUnavailable, "upstream_unavailable"
	}
	return 0, ""
}

// idempotencyKey reads the optional X-Idempotency-Key header.
func idempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
}
