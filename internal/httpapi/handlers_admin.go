package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/httpx"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/iamauth"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/id"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/timetable"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// termRequest is the body of POST /api/v1/terms.
type termRequest struct {
	TermCode    string `json:"term_code"`
	Name        string `json:"name"`
	Week1Monday string `json:"week1_monday"`
	Weeks       *int   `json:"weeks"`
}

// handleTermCreate answers POST /api/v1/terms. Creating an existing term_code
// replaces the term's fields, which keeps an import re-run deterministic.
func (s *server) handleTermCreate(w http.ResponseWriter, r *http.Request) {
	var request termRequest
	if err := httpx.DecodeJSON(w, r, &request); err != nil {
		s.writeInvalidRequest(w, r, "invalid JSON request body")
		return
	}
	request.TermCode = strings.TrimSpace(request.TermCode)
	request.Name = strings.TrimSpace(request.Name)
	request.Week1Monday = strings.TrimSpace(request.Week1Monday)
	if request.TermCode == "" {
		s.writeInvalidRequest(w, r, "term_code is required")
		return
	}
	if request.Name == "" {
		s.writeInvalidRequest(w, r, "name is required")
		return
	}
	monday, err := time.Parse(dateLayout, request.Week1Monday)
	if err != nil {
		s.writeInvalidRequest(w, r, "week1_monday must be formatted as YYYY-MM-DD")
		return
	}
	weeks := 0
	if request.Weeks != nil {
		weeks = *request.Weeks
	}
	if weeks < 1 || weeks > 30 {
		s.writeInvalidRequest(w, r, "weeks must be between 1 and 30")
		return
	}
	term := domain.Term{
		TermCode:    request.TermCode,
		Name:        request.Name,
		Week1Monday: monday.UTC(),
		Weeks:       weeks,
	}
	saved, err := s.deps.Store.Terms.Upsert(r.Context(), term)
	if err != nil {
		s.writeStoreError(w, r, err, "term_not_found")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, saved)
}

// periodsRequest is the body of PUT /api/v1/terms/{term_code}/periods.
type periodsRequest struct {
	Periods []domain.Period `json:"periods"`
}

