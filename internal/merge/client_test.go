package merge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func staticToken(token string) TokenSource {
	return TokenSourceFunc(func(context.Context) (string, error) { return token, nil })
}

// capturedRequest is one request as the test handler saw it.
type capturedRequest struct {
	method        string
	path          string
	query         string
	authorization string
	accept        string
	contentType   string
	idempotency   string
	body          string
}

type recorder struct {
	mu    sync.Mutex
	items []capturedRequest
}

func (r *recorder) add(item capturedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, item)
}

func (r *recorder) all() []capturedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capturedRequest(nil), r.items...)
}

type testServer struct {
	client *Client
	rec    *recorder
	url    string
}

func newTestServer(t *testing.T, tokens TokenSource, handler http.HandlerFunc) *testServer {
	t.Helper()
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(capturedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			query:         r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			accept:        r.Header.Get("Accept"),
			contentType:   r.Header.Get("Content-Type"),
			idempotency:   r.Header.Get(idempotencyHeader),
			body:          string(body),
		})
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return &testServer{
		client: New(server.URL, tokens, 5*time.Second, discardLogger()),
		rec:    rec,
		url:    server.URL,
	}
}

// newAttemptServer is newTestServer plus a 1-based attempt counter and the
// captured requests, for retry and token-refresh assertions.
func newAttemptServer(t *testing.T, tokens TokenSource, handler func(attempt int, w http.ResponseWriter, r *http.Request)) (*Client, *recorder, *int32) {
	t.Helper()
	var count int32
	server := newTestServer(t, tokens, func(w http.ResponseWriter, r *http.Request) {
		handler(int(atomic.AddInt32(&count, 1)), w, r)
	})
	return server.client, server.rec, &count
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func abortHandler() http.HandlerFunc {
	return func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}
}

const createdJobBody = `{"id":"job1","status":"queued","stage":"queued","error":"",` +
	`"dest":{"bucket":"recordings","key":"merged/A301/2026-10-12/s1.mp4","sha256":"ab12","size_bytes":2048},` +
	`"source":{"device_id":"dev1","stream_id":"st1"},` +
	`"attempts":0,"max_attempts":3,"created_at":"2026-10-12T08:00:00Z","updated_at":"2026-10-12T08:00:00Z",` +
	`"started_at":"2026-10-12T08:00:01Z","finished_at":"2026-10-12T08:00:02Z"}`

