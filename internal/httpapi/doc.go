// Package httpapi exposes dispatchub's HTTP API over the chi router built by
// NewRouter.
//
// # Routes
//
// Public endpoints:
//
//	GET /healthz  liveness probe
//	GET /readyz   readiness probe (PostgreSQL ping + webcam-server /readyz)
//
// Endpoints under /api/v1 require a teamusers Bearer token carrying a
// dispatch:read, dispatch:manage or dispatch:control permission scoped to own,
// team or any (401 without verified claims, 403 when denied, 503 when
// teamusers cannot answer). Room- and session-scoped routes resolve their
// resource first and try the any, team and own rungs in that order:
//
//	GET    /api/v1/terms                              list terms
//	POST   /api/v1/terms                              create or replace a term
//	GET    /api/v1/terms/{term_code}/periods          fetch the 节次 table
//	PUT    /api/v1/terms/{term_code}/periods          replace the 节次 table
//	GET    /api/v1/rooms                              list room bindings
//	PUT    /api/v1/rooms/{room_code}                  bind a room to a device
//	DELETE /api/v1/rooms/{room_code}                  unbind a room
//	POST   /api/v1/timetable/imports                  import a CSV timetable
//	GET    /api/v1/timetable/imports                  list import batches
//	GET    /api/v1/timetable/imports/{id}             fetch one import batch
//	GET    /api/v1/sessions                           list recording sessions
//	GET    /api/v1/sessions/{id}                      fetch one session
//	GET    /api/v1/sessions/{id}/artifacts            list recorded segments and photos
//	POST   /api/v1/sessions/{id}/recording/start      start a planned session
//	POST   /api/v1/sessions/{id}/recording/stop       stop a session
//	POST   /api/v1/rooms/{room_code}/recording/start  ad-hoc start
//	POST   /api/v1/rooms/{room_code}/recording/stop   ad-hoc stop
//	POST   /api/v1/rooms/{room_code}/camera/switch    switch the active camera
//	POST   /api/v1/rooms/{room_code}/photo            capture a snapshot
//	GET    /api/v1/rooms/{room_code}/live             live room state
//
// # Responses
//
// Collection endpoints return {"items": [...]}. Single-resource endpoints
// return the resource object itself. Errors use RFC 9457 problem details
// (application/problem+json) whose detail member is one of the stable error
// codes; the missing-claims 401 emitted by teamusers uses the SDK's own
// {"allow": false, "reason": ...} shape instead.
//
// DocOperations lists every route together with its request and response
// shapes. cmd/genspec walks the router and reflects the table into
// docs/public/openapi.yaml, failing when a route and its metadata drift apart.
package httpapi

import (
	"net/http"

	apidocs "github.com/crazy4chicken/nsc-teamusers/apidocs/go"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
)

// DocPermissionDeriver returns the dispatch:<action>:any and
// dispatch:<action>:team keys that guard method and path for the generated
// reference. The own-scoped rung depends on the resolved room's owner_id, so it
// cannot be derived statically and is documented in the guide and permission
// notes only. Collection routes and terms carry no resource identity, so they
// declare the any-scoped key alone.
func DocPermissionDeriver(method, path string) (anyKey, teamKey string) {
	var action string
	switch path {
	case "/healthz", "/readyz":
		return "", ""
	case "/api/v1/terms":
		if method == http.MethodPost {
			action = "manage"
		} else {
			action = "read"
		}
		return "dispatch:" + action + ":any", ""
	case "/api/v1/terms/{term_code}/periods":
		if method == http.MethodPut {
			action = "manage"
		} else {
			action = "read"
		}
		return "dispatch:" + action + ":any", ""
	case "/api/v1/rooms":
		return "dispatch:read:any", ""
	case "/api/v1/rooms/{room_code}":
		return "dispatch:manage:any", "dispatch:manage:team"
	case "/api/v1/timetable/imports":
		if method == http.MethodPost {
			action = "manage"
		} else {
			action = "read"
		}
		return "dispatch:" + action + ":any", ""
	case "/api/v1/timetable/imports/{id}":
		return "dispatch:read:any", ""
	case "/api/v1/sessions":
		return "dispatch:read:any", ""
	case "/api/v1/sessions/{id}", "/api/v1/sessions/{id}/artifacts", "/api/v1/rooms/{room_code}/live":
		return "dispatch:read:any", "dispatch:read:team"
	case "/api/v1/sessions/{id}/recording/start",
		"/api/v1/sessions/{id}/recording/stop",
		"/api/v1/rooms/{room_code}/recording/start",
		"/api/v1/rooms/{room_code}/recording/stop",
		"/api/v1/rooms/{room_code}/camera/switch",
		"/api/v1/rooms/{room_code}/photo":
		return "dispatch:control:any", "dispatch:control:team"
	default:
		return "", ""
	}
}