// handlePeriodsPut answers PUT /api/v1/terms/{term_code}/periods, replacing the
// term's whole 节次 table.
func (s *server) handlePeriodsPut(w http.ResponseWriter, r *http.Request) {
	termCode := chi.URLParam(r, "term_code")
	var request periodsRequest
	if err := httpx.DecodeJSON(w, r, &request); err != nil {
		s.writeInvalidRequest(w, r, "invalid JSON request body")
		return
	}
	if len(request.Periods) == 0 {
		s.writeInvalidRequest(w, r, "periods must not be empty")
		return
	}
	seen := make(map[int]struct{}, len(request.Periods))
	for _, period := range request.Periods {
		if period.PeriodNo < 1 || period.PeriodNo > timetable.MaxPeriod {
			s.writeInvalidRequest(w, r, "period_no must be between 1 and 20")
			return
		}
		if _, duplicate := seen[period.PeriodNo]; duplicate {
			s.writeInvalidRequest(w, r, "period_no must be unique within the term")
			return
		}
		seen[period.PeriodNo] = struct{}{}
		start, startErr := time.Parse("15:04", period.StartTime)
		end, endErr := time.Parse("15:04", period.EndTime)
		if startErr != nil || endErr != nil {
			s.writeInvalidRequest(w, r, "start_time and end_time must be formatted as HH:MM")
			return
		}
		if !end.After(start) {
			s.writeInvalidRequest(w, r, "end_time must be after start_time")
			return
		}
	}
	if err := s.deps.Store.Terms.ReplacePeriods(r.Context(), termCode, request.Periods); err != nil {
		s.writeStoreError(w, r, err, "term_not_found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newListResponse(request.Periods))
}

// roomRequest is the body of PUT /api/v1/rooms/{room_code}.
type roomRequest struct {
	Name       string `json:"name"`
	DeviceID   string `json:"device_id"`
	CameraEnum *int   `json:"camera_enum"`
	Enabled    *bool  `json:"enabled"`
}

// handleRoomUpsert answers PUT /api/v1/rooms/{room_code}: it validates the
// device against webcam-server and snapshots its team and owner for the
// permission ladder.
func (s *server) handleRoomUpsert(w http.ResponseWriter, r *http.Request) {
	roomCode := chi.URLParam(r, "room_code")
	var request roomRequest
	if err := httpx.DecodeJSON(w, r, &request); err != nil {
		s.writeInvalidRequest(w, r, "invalid JSON request body")
		return
	}
	request.DeviceID = strings.TrimSpace(request.DeviceID)
	if request.DeviceID == "" {
		s.writeInvalidRequest(w, r, "device_id is required")
		return
	}
	cameraEnum := 0
	if request.CameraEnum != nil {
		cameraEnum = *request.CameraEnum
	}
	if cameraEnum < 0 {
		s.writeInvalidRequest(w, r, "camera_enum must not be negative")
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	client, ok := s.webcamClient(w, r)
	if !ok {
		return
	}
	device, err := client.Device(r.Context(), request.DeviceID)
	if err != nil {
		switch {
		case webcam.IsStatus(err, http.StatusNotFound):
			httpx.WriteProblem(w, r, http.StatusNotFound, "device_not_found")
		case webcam.IsStatus(err, http.StatusBadRequest):
			s.writeInvalidRequest(w, r, "webcam-server rejected the device id")
		default:
			s.writeUpstreamUnavailable(w, r, "webcam-server device lookup failed")
			s.log.Warn("room binding device lookup failed", "room_code", roomCode, "device_id", request.DeviceID, "error", err)
		}
		return
	}
	deviceID := strings.TrimSpace(device.ID)
	if deviceID == "" {
		deviceID = request.DeviceID
	}
	room := domain.Room{
		RoomCode:   roomCode,
		Name:       strings.TrimSpace(request.Name),
		DeviceID:   deviceID,
		CameraEnum: cameraEnum,
		TeamID:     device.TeamID,
		OwnerID:    device.OwnerID,
		Enabled:    enabled,
	}
	saved, err := s.deps.Store.Rooms.Upsert(r.Context(), room)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			httpx.WriteProblemReason(w, r, http.StatusConflict, "invalid_request", "device_id and camera_enum are already bound to another room")
			return
		}
		s.writeStoreError(w, r, err, "room_not_bound")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// handleRoomDelete answers DELETE /api/v1/rooms/{room_code}. The binding is
// only removed while nothing references it: a room with timetable or session
// history answers 409 room_has_history and is disabled instead (PUT with
// "enabled": false), which keeps the historical rows readable.
func (s *server) handleRoomDelete(w http.ResponseWriter, r *http.Request) {
	roomCode := chi.URLParam(r, "room_code")
	refs, err := s.deps.Store.Rooms.CountReferences(r.Context(), roomCode)
	if err != nil {
		s.writeStoreError(w, r, err, "room_not_bound")
		return
	}
	if refs > 0 {
		httpx.WriteProblemReason(w, r, http.StatusConflict, "room_has_history",
			"the room is referenced by "+strconv.Itoa(refs)+" timetable or session rows; disable it with PUT /api/v1/rooms/"+roomCode+` {"enabled":false} instead of deleting it`)
		return
	}
	if err := s.deps.Store.Rooms.Delete(r.Context(), roomCode); err != nil {
		s.writeStoreError(w, r, err, "room_not_bound")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// importPreview reports one CSV row's materialized occurrence range; it is
// returned by dry runs so a wrong week1_monday or 节次 table is visible before
// anything is committed.
type importPreview struct {
	Row           int       `json:"row"`
	CourseCode    string    `json:"course_code"`
	SessionCount  int       `json:"session_count"`
	FirstStartsAt time.Time `json:"first_starts_at"`
	LastEndsAt    time.Time `json:"last_ends_at"`
}

// importResult is the body of both import modes: the stored batch plus the
// dry-run preview.
type importResult struct {
	domain.Import
	Preview []importPreview `json:"preview,omitempty"`
}

// validationProblem is the 422 body of a rejected import: the RFC 9457 problem
// with the stable detail code plus the per-row error list.
type validationProblem struct {
	Type     string               `json:"type"`
	Title    string               `json:"title"`
	Status   int                  `json:"status"`
	Detail   string               `json:"detail"`
	Instance string               `json:"instance"`
	Errors   []domain.ImportError `json:"errors"`
}

// writeValidationProblem answers 422 import_validation_failed with the full
// error list.
func writeValidationProblem(w http.ResponseWriter, r *http.Request, errs []domain.ImportError) {
	body, err := json.Marshal(validationProblem{
		Type:     "about:blank",
		Title:    http.StatusText(http.StatusUnprocessableEntity),
		Status:   http.StatusUnprocessableEntity,
		Detail:   "import_validation_failed",
		Instance: r.URL.Path,
		Errors:   errs,
	})
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "internal_error")
		return
	}
	w.Header().Set("Content-Type", httpx.ProblemContentType)
	w.WriteHeader(http.StatusUnprocessableEntity)
	_, _ = w.Write(body)
}

// sessionCollisions reports the blocking row errors for one imported row whose
// occurrences collide with an existing live or planned recording of the same
// device and camera (plan §8.2). A camera can only record one session at a
// time, so a collision rejects the whole batch before anything is committed.
// excludeTerm is the batch's term in replace mode: those sessions are
// superseded by the same commit, so they are not collisions.
func (s *server) sessionCollisions(ctx context.Context, row int, excludeTerm string, sessions []domain.Session) ([]domain.ImportError, error) {
	for i := range sessions {
		session := sessions[i]
		if session.Status != domain.SessionPlanned {
			continue
		}
		overlap, err := s.deps.Store.Sessions.OverlappingLive(ctx, session.DeviceID, session.CameraEnum, session.StartsAt, session.EndsAt, session.ID, excludeTerm)
		if err != nil {
			return nil, err
		}
		if !overlap {
			continue
		}
		return []domain.ImportError{{
			Row:     row,
			Column:  "room_code",
			Code:    "session_collision",
			Message: "room " + session.RoomCode + " already has a live or planned recording on device " + session.DeviceID + " camera " + strconv.Itoa(session.CameraEnum) + " at an overlapping time; stop or cancel it before importing",
		}}, nil
	}
	return nil, nil
}

// handleImportCreate answers POST /api/v1/timetable/imports. The multipart
// contract (part name "file", text/csv, 1 MiB, DISPATCH_MAX_IMPORT_ROWS rows),
// the ?dry_run and ?mode and ?force query parameters and the 201/200/422/409
// status codes are documented in the operation description because the OpenAPI
// emitter cannot express them.
func (s *server) handleImportCreate(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	dryRun, ok := parseBoolQuery(w, r, "dry_run", query.Get("dry_run"), false)
	if !ok {
		return
	}
	force, ok := parseBoolQuery(w, r, "force", query.Get("force"), false)
	if !ok {
		return
	}
	mode := strings.TrimSpace(query.Get("mode"))
	if mode == "" {
		mode = store.ModeReplace
	}
	if mode != store.ModeReplace && mode != store.ModeAppend {
		s.writeInvalidRequest(w, r, "mode must be replace or append")
		return
	}

	data, filename, ok := s.readImportFile(w, r)
	if !ok {
		return
	}
	digest := sha256.Sum256(data)
	checksum := hex.EncodeToString(digest[:])

	rows, parseErrs, parseErr := timetable.ParseCSV(bytes.NewReader(data), s.maxImportRows())
	if parseErr != nil {
		parseErrs = append(parseErrs, domain.ImportError{Row: 0, Code: "invalid_csv", Message: parseErr.Error()})
	}
	if len(rows) == 0 && len(parseErrs) == 0 {
		parseErrs = append(parseErrs, domain.ImportError{Row: 0, Code: "empty_file", Message: "the CSV contains no data rows"})
	}
	batchTerm := ""
	if len(rows) > 0 {
		batchTerm = rows[0].TermCode
	}

	if batchTerm == "" {
		// Without rows there is no term to attach the batch row to; the error
		// list is still returned in full.
		writeValidationProblem(w, r, parseErrs)
		return
	}
	term, err := s.deps.Store.Terms.Get(r.Context(), batchTerm)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteProblem(w, r, http.StatusNotFound, "term_not_found")
			return
		}
		s.writeStoreError(w, r, err, "term_not_found")
		return
	}
	if !force {
		existing, err := s.deps.Store.Imports.FindBySHA(r.Context(), batchTerm, checksum)
		switch {
		case err == nil && existing.Status == domain.ImportCommitted:
			httpx.WriteProblem(w, r, http.StatusConflict, "duplicate_import")
			return
		case err == nil, errors.Is(err, store.ErrNotFound):
		default:
			s.writeStoreError(w, r, err, "invalid_request")
			return
		}
	}

	periods, err := s.deps.Store.Terms.Periods(r.Context(), batchTerm)
	if err != nil {
		s.writeStoreError(w, r, err, "term_not_found")
		return
	}
	roomList, err := s.deps.Store.Rooms.List(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err, "invalid_request")
		return
	}
	rooms := make(map[string]domain.Room, len(roomList))
	for _, room := range roomList {
		rooms[room.RoomCode] = room
	}
	// Parse failures are always blocking. Validation separates the blocking
	// rules from non-blocking warnings such as a teacher double-booking, which
	// is reported but never rejects the file (plan §8.1).
	blocking := parseErrs
	var warnings []domain.ImportError
	if len(periods) == 0 {
		blocking = append(blocking, domain.ImportError{Row: 0, Code: "periods_not_configured", Message: "the term has no 节次 table; PUT /api/v1/terms/" + batchTerm + "/periods first"})
	} else {
		for _, validationErr := range timetable.Validate(rows, term, periods, rooms) {
			if timetable.IsBlocking(validationErr.Code) {
				blocking = append(blocking, validationErr)
			} else {
				warnings = append(warnings, validationErr)
			}
		}
	}

	claims, _ := iamauth.ClaimsFromContext(r.Context())
	imp := domain.Import{
		ID:         id.New(),
		TermCode:   batchTerm,
		Mode:       mode,
		Filename:   filename,
		SHA256:     checksum,
		RowCount:   importRowCount(rows, append(append([]domain.ImportError{}, blocking...), warnings...)),
		Errors:     []domain.ImportError{},
		ImportedBy: claims.Subject,
	}

	periodsByNo := make(map[int]domain.Period, len(periods))
	for _, period := range periods {
		periodsByNo[period.PeriodNo] = period
	}
	now := time.Now()
	entries := make([]domain.Entry, 0, len(rows))
	sessions := make([]domain.Session, 0, len(rows))
	preview := make([]importPreview, 0, len(rows))
	// In replace mode the batch's own term is being superseded by this commit,
	// so its pending sessions are not collisions; an append import must still
	// see them.
	collisionExcludeTerm := ""
	if mode == store.ModeReplace {
		collisionExcludeTerm = batchTerm
	}
	// Validation failures make materialization meaningless; the batch is
	// recorded as failed with the error list below.
	if len(blocking) == 0 {
		for _, row := range rows {
			entry := domain.Entry{
				ID:              id.New(),
				ImportID:        imp.ID,
				TermCode:        batchTerm,
				CourseCode:      row.CourseCode,
				CourseName:      row.CourseName,
				TeacherUsername: row.TeacherUsername,
				RoomCode:        row.RoomCode,
				Weekday:         row.Weekday,
				PeriodStart:     row.PeriodStart,
				PeriodEnd:       row.PeriodEnd,
				Weeks:           row.Weeks,
			}
			materialized, err := timetable.Materialize(entry, term, periodsByNo, s.location(), now)
			if err != nil {
				blocking = append(blocking, domain.ImportError{Row: row.Line, Code: "materialize_failed", Message: err.Error()})
				break
			}
			room := rooms[row.RoomCode]
			for i := range materialized {
				session := &materialized[i]
				if session.ID == "" {
					session.ID = id.New()
				}
				if session.Status == "" {
					session.Status = domain.SessionPlanned
				}
				session.EntryID = &entry.ID
				session.RoomCode = row.RoomCode
				session.DeviceID = room.DeviceID
				session.CameraEnum = room.CameraEnum
				session.Origin = domain.OriginSchedule
			}
			collisions, cerr := s.sessionCollisions(r.Context(), row.Line, collisionExcludeTerm, materialized)
			if cerr != nil {
				s.writeStoreError(w, r, cerr, "invalid_request")
				return
			}
			blocking = append(blocking, collisions...)
			entries = append(entries, entry)
			sessions = append(sessions, materialized...)
			preview = append(preview, newImportPreview(row.Line, row.CourseCode, materialized))
		}
	}

	if len(blocking) > 0 {
		reported := append(append([]domain.ImportError{}, blocking...), warnings...)
		imp.Status = domain.ImportFailed
		imp.OKCount = 0
		imp.ErrorCount = len(reported)
		imp.Errors = reported
		if _, err := s.deps.Store.Imports.Create(r.Context(), imp); err != nil {
			s.log.Error("record failed import batch", "import_id", imp.ID, "error", err)
		}
		writeValidationProblem(w, r, reported)
		return
	}

	// Non-blocking warnings stay visible in the stored batch.
	imp.OKCount = len(entries)
	imp.ErrorCount = len(warnings)
	if len(warnings) > 0 {
		imp.Errors = warnings
	}
	if dryRun {
		imp.Status = domain.ImportDryRun
		stored, err := s.deps.Store.Imports.Create(r.Context(), imp)
		if err != nil {
			s.writeStoreError(w, r, err, "invalid_request")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, importResult{Import: stored, Preview: preview})
		return
	}

	imp.Status = domain.ImportCommitted
	if err := s.deps.Store.Imports.Commit(r.Context(), imp, entries, sessions); err != nil {
		if errors.Is(err, store.ErrConflict) {
			httpx.WriteProblem(w, r, http.StatusConflict, "duplicate_import")
			return
		}
		s.writeStoreError(w, r, err, "term_not_found")
		return
	}
	w.Header().Set("Location", "/api/v1/timetable/imports/"+imp.ID)
	httpx.WriteJSON(w, http.StatusCreated, importResult{Import: imp})
}