// TestClientRequestShape pins the upstream route, method, body and headers of
// every typed call.
func TestClientRequestShape(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		status         int
		call           func(*Client) error
		method         string
		path           string
		body           string
		idempotencyKey string
		authorization  string // "" for a public route
	}{
		{
			name: "create with every member", status: http.StatusCreated,
			call: func(c *Client) error {
				job, err := c.Create(ctx, CreateRequest{
					DeviceID:       "dev1",
					StreamID:       "st1",
					FPS:            30,
					Codec:          "h264",
					DestKey:        "merged/A301/2026-10-12/s1.mp4",
					IdempotencyKey: "s1",
				})
				if err == nil && job.ID != "job1" {
					return fmt.Errorf("job id = %q, want job1", job.ID)
				}
				return err
			},
			method: http.MethodPost, path: "/api/v1/merges",
			body:           `{"device_id":"dev1","stream_id":"st1","fps":30,"codec":"h264","dest":{"key":"merged/A301/2026-10-12/s1.mp4"}}`,
			idempotencyKey: "s1",
			authorization:  "Bearer test-token",
		},
		{
			name: "create omits fps, codec and dest bucket", status: http.StatusCreated,
			call: func(c *Client) error {
				_, err := c.Create(ctx, CreateRequest{
					DeviceID: "dev1",
					StreamID: "st1",
					DestKey:  "merged/A301/2026-10-12/s1.mp4",
				})
				return err
			},
			method: http.MethodPost, path: "/api/v1/merges",
			body:          `{"device_id":"dev1","stream_id":"st1","dest":{"key":"merged/A301/2026-10-12/s1.mp4"}}`,
			authorization: "Bearer test-token",
		},
		{
			name: "get by id", status: http.StatusOK,
			call:   func(c *Client) error { _, err := c.Get(ctx, "job1"); return err },
			method: http.MethodGet, path: "/api/v1/merges/job1",
			authorization: "Bearer test-token",
		},
		{
			name: "ready probe is public", status: http.StatusOK,
			call:   func(c *Client) error { return c.Ready(ctx) },
			method: http.MethodGet, path: "/readyz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestServer(t, staticToken("test-token"), jsonHandler(tt.status, createdJobBody))
			if err := tt.call(server.client); err != nil {
				t.Fatalf("call error = %v, want nil", err)
			}
			requests := server.rec.all()
			if len(requests) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(requests))
			}
			got := requests[0]
			if got.method != tt.method {
				t.Fatalf("method = %s, want %s", got.method, tt.method)
			}
			if got.path != tt.path {
				t.Fatalf("path = %s, want %s", got.path, tt.path)
			}
			if got.authorization != tt.authorization {
				t.Fatalf("Authorization = %q, want %q", got.authorization, tt.authorization)
			}
			if got.accept != "application/json" {
				t.Fatalf("Accept = %q, want application/json", got.accept)
			}
			if got.idempotency != tt.idempotencyKey {
				t.Fatalf("X-Idempotency-Key = %q, want %q", got.idempotency, tt.idempotencyKey)
			}
			if tt.body == "" {
				if got.contentType != "" {
					t.Fatalf("Content-Type = %q on a bodyless call, want none", got.contentType)
				}
				if got.body != "" {
					t.Fatalf("body = %q on a bodyless call, want none", got.body)
				}
			} else {
				if got.contentType != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got.contentType)
				}
				if got.body != tt.body {
					t.Fatalf("body = %q, want %q", got.body, tt.body)
				}
			}
		})
	}
}

// TestReadyIsPublic pins that the readiness probe needs no credential: the
// caller may run it before a token source is usable (a doctor run against a
// half-configured fleet), and a probe that needs a token cannot report whether
// the service itself is up.
func TestReadyIsPublic(t *testing.T) {
	ctx := context.Background()
	failing := TokenSourceFunc(func(context.Context) (string, error) {
		return "", errors.New("readiness probe requires no service token")
	})

	t.Run("succeeds with a token source that always errors", func(t *testing.T) {
		server := newTestServer(t, failing, jsonHandler(http.StatusOK, ""))
		if err := server.client.Ready(ctx); err != nil {
			t.Fatalf("Ready() error = %v, want the public probe to pass", err)
		}
		requests := server.rec.all()
		if len(requests) != 1 {
			t.Fatalf("upstream saw %d requests, want 1", len(requests))
		}
		if got := requests[0].authorization; got != "" {
			t.Fatalf("Authorization = %q on the public probe, want none", got)
		}
	})

	t.Run("fails on a 503", func(t *testing.T) {
		server := newTestServer(t, failing, jsonHandler(http.StatusServiceUnavailable, `{"detail":"source_unavailable"}`))
		err := server.client.Ready(ctx)
		if !IsStatus(err, http.StatusServiceUnavailable) {
			t.Fatalf("Ready() error = %v, want a 503 UpstreamError", err)
		}
	})
}

func TestClientBaseURLTrailingSlashIsNormalized(t *testing.T) {
	server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, createdJobBody))
	client := New(server.url+"/", staticToken("t"), 5*time.Second, discardLogger())
	if _, err := client.Get(context.Background(), "job1"); err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if got := server.rec.all()[0].path; got != "/api/v1/merges/job1" {
		t.Fatalf("path = %q, want a single slash", got)
	}
}