// docError builds one documented problem response. The code is rendered as the
// detail of the response example, matching the problem body handlers emit.
func docError(status int, code, title string) apidocs.ErrorDoc {
	return apidocs.ErrorDoc{Status: status, Code: code, Title: title}
}

var (
	docUnauthorized            = docError(401, "authentication is required", "Unauthorized")
	docForbidden               = docError(403, "permission denied", "Forbidden")
	docInvalidRequest          = docError(400, "invalid_request", "Invalid Request")
	docTermNotFound            = docError(404, "term_not_found", "Not Found")
	docPeriodsNotConfigured    = docError(404, "periods_not_configured", "Not Found")
	docRoomNotBound            = docError(404, "room_not_bound", "Not Found")
	docDeviceNotFound          = docError(404, "device_not_found", "Not Found")
	docSessionNotFound         = docError(404, "session_not_found", "Not Found")
	docSessionNotPlanned       = docError(409, "session_not_planned", "Conflict")
	docAlreadyStreaming        = docError(409, "already_streaming", "Conflict")
	docDeviceOffline           = docError(409, "device_offline", "Conflict")
	docNoActiveStream          = docError(404, "no_active_stream", "Not Found")
	docDuplicateIdempotencyKey = docError(409, "duplicate_idempotency_key", "Conflict")
	docDuplicateImport         = docError(409, "duplicate_import", "Conflict")
	docRoomHasHistory          = docError(409, "room_has_history", "Conflict")
	docSessionCollision        = docError(422, "session_collision", "Unprocessable Content")
	docImportValidationFailed  = docError(422, "import_validation_failed", "Unprocessable Content")
	docUpstreamUnavailable     = docError(503, "upstream_unavailable", "Service Unavailable")
	docInternalError           = docError(500, "internal_error", "Internal Server Error")
	docConflictInvalidRequest  = docError(409, "invalid_request", "Conflict")
)

// permissionNoteRoom documents the dynamic own rung for room-scoped routes.
const permissionNoteRoom = "own-scoped access additionally requires the corresponding dispatch:*:own key and rooms.owner_id equal to the token subject."

// permissionNoteRoomBinding documents the create-versus-rebind split of the
// binding route: a room that does not exist yet has no owner to match, so only
// the any-scoped key can create it.
const permissionNoteRoomBinding = "creating a new binding requires dispatch:manage:any; re-binding an existing room also accepts dispatch:manage:team or dispatch:manage:own when the room's ownership snapshot matches the caller."

// permissionNoteSession documents the dynamic own rung for session routes.
const permissionNoteSession = "own-scoped access additionally requires the corresponding dispatch:*:own key and the session's room owner_id equal to the token subject."

// permissionNoteCollection documents why collection routes have no team rung.
const permissionNoteCollection = "collection routes require the any-scoped key: rows are returned unfiltered, so no narrower scope can grant access."