// readImportFile reads the multipart "file" part, enforcing the 1 MiB cap.
func (s *server) readImportFile(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes+multipartAllowance)
	file, header, err := r.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.WriteProblemReason(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "the multipart body is too large")
			return nil, "", false
		}
		s.writeInvalidRequest(w, r, `a multipart form with a file part named "file" is required`)
		return nil, "", false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxImportBytes+1))
	if err != nil {
		s.writeInvalidRequest(w, r, "the uploaded file could not be read")
		return nil, "", false
	}
	if int64(len(data)) > maxImportBytes {
		httpx.WriteProblemReason(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "the CSV file must not exceed 1 MiB")
		return nil, "", false
	}
	filename := filepath.Base(strings.TrimSpace(header.Filename))
	if filename == "" || filename == "." || filename == "/" {
		filename = "timetable.csv"
	}
	return data, filename, true
}

// newImportPreview summarizes one row's materialized sessions.
func newImportPreview(row int, courseCode string, sessions []domain.Session) importPreview {
	preview := importPreview{Row: row, CourseCode: courseCode, SessionCount: len(sessions)}
	if len(sessions) == 0 {
		return preview
	}
	preview.FirstStartsAt = sessions[0].StartsAt
	preview.LastEndsAt = sessions[0].EndsAt
	for _, session := range sessions[1:] {
		if session.StartsAt.Before(preview.FirstStartsAt) {
			preview.FirstStartsAt = session.StartsAt
		}
		if session.EndsAt.After(preview.LastEndsAt) {
			preview.LastEndsAt = session.EndsAt
		}
	}
	return preview
}

// importRowCount reports the number of data rows read: the accepted rows plus
// every distinct row that produced an error.
func importRowCount(rows []timetable.Row, errs []domain.ImportError) int {
	failed := make(map[int]struct{})
	for _, importErr := range errs {
		if importErr.Row > 0 {
			failed[importErr.Row] = struct{}{}
		}
	}
	return len(rows) + len(failed)
}

// parseBoolQuery parses a boolean query parameter, answering 400 when it is
// neither true nor false.
func parseBoolQuery(w http.ResponseWriter, r *http.Request, name, value string, fallback bool) (bool, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, true
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		httpx.WriteProblemReason(w, r, http.StatusBadRequest, "invalid_request", name+" must be true or false")
		return false, false
	}
	return parsed, true
}