// TestClientDecodesJobResponse covers the 201 job view, including the fields a
// poller reads to decide the next move.
func TestClientDecodesJobResponse(t *testing.T) {
	body := `{"id":"job1","status":"succeeded","stage":"muxing","error":"",` +
		`"dest":{"bucket":"recordings","key":"merged/A301/2026-10-12/s1.mp4","sha256":"ab12","size_bytes":2048},` +
		`"attempts":2,"max_attempts":3,"created_at":"2026-10-12T08:00:00Z","updated_at":"2026-10-12T08:05:00Z",` +
		`"started_at":"2026-10-12T08:00:01Z","finished_at":"2026-10-12T08:04:00Z"}`
	server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusCreated, body))
	job, err := server.client.Create(context.Background(), CreateRequest{DeviceID: "dev1", StreamID: "st1", DestKey: "k"})
	if err != nil {
		t.Fatalf("Create() error = %v, want nil", err)
	}
	if job.ID != "job1" || job.Status != JobSucceeded {
		t.Fatalf("job = %+v", job)
	}
	if job.Dest.Bucket != "recordings" || job.Dest.Key != "merged/A301/2026-10-12/s1.mp4" ||
		job.Dest.SizeBytes != 2048 || job.Dest.SHA256 != "ab12" {
		t.Fatalf("dest = %+v", job.Dest)
	}
	if job.Attempts != 2 || job.MaxAttempts != 3 {
		t.Fatalf("attempts = %d/%d, want 2/3", job.Attempts, job.MaxAttempts)
	}
	if job.StartedAt == nil || job.FinishedAt == nil || job.FinishedAt.IsZero() {
		t.Fatalf("timestamps = %+v", job)
	}

	t.Run("optional timestamps stay nil", func(t *testing.T) {
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, `{"id":"job2","status":"queued"}`))
		job, err := server.client.Get(context.Background(), "job2")
		if err != nil || job.Status != JobQueued || job.StartedAt != nil || job.FinishedAt != nil {
			t.Fatalf("Get() = %+v, err %v", job, err)
		}
	})
}

// TestUpstreamErrorMapping covers RFC 9457 decoding into *UpstreamError and the
// two inspection helpers, over the source_unavailable retry signal.
func TestUpstreamErrorMapping(t *testing.T) {
	ctx := context.Background()

	t.Run("problem document", func(t *testing.T) {
		body := `{"type":"about:blank","title":"Service Unavailable","status":503,"detail":"source_unavailable","instance":"/api/v1/merges"}`
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusServiceUnavailable, body))
		_, err := server.client.Create(ctx, CreateRequest{DeviceID: "dev1", StreamID: "st1", DestKey: "k"})
		var upstream *UpstreamError
		if !errors.As(err, &upstream) {
			t.Fatalf("error = %v, want *UpstreamError", err)
		}
		if upstream.Status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", upstream.Status)
		}
		if upstream.Detail != "source_unavailable" {
			t.Fatalf("detail = %q, want source_unavailable", upstream.Detail)
		}
		if upstream.Body == "" {
			t.Fatalf("body is empty, want a bounded copy for logs")
		}
		if !IsStatus(err, http.StatusServiceUnavailable) || IsStatus(err, http.StatusNotFound) {
			t.Fatalf("IsStatus did not answer for the decoded problem")
		}
		if got := DetailOf(err); got != "source_unavailable" {
			t.Fatalf("DetailOf() = %q, want source_unavailable", got)
		}
		// Callers wrap control errors; inspection must still work.
		wrapped := fmt.Errorf("submit merge: %w", err)
		if !IsStatus(wrapped, http.StatusServiceUnavailable) || DetailOf(wrapped) != "source_unavailable" {
			t.Fatalf("IsStatus/DetailOf do not unwrap the caller error")
		}
	})

	t.Run("non problem body falls back to the status text", func(t *testing.T) {
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusNotFound, "  no such job  "))
		_, err := server.client.Get(ctx, "job1")
		var upstream *UpstreamError
		if !errors.As(err, &upstream) {
			t.Fatalf("error = %v, want *UpstreamError", err)
		}
		if upstream.Detail != http.StatusText(http.StatusNotFound) {
			t.Fatalf("detail = %q, want %q", upstream.Detail, http.StatusText(http.StatusNotFound))
		}
		if upstream.Body != "no such job" {
			t.Fatalf("body = %q, want the trimmed payload", upstream.Body)
		}
	})

	t.Run("body is capped", func(t *testing.T) {
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusInternalServerError, strings.Repeat("x", 8192)))
		_, err := server.client.Get(ctx, "job1")
		var upstream *UpstreamError
		if !errors.As(err, &upstream) {
			t.Fatalf("error = %v, want *UpstreamError", err)
		}
		if len(upstream.Body) != maxErrorBodyBytes+len("…") || !strings.HasSuffix(upstream.Body, "…") {
			t.Fatalf("body length = %d, want the cap plus an ellipsis", len(upstream.Body))
		}
	})

	t.Run("foreign errors answer nothing", func(t *testing.T) {
		if IsStatus(nil, http.StatusServiceUnavailable) || DetailOf(nil) != "" {
			t.Fatalf("IsStatus/DetailOf answered for a nil error")
		}
		plain := errors.New("connection refused")
		if IsStatus(plain, http.StatusServiceUnavailable) || DetailOf(plain) != "" {
			t.Fatalf("IsStatus/DetailOf answered for a foreign error")
		}
	})
}