var (
	termExample = map[string]any{
		"term_code":    "2026-FALL",
		"name":         "2026 秋季学期",
		"week1_monday": "2026-09-07T00:00:00Z",
		"weeks":        16,
		"created_at":   "2026-08-20T09:00:00Z",
		"updated_at":   "2026-08-20T09:00:00Z",
	}
	periodExample = map[string]any{
		"period_no":  1,
		"start_time": "08:00",
		"end_time":   "08:45",
	}
	roomExample = map[string]any{
		"room_code":   "A301",
		"name":        "Building A / Room 301",
		"device_id":   "01J8Z4W3K5M7Q9R1T3V5X7Z9B1",
		"camera_enum": 0,
		"team_id":     "01J8Z2TEAM5A7C9E1G3J5L7N9P1",
		"owner_id":    "01J8Z1USER3Y5W7A9C1E3G5J7L9",
		"enabled":     true,
		"created_at":  "2026-08-20T09:00:00Z",
		"updated_at":  "2026-08-20T09:00:00Z",
	}
	importExample = map[string]any{
		"id":          "01J8ZC1IMP2M4P6R8T0V2X4Z6B8",
		"term_code":   "2026-FALL",
		"mode":        "replace",
		"filename":    "timetable-2026-fall.csv",
		"sha256":      "9f2c1d4e5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d",
		"row_count":   3,
		"ok_count":    3,
		"error_count": 0,
		"errors":      []any{},
		"status":      "committed",
		"imported_by": "01J8Z1USER3Y5W7A9C1E3G5J7L9",
		"created_at":  "2026-08-21T10:30:00Z",
	}
	importDryRunExample = map[string]any{
		"id":          "01J8ZC1IMP2M4P6R8T0V2X4Z6B8",
		"term_code":   "2026-FALL",
		"mode":        "replace",
		"filename":    "timetable-2026-fall.csv",
		"sha256":      "9f2c1d4e5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d",
		"row_count":   3,
		"ok_count":    3,
		"error_count": 0,
		"errors":      []any{},
		"status":      "dry_run",
		"imported_by": "01J8Z1USER3Y5W7A9C1E3G5J7L9",
		"created_at":  "2026-08-21T10:30:00Z",
		"preview":     []any{importPreviewExample},
	}
	importPreviewExample = map[string]any{
		"row":             2,
		"course_code":     "CS101",
		"session_count":   16,
		"first_starts_at": "2026-09-07T08:00:00+08:00",
		"last_ends_at":    "2026-12-21T09:40:00+08:00",
	}
	sessionExample = map[string]any{
		"id":          "01J8ZD2SES3N5Q7S9U1W3Y5A7C9",
		"entry_id":    "01J8ZE3ENT4P6R8T0V2X4Z6B8D0",
		"room_code":   "A301",
		"device_id":   "01J8Z4W3K5M7Q9R1T3V5X7Z9B1",
		"camera_enum": 0,
		"starts_at":   "2026-09-07T08:00:00+08:00",
		"ends_at":     "2026-09-07T09:40:00+08:00",
		"status":      "completed",
		"origin":      "schedule",
		"stream_id":   "01J8ZF4STR5Q7T9V1X3Z5B7D9F1",
		"started_at":  "2026-09-07T07:58:02+08:00",
		"stopped_at":  "2026-09-07T09:40:11+08:00",
		"retry_count": 0,
		"created_at":  "2026-08-21T10:30:00Z",
		"updated_at":  "2026-09-07T01:40:11Z",
	}
	photoExample = map[string]any{
		"id":          "01J8ZG5PHO6R8U0W2Y4A6C8E0G2",
		"session_id":  "01J8ZD2SES3N5Q7S9U1W3Y5A7C9",
		"room_code":   "A301",
		"device_id":   "01J8Z4W3K5M7Q9R1T3V5X7Z9B1",
		"camera_enum": 0,
		"request_id":  "01J8ZH6REQ7S9V1X3Z5B7D9F1H3",
		"photo_id":    "01J8ZJ7PHO8T0W2Y4A6C8E0G2J4",
		"source":      "manual",
		"status":      "resolved",
		"attempts":    1,
		"taken_at":    "2026-09-07T08:20:00+08:00",
		"resolved_at": "2026-09-07T08:20:04+08:00",
		"created_at":  "2026-09-07T08:20:00+08:00",
	}
	sessionDetailExample = map[string]any{
		"id":          "01J8ZD2SES3N5Q7S9U1W3Y5A7C9",
		"entry_id":    "01J8ZE3ENT4P6R8T0V2X4Z6B8D0",
		"room_code":   "A301",
		"device_id":   "01J8Z4W3K5M7Q9R1T3V5X7Z9B1",
		"camera_enum": 0,
		"starts_at":   "2026-09-07T08:00:00+08:00",
		"ends_at":     "2026-09-07T09:40:00+08:00",
		"status":      "recording",
		"origin":      "schedule",
		"stream_id":   "01J8ZF4STR5Q7T9V1X3Z5B7D9F1",
		"started_at":  "2026-09-07T07:58:02+08:00",
		"retry_count": 0,
		"created_at":  "2026-08-21T10:30:00Z",
		"updated_at":  "2026-09-07T00:00:02Z",
		"photos":      []any{photoExample},
		"upstream": map[string]any{
			"id":          "01J8ZF4STR5Q7T9V1X3Z5B7D9F1",
			"device_id":   "01J8Z4W3K5M7Q9R1T3V5X7Z9B1",
			"camera_enum": 0,
			"status":      "active",
			"started_at":  "2026-09-07T07:58:02+08:00",
		},
	}
	artifactsExample = map[string]any{
		"stream_id": "01J8ZF4STR5Q7T9V1X3Z5B7D9F1",
		"status":    "completed",
		"segments": []any{
			map[string]any{
				"segment_seq":  1,
				"size_bytes":   1048576,
				"duration_ms":  6000,
				"download_url": "https://files.example.edu/webcam/segments/01J8ZF4STR5Q7T9V1X3Z5B7D9F1/1.bin?X-Amz-Expires=900",
			},
		},
		"photos": []any{
			map[string]any{
				"photo_id":     "01J8ZJ7PHO8T0W2Y4A6C8E0G2J4",
				"taken_at":     "2026-09-07T08:20:00+08:00",
				"download_url": "https://files.example.edu/webcam/photos/01J8ZJ7PHO8T0W2Y4A6C8E0G2J4?X-Amz-Expires=900",
			},
		},
	}
	liveExample = map[string]any{
		"online": true,
		"cameras": []any{
			map[string]any{
				"camera_enum":     0,
				"resolution":      "1920x1080",
				"fps":             30,
				"supported_codec": []any{"h264", "mjpeg"},
			},
		},
		"active_session": map[string]any{
			"id":         "01J8ZD2SES3N5Q7S9U1W3Y5A7C9",
			"stream_id":  "01J8ZF4STR5Q7T9V1X3Z5B7D9F1",
			"started_at": "2026-09-07T07:58:02+08:00",
		},
	}
	cameraSwitchExample = map[string]any{
		"command_id":  "01J8ZK8CMD9U1W3Y5A7C9E1G3J5",
		"camera_enum": 1,
	}
	photoAcceptedExample = map[string]any{
		"session_photo_id": "01J8ZG5PHO6R8U0W2Y4A6C8E0G2",
		"request_id":       "01J8ZH6REQ7S9V1X3Z5B7D9F1H3",
		"status":           "pending",
	}
)

