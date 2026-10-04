package httpx_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/httpx"
)

// problemBody is the decoded wire form of an RFC 9457 problem document.
type problemBody struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Reason   string `json:"reason"`
}

func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) (problemBody, map[string]json.RawMessage) {
	t.Helper()
	var body problemBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem body %q: %v", recorder.Body.String(), err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw problem body: %v", err)
	}
	return body, raw
}

func TestWriteProblemBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/rooms/A301", nil)
	recorder := httptest.NewRecorder()

	httpx.WriteProblem(recorder, request, http.StatusConflict, "room_not_bound")

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != httpx.ProblemContentType {
		t.Fatalf("Content-Type = %q, want %q", got, httpx.ProblemContentType)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	body, raw := decodeProblem(t, recorder)
	if body.Type != "about:blank" {
		t.Fatalf("type = %q, want about:blank", body.Type)
	}
	if body.Title != http.StatusText(http.StatusConflict) {
		t.Fatalf("title = %q, want %q", body.Title, http.StatusText(http.StatusConflict))
	}
	if body.Status != http.StatusConflict {
		t.Fatalf("status member = %d, want 409", body.Status)
	}
	if body.Detail != "room_not_bound" {
		t.Fatalf("detail = %q, want room_not_bound", body.Detail)
	}
	if body.Instance != "/api/v1/rooms/A301" {
		t.Fatalf("instance = %q, want the request path", body.Instance)
	}
	if _, ok := raw["reason"]; ok {
		t.Fatalf("body %s carries a reason member, want it omitted when empty", recorder.Body.String())
	}
}

func TestWriteProblemReasonCarriesTheReason(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/terms", nil)
	recorder := httptest.NewRecorder()

	httpx.WriteProblemReason(recorder, request, http.StatusForbidden, "permission denied", "dispatch:manage:any")

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	body, _ := decodeProblem(t, recorder)
	if body.Detail != "permission denied" || body.Reason != "dispatch:manage:any" {
		t.Fatalf("body = %+v, want detail permission denied and the permission reason", body)
	}
}

// TestWriteProblemHeadHasNoBody keeps HEAD responses well formed: headers and
// status are written, the body is not.
func TestWriteProblemHeadHasNoBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodHead, "/api/v1/rooms", nil)
	recorder := httptest.NewRecorder()

	httpx.WriteProblem(recorder, request, http.StatusNotFound, "session_not_found")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("HEAD problem body = %q, want empty", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != httpx.ProblemContentType {
		t.Fatalf("Content-Type = %q, want %q", got, httpx.ProblemContentType)
	}
}

// TestWriteProblemStatusTitleMapping covers the 413 shape callers produce when
// DecodeJSON rejects an oversized body.
func TestWriteProblemStatusTitleMapping(t *testing.T) {
	tests := []struct {
		status    int
		wantTitle string
	}{
		{http.StatusBadRequest, "Bad Request"},
		{http.StatusRequestEntityTooLarge, "Request Entity Too Large"},
		{http.StatusUnprocessableEntity, "Unprocessable Entity"},
		{http.StatusServiceUnavailable, "Service Unavailable"},
	}
	for _, tt := range tests {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/timetable/imports", nil)
		httpx.WriteProblem(recorder, request, tt.status, "invalid_request")
		body, _ := decodeProblem(t, recorder)
		if recorder.Code != tt.status || body.Status != tt.status || body.Title != tt.wantTitle {
			t.Fatalf("WriteProblem(%d) = status %d, body %+v; want %d / title %q", tt.status, recorder.Code, body, tt.status, tt.wantTitle)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	httpx.WriteJSON(recorder, http.StatusCreated, map[string]string{"term_code": "2026-FALL"})

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["term_code"] != "2026-FALL" {
		t.Fatalf("body = %q (err %v), want the encoded value", recorder.Body.String(), err)
	}
}

// TestWriteJSONMarshalFailureBecomesAProblem locks the documented fallback: an
// unencodable value answers a 500 problem instead of a truncated body.
func TestWriteJSONMarshalFailureBecomesAProblem(t *testing.T) {
	recorder := httptest.NewRecorder()
	httpx.WriteJSON(recorder, http.StatusOK, make(chan int))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != httpx.ProblemContentType {
		t.Fatalf("Content-Type = %q, want %q", got, httpx.ProblemContentType)
	}
	body, _ := decodeProblem(t, recorder)
	if body.Detail != "internal_error" || body.Status != http.StatusInternalServerError {
		t.Fatalf("problem = %+v, want internal_error/500", body)
	}
}

func TestDecodeJSONAcceptsBodyAndUnknownFields(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/terms", strings.NewReader(`{"term_code":"2026-FALL","week1_monday":"2026-03-02","weeks":16,"future_field":true}`))
	var target struct {
		TermCode    string `json:"term_code"`
		Week1Monday string `json:"week1_monday"`
		Weeks       int    `json:"weeks"`
	}
	if err := httpx.DecodeJSON(httptest.NewRecorder(), request, &target); err != nil {
		t.Fatalf("DecodeJSON() error = %v, want nil", err)
	}
	if target.TermCode != "2026-FALL" || target.Week1Monday != "2026-03-02" || target.Weeks != 16 {
		t.Fatalf("decoded value = %+v", target)
	}
}

func TestDecodeJSONRejectsMalformedBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/terms", strings.NewReader(`{"term_code":`))
	err := httpx.DecodeJSON(httptest.NewRecorder(), request, &struct{}{})
	if err == nil {
		t.Fatalf("DecodeJSON() error = nil, want a syntax error")
	}
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		t.Fatalf("DecodeJSON() error = %v, want a syntax error not a size error", err)
	}
}

// TestDecodeJSONOversizeWrapsMaxBytesError is the contract handlers use to
// answer 413: the returned error unwraps to *http.MaxBytesError.
func TestDecodeJSONOversizeWrapsMaxBytesError(t *testing.T) {
	payload := `{"padding":"` + strings.Repeat("a", int(httpx.MaxBodyBytes)) + `"}`
	if int64(len(payload)) <= httpx.MaxBodyBytes {
		t.Fatalf("test payload is %d bytes, want it to exceed MaxBodyBytes (%d)", len(payload), httpx.MaxBodyBytes)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/timetable/imports", strings.NewReader(payload))
	recorder := httptest.NewRecorder()

	var target struct {
		Padding string `json:"padding"`
	}
	err := httpx.DecodeJSON(recorder, request, &target)
	if err == nil {
		t.Fatalf("DecodeJSON() error = nil for a %d byte body", len(payload))
	}
	var maxBytes *http.MaxBytesError
	if !errors.As(err, &maxBytes) {
		t.Fatalf("DecodeJSON() error = %v, want it to unwrap to *http.MaxBytesError", err)
	}
	if maxBytes.Limit != httpx.MaxBodyBytes {
		t.Fatalf("MaxBytesError.Limit = %d, want %d", maxBytes.Limit, httpx.MaxBodyBytes)
	}
}