// TestRetryPolicy pins the unknown-safe retry rules: Create and Get repeat a
// transport failure and a 5xx, never a 4xx, and Ready never repeats.
func TestRetryPolicy(t *testing.T) {
	ctx := context.Background()

	t.Run("create retries a transport error twice", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			if attempt < 3 {
				abortHandler()(w, nil)
				return
			}
			jsonHandler(http.StatusCreated, createdJobBody)(w, nil)
		})
		if _, err := client.Create(ctx, CreateRequest{DeviceID: "dev1", StreamID: "st1", DestKey: "k"}); err != nil {
			t.Fatalf("Create() error = %v, want the third attempt to succeed", err)
		}
		if got := atomic.LoadInt32(count); got != 3 {
			t.Fatalf("attempts = %d, want 3", got)
		}
	})

	t.Run("create retries a 500", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			if attempt == 1 {
				jsonHandler(http.StatusInternalServerError, `{"detail":"service_unavailable"}`)(w, nil)
				return
			}
			jsonHandler(http.StatusCreated, createdJobBody)(w, nil)
		})
		if _, err := client.Create(ctx, CreateRequest{DeviceID: "dev1", StreamID: "st1", DestKey: "k"}); err != nil {
			t.Fatalf("Create() error = %v, want the retry to succeed", err)
		}
		if got := atomic.LoadInt32(count); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})

	t.Run("get retries a 503 source_unavailable", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			if attempt < 3 {
				jsonHandler(http.StatusServiceUnavailable, `{"detail":"source_unavailable"}`)(w, nil)
				return
			}
			jsonHandler(http.StatusOK, createdJobBody)(w, nil)
		})
		if _, err := client.Get(ctx, "job1"); err != nil {
			t.Fatalf("Get() error = %v, want the retry to succeed", err)
		}
		if got := atomic.LoadInt32(count); got != 3 {
			t.Fatalf("attempts = %d, want 3", got)
		}
	})

	t.Run("create does not retry a 400", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(_ int, w http.ResponseWriter, r *http.Request) {
			jsonHandler(http.StatusBadRequest, `{"detail":"invalid_request"}`)(w, r)
		})
		_, err := client.Create(ctx, CreateRequest{DeviceID: "dev1", StreamID: "st1", DestKey: "k"})
		if !IsStatus(err, http.StatusBadRequest) || DetailOf(err) != "invalid_request" {
			t.Fatalf("Create() error = %v, want a 400 invalid_request", err)
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1 (a 400 is a verdict)", got)
		}
	})

	t.Run("get does not retry a 404", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(_ int, w http.ResponseWriter, r *http.Request) {
			jsonHandler(http.StatusNotFound, `{"detail":"job_not_found"}`)(w, r)
		})
		_, err := client.Get(ctx, "job1")
		if !IsStatus(err, http.StatusNotFound) {
			t.Fatalf("Get() error = %v, want a 404", err)
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
	})

	t.Run("ready fails fast", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(_ int, w http.ResponseWriter, _ *http.Request) {
			abortHandler()(w, nil)
		})
		if err := client.Ready(ctx); err == nil {
			t.Fatalf("Ready() error = nil, want the transport failure")
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1 (a health probe fails fast)", got)
		}
	})
}