// DocOperations is the route contract used by the OpenAPI generator. Every
// route served by NewRouter must appear here exactly once, with its chi
// pattern, so cmd/genspec fails instead of publishing a stale reference. The
// emitter cannot express query parameters, multipart bodies, 201/202/422
// statuses or the X-Idempotency-Key header, so those live in the operation
// descriptions.
var DocOperations = []apidocs.Operation{
	{
		Method:          "GET",
		Path:            "/healthz",
		Tag:             "Health",
		Summary:         "Check liveness",
		Description:     "Use this unauthenticated probe to decide whether the HTTP process is alive. It never touches PostgreSQL or webcam-server, so it keeps answering ok during an upstream outage and is safe to call on a short interval.",
		Response:        statusResponse{},
		ResponseExample: map[string]string{"status": "ok"},
		Errors:          []apidocs.ErrorDoc{},
	},
	{
		Method:          "GET",
		Path:            "/readyz",
		Tag:             "Health",
		Summary:         "Check readiness",
		Description:     "Use this unauthenticated probe on the operator dashboard. It pings PostgreSQL and calls webcam-server's /readyz with a 5-second timeout; both must answer, otherwise the probe reports 503 with detail upstream_unavailable and names the unreachable dependencies in the reason member.",
		Response:        statusResponse{},
		ResponseExample: map[string]string{"status": "ready"},
		Errors:          []apidocs.ErrorDoc{docUpstreamUnavailable},
	},

	{
		Method:          "GET",
		Path:            "/api/v1/terms",
		Tag:             "Terms",
		Summary:         "List terms",
		Description:     "Use to enumerate the teaching terms. Pass limit to cap the number returned; the default is 100, values above 1000 are clamped and a non-positive or non-numeric limit answers 400 invalid_request. Terms are not filtered by scope: " + permissionNoteCollection,
		Security:        "bearerAuth",
		Response:        listResponse[domain.Term]{},
		ResponseExample: map[string]any{"items": []any{termExample}},
		Errors:          []apidocs.ErrorDoc{docInvalidRequest, docUnauthorized, docForbidden, docInternalError},
	},
	{
		Method:      "POST",
		Path:        "/api/v1/terms",
		Tag:         "Terms",
		Summary:     "Create or replace a term",
		Description: "Use to create a term or replace its fields. term_code identifies the term, week1_monday is the Monday of teaching week 1 as YYYY-MM-DD and weeks counts teaching weeks (1-30). Posting an existing term_code replaces name, week1_monday and weeks, which keeps a corrected re-import deterministic. The request answers 201 Created with the stored term.",
		Security:    "bearerAuth",
		Request:     termRequest{},
		RequestExample: map[string]any{
			"term_code":    "2026-FALL",
			"name":         "2026 秋季学期",
			"week1_monday": "2026-09-07",
			"weeks":        16,
		},
		Response:        domain.Term{},
		ResponseExample: termExample,
		Errors: []apidocs.ErrorDoc{
			docInvalidRequest,
			docUnauthorized,
			docForbidden,
			docConflictInvalidRequest,
			docInternalError,
		},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/terms/{term_code}/periods",
		Tag:             "Terms",
		Summary:         "Get a term's period table",
		Description:     "Use to fetch the term's 节次 table. Periods define the wall-clock window of every period_no; imports validate rows against them. A term without a configured table answers 404 periods_not_configured instead of an empty list.",
		Security:        "bearerAuth",
		Response:        listResponse[domain.Period]{},
		ResponseExample: map[string]any{"items": []any{periodExample}},
		Errors:          []apidocs.ErrorDoc{docUnauthorized, docForbidden, docTermNotFound, docPeriodsNotConfigured, docInternalError},
	},
	{
		Method:      "PUT",
		Path:        "/api/v1/terms/{term_code}/periods",
		Tag:         "Terms",
		Summary:     "Replace a term's period table",
		Description: "Use to replace the term's whole 节次 table in one call. The body is {\"periods\":[{\"period_no\":N,\"start_time\":\"HH:MM\",\"end_time\":\"HH:MM\"}]}; period_no must be 1-20 and unique, both times are HH:MM local time and end_time must be after start_time. The period numbers of existing sessions are not re-validated, so re-import a term's timetable after changing its table. The response is 200 with the replaced table.",
		Security:    "bearerAuth",
		Request:     periodsRequest{},
		RequestExample: map[string]any{
			"periods": []any{
				map[string]any{"period_no": 1, "start_time": "08:00", "end_time": "08:45"},
				map[string]any{"period_no": 2, "start_time": "08:55", "end_time": "09:40"},
			},
		},
		Response:        listResponse[domain.Period]{},
		ResponseExample: map[string]any{"items": []any{periodExample}},
		Errors:          []apidocs.ErrorDoc{docInvalidRequest, docUnauthorized, docForbidden, docTermNotFound, docInternalError},
	},

	{
		Method:          "GET",
		Path:            "/api/v1/rooms",
		Tag:             "Rooms",
		Summary:         "List room bindings",
		Description:     "Use to enumerate the room_code → (device_id, camera_enum) bindings. Pass limit to cap the number returned; the default is 100, values above 1000 are clamped and a non-positive or non-numeric limit answers 400 invalid_request. Rooms are not filtered by scope: " + permissionNoteCollection,
		Security:        "bearerAuth",
		Response:        listResponse[domain.Room]{},
		ResponseExample: map[string]any{"items": []any{roomExample}},
		Errors:          []apidocs.ErrorDoc{docInvalidRequest, docUnauthorized, docForbidden, docInternalError},
	},
	{
		Method:         "PUT",
		Path:           "/api/v1/rooms/{room_code}",
		Tag:            "Rooms",
		Summary:        "Bind a room",
		Description:    "Use to bind a room_code to a webcam-server device and camera. The device is validated live with GET /api/devices/{device_id}/ on webcam-server and its team_id and owner_id are snapshotted onto the binding, because the permission ladder resolves against them; re-binding refreshes the snapshot. enabled defaults to true and can be set false to keep the binding without scheduling; it can be flipped at any time, even once the room has timetable or session history and can no longer be deleted (DELETE answers 409 room_has_history), so disabling is the supported way to take a used room out of service. The request answers 200 with the stored binding, 404 device_not_found when webcam-server does not know the device, 409 invalid_request when device_id and camera_enum are already bound to another room, and 503 upstream_unavailable when webcam-server cannot be reached.",
		Security:       "bearerAuth",
		PermissionNote: permissionNoteRoomBinding,
		Request:        roomRequest{},
		RequestExample: map[string]any{
			"name":        "Building A / Room 301",
			"device_id":   "01J8Z4W3K5M7Q9R1T3V5X7Z9B1",
			"camera_enum": 0,
		},
		Response:        domain.Room{},
		ResponseExample: roomExample,
		Errors: []apidocs.ErrorDoc{
			docInvalidRequest,
			docUnauthorized,
			docForbidden,
			docDeviceNotFound,
			docConflictInvalidRequest,
			docUpstreamUnavailable,
			docInternalError,
		},
	},
	{
		Method:         "DELETE",
		Path:           "/api/v1/rooms/{room_code}",
		Tag:            "Rooms",
		Summary:        "Unbind a room",
		Description:    "Use to remove a room binding that nothing references. Once timetable or session history references the room the foreign keys keep the row alive, so the call answers 409 room_has_history and the operator should disable the binding instead with PUT /api/v1/rooms/{room_code} and {\"enabled\": false}, which takes it out of scheduling while keeping history readable. A successful delete answers 204 No Content; an unknown room answers 404 room_not_bound.",
		Security:       "bearerAuth",
		PermissionNote: permissionNoteRoom,
		Errors: []apidocs.ErrorDoc{
			docUnauthorized,
			docForbidden,
			docRoomNotBound,
			docRoomHasHistory,
			docInternalError,
		},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/rooms/{room_code}/live",
		Tag:             "Rooms",
		Summary:         "Get live room state",
		Description:     "Use to fetch what the room's device is doing right now: online comes from webcam-server's device record, cameras lists the current registration's parameters and active_session is dispatchub's live session for the room when one exists. The call is proxied from webcam-server on every request, never cached; an unreachable upstream answers 503 upstream_unavailable and an unknown device 404 device_not_found.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteRoom,
		Response:        roomLiveResponse{},
		ResponseExample: liveExample,
		Errors:          []apidocs.ErrorDoc{docUnauthorized, docForbidden, docRoomNotBound, docDeviceNotFound, docUpstreamUnavailable, docInternalError},
	},

	{
		Method:          "POST",
		Path:            "/api/v1/timetable/imports",
		Tag:             "Timetable",
		Summary:         "Import a timetable CSV",
		Description:     "Use to upload one term's timetable as a multipart/form-data body whose single file part is named file (text/csv, at most 1 MiB and at most DISPATCH_MAX_IMPORT_ROWS data rows, default 5000). The header row is mandatory and matched by name in any order: term_code,course_code,course_name,teacher_username,room_code,weekday,period_start,period_end,weeks; unknown or missing columns reject the whole file. Every row must carry the same term_code, the term must exist, its 节次 table must cover period_start..period_end, room_code must be a bound enabled room and weeks must stay within the term's teaching weeks. Overlapping rows for the same room, weekday, period range and weeks are rejected; a teacher double-booking is not. A row whose occurrences collide with an existing live or planned recording on the same (device_id, camera_enum) is rejected with code session_collision, because one camera cannot record two sessions at once; in replace mode the batch's own term is excluded from that check, since the commit supersedes its pending sessions, while an append import still collides with them. Query parameters: mode=replace|append (default replace; replace supersedes the term's previous entries and cancels their future planned sessions, already started or completed sessions are never touched), dry_run=true (validate only, no entries or sessions are written) and force=true (commit even though the same sha256 was already committed for the term). A dry run answers 200 with the stored batch object plus a preview array ([{row,course_code,session_count,first_starts_at,last_ends_at}]) so a wrong week1_monday or period table is visible before anything is written; a commit answers 201 Created with the stored batch and a Location header; a blocking row error answers 422 with detail import_validation_failed and the full errors array ([{row,column,code,message}]) while writing the batch with status failed and nothing else; a teacher double-booking is a non-blocking warning that is stored in the batch errors array without rejecting the file, so a committed batch can carry warnings and error_count counts every reported entry; an identical already-committed file answers 409 duplicate_import unless force=true.",
		Security:        "bearerAuth",
		Response:        importResult{},
		ResponseExample: importDryRunExample,
		Errors: []apidocs.ErrorDoc{
			docInvalidRequest,
			docUnauthorized,
			docForbidden,
			docTermNotFound,
			docDuplicateImport,
			docSessionCollision,
			docImportValidationFailed,
			docInternalError,
		},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/timetable/imports",
		Tag:             "Timetable",
		Summary:         "List import batches",
		Description:     "Use to review the import history, newest first. Pass term_code to restrict the list to one term and limit to cap the number returned; the default is 100, values above 1000 are clamped and a non-positive or non-numeric limit answers 400 invalid_request. Each batch carries its status (dry_run, committed or failed), row counts and the per-row error list of the failed attempt.",
		Security:        "bearerAuth",
		Response:        listResponse[domain.Import]{},
		ResponseExample: map[string]any{"items": []any{importExample}},
		Errors:          []apidocs.ErrorDoc{docInvalidRequest, docUnauthorized, docForbidden, docInternalError},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/timetable/imports/{id}",
		Tag:             "Timetable",
		Summary:         "Get an import batch",
		Description:     "Use to fetch one batch together with the per-row errors of a failed validation, so the operator can correct the CSV. An unknown batch id answers 404 invalid_request.",
		Security:        "bearerAuth",
		Response:        domain.Import{},
		ResponseExample: importExample,
		Errors:          []apidocs.ErrorDoc{docUnauthorized, docForbidden, docInvalidRequest, docInternalError},
	},

	{
		Method:          "GET",
		Path:            "/api/v1/sessions",
		Tag:             "Sessions",
		Summary:         "List recording sessions",
		Description:     "Use to browse sessions, newest starts_at first. Query parameters: term_code, room_code, status (one of planned, starting, recording, stopping, completed, failed, canceled, missed), date=YYYY-MM-DD (a UTC calendar day matched against starts_at, independent of DISPATCH_TIMEZONE), course_code (scheduled sessions only, manual ones have no entry) and limit (default 100, clamped to 1000). An invalid status, date or limit answers 400 invalid_request. Sessions are not filtered by scope: " + permissionNoteCollection,
		Security:        "bearerAuth",
		Response:        listResponse[domain.Session]{},
		ResponseExample: map[string]any{"items": []any{sessionExample}},
		Errors:          []apidocs.ErrorDoc{docInvalidRequest, docUnauthorized, docForbidden, docInternalError},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/sessions/{id}",
		Tag:             "Sessions",
		Summary:         "Get a session",
		Description:     "Use to fetch one session together with its photo ledger and the live upstream stream when a stream_id is stored. The upstream lookup is best-effort: a webcam-server outage degrades the response to the stored session instead of failing it. An unknown session answers 404 session_not_found.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteSession,
		Response:        sessionDetail{},
		ResponseExample: sessionDetailExample,
		Errors:          []apidocs.ErrorDoc{docUnauthorized, docForbidden, docSessionNotFound, docInternalError},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/sessions/{id}/artifacts",
		Tag:             "Sessions",
		Summary:         "List session artifacts",
		Description:     "Use to enumerate a session's recorded segments and resolved photos. Segments are re-derived from GET /api/streams/{stream_id}/ on webcam-server and photos are resolved through the session's photo ledger; every download_url is minted by webcam-server for this response, is valid for about 15 minutes and is never cached by dispatchub, so request the artifacts again when a URL expires. A session without a stored stream_id reports its own status with empty lists, and an unreachable webcam-server answers 503 upstream_unavailable.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteSession,
		Response:        artifactsResponse{},
		ResponseExample: artifactsExample,
		Errors:          []apidocs.ErrorDoc{docUnauthorized, docForbidden, docSessionNotFound, docUpstreamUnavailable, docInternalError},
	},

	{
		Method:          "POST",
		Path:            "/api/v1/sessions/{id}/recording/start",
		Tag:             "Recording",
		Summary:         "Start a session recording",
		Description:     "Use for the manual early start of a planned session or to retry a failed one. The scheduler claims the session, calls webcam-server's recording/start and stores the returned stream_id; the request answers 200 with the updated session. Send X-Idempotency-Key to make the call replayable: the first accepted command is stored in camera_commands and a replayed key does not issue a second upstream command (a conflicting reuse answers 409 duplicate_idempotency_key). Other failures: 409 session_not_planned, 409 device_offline, 409 already_streaming when the camera is already recording, 404 session_not_found and 503 upstream_unavailable.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteSession,
		Response:        domain.Session{},
		ResponseExample: sessionExample,
		Errors: []apidocs.ErrorDoc{
			docUnauthorized,
			docForbidden,
			docSessionNotFound,
			docSessionNotPlanned,
			docDeviceOffline,
			docAlreadyStreaming,
			docDuplicateIdempotencyKey,
			docUpstreamUnavailable,
			docInternalError,
		},
	},
	{
		Method:          "POST",
		Path:            "/api/v1/sessions/{id}/recording/stop",
		Tag:             "Recording",
		Summary:         "Stop a session recording",
		Description:     "Use to end a session's recording before its scheduled end. The scheduler calls webcam-server's recording/stop, stores ended_at as stopped_at and marks the session completed; a stream that already finished upstream counts as success and the stored stream row is reconciled instead. The request answers 200 with the updated session; X-Idempotency-Key behaves as on start. Other failures: 404 session_not_found, 404 no_active_stream, 409 device_offline, 409 duplicate_idempotency_key and 503 upstream_unavailable.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteSession,
		Response:        domain.Session{},
		ResponseExample: sessionExample,
		Errors: []apidocs.ErrorDoc{
			docUnauthorized,
			docForbidden,
			docSessionNotFound,
			docNoActiveStream,
			docDeviceOffline,
			docDuplicateIdempotencyKey,
			docUpstreamUnavailable,
			docInternalError,
		},
	},

	{
		Method:          "POST",
		Path:            "/api/v1/rooms/{room_code}/recording/start",
		Tag:             "Recording",
		Summary:         "Start an ad-hoc recording",
		Description:     "Use to start recording a room outside its timetable. The scheduler creates a manual session for the room's binding, calls webcam-server's recording/start and stores the stream_id; the manual session ends when it is stopped explicitly (there is no scheduled end). The request answers 201 Created with the new session and its Location header. It has no body; send X-Idempotency-Key to make it replayable. Failures: 404 room_not_bound, 409 device_offline, 409 already_streaming, 409 duplicate_idempotency_key and 503 upstream_unavailable.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteRoom,
		Response:        domain.Session{},
		ResponseExample: sessionExample,
		Errors: []apidocs.ErrorDoc{
			docUnauthorized,
			docForbidden,
			docRoomNotBound,
			docDeviceOffline,
			docAlreadyStreaming,
			docDuplicateIdempotencyKey,
			docUpstreamUnavailable,
			docInternalError,
		},
	},
	{
		Method:          "POST",
		Path:            "/api/v1/rooms/{room_code}/recording/stop",
		Tag:             "Recording",
		Summary:         "Stop the room's live recording",
		Description:     "Use to stop whichever session is live in the room, scheduled or manual. The request has no body and answers 200 with the stopped session; X-Idempotency-Key behaves as on start. Failures: 404 room_not_bound, 404 no_active_stream, 409 device_offline, 409 duplicate_idempotency_key and 503 upstream_unavailable.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteRoom,
		Response:        domain.Session{},
		ResponseExample: sessionExample,
		Errors: []apidocs.ErrorDoc{
			docUnauthorized,
			docForbidden,
			docRoomNotBound,
			docNoActiveStream,
			docDeviceOffline,
			docDuplicateIdempotencyKey,
			docUpstreamUnavailable,
			docInternalError,
		},
	},
	{
		Method:          "POST",
		Path:            "/api/v1/rooms/{room_code}/camera/switch",
		Tag:             "Recording",
		Summary:         "Switch the active camera",
		Description:     "Use to make one of the room device's cameras active, legal at any time while the device is online. The body is {\"camera_enum\":N}. webcam-server queues the command and answers 202 Accepted with its command id; the device reports the outcome asynchronously, so dispatchub records the accepted command in camera_commands and the response is 202 with command_id and camera_enum. Failures: 400 invalid_request when camera_enum is missing or unknown to the device, 404 room_not_bound, 409 device_offline and 503 upstream_unavailable.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteRoom,
		Request:         cameraRequest{},
		RequestExample:  map[string]any{"camera_enum": 1},
		Response:        commandAck{},
		ResponseExample: cameraSwitchExample,
		Errors: []apidocs.ErrorDoc{
			docInvalidRequest,
			docUnauthorized,
			docForbidden,
			docRoomNotBound,
			docDeviceOffline,
			docUpstreamUnavailable,
			docInternalError,
		},
	},
	{
		Method:          "POST",
		Path:            "/api/v1/rooms/{room_code}/photo",
		Tag:             "Recording",
		Summary:         "Capture a snapshot",
		Description:     "Use to ask the room device for a single still image; legal while a stream is live and while the room is idle (an idle capture is stored with no session). webcam-server answers 202 with a request id and mints the photo id only when the device uploads the image, so dispatchub writes a session_photos ledger row and resolves the photo id asynchronously by polling the device's photo list; the response is 202 with session_photo_id, request_id and status pending. Read the resolved photo through GET /api/v1/sessions/{id}/artifacts once its status becomes resolved. The request has no body; X-Idempotency-Key is accepted. Failures: 404 room_not_bound, 409 device_offline and 503 upstream_unavailable.",
		Security:        "bearerAuth",
		PermissionNote:  permissionNoteRoom,
		Response:        photoAccepted{},
		ResponseExample: photoAcceptedExample,
		Errors: []apidocs.ErrorDoc{
			docUnauthorized,
			docForbidden,
			docRoomNotBound,
			docDeviceOffline,
			docUpstreamUnavailable,
			docInternalError,
		},
	},
}
