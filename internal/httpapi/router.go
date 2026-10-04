package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/config"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/httpx"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/iamauth"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/scheduler"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

const (
	// readyTimeout bounds the dependency checks behind GET /readyz.
	readyTimeout = 5 * time.Second
	// defaultListLimit is the page size of every collection route when limit is
	// omitted.
	defaultListLimit = 100
	// maxListLimit caps a caller-supplied limit.
	maxListLimit = 1000
)

// Deps carries the HTTP layer's dependencies. NewRouter only stores them: it
// opens no connection and performs no network call, so cmd/genspec constructs
// the router from zero-valued dependencies and walks it for the OpenAPI
// document.
type Deps struct {
	// Store is the persistence layer. It may be nil when the router is built
	// only to enumerate routes.
	Store *store.Store
	// Commands owns the recording state machine and the audit trail.
	Commands *scheduler.Scheduler
	// Webcam is the typed webcam-server client used for device validation,
	// live state, control commands and artifact URLs.
	Webcam *webcam.Client
	// Auth verifies bearer tokens and enforces the dispatch:* permissions.
	Auth *iamauth.Authorizer
	// Config is the effective service configuration.
	Config config.Config
	// Logger receives request-scoped warnings and errors.
	Logger *slog.Logger
}

// server is the handler set every route closes over.
type server struct {
	deps Deps
	log  *slog.Logger
}

// NewRouter builds the HTTP router. All 22 operations hang off one *chi.Mux:
// the public health probes and the /api/v1 group guarded by the teamusers
// middleware and per-route permission ladder.
func NewRouter(deps Deps) chi.Router {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &server{deps: deps, log: logger}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)
	registerRoutes(s, r)
	return r
}

// corsMiddleware applies the fleet's permissive browser policy. It mirrors the
// policy smartclass-webcam-server installs with go-chi/cors, reimplemented
// here so this service keeps the dependency set of go.mod.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Access-Control-Allow-Origin", "*")
		header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		header.Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-Idempotency-Key")
		header.Set("Access-Control-Expose-Headers", "Location, X-Request-Id")
		header.Set("Access-Control-Max-Age", "300")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusResponse is the body of both health probes.
type statusResponse struct {
	Status string `json:"status"`
}

// listResponse is the envelope every collection route returns.
type listResponse[T any] struct {
	Items []T `json:"items"`
}

// newListResponse wraps items, encoding a nil slice as [].
func newListResponse[T any](items []T) listResponse[T] {
	if items == nil {
		items = []T{}
	}
	return listResponse[T]{Items: items}
}

// handleHealthz answers GET /healthz. It never touches a dependency, so it
// keeps reporting liveness while PostgreSQL or webcam-server is down.
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// handleReadyz answers GET /readyz after pinging PostgreSQL and probing
// webcam-server's own /readyz.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	var unreachable []string
	if s.deps.Store == nil {
		unreachable = append(unreachable, "postgres")
	} else if err := s.deps.Store.Ping(ctx); err != nil {
		s.log.Warn("readiness: postgres is unreachable", "error", err)
		unreachable = append(unreachable, "postgres")
	}
	if s.deps.Webcam == nil {
		unreachable = append(unreachable, "webcam-server")
	} else if err := s.deps.Webcam.Ready(ctx); err != nil {
		s.log.Warn("readiness: webcam-server is unreachable", "error", err)
		unreachable = append(unreachable, "webcam-server")
	}
	if len(unreachable) > 0 {
		httpx.WriteProblemReason(w, r, http.StatusServiceUnavailable, "upstream_unavailable", "unreachable dependencies: "+strings.Join(unreachable, ", "))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}

// writeStoreError maps a store error onto a problem response: ErrNotFound
// becomes 404 with the route's not-found detail, ErrValidation and ErrConflict
// become 400 and 409 invalid_request, everything else a logged 500.
func (s *server) writeStoreError(w http.ResponseWriter, r *http.Request, err error, notFoundDetail string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, notFoundDetail)
	case errors.Is(err, store.ErrValidation):
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, store.ErrConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "invalid_request")
	default:
		s.writeInternalError(w, r, err)
	}
}

// writeInternalError logs err and answers a generic 500. The cause never
// reaches the client.
func (s *server) writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	httpx.WriteProblem(w, r, http.StatusInternalServerError, "internal_error")
}

// writeInvalidRequest answers 400 invalid_request with a human-readable reason.
func (s *server) writeInvalidRequest(w http.ResponseWriter, r *http.Request, reason string) {
	httpx.WriteProblemReason(w, r, http.StatusBadRequest, "invalid_request", reason)
}

// writeUpstreamUnavailable answers 503 upstream_unavailable.
func (s *server) writeUpstreamUnavailable(w http.ResponseWriter, r *http.Request, reason string) {
	httpx.WriteProblemReason(w, r, http.StatusServiceUnavailable, "upstream_unavailable", reason)
}

// parseLimit reads the optional limit query parameter: missing selects the
// default page size, a non-positive or non-numeric value is a 400, and a value
// above maxListLimit is clamped.
func parseLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return defaultListLimit, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		httpx.WriteProblemReason(w, r, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
		return 0, false
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	return limit, true
}

// location returns the timezone sessions are materialized in, falling back to
// the process timezone when the config was not validated (inert routers).
func (s *server) location() *time.Location {
	if loc := s.deps.Config.Location; loc != nil {
		return loc
	}
	return time.Local
}

// maxImportRows returns the configured CSV row cap, falling back to the
// documented default.
func (s *server) maxImportRows() int {
	if rows := s.deps.Config.MaxImportRows; rows > 0 {
		return rows
	}
	return 5000
}

// maxImportBytes caps the CSV part at 1 MiB, aligned with httpx.MaxBodyBytes.
const maxImportBytes = httpx.MaxBodyBytes

// multipartAllowance is the multipart envelope overhead accepted on top of the
// file part.
const multipartAllowance = 64 << 10