// refreshTokenSource records token and forced-refresh calls.
type refreshTokenSource struct {
	mu           sync.Mutex
	token        string
	refreshed    string
	tokenCalls   int
	refreshCalls int
}

func newRefreshTokenSource(stale, fresh string) *refreshTokenSource {
	return &refreshTokenSource{token: stale, refreshed: fresh}
}

func (s *refreshTokenSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenCalls++
	return s.token, nil
}

func (s *refreshTokenSource) Refresh(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls++
	s.token = s.refreshed
	return s.refreshed, nil
}

func (s *refreshTokenSource) calls() (tokenCalls, refreshCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenCalls, s.refreshCalls
}

// TestForcedTokenRefreshOn401 pins the "one forced refresh, one retry" rule and
// the local rejection of a missing credential.
func TestForcedTokenRefreshOn401(t *testing.T) {
	ctx := context.Background()

	t.Run("refresher source recovers", func(t *testing.T) {
		tokens := newRefreshTokenSource("stale", "fresh")
		client, requests, _ := newAttemptServer(t, tokens, func(_ int, w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer fresh" {
				jsonHandler(http.StatusUnauthorized, `{"detail":"invalid_token"}`)(w, r)
				return
			}
			jsonHandler(http.StatusOK, createdJobBody)(w, r)
		})
		if _, err := client.Get(ctx, "job1"); err != nil {
			t.Fatalf("Get() error = %v, want the forced refresh to recover", err)
		}
		captured := requests.all()
		if len(captured) != 2 {
			t.Fatalf("requests = %d, want 2 (original plus one retry)", len(captured))
		}
		if captured[1].authorization != "Bearer fresh" {
			t.Fatalf("retry Authorization = %q, want the refreshed token", captured[1].authorization)
		}
		tokenCalls, refreshCalls := tokens.calls()
		if tokenCalls != 1 || refreshCalls != 1 {
			t.Fatalf("token calls = %d, refresh calls = %d; want 1 and 1", tokenCalls, refreshCalls)
		}
	})

	t.Run("nil token source is rejected locally", func(t *testing.T) {
		server := newTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := server.client.Get(ctx, "job1")
		if err == nil || !strings.Contains(err.Error(), "no token source") {
			t.Fatalf("Get() error = %v, want the missing-token-source failure", err)
		}
		if got := len(server.rec.all()); got != 0 {
			t.Fatalf("requests = %d, want 0", got)
		}
	})
}

// TestResponseCap rejects an oversized upstream body instead of truncating it.
func TestResponseCap(t *testing.T) {
	server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, strings.Repeat("a", maxResponseBytes+1)))
	_, err := server.client.Get(context.Background(), "job1")
	if err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("Get() error = %v, want the response cap failure", err)
	}
	if got := len(server.rec.all()); got != 1 {
		t.Fatalf("attempts = %d, want 1 (a capped response is not retried)", got)
	}
}

// TestDecodeFailureIsReported keeps a malformed 2xx body from silently
// producing an empty result.
func TestDecodeFailureIsReported(t *testing.T) {
	server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, `{"id":`))
	_, err := server.client.Get(context.Background(), "job1")
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("Get() error = %v, want a decode failure", err)
	}
}
